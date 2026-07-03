package cli

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/iamtaehyunpark/livegit/internal/fuse"
	"github.com/iamtaehyunpark/livegit/internal/logx"
	"github.com/spf13/cobra"
)

// newMountCmd is the headless sibling of `lg shell`: it mounts the remote tree
// at local_root and returns, leaving a background process holding the mount
// until `lg unmount`. This is what makes the mount usable from scripts and
// coding agents — `lg shell` is interactive (it runs the user's shell on top
// of the mount), while `lg mount` needs no terminal at all.
func newMountCmd() *cobra.Command {
	var foreground bool
	c := &cobra.Command{
		Use:   "mount",
		Short: "Mount the remote tree at local_root (headless; `lg unmount` to stop)",
		Long: `Mount Source's tree at local_root without starting a shell.

The whole remote tree appears as a normal local folder: browse and edit it with
any tool, and changes sync both ways automatically. The mount is held by a
background process; it survives this command returning and stops with
'lg unmount'. Idempotent: if the tree is already mounted (by lg mount or a
running lg shell), this just reports it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if foreground {
				return runMountForeground()
			}
			return startMountDaemon()
		},
	}
	c.Flags().BoolVar(&foreground, "foreground", false, "hold the mount in this process (internal; used by the launcher)")
	_ = c.Flags().MarkHidden("foreground")
	return c
}

// startMountDaemon is the user-facing path: preflight, spawn the detached
// holder process, and wait until the mount is actually usable.
func startMountDaemon() error {
	c, err := loadGhost()
	if err != nil {
		return err
	}
	if c.LocalRoot == "" { // same fallback as setupProjectMount
		name := filepath.Base(strings.TrimRight(c.Source.RemoteRoot, "/"))
		c.LocalRoot = filepath.Join(filepath.Dir(config.Dir()), name)
	}

	// Idempotent: a live mount (ours or an lg shell's) is already the goal state.
	if fuse.IsMounted(c.LocalRoot) {
		fmt.Printf("already mounted at %s\n", c.LocalRoot)
		return nil
	}

	// Fail fast with actionable guidance when the connection needs a human
	// (Duo) — a mount with no connection would just be an empty tree. On a
	// terminal this bootstraps the connection interactively, like `lg shell`.
	if err := ensureAuthenticated(c); err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	// The holder's own logs go to lg.log via routeLogsToFile; its stdout/stderr
	// catch only pre-init prints and panics, so point them at the same file.
	logFile, err := os.OpenFile(config.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	holder := exec.Command(self, "mount", "--foreground")
	holder.Stdout, holder.Stderr = logFile, logFile
	holder.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // survive this process/terminal
	if err := holder.Start(); err != nil {
		return fmt.Errorf("start mount holder: %w", err)
	}
	died := make(chan struct{})
	go func() { _ = holder.Wait(); close(died) }()

	// Wait for the mount to appear (FUSE attach is quick; the tree fills in
	// behind it from the snapshot/first sync).
	deadline := time.After(20 * time.Second)
	for !fuse.IsMounted(c.LocalRoot) {
		select {
		case <-died:
			return fmt.Errorf("mount didn't come up — see %s", config.LogPath())
		case <-deadline:
			return fmt.Errorf("mount didn't come up within 20s — see %s", config.LogPath())
		case <-time.After(100 * time.Millisecond):
		}
	}

	// Soft-wait for the tree to be browsable (the snapshot loads instantly on a
	// re-mount; a first-ever sync can take a few seconds). Not fatal on
	// timeout — the mount is up and fills in as the sync completes.
	entries := 0
	treeWait := time.After(30 * time.Second)
wait:
	for {
		if ents, err := os.ReadDir(c.LocalRoot); err == nil && len(ents) > 0 {
			entries = len(ents)
			break
		}
		select {
		case <-treeWait:
			break wait
		case <-time.After(200 * time.Millisecond):
		}
	}

	if entries > 0 {
		fmt.Printf("✓ mounted %s (%d top-level entries)\n", c.LocalRoot, entries)
	} else {
		fmt.Printf("✓ mounted %s (tree still syncing — watch %s)\n", c.LocalRoot, config.LogPath())
	}
	fmt.Println("  Browse and edit it with normal file tools; changes sync automatically.")
	fmt.Println("  Stop with:  lg unmount")
	return nil
}

// runMountForeground holds the mount until it is unmounted (lg unmount, or a
// signal). It is only ever launched by startMountDaemon.
func runMountForeground() error {
	c, err := loadGhost()
	if err != nil {
		return err
	}
	mount, cleanup, _, err := setupProjectMount(c)
	if err != nil {
		return err
	}
	defer cleanup()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		cleanup()
		os.Exit(0)
	}()
	logx.For("mount").Info("holding mount until unmounted", "mountpoint", c.LocalRoot)
	mount.Wait() // returns when the kernel unmounts (lg unmount / umount)
	cleanup()
	return nil
}
