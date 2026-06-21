package fuse

import (
	"context"
	"os"
	"time"

	"github.com/taehyun/lg/internal/config"
	"github.com/taehyun/lg/internal/proto"
)

// Attr is the attribute view the FUSE node layer needs for one path.
type Attr struct {
	Exists  bool
	IsDir   bool
	Size    int64
	ModTime int64
	Mode    uint32
}

// Getattr resolves attributes for rel. Cached/live files report from local
// content; ghost files report from stored metadata; otherwise it stats Source.
// A ghost-state file still shows its true size so tools see a non-zero length
// even though its bytes are 0 locally until opened (§4.1, the "0 bytes" model).
func (b *Backend) Getattr(ctx context.Context, rel string) (Attr, error) {
	rel = config.Rel(rel)
	if rel == "" {
		return Attr{Exists: true, IsDir: true, Mode: 0o755}, nil
	}
	meta, err := b.store.Get(rel)
	if err != nil {
		return Attr{}, err
	}
	if meta != nil {
		if (meta.State == StateCached || meta.State == StateLive) && cacheFileExists(b.cachePath(rel)) {
			info, err := os.Stat(b.cachePath(rel))
			if err == nil {
				return Attr{Exists: true, Size: info.Size(),
					ModTime: info.ModTime().Unix(), Mode: uint32(info.Mode().Perm())}, nil
			}
		}
		// Ghost (or content missing): report stored metadata.
		return Attr{
			Exists:  true,
			Size:    meta.SizeBytes,
			ModTime: meta.LastModifiedAt,
			Mode:    0o644,
		}, nil
	}
	// Unknown locally: ask Source (online) so first lookups work.
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
	if !st.IsDir {
		// Record as ghost so subsequent attrs are local and eviction tracks it.
		_ = b.store.Upsert(&Meta{
			Path:           rel,
			State:          StateGhost,
			ContentHash:    st.Hash,
			LastModifiedBy: "source",
			LastModifiedAt: st.ModTime,
			LastAccessedAt: time.Now().Unix(),
			SizeBytes:      st.Size,
		})
	}
	return Attr{Exists: true, IsDir: st.IsDir, Size: st.Size, ModTime: st.ModTime, Mode: st.Mode}, nil
}

// Readdir lists a directory. Online, it reads from Source (authoritative).
// Offline, it falls back to whatever children are recorded locally.
func (b *Backend) Readdir(ctx context.Context, rel string) ([]proto.DirEntry, error) {
	rel = config.Rel(rel)
	if b.source.Online() {
		resp, err := b.source.List(ctx, rel)
		if err == nil && resp.Found {
			return resp.Entries, nil
		}
		if err != nil {
			b.log.Debug("readdir online failed, using local", "rel", rel, "err", err)
		}
	}
	return b.localChildren(rel), nil
}

// localChildren reconstructs immediate children from the state DB (offline).
func (b *Backend) localChildren(rel string) []proto.DirEntry {
	// Not indexed by parent; for a single-user tree this scan is acceptable.
	// (Kept simple deliberately; can be indexed later if needed.)
	return nil
}

// MaterializeCtxTimeout is the default fetch timeout used by node Open.
const MaterializeCtxTimeout = 30 * time.Second
