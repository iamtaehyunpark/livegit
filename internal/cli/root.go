// Package cli wires the cobra commands that make up the `lg` binary. The same
// binary is Ghost or Source depending on `lg init --role` / the subcommand.
package cli

import (
	"fmt"

	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/iamtaehyunpark/livegit/internal/logx"
	"github.com/spf13/cobra"
)

var logLevel string

// Version is the build version, injected at link time via -ldflags
// "-X github.com/iamtaehyunpark/livegit/internal/cli.Version=...". Defaults to "dev".
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
		// Bare `lg`: greet first-time users and point them at `lg init`.
		RunE: func(cmd *cobra.Command, args []string) error {
			if !config.Exists() {
				fmt.Fprintln(cmd.OutOrStdout(),
					"👋 Welcome to lg!\n\nlg is project-local. cd into the project you want to work on and run:\n\n    lg init\n\nto set it up (it writes ./.lg/config.yaml and walks you through it).")
				return nil
			}
			return cmd.Help()
		},
	}
	root.SetVersionTemplate("lg {{.Version}}\n")
	root.PersistentFlags().StringVar(&logLevel, "log", "info", "log level: debug|info|warn|error")

	root.AddCommand(
		newInitCmd(),
		newConfigCmd(),
		newServeCmd(),
		newShellCmd(),
		newUnmountCmd(),
		newRunCmd(),
		newJobsCmd(),
		newLogsCmd(),
		newToggleCmd(),
		newLocalCmd(),
		newStatusCmd(),
		newHookCmd(),
	)
	return root
}

// IsKnownSubcommand reports whether name matches a registered subcommand (or a
// built-in like help/completion) of root. Used by the bare-command passthrough
// in main: `lg <anything-else>` runs <anything-else> on Source.
func IsKnownSubcommand(root *cobra.Command, name string) bool {
	switch name {
	case "help", "completion", "__complete", "__completeNoDesc":
		return true
	}
	for _, c := range root.Commands() {
		if c.Name() == name {
			return true
		}
		for _, a := range c.Aliases {
			if a == name {
				return true
			}
		}
	}
	return false
}

// RunPassthrough executes argv as a remote command and returns its exit code.
// main calls this when the first arg is not a known subcommand or flag.
func RunPassthrough(argv []string) int {
	logx.Init(logLevel, nil)
	return runRemote(argv, false, false) // explicit `lg <cmd>`: strict remote, no local fallback
}
