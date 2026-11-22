package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/punt-labs/mcp-proxy/internal/config"
	"github.com/punt-labs/mcp-proxy/internal/hook"
	"github.com/punt-labs/mcp-proxy/internal/transport"
	"github.com/punt-labs/punt-observe-go/logging"
	"github.com/punt-labs/punt-observe-go/telemetry"
)

const toolName = "mcp-proxy"

type profileRejectedError struct{ err error }

func (e *profileRejectedError) Error() string { return e.err.Error() }
func (e *profileRejectedError) Unwrap() error { return e.err }

func openDiagnostics(role string, stderr io.Writer) (*slog.Logger, func()) {
	level := slog.LevelInfo
	if debug := os.Getenv("MCP_PROXY_DEBUG"); debug == "1" || debug == "true" {
		level = slog.LevelDebug
	}
	handler, err := logging.New(toolName, role, logging.WithLevel(level))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "mcp-proxy: warning: could not initialize private diagnostics")
		return slog.New(slog.DiscardHandler), func() {}
	}
	return slog.New(handler).With("component", toolName+"."+role), func() { _ = handler.Close() }
}

func observeOperation(logger *slog.Logger, role string, settings config.Telemetry, run func() error) error {
	name := toolName + "." + role + ".run"
	const (
		success  telemetry.EnumMember = "success"
		failure  telemetry.EnumMember = "failure"
		canceled telemetry.EnumMember = "canceled"
	)
	client := telemetry.New(toolName, telemetry.Config{
		Disabled:      settings.Disabled,
		RetentionDays: settings.RetentionDays,
		Diagnostics:   logger.With("component", toolName+".telemetry"),
	}, telemetry.Schema{
		Name:    name,
		Version: 1,
		Signal:  telemetry.Span,
		Fields: map[string]telemetry.Field{
			"outcome": {Kind: telemetry.Enum, Members: []telemetry.EnumMember{success, failure, canceled}, Required: true},
		},
	})
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		client.Shutdown(ctx)
	}()

	logger.Info("Starting operation")
	start := time.Now()
	err := run()
	duration := time.Since(start)
	outcome := success
	var profileErr *profileRejectedError
	switch {
	case errors.As(err, &profileErr):
		outcome = failure
	case errors.Is(err, context.Canceled):
		outcome = canceled
	case err != nil:
		outcome = failure
		var invalidURL *transport.InvalidURLError
		var caCert *transport.CACertError
		switch {
		case isUsageError(err), errors.As(err, &invalidURL), errors.As(err, &caCert):
			logger.Warn("Rejected invalid connection configuration")
		case errors.Is(err, hook.ErrInvalidPayload):
			logger.Warn("Rejected malformed hook payload")
		default:
			logger.Error("Operation failed unexpectedly", "reason", transport.FailureReason(err))
		}
	}
	if profileErr != nil {
		logger.Warn("Operation finished", "outcome", string(outcome), "reason", "profile_rejected")
	} else {
		logger.Info("Operation finished", "outcome", string(outcome))
	}
	client.Record(telemetry.Observation{
		Name: name, Version: 1, Started: start, Duration: duration,
		Attributes: []telemetry.Value{telemetry.EnumValue("outcome", outcome)},
	})
	return err
}
