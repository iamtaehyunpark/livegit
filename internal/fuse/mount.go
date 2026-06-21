package fuse

import (
	"context"
	"fmt"
	"os"

	"github.com/hanwen/go-fuse/v2/fs"
	gofuse "github.com/hanwen/go-fuse/v2/fuse"
	"github.com/taehyun/lg/internal/logx"
)

// Mount mounts the virtual filesystem at mountpoint and starts the Backend's
// background workers (flush + eviction). It returns a Server whose Wait blocks
// until unmount. Requires a FUSE implementation present (macFUSE on darwin,
// libfuse on linux); mounting fails clearly if absent.
type Mount struct {
	server  *gofuse.Server
	backend *Backend
	cancel  context.CancelFunc
}

// NewMount creates the mount but does not block.
func NewMount(mountpoint string, b *Backend) (*Mount, error) {
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		return nil, err
	}
	root := &lgNode{b: b, rel: ""}
	opts := &fs.Options{}
	opts.MountOptions.FsName = "lg"
	opts.MountOptions.Name = "livegit"
	opts.MountOptions.AllowOther = false

	server, err := fs.Mount(mountpoint, root, opts)
	if err != nil {
		return nil, fmt.Errorf("mount %s: %w (is macFUSE/libfuse installed?)", mountpoint, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go b.RunFlush(ctx)
	go b.RunEviction(ctx)

	logx.For("fuse").Info("mounted", "mountpoint", mountpoint)
	return &Mount{server: server, backend: b, cancel: cancel}, nil
}

// Wait blocks until the filesystem is unmounted.
func (m *Mount) Wait() { m.server.Wait() }

// Unmount tears down the mount and stops workers.
func (m *Mount) Unmount() error {
	m.cancel()
	m.backend.Stop()
	return m.server.Unmount()
}
