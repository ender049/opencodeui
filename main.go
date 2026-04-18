package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "ocgo",
		Short: "ocgo frontend server",
		Long: `Static file server with API proxy to opencode backend.

Examples:
	  ocgo start                  # Start in background and manage opencode
	  ocgo start --foreground     # Run in foreground
	  ocgo start --external       # Use default external backend
	  ocgo start --backend 127.0.0.1:4096
	                                   # Use external opencode backend
	  ocgo start --path /srv/my-project --oc-port 4097
	  ocgo restart                # Restart server
	  ocgo status                # Check server status
	  ocgo stop                  # Stop server
	  ocgo serve                 # Internal foreground entrypoint
	  ocgo update                # Update tool/frontend
	  ocgo version               # Show versions
`,
	}

	rootCmd.AddCommand(startCmd, restartCmd, stopCmd, statusCmd, versionCmd, updateCmd, serveCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
