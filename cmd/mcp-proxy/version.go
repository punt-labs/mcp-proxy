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
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version and exit",
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.NoArgs(cmd, args); err != nil {
				return &usageError{msg: err.Error()}
			}
			return nil
		},
		RunE: func(_ *cobra.Command, _ []string) error {
			_, _ = fmt.Fprintf(stdout, "mcp-proxy %s\n", version)
			return nil
		},
	}
	cmd.SetFlagErrorFunc(wrapFlagError)
	return cmd
}
