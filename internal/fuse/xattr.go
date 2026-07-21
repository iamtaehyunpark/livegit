package fuse

// Ghost-side extended-attribute (xattr) support.
//
// macOS Finder cannot copy, duplicate, or drag files into the mount without
// it: every Finder copy goes through copyfile(3), which creates the
// destination and then setxattr's com.apple.FinderInfo / com.apple.quarantine
// onto it. go-fuse's default for an unimplemented Setxattr is ENOATTR, which
// aborts copyfile — Finder reports error 100093 (100000 + ENOATTR) and leaves
// a zero-byte file behind (found live 2026-07-20 under ~/sclab: Finder
// Duplicate and drag-in both failed while shell `cp` worked).
//
// The attributes themselves are macOS bookkeeping with no meaning on the Linux
// Source, so they are held in memory for the lifetime of the mount and never
// synced or persisted. Returning ENOTSUP instead would also unblock copyfile,
// but then Finder falls back to AppleDouble "._*" sidecar files, which would
// litter Source — accepting and remembering the attrs is the quiet option.

import (
	"context"
	"sync"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	gofuse "github.com/hanwen/go-fuse/v2/fuse"
	"github.com/iamtaehyunpark/livegit/internal/config"
)

// errNoAttr is ENOATTR on darwin and its alias ENODATA on linux.
var errNoAttr = syscall.Errno(gofuse.ENOATTR)

// xattrStore maps rel path -> attr name -> value. Zero value is ready to use.
type xattrStore struct {
	mu sync.Mutex
	m  map[string]map[string][]byte
}

func (s *xattrStore) set(rel, attr string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = make(map[string]map[string][]byte)
	}
	if s.m[rel] == nil {
		s.m[rel] = make(map[string][]byte)
	}
	s.m[rel][attr] = append([]byte(nil), data...)
}

func (s *xattrStore) get(rel, attr string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.m[rel][attr]
	return data, ok
}

func (s *xattrStore) list(rel string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.m[rel]))
	for name := range s.m[rel] {
		names = append(names, name)
	}
	return names
}

func (s *xattrStore) remove(rel, attr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[rel][attr]; !ok {
		return false
	}
	delete(s.m[rel], attr)
	return true
}

// forget drops all attrs for rel and everything under it (delete/rmdir).
func (s *xattrStore) forget(rel string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for p := range s.m {
		if p == rel || underPrefix(p, rel) {
			delete(s.m, p)
		}
	}
}

// rename moves the attrs of oldRel (and its subtree) to the new location.
func (s *xattrStore) rename(oldRel, newRel string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for p, attrs := range s.m {
		switch {
		case p == oldRel:
			delete(s.m, p)
			s.m[newRel] = attrs
		case underPrefix(p, oldRel):
			delete(s.m, p)
			s.m[newRel+p[len(oldRel):]] = attrs
		}
	}
}

// underPrefix reports whether p lies strictly inside directory dir.
func underPrefix(p, dir string) bool {
	return dir != "" && len(p) > len(dir)+1 && p[:len(dir)] == dir && p[len(dir)] == '/'
}

var (
	_ = (fs.NodeGetxattrer)((*lgNode)(nil))
	_ = (fs.NodeSetxattrer)((*lgNode)(nil))
	_ = (fs.NodeListxattrer)((*lgNode)(nil))
	_ = (fs.NodeRemovexattrer)((*lgNode)(nil))
)

func (n *lgNode) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	data, ok := n.b.xattrs.get(n.rel, attr)
	if !ok {
		return 0, errNoAttr
	}
	if len(dest) < len(data) {
		return uint32(len(data)), syscall.ERANGE
	}
	copy(dest, data)
	return uint32(len(data)), 0
}

// Setxattr always accepts. XATTR_CREATE/XATTR_REPLACE flags are ignored —
// copyfile and Finder never pass them, and honoring them buys nothing here.
func (n *lgNode) Setxattr(ctx context.Context, attr string, data []byte, flags uint32) syscall.Errno {
	n.b.xattrs.set(n.rel, attr, data)
	return 0
}

func (n *lgNode) Listxattr(ctx context.Context, dest []byte) (uint32, syscall.Errno) {
	var size int
	names := n.b.xattrs.list(n.rel)
	for _, name := range names {
		size += len(name) + 1 // null-terminated, per listxattr(2)
	}
	if len(dest) < size {
		return uint32(size), syscall.ERANGE
	}
	off := 0
	for _, name := range names {
		off += copy(dest[off:], name)
		dest[off] = 0
		off++
	}
	return uint32(size), 0
}

func (n *lgNode) Removexattr(ctx context.Context, attr string) syscall.Errno {
	if !n.b.xattrs.remove(n.rel, attr) {
		return errNoAttr
	}
	return 0
}

// xattrRename / xattrForget keep the store aligned with renames and deletes.
// Called from RecordRename / RecordDelete so every path (file, dir subtree)
// goes through one place.
func (b *Backend) xattrRename(oldRel, newRel string) {
	b.xattrs.rename(config.Rel(oldRel), config.Rel(newRel))
}

func (b *Backend) xattrForget(rel string) {
	b.xattrs.forget(config.Rel(rel))
}
