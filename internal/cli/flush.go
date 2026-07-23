package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/iamtaehyunpark/livegit/internal/fuse"
	"github.com/spf13/cobra"
)

// newFlushCmd pushes pending journal writes to Source now and waits for them
// to land. With a live mount, the mount's flush worker is already pushing
// continuously — this just watches until the queue drains. Without a mount
// there IS no worker (pending writes would sit until the next `lg mount`), so
// the command connects and drains the journal itself.
func newFlushCmd() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "flush",
		Short: "Push pending writes to Source now and wait until they land",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadGhost()
			if err != nil {
				return err
			}
			pending, err := countPending()
			if err != nil {
				return err
			}
			if pending == 0 {
				fmt.Println("nothing pending — Source is up to date")
				return nil
			}

			deadline := time.Now().Add(timeout)
			expired := func() bool { return timeout > 0 && time.Now().After(deadline) }

			if fuse.IsMounted(c.MountDir()) {
				// The mount's worker owns the journal; watch it drain. Big
				// files upload at link speed — the count is the honest signal.
				fmt.Printf("%d pending write(s); the mount is pushing them…\n", pending)
				last := pending
				for {
					time.Sleep(1 * time.Second)
					n, err := countPending()
					if err != nil {
						return err
					}
					if n == 0 {
						fmt.Println("✓ all writes on Source")
						return nil
					}
					if n != last {
						fmt.Printf("  %d left…\n", n)
						last = n
					}
					if expired() {
						return fmt.Errorf("%d write(s) still pending after %s (they keep flushing in the background)", n, timeout)
					}
				}
			}

			// No mount: drain the journal ourselves over a fresh connection.
			fmt.Printf("%d pending write(s); no mount running — flushing directly…\n", pending)
			matcher, err := buildMatcher(c)
			if err != nil {
				return err
			}
			journal, err := openGhostJournal()
			if err != nil {
				return err
			}
			defer journal.Close()
			client, err := connectedClient(20 * time.Second)
			if err != nil {
				return err
			}
			defer client.Close()

			b := fuse.NewBackend(c, journal, fuse.NewClientSource(client), matcher)
			ctx := cmd.Context()
			if timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, deadline)
				defer cancel()
			}
			err = b.FlushAll(ctx, func(left int, rel string) {
				fmt.Printf("  pushing %s (%d left)…\n", rel, left)
			})
			if err != nil {
				return err
			}
			fmt.Println("✓ all writes on Source")
			return nil
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "give up after this long (0 = wait until done)")
	return cmd
}
