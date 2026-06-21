package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/taehyun/lg/internal/fuse"
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
			if err := fuse.ForceUnmount(c.LocalRoot); err != nil {
				return fmt.Errorf("unmount %s failed: %w", c.LocalRoot, err)
			}
			fmt.Fprintf(os.Stdout, "unmounted %s\n", c.LocalRoot)
			return nil
		},
	}
	return cmd
}
