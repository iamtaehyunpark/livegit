package fuse

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	gofuse "github.com/hanwen/go-fuse/v2/fuse"
	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/iamtaehyunpark/livegit/internal/logx"
)

// Mount mounts the virtual filesystem at mountpoint and starts the Backend's
// background workers (flush + eviction). It returns a Server whose Wait blocks
// until unmount. Requires a FUSE implementation present (macFUSE on darwin,
// libfuse on linux); mounting fails clearly if absent.
type Mount struct {
	server     *gofuse.Server
	backend    *Backend
	cancel     context.CancelFunc
	mountpoint string
}

// NewMount creates the mount but does not block.
func NewMount(mountpoint string, b *Backend) (*Mount, error) {
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		return nil, err
	}
	root := &lgNode{b: b, rel: ""}
	opts := &fs.Options{}
	// Let the kernel cache lookups/attrs briefly. Without these every path
	// component of every syscall round-trips into this process; all answers come
	// from the in-memory index anyway, and Source-side changes already arrive
	// asynchronously (watcher push), so ≤1s of kernel-side staleness changes
	// nothing observable while making ls/git/tab-completion far cheaper.
	second := time.Second
	opts.AttrTimeout = &second
	opts.EntryTimeout = &second
	opts.NegativeTimeout = &second
	opts.MountOptions.FsName = "lg"
	opts.MountOptions.Name = "livegit"
	opts.MountOptions.AllowOther = false
	if runtime.GOOS == "darwin" {
		// macFUSE: refuse AppleDouble companions (._*, .DS_Store) in the kernel,
		// so Finder's per-folder junk probes never even reach this process.
		// Standard practice for network filesystems (sshfs ships with it).
		opts.MountOptions.Options = append(opts.MountOptions.Options, "noappledouble")
	}

	server, err := fs.Mount(mountpoint, root, opts)
	if err != nil {
		return nil, fmt.Errorf("mount %s: %w (is macFUSE/libfuse installed?)", mountpoint, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go b.RunFlush(ctx)
	go b.RunEviction(ctx)
	go b.RunTreeSync(ctx)
	go b.runSnapshotSaver(ctx)

	// `lg cancel` control: the mount process advertises its pid, and SIGUSR1
	// aborts every in-flight download. Registered here so both `lg mount` and
	// `lg shell` mounts respond. (Old holders never wrote a pidfile, so a new
	// `lg cancel` can't accidentally SIGUSR1-kill a handler-less process.)
	writeMountPid()
	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				signal.Stop(usr1)
				return
			case <-usr1:
				// A request file narrows the signal to specific downloads
				// (`lg cancel <path>`); absent/empty means cancel everything.
				if rels := takeCancelRequests(); len(rels) > 0 {
					n := 0
					for _, rel := range rels {
						if b.CancelFetch(rel) {
							n++
						}
					}
					logx.For("fuse").Info("canceled requested downloads", "count", n, "requested", len(rels))
				} else {
					n := b.CancelFetches()
					logx.For("fuse").Info("canceled in-flight downloads", "count", n)
				}
			}
		}
	}()

	logx.For("fuse").Info("mounted", "mountpoint", mountpoint)
	return &Mount{server: server, backend: b, cancel: cancel, mountpoint: mountpoint}, nil
}

// MountPidPath is where a live mount records its pid (`lg cancel` reads it).
func MountPidPath() string { return filepath.Join(config.Dir(), "run", "mount.pid") }

// CancelReqPath carries the rel list for a targeted cancel: `lg cancel <path>`
// writes it just before SIGUSR1, the handler consumes it (one rel per line).
func CancelReqPath() string { return filepath.Join(config.Dir(), "run", "cancel.req") }

func takeCancelRequests() []string {
	b, err := os.ReadFile(CancelReqPath())
	if err != nil {
		return nil
	}
	_ = os.Remove(CancelReqPath())
	var rels []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			rels = append(rels, line)
		}
	}
	return rels
}

func writeMountPid() {
	p := MountPidPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(p, []byte(strconv.Itoa(os.Getpid())), 0o644)
}

// runSnapshotSaver periodically persists the metadata index so browsing survives
// an unclean exit / offline restart.
func (b *Backend) runSnapshotSaver(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			b.index.SaveSnapshot()
			return
		case <-b.stop:
			b.index.SaveSnapshot()
			return
		case <-t.C:
			b.index.SaveSnapshot()
		}
	}
}

// Wait blocks until the filesystem is unmounted.
func (m *Mount) Wait() { m.server.Wait() }

// Unmount tears down the mount and stops workers.
func (m *Mount) Unmount() error {
	logx.For("fuse").Info("unmount requested", "mountpoint", m.mountpoint)
	_ = os.Remove(MountPidPath())
	m.cancel()
	m.backend.Stop()
	err := m.server.Unmount()
	// go-fuse's Unmount can return while the unmount is still in-flight; the
	// serving goroutines then die with the process before the kernel completes
	// it, orphaning the mount. Wait briefly for the clean unmount to take, then
	// force-unmount as a backstop so we never leave a stale mount on exit.
	for i := 0; i < 20; i++ {
		if !IsMounted(m.mountpoint) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	logx.For("fuse").Warn("clean unmount didn't take; forcing", "mountpoint", m.mountpoint, "err", err)
	return ForceUnmount(m.mountpoint)
}
