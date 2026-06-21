package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/taehyun/lg/internal/config"
	"github.com/taehyun/lg/internal/fuse"
	"github.com/taehyun/lg/internal/shell"
	"github.com/taehyun/lg/internal/transport"
)

// newEnterSourceCmd performs SOURCE-mode entry (§5.3). The shell integration
// rewrites a trigger command into `lg enter-source --via X -- <cmd>`, so this
// runs in the foreground with the terminal, ready to bridge.
func newEnterSourceCmd() *cobra.Command {
	var via string
	c := &cobra.Command{
		Use:   "enter-source --via <kind> -- <command>",
		Short: "Switch this shell into SOURCE mode (bridge to remote tmux)",
		RunE: func(cmd *cobra.Command, args []string) error {
			command := strings.Join(args, " ")
			cfg, err := loadGhost()
			if err != nil {
				return err
			}
			tabID := os.Getenv("LG_TAB_ID")
			if tabID == "" {
				return fmt.Errorf("not inside an lg shell (LG_TAB_ID unset)")
			}
			project := os.Getenv("LG_PROJECT")
			if project == "" {
				project = projectID(cfg)
			}

			client := newClient(cfg)
			defer client.Close()

			// Wait briefly for the connection to come up.
			if err := waitOnline(client, 10*time.Second); err != nil {
				if cfg.Offline.OnSourceTrigger == "error" {
					return fmt.Errorf("offline: %w", err)
				}
				// queue policy: run the command locally as a fallback rather than block.
				fmt.Fprintln(os.Stderr, "lg: offline, running command locally")
				return nil
			}

			// §5.3 step 2: flush barrier — ensure Source has our latest edits for
			// the current directory before executing there.
			cwd, _ := os.Getwd()
			relDir := relDirOf(config.NewPathMapper(cfg), cwd)
			if err := flushBarrier(relDir, 10*time.Second, client); err != nil {
				fmt.Fprintf(os.Stderr, "lg: flush barrier: %v (continuing)\n", err)
			}

			st := shell.LoadState(tabID)
			st.SetSource(shell.EnteredVia(via), relDir, "")
			_ = st.Save()

			sessName, berr := shell.Bridge(client, project, tabID, command)

			// Back to LOCAL once the remote session detaches/exits (§5.4).
			st = shell.LoadState(tabID)
			st.Session = sessName
			st.SetLocal()
			_ = st.Save()
			return berr
		},
	}
	c.Flags().StringVar(&via, "via", "pattern", "what triggered entry (conda|venv|poetry|dir:..|always)")
	return c
}

// waitOnline blocks until the client reports online or the timeout elapses.
func waitOnline(client *transport.Client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if client.Status().Online() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("connection not established")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// flushBarrier polls the on-disk journal until no pending entries remain for
// relDir (the cross-process barrier; the flush worker lives in `lg shell`).
func flushBarrier(relDir string, timeout time.Duration, client *transport.Client) error {
	deadline := time.Now().Add(timeout)
	for {
		pending, err := fuse.ScanPendingDir(config.JournalPath(), relDir)
		if err != nil {
			return err
		}
		if !pending {
			return nil
		}
		if time.Now().After(deadline) {
			return fuse.ErrBarrierTimeout
		}
		if !client.Status().Online() {
			return fuse.ErrBarrierOffline
		}
		time.Sleep(20 * time.Millisecond)
	}
}
