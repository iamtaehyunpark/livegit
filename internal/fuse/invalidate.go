package fuse

import (
	"os"

	"github.com/taehyun/lg/internal/config"
	"github.com/taehyun/lg/internal/proto"
)

// Invalidate handles a Source->Ghost change push: it keeps the full-tree index
// current (create/delete/rename/size-change) and drops any stale cached content
// so the next open refetches. With Source as the source of truth for content,
// there is no conflict apparatus — but a path with an unflushed local edit keeps
// its cached bytes until the flush worker pushes them (last-write-wins).
func (b *Backend) Invalidate(inv proto.Invalidate) {
	rel := config.Rel(inv.Rel)

	if inv.Deleted {
		b.index.Remove(rel)
		if !b.journal.HasPending(rel) {
			_ = os.Remove(b.cachePath(rel))
		}
		b.log.Debug("invalidate: deleted on source", "rel", rel)
		return
	}

	prev, had := b.index.Get(rel)
	b.index.Put(&Entry{
		Rel: rel, IsDir: inv.IsDir, Size: inv.Size,
		ModTime: inv.ModTime, Mode: inv.Mode, Hash: inv.Hash,
	})

	// Content changed on Source: drop our cached copy (next open refetches),
	// unless we have an unflushed local edit for this path.
	if !inv.IsDir && !b.journal.HasPending(rel) {
		if !had || prev.Hash != inv.Hash {
			_ = os.Remove(b.cachePath(rel))
			b.index.SetHaveContent(rel, false)
		}
	}
}
