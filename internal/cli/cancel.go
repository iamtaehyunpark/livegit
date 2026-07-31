package cli

import (
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/iamtaehyunpark/livegit/internal/fuse"
	"github.com/spf13/cobra"
)

// newCancelCmd stops every in-flight download and removes staging leftovers.
// A live mount is told to abort via SIGUSR1 (targeted through its pidfile);
// each canceled fetch cleans up its own staging, and a final sweep removes
// stale .lg-tmp files from crashed/interrupted fetches. Waiting readers get
// EIO; re-opening a file simply starts a fresh download.
func newCancelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cancel",
		Short: "Stop all in-flight downloads and delete stale staging files",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadGhost()
			if err != nil {
				return err
			}
			if pid := mountPid(); pid > 0 && fuse.IsMounted(c.MountDir()) &&
				syscall.Kill(pid, 0) == nil {
				if err := syscall.Kill(pid, syscall.SIGUSR1); err == nil {
					fmt.Println("told the mount to abort its downloads…")
					// Canceled fetches remove their own staging within moments.
					for i := 0; i < 30 && len(downloads()) > 0; i++ {
						time.Sleep(100 * time.Millisecond)
					}
				}
			}
			n, freed := sweepStaging()
			switch {
			case n > 0:
				fmt.Printf("✓ downloads stopped; removed %d staging file(s), freed %s\n", n, fmtMB(freed))
			default:
				fmt.Println("✓ no downloads in flight, nothing to clean")
			}
			return nil
		},
	}
}

// mountPid reads the live mount's pidfile (0 = none).
func mountPid() int {
	b, err := os.ReadFile(fuse.MountPidPath())
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return pid
}

// sweepStaging removes every .lg-tmp staging file left in the cache.
func sweepStaging() (int, int64) {
	var n int
	var freed int64
	dir := config.CacheDir()
	_ = filepath.WalkDir(dir, func(p string, d iofs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".lg-tmp") {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			if os.Remove(p) == nil {
				n++
				freed += info.Size()
			}
		}
		return nil
	})
	return n, freed
}
