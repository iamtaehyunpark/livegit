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

// newCancelCmd stops in-flight downloads — all of them, or just the given
// paths. A live mount is told to abort via SIGUSR1 (targeted rels ride in a
// request file). Waiting readers get EIO. Plain `lg cancel` also sweeps ALL
// staging leftovers (a clean slate); targeted cancels keep their staging so a
// later open resumes where it stopped.
func newCancelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cancel [path...]",
		Short: "Stop in-flight downloads (all, or just the given paths)",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadGhost()
			if err != nil {
				return err
			}
			holderLive := false
			pid := mountPid()
			if pid > 0 && fuse.IsMounted(c.MountDir()) && syscall.Kill(pid, 0) == nil {
				holderLive = true
			}

			if len(args) > 0 {
				rels, err := resolveDownloadArgs(args, downloads(), c.MountDir())
				if err != nil {
					return err
				}
				if !holderLive {
					return fmt.Errorf("no mount is running — nothing is downloading")
				}
				if err := os.WriteFile(fuse.CancelReqPath(), []byte(strings.Join(rels, "\n")+"\n"), 0o644); err != nil {
					return err
				}
				if err := syscall.Kill(pid, syscall.SIGUSR1); err != nil {
					_ = os.Remove(fuse.CancelReqPath())
					return err
				}
				// Staging stays (resume), so the entry remains visible in
				// lg status; the fetch itself dies within moments.
				time.Sleep(300 * time.Millisecond)
				for _, rel := range rels {
					fmt.Printf("✓ canceled %s (staging kept — re-opening resumes)\n", rel)
				}
				return nil
			}

			active := len(downloads())
			if holderLive {
				if err := syscall.Kill(pid, syscall.SIGUSR1); err == nil && active > 0 {
					for i := 0; i < 30 && len(downloads()) > 0; i++ {
						time.Sleep(100 * time.Millisecond)
					}
				}
			}
			// Cancel-all is the clean-slate command: drop every staging file,
			// resumable or not (crashed fetches, dead holders included).
			swept, freed := sweepStaging()
			switch {
			case active > 0 && swept > 0:
				fmt.Printf("✓ canceled %d download(s); removed %d staging file(s) (%s)\n", active, swept, fmtMB(freed))
			case active > 0:
				fmt.Printf("✓ canceled %d download(s)\n", active)
			case swept > 0:
				fmt.Printf("✓ no downloads in flight; removed %d staging file(s), freed %s\n", swept, fmtMB(freed))
			default:
				fmt.Println("✓ no downloads in flight, nothing to clean")
			}
			return nil
		},
	}
}

// resolveDownloadArgs maps user-typed paths onto in-flight download rels.
// Accepted forms: the exact rel `lg status` shows, a trailing fragment of it
// (just the filename works), or an absolute/mount path. Ambiguity errors with
// the candidates; no match errors with a pointer at lg status.
func resolveDownloadArgs(args []string, dls []inflight, mountDir string) ([]string, error) {
	var rels []string
	for _, arg := range args {
		norm := filepath.ToSlash(arg)
		if abs, err := filepath.Abs(arg); err == nil {
			if under, err := filepath.Rel(mountDir, abs); err == nil && !strings.HasPrefix(under, "..") {
				norm = filepath.ToSlash(under)
			}
		}
		var matches []string
		for _, d := range dls {
			if d.rel == norm || strings.HasSuffix(d.rel, "/"+norm) {
				matches = append(matches, d.rel)
			}
		}
		switch len(matches) {
		case 0:
			return nil, fmt.Errorf("no in-flight download matches %q (see `lg status`)", arg)
		case 1:
			rels = append(rels, matches[0])
		default:
			return nil, fmt.Errorf("%q matches %d downloads: %s — be more specific", arg, len(matches), strings.Join(matches, ", "))
		}
	}
	return rels, nil
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
		if err != nil || d.IsDir() || !strings.Contains(filepath.Base(p), ".lg-tmp") {
			return nil // staging is <rel>.lg-tmp plus its .id sidecar
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
