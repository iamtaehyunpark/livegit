// Package cli wires the cobra commands that make up the `lg` binary. The same
// binary is Ghost or Source depending on `lg init --role` / the subcommand.
package cli

import (
	"github.com/spf13/cobra"
	"github.com/taehyun/lg/internal/logx"
)

var logLevel string

// Version is the build version, injected at link time via -ldflags
// "-X github.com/taehyun/lg/internal/cli.Version=...". Defaults to "dev".
var Version = "dev"

// NewRoot builds the root command tree.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "lg",
		Short:         "Live Git — real-time codebase sync + remote execution (Ghost <-> Source)",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			logx.Init(logLevel, nil)
		},
	}
	root.SetVersionTemplate("lg {{.Version}}\n")
	root.PersistentFlags().StringVar(&logLevel, "log", "info", "log level: debug|info|warn|error")

	root.AddCommand(
		newInitCmd(),
		newServeCmd(),
		newShellCmd(),
		newLocalCmd(),
		newStatusCmd(),
		newSessionsCmd(),
		newEnterSourceCmd(),
		newHookCmd(),
	)
	return root
}
