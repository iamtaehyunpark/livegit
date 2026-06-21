package fuse

import (
	"context"
	"os"
	"sync"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	gofuse "github.com/hanwen/go-fuse/v2/fuse"
	"github.com/taehyun/lg/internal/config"
)

// lgNode is one inode in the virtual tree. It translates syscalls into Backend
// calls; all ghost/cached/live policy lives in the Backend, not here.
type lgNode struct {
	fs.Inode
	b   *Backend
	rel string // canonical rel path of this node ("" = root)
}

var (
	_ = (fs.NodeGetattrer)((*lgNode)(nil))
	_ = (fs.NodeLookuper)((*lgNode)(nil))
	_ = (fs.NodeReaddirer)((*lgNode)(nil))
	_ = (fs.NodeOpener)((*lgNode)(nil))
	_ = (fs.NodeCreater)((*lgNode)(nil))
	_ = (fs.NodeUnlinker)((*lgNode)(nil))
	_ = (fs.NodeMkdirer)((*lgNode)(nil))
)

func (n *lgNode) child(name string) string {
	if n.rel == "" {
		return config.Rel(name)
	}
	return config.Rel(n.rel + "/" + name)
}

func fillAttr(out *gofuse.Attr, a Attr) {
	if a.IsDir {
		out.Mode = syscall.S_IFDIR | (a.Mode & 0o777)
		if a.Mode == 0 {
			out.Mode = syscall.S_IFDIR | 0o755
		}
	} else {
		mode := a.Mode & 0o777
		if mode == 0 {
			mode = 0o644
		}
		out.Mode = syscall.S_IFREG | mode
	}
	out.Size = uint64(a.Size)
	if a.ModTime > 0 {
		out.Mtime = uint64(a.ModTime)
		out.Atime = uint64(a.ModTime)
		out.Ctime = uint64(a.ModTime)
	}
}

func (n *lgNode) Getattr(ctx context.Context, fh fs.FileHandle, out *gofuse.AttrOut) syscall.Errno {
	a, err := n.b.Getattr(ctx, n.rel)
	if err != nil {
		return syscall.EIO
	}
	if !a.Exists {
		return syscall.ENOENT
	}
	fillAttr(&out.Attr, a)
	return 0
}

func (n *lgNode) Lookup(ctx context.Context, name string, out *gofuse.EntryOut) (*fs.Inode, syscall.Errno) {
	rel := n.child(name)
	a, err := n.b.Getattr(ctx, rel)
	if err != nil {
		return nil, syscall.EIO
	}
	if !a.Exists {
		return nil, syscall.ENOENT
	}
	fillAttr(&out.Attr, a)
	mode := uint32(syscall.S_IFREG)
	if a.IsDir {
		mode = syscall.S_IFDIR
	}
	child := n.NewInode(ctx, &lgNode{b: n.b, rel: rel}, fs.StableAttr{Mode: mode})
	return child, 0
}

func (n *lgNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	entries, err := n.b.Readdir(ctx, n.rel)
	if err != nil {
		return nil, syscall.EIO
	}
	list := make([]gofuse.DirEntry, 0, len(entries))
	for _, e := range entries {
		mode := uint32(syscall.S_IFREG)
		if e.IsDir {
			mode = syscall.S_IFDIR
		}
		list = append(list, gofuse.DirEntry{Name: e.Name, Mode: mode})
	}
	return fs.NewListDirStream(list), 0
}

func (n *lgNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	cp, err := n.b.Materialize(ctx, n.rel)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, syscall.ENOENT
		}
		return nil, 0, syscall.EIO
	}
	osFlags := int(flags) &^ syscall.O_CREAT
	f, err := os.OpenFile(cp, osFlags, 0o644)
	if err != nil {
		return nil, 0, fs.ToErrno(err)
	}
	writable := osFlags&(os.O_WRONLY|os.O_RDWR) != 0
	return &lgHandle{f: f, b: n.b, rel: n.rel, writable: writable}, 0, 0
}

