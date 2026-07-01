package cli

import (
	"os"

	"github.com/spf13/cobra"
	"github.com/taehyun/lg/internal/shell"
)

// newHookCmd groups the fast, short-lived callbacks the shell integration runs
// per command. They must be cheap: the precmd/accept-line hooks call them on
// every prompt.
func newHookCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "hook", Short: "shell-integration callbacks (internal)", Hidden: true}
	cmd.AddCommand(newHookIsToggledCmd())
	return cmd
}

func newHookIsToggledCmd() *cobra.Command {
	var tab string
	c := &cobra.Command{
		Use:   "is-toggled",
		Short: "exit 0 if the tab has toggle mode on",
		RunE: func(cmd *cobra.Command, args []string) error {
			if shell.ToggleOn(tab) {
				return nil
			}
			os.Exit(1)
			return nil
		},
	}
	c.Flags().StringVar(&tab, "tab", "", "terminal tab id")
	return c
}
