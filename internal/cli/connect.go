package cli

import (
	"fmt"

	"github.com/iamtaehyunpark/livegit/internal/transport"
	"github.com/spf13/cobra"
)

// newConnectCmd establishes lg's own ssh ControlMaster to Source. On a Duo/2FA
// host this is where the (single) prompt happens — the connection is then cached
// for `source.control_persist` (default 8h) and every later `lg` command reuses
// it without re-prompting. Idempotent: a no-op when a master is already live.
func newConnectCmd() *cobra.Command {
	var check, stop bool
	c := &cobra.Command{
		Use:   "connect",
		Short: "Authenticate to Source once (handles Duo/2FA), then reuse the connection",
		Long: `Open (and cache) the ssh connection to Source.

On a server with Duo/2FA, this is the one place the prompt appears: approve it
once and lg keeps the authenticated connection alive (source.control_persist,
default 8h). Every later 'lg <cmd>', 'lg run', and 'lg shell' multiplexes over
it with no second prompt.

You rarely need to run this by hand — 'lg <cmd>' and 'lg shell' auto-connect on
a terminal. Use it to pre-authenticate (e.g. before a scripted run or a coding
agent that can't answer a Duo prompt), or to check/reset the connection.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadGhost()
			if err != nil {
				return err
			}

			// Native/password auth doesn't use a master — connections carry the
			// stored credentials directly. Nothing to establish.
			if cfg.Source.SSHMode == "native" || cfg.Source.Auth == "password" {
				fmt.Println("This project authenticates with the native ssh client (native/password mode);")
				fmt.Println("there is no ssh master to establish — connections use the stored credentials.")
				return nil
			}

			switch {
			case stop:
				if err := transport.StopMaster(cfg); err != nil {
					return fmt.Errorf("stop connection: %w", err)
				}
				fmt.Println("ssh connection closed — the next lg command will re-authenticate.")
				return nil
			case check:
				if transport.MasterLive(cfg) {
					fmt.Printf("connection: live (cached; new commands to %s won't re-prompt)\n", cfg.Source.Host)
				} else {
					fmt.Printf("connection: down — run `lg connect` to authenticate to %s\n", cfg.Source.Host)
				}
				return nil
			}

			if transport.MasterLive(cfg) {
				fmt.Printf("Already connected to %s (reusing the cached ssh connection).\n", cfg.Source.Host)
				return nil
			}
			fmt.Printf("Connecting to %s — approve the Duo/2FA prompt if one appears…\n", cfg.Source.Host)
			if err := transport.Connect(cfg); err != nil {
				return err
			}
			fmt.Printf("✓ connected. Cached for %s; further lg commands won't re-prompt.\n", transport.PersistLabel(cfg))
			return nil
		},
	}
	c.Flags().BoolVar(&check, "check", false, "just report whether the connection is live")
	c.Flags().BoolVar(&stop, "stop", false, "close the cached connection (next command re-authenticates)")
	return c
}
