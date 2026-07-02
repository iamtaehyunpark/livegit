package fuse

import (
	"context"
	"os"
	"time"

	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/iamtaehyunpark/livegit/internal/proto"
)

// Attr is the attribute view the FUSE node layer needs for one path.
type Attr struct {
	Exists  bool
	IsDir   bool
	Size    int64
	ModTime int64
	Mode    uint32
}

// Getattr resolves attributes for rel entirely from the full-tree index, so a
// file shows its real size before its bytes are ever fetched. If the
// local cache holds newer bytes (an unflushed edit), those win for size/mtime.
// A path missing from the index falls back to a Source stat when online (covers
// the brief window before the first tree sync completes).
func (b *Backend) Getattr(ctx context.Context, rel string) (Attr, error) {
	rel = config.Rel(rel)
	if rel == "" {
		return Attr{Exists: true, IsDir: true, Mode: 0o755}, nil
	}
	if e, ok := b.index.Get(rel); ok {
		if !e.IsDir && cacheFileExists(b.cachePath(rel)) {
			if info, err := os.Stat(b.cachePath(rel)); err == nil {
				return Attr{Exists: true, Size: info.Size(),
					ModTime: info.ModTime().Unix(), Mode: uint32(info.Mode().Perm())}, nil
			}
		}
		return Attr{Exists: true, IsDir: e.IsDir, Size: e.Size, ModTime: e.ModTime, Mode: e.Mode}, nil
	}
	// Not yet in the index. If online, ask Source so first lookups during the
	// initial sync still resolve; record what we learn.
	if !b.source.Online() {
		return Attr{Exists: false}, nil
	}
	st, err := b.source.Stat(ctx, rel)
	if err != nil {
		return Attr{}, err
	}
	if !st.Exists {
		return Attr{Exists: false}, nil
	}
	b.index.Put(&Entry{
		Rel: rel, IsDir: st.IsDir, Size: st.Size, ModTime: st.ModTime, Mode: st.Mode, Hash: st.Hash,
	})
	return Attr{Exists: true, IsDir: st.IsDir, Size: st.Size, ModTime: st.ModTime, Mode: st.Mode}, nil
}

// Readdir lists a directory from the full-tree index — the complete listing is
// always available locally, online or offline, no per-ls round-trip.
func (b *Backend) Readdir(ctx context.Context, rel string) ([]proto.DirEntry, error) {
	return b.index.Children(rel), nil
}

// MaterializeCtxTimeout is the default fetch timeout used by node Open.
const MaterializeCtxTimeout = 30 * time.Second
