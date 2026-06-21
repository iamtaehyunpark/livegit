package fuse

import (
	"context"
	"os"
	"time"

	"github.com/taehyun/lg/internal/config"
	"github.com/taehyun/lg/internal/proto"
)

// Invalidate handles a Source->Ghost change push (§4.3). No content arrives with
// the push; Ghost decides lazily based on the file's local state.
func (b *Backend) Invalidate(inv proto.Invalidate) {
	rel := config.Rel(inv.Rel)
	meta, _ := b.store.Get(rel)

	if inv.Deleted {
		if meta != nil {
			_ = os.Remove(b.cachePath(rel))
			_ = b.store.Delete(rel)
		}
		b.log.Debug("invalidate: deleted on source", "rel", rel)
		return
	}

	switch {
	case meta == nil || meta.State == StateGhost:
		// Ghost state: just mark metadata stale; refetch lazily on next open.
		_ = b.store.Upsert(&Meta{
			Path:           rel,
			State:          StateGhost,
			ContentHash:    inv.Hash,
			LastModifiedBy: "source",
			LastModifiedAt: inv.ModTime,
			LastAccessedAt: time.Now().Unix(),
		})
	case meta.State == StateCached:
		// Actively cached (and not dirty): refetch immediately to reflect it.
		if inv.Hash != "" && inv.Hash == meta.ContentHash {
			return // already current
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Force a fresh fetch by dropping to ghost first, then materializing.
		_ = b.store.SetState(rel, StateGhost)
		if _, err := b.Materialize(ctx, rel); err != nil {
			b.log.Warn("invalidate refetch failed", "rel", rel, "err", err)
		}
	case meta.State == StateLive:
		// Local has unflushed edits AND source changed: a true conflict. The
		// flush worker will detect the BaseHash mismatch and Source will back up
		// its copy (§4.4). Don't clobber the local edit by refetching here.
		b.log.Warn("invalidate while live (pending local edits); flush will resolve", "rel", rel)
	}
}
