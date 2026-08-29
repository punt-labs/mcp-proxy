package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// version is overridden at build time via `-ldflags -X main.version=<semver>`.
var version = "dev"

// newVersionCmd exposes `mcp-proxy version` per cli.md §Layer 2.
func newVersionCmd(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version and exit",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			_, _ = fmt.Fprintf(stdout, "mcp-proxy %s\n", version)
			return nil
		},
	}
}
