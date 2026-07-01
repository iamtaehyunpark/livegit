package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/taehyun/lg/internal/shell"
)

// newToggleCmd flips toggle mode for the current shell tab (§1.2). When on,
// every command typed in this shell is sent to Source; when off, it's a normal
// local shell. `lg local` is an explicit alias for "turn toggle off".
func newToggleCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "toggle",
		Short: "Toggle sending every command in this shell to Source",
		RunE: func(cmd *cobra.Command, args []string) error {
			tabID := os.Getenv("LG_TAB_ID")
			if tabID == "" {
				return fmt.Errorf("not inside an lg shell (run `lg shell` first)")
			}
			on := !shell.ToggleOn(tabID)
			if err := shell.SetToggle(tabID, on); err != nil {
				return err
			}
			if on {
				fmt.Fprintln(os.Stderr, "lg: toggle ON — every command now runs on Source. `lg toggle` again to stop.")
			} else {
				fmt.Fprintln(os.Stderr, "lg: toggle OFF — back to a local shell.")
			}
			return nil
		},
	}
}

// newLocalCmd is the explicit "turn toggle off" alias.
func newLocalCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "local",
		Short: "Turn toggle mode off (run commands locally again)",
		RunE: func(cmd *cobra.Command, args []string) error {
			tabID := os.Getenv("LG_TAB_ID")
			if tabID == "" {
				return fmt.Errorf("not inside an lg shell")
			}
			if err := shell.SetToggle(tabID, false); err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "lg: toggle OFF — back to a local shell.")
			return nil
		},
	}
}
