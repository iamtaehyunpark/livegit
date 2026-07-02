package fuse

import (
	"context"
	"os"
	"time"

	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/iamtaehyunpark/livegit/internal/proto"
)

// RunFlush is the background worker that drains the journal to Source. It is the
// single write path for both online and offline: online it flushes
// within ms; offline it parks until the journal is woken on reconnect.
func (b *Backend) RunFlush(ctx context.Context) {
	for {
		// Drain everything currently flushable.
		for {
			e, ok := b.journal.Peek()
			if !ok {
				break
			}
			if !b.source.Online() {
				break // park; reconnect will Wake the journal
			}
			if err := b.flushEntry(ctx, e); err != nil {
				b.log.Debug("flush deferred", "rel", e.Rel, "err", err)
				break // transient (e.g. dropped mid-flush); retry on next wake
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-b.stop:
			return
		case <-b.journal.Notify():
		case <-time.After(2 * time.Second): // safety re-check
		}
	}
}

func (b *Backend) flushEntry(ctx context.Context, e JournalEntry) error {
	// Drop entries for ignored paths (e.g. a .DS_Store journaled before the guard
	// existed): never push them to Source, just clear them from the journal.
	if b.ignored(e.Rel) {
		return b.journal.Ack(e.Seq)
	}
	switch e.Op {
	case OpDelete:
		// Last-write-wins: empty BaseHash means Source just removes it.
		if _, err := b.source.Delete(ctx, proto.DelReq{Rel: e.Rel, BaseHash: e.BaseHash}); err != nil {
			return err
		}
		return b.journal.Ack(e.Seq)

	default: // write / create
		content, err := os.ReadFile(b.cachePath(e.Rel))
		if err != nil {
			if os.IsNotExist(err) {
				// File vanished before flush (e.g. deleted); drop entry.
				return b.journal.Ack(e.Seq)
			}
			return err
		}
		ack, err := b.source.Write(ctx, proto.WriteReq{
			Rel:      e.Rel,
			Content:  content,
			BaseHash: e.BaseHash, // empty: last-write-wins, Source overwrites
			ModTime:  e.ModTime,
			Mode:     e.Mode,
		})
		if err != nil {
			return err
		}
		// Record the synced hash so a later Source invalidation for the same
		// content doesn't needlessly drop our cache.
		if ack.NewHash != "" {
			b.index.Put(&Entry{
				Rel: e.Rel, Size: int64(len(content)), ModTime: e.ModTime,
				Mode: e.Mode, Hash: ack.NewHash, HaveContent: true,
			})
		}
		return b.journal.Ack(e.Seq)
	}
}

// FlushBarrier blocks until no pending journal entries remain for relDir, or the
// timeout elapses. This is the SOURCE-mode-entry barrier:
// before running commands on Source, guarantee Source has the latest edits.
func (b *Backend) FlushBarrier(ctx context.Context, relDir string, timeout time.Duration) error {
	relDir = config.Rel(relDir)
	b.journal.Wake()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if !b.journal.PendingForDir(relDir) {
			return nil
		}
		if !b.source.Online() {
			return ErrBarrierOffline
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return ErrBarrierTimeout
		case <-tick.C:
		}
	}
}