func (n *lgNode) Create(ctx context.Context, name string, flags, mode uint32, out *gofuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	rel := n.child(name)
	cp := n.b.cachePath(rel)
	if err := os.MkdirAll(parentDir(cp), 0o755); err != nil {
		return nil, nil, 0, fs.ToErrno(err)
	}
	f, err := os.OpenFile(cp, int(flags)|os.O_CREATE, os.FileMode(mode&0o777))
	if err != nil {
		return nil, nil, 0, fs.ToErrno(err)
	}
	// Register as live immediately so a zero-byte create still flushes.
	_ = n.b.markLiveNew(rel, mode)
	a, _ := n.b.Getattr(ctx, rel)
	fillAttr(&out.Attr, a)
	child := n.NewInode(ctx, &lgNode{b: n.b, rel: rel}, fs.StableAttr{Mode: syscall.S_IFREG})
	return child, &lgHandle{f: f, b: n.b, rel: rel, writable: true, dirty: true}, 0, 0
}

func (n *lgNode) Mkdir(ctx context.Context, name string, mode uint32, out *gofuse.EntryOut) (*fs.Inode, syscall.Errno) {
	rel := n.child(name)
	if err := os.MkdirAll(n.b.cachePath(rel), os.FileMode(mode&0o777)); err != nil {
		return nil, fs.ToErrno(err)
	}
	out.Attr.Mode = syscall.S_IFDIR | (mode & 0o777)
	child := n.NewInode(ctx, &lgNode{b: n.b, rel: rel}, fs.StableAttr{Mode: syscall.S_IFDIR})
	return child, 0
}

func (n *lgNode) Unlink(ctx context.Context, name string) syscall.Errno {
	rel := n.child(name)
	if err := n.b.RecordDelete(rel); err != nil {
		return syscall.EIO
	}
	return 0
}

// lgHandle is an open file. Reads/writes hit the local cache file directly;
// on release of a dirty handle the write is journaled (§4.2).
type lgHandle struct {
	f        *os.File
	b        *Backend
	rel      string
	writable bool

	mu    sync.Mutex
	dirty bool
}

var (
	_ = (fs.FileReader)((*lgHandle)(nil))
	_ = (fs.FileWriter)((*lgHandle)(nil))
	_ = (fs.FileFlusher)((*lgHandle)(nil))
	_ = (fs.FileReleaser)((*lgHandle)(nil))
	_ = (fs.FileFsyncer)((*lgHandle)(nil))
)

func (h *lgHandle) Read(ctx context.Context, dest []byte, off int64) (gofuse.ReadResult, syscall.Errno) {
	return gofuse.ReadResultFd(uintptr(h.f.Fd()), off, len(dest)), 0
}

func (h *lgHandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	nw, err := h.f.WriteAt(data, off)
	if err != nil {
		return uint32(nw), fs.ToErrno(err)
	}
	h.mu.Lock()
	h.dirty = true
	h.mu.Unlock()
	return uint32(nw), 0
}

func (h *lgHandle) Fsync(ctx context.Context, flags uint32) syscall.Errno {
	return fs.ToErrno(h.f.Sync())
}

func (h *lgHandle) Flush(ctx context.Context) syscall.Errno {
	return h.journalIfDirty()
}

func (h *lgHandle) Release(ctx context.Context) syscall.Errno {
	errno := h.journalIfDirty()
	_ = h.f.Close()
	return errno
}

// journalIfDirty records the write-through on close/flush (§4.2).
func (h *lgHandle) journalIfDirty() syscall.Errno {
	h.mu.Lock()
	dirty := h.dirty
	h.dirty = false
	h.mu.Unlock()
	if !dirty {
		return 0
	}
	_ = h.f.Sync()
	if err := h.b.RecordWrite(h.rel); err != nil {
		return syscall.EIO
	}
	return 0
}

func parentDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}
