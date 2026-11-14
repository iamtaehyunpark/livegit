package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/iamtaehyunpark/livegit/internal/fuse"
	"github.com/iamtaehyunpark/livegit/internal/transport"
	"github.com/spf13/cobra"
)

func newUninitCmd() *cobra.Command {
	var yes bool
	var keepRemote bool

	cmd := &cobra.Command{
		Use:   "uninit",
		Short: "Remove lg from this project (deletes remote agent first, then local .lg state)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := config.Load()
			if err != nil {
				return err
			}
			lgDir := config.Dir()
			if lgDir == "" {
				return config.ErrNotSetUp
			}

			if !yes {
				fmt.Printf("This will remove lg from %s", filepath.Dir(lgDir))
				if c.Role == config.RoleGhost && !keepRemote {
					fmt.Printf(" and delete ~/.local/bin/lg on %s", c.Source.Host)
				}
				fmt.Print(".\nProceed? [y/N]: ")
				r := bufio.NewReader(os.Stdin)
				ans, _ := r.ReadString('\n')
				ans = strings.TrimSpace(strings.ToLower(ans))
				if ans != "y" && ans != "yes" {
					fmt.Println("aborted")
					return nil
				}
			}

			// 1. Delete lg from remote first (while config + connection still exist).
			if c.Role == config.RoleGhost && !keepRemote {
				if msg, err := transport.RemoveAgent(c); err != nil {
					fmt.Fprintf(os.Stderr, "⚠ could not remove remote ~/.local/bin/lg on %s: %v (continuing local cleanup)\n", c.Source.Host, err)
				} else {
					fmt.Printf("✓ %s (%s)\n", msg, c.Source.Host)
				}
			}

			// 2. Uninit local: unmount, disconnect, clean up marker-gated GUIDE.md,
			// remove empty mount dir, and remove .lg/.
			return uninitLocal(c, lgDir)
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation prompt")
	cmd.Flags().BoolVar(&keepRemote, "keep-remote", false, "keep ~/.local/bin/lg on the remote server")
	return cmd
}

func uninitLocal(c *config.Config, lgDir string) error {
	if c.Role == config.RoleGhost {
		mp := c.MountDirFrom(lgDir)
		if fuse.IsMounted(mp) || fuse.IsStaleMount(mp) {
			if err := fuse.ForceUnmount(mp); err != nil {
				return fmt.Errorf("unmount %s: %w", mp, err)
			}
			fmt.Printf("✓ unmounted %s\n", mp)
		}
		if c.Source.SSHMode != "native" {
			_ = closeConnection(c)
		}
		// Only remove the mount directory if it is empty (never delete user files).
		if err := os.Remove(mp); err == nil {
			fmt.Printf("✓ removed empty mount dir %s\n", mp)
		}
	}

	// Remove lg-managed GUIDE.md only if it still carries the lg:doc marker.
	projRoot := filepath.Dir(lgDir)
	guidePath := filepath.Join(projRoot, "GUIDE.md")
	if b, err := os.ReadFile(guidePath); err == nil && docVersion(b) != "" {
		if err := os.Remove(guidePath); err == nil {
			fmt.Printf("✓ removed %s\n", guidePath)
		}
	}

	if err := os.RemoveAll(lgDir); err != nil {
		return fmt.Errorf("remove %s: %w", lgDir, err)
	}
	fmt.Printf("✓ removed %s\n", lgDir)
	return nil
}
