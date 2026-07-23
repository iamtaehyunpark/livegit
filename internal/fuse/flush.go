package fuse

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/iamtaehyunpark/livegit/internal/proto"
)

// RunFlush is the background worker that drains the journal to Source. It is the
// single write path for both online and offline: online it flushes
// within ms; offline it parks until the journal is woken on reconnect.
func (b *Backend) RunFlush(ctx context.Context) {
	// Track consecutive failures of the head entry: the queue drains oldest-
	// first, so a persistently failing head blocks everything behind it.
	// Surface that at Warn (once, to avoid a line every 2s) instead of only
	// Debug — a wedged journal was invisible in lg.log at INFO.
	var failSeq uint64
	failCount := 0
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
				if e.Seq != failSeq {
					failSeq, failCount = e.Seq, 0
				}
				failCount++
				if failCount == 5 {
					b.log.Warn("flush stuck: entry keeps failing and blocks the queue",
						"rel", e.Rel, "op", e.Op, "attempts", failCount, "err", err)
				} else {
					b.log.Debug("flush deferred", "rel", e.Rel, "err", err)
				}
				break // transient (e.g. dropped mid-flush); retry on next wake
			}
			if e.Seq == failSeq {
				failSeq, failCount = 0, 0 // head recovered
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
	case OpMkdir:
		if _, err := b.source.Write(ctx, proto.WriteReq{
			Rel: e.Rel, IsDir: true, Mode: e.Mode, ModTime: e.ModTime,
		}); err != nil {
			return err
		}
		return b.journal.Ack(e.Seq)

	case OpDelete:
		// Last-write-wins: empty BaseHash means Source just removes it.
		ack, err := b.source.Delete(ctx, proto.DelReq{Rel: e.Rel, BaseHash: e.BaseHash})
		if err != nil {
			return err
		}
		if !ack.OK {
			// Source declined (e.g. changed since journaled, or dir not empty
			// there). Drop the entry — the next tree sync re-shows the path —
			// but say so; silently keeping it would wedge the queue forever.
			b.log.Warn("delete not applied on source, dropping journal entry",
				"rel", e.Rel, "msg", ack.Message)
		}
		return b.journal.Ack(e.Seq)

	default: // write / create
		cp := b.cachePath(e.Rel)
		st, err := os.Stat(cp)
		if err != nil {
			if os.IsNotExist(err) {
				// File vanished before flush (e.g. deleted); drop entry.
				return b.journal.Ack(e.Seq)
			}
			return err
		}
		// Streamed from disk in chunks (resumable) — never a whole-file buffer.
		ack, err := b.source.WriteFile(ctx, proto.WriteReq{
			Rel:      e.Rel,
			BaseHash: e.BaseHash, // empty: last-write-wins, Source overwrites
			ModTime:  e.ModTime,
			Mode:     e.Mode,
		}, cp)
		if err != nil {
			return err
		}
		// Record the synced hash so a later Source invalidation for the same
		// content doesn't needlessly drop our cache.
		if ack.NewHash != "" {
			b.index.Put(&Entry{
				Rel: e.Rel, Size: st.Size(), ModTime: e.ModTime,
				Mode: e.Mode, Hash: ack.NewHash, HaveContent: true,
			})
		}
		return b.journal.Ack(e.Seq)
	}
}

// FlushAll drains the journal synchronously, reporting each entry as it goes.
// This is `lg flush` when no mount is up — without a mount there is no flush
// worker, so pending writes would otherwise sit until the next `lg mount`.
func (b *Backend) FlushAll(ctx context.Context, progress func(left int, rel string)) error {
	for {
		e, ok := b.journal.Peek()
		if !ok {
			return nil
		}
		if progress != nil {
			progress(b.journal.PendingCount(), e.Rel)
		}
		if err := b.flushEntry(ctx, e); err != nil {
			return fmt.Errorf("flushing %s: %w", e.Rel, err)
		}
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
