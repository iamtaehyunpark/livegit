package cli

import (
	"fmt"
	"os"

	"github.com/iamtaehyunpark/livegit/internal/fuse"
	"github.com/spf13/cobra"
)

// newUnmountCmd tears down the FUSE mount at local_root. Handy after a crash or
// a force-killed `lg shell` left a stale mount (symptom: "device not configured"
// / ENXIO when touching the mount path).
func newUnmountCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "unmount",
		Aliases: []string{"umount"},
		Short:   "Unmount the lg FUSE filesystem (fixes a stale/leftover mount)",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadGhost()
			if err != nil {
				return err
			}
			// Idempotent: nothing mounted is the goal state, not an error. A
			// STALE mount (dead holder) still needs the force path though, and
			// IsMounted can't see one (its stat fails), so check separately.
			if !fuse.IsMounted(c.LocalRoot) && !fuse.IsStaleMount(c.LocalRoot) {
				fmt.Printf("nothing mounted at %s\n", c.LocalRoot)
				return nil
			}
			if err := fuse.ForceUnmount(c.LocalRoot); err != nil {
				return fmt.Errorf("unmount %s failed: %w", c.LocalRoot, err)
			}
			fmt.Fprintf(os.Stdout, "unmounted %s\n", c.LocalRoot)
			return nil
		},
	}
	return cmd
}
