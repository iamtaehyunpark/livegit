package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/taehyun/lg/internal/shell"
)

// newLocalCmd is the forced-exit escape hatch (§5.4): always returns the tab to
// LOCAL mode. (The normal SOURCE exit is detaching the remote tmux session.)
func newLocalCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "local",
		Short: "Force this shell back to LOCAL mode",
		RunE: func(cmd *cobra.Command, args []string) error {
			tabID := os.Getenv("LG_TAB_ID")
			if tabID == "" {
				return fmt.Errorf("not inside an lg shell (LG_TAB_ID unset)")
			}
			st := shell.LoadState(tabID)
			st.SetLocal()
			if err := st.Save(); err != nil {
				return err
			}
			fmt.Fprintln(os.Stdout, "lg: switched to LOCAL mode")
			return nil
		},
	}
}
