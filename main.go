package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "opencodeui",
		Short: "OpenCodeUI Frontend Server",
		Long: `Static file server with API proxy to opencode backend.

Examples:
  opencodeui start                  # Start in background and manage opencode
  opencodeui start --foreground     # Run in foreground
  opencodeui start --external       # Use default external backend
  opencodeui start --backend 127.0.0.1:4096
                                   # Use external opencode backend
  opencodeui start --path /srv/my-project --oc-port 4097
  opencodeui restart                # Restart server
  opencodeui status                # Check server status
  opencodeui stop                  # Stop server
  opencodeui serve                 # Internal foreground entrypoint
  opencodeui update                # Update tool/frontend
  opencodeui version               # Show versions
`,
	}

	rootCmd.AddCommand(startCmd, restartCmd, stopCmd, statusCmd, versionCmd, updateCmd, serveCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
