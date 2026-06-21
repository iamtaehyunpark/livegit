package fuse

import (
	"context"
	"errors"
	"os"
	"time"
)

// Barrier sentinel errors.
var (
	ErrBarrierTimeout = errors.New("flush barrier: timed out")
	ErrBarrierOffline = errors.New("flush barrier: went offline")
)

// RunEviction is the LRU eviction daemon (§4.2). Periodically it returns cached,
// idle, non-dirty files to ghost state by deleting their local content. live
// files and files with pending journal entries are never evicted.
func (b *Backend) RunEviction(ctx context.Context) {
	interval := time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.stop:
			return
		case <-time.After(interval):
			b.EvictOnce()
		}
	}
}

// EvictOnce performs one eviction sweep. Exposed for tests.
func (b *Backend) EvictOnce() {
	idle := time.Duration(b.cfg.Cache.EvictAfterIdleMinutes) * time.Minute
	cutoff := time.Now().Add(-idle).Unix()
	candidates, err := b.store.EvictCandidates(cutoff)
	if err != nil {
		b.log.Warn("evict query failed", "err", err)
		return
	}
	for _, m := range candidates {
		if b.journal.HasPending(m.Path) {
			continue // §4.2: never evict while unflushed entries remain
		}
		cp := b.cachePath(m.Path)
		if err := os.Remove(cp); err != nil && !os.IsNotExist(err) {
			b.log.Warn("evict remove failed", "rel", m.Path, "err", err)
			continue
		}
		if err := b.store.SetState(m.Path, StateGhost); err != nil {
			b.log.Warn("evict state update failed", "rel", m.Path, "err", err)
			continue
		}
		b.log.Debug("evicted to ghost", "rel", m.Path, "bytes", m.SizeBytes)
	}
	b.enforceSizeCap()
}

// enforceSizeCap evicts the least-recently-accessed cached files until total
// materialized size is under max_cache_size_gb (§4.2 disk-pressure trigger).
func (b *Backend) enforceSizeCap() {
	capBytes := int64(b.cfg.Cache.MaxCacheSizeGB) << 30
	if capBytes <= 0 {
		return
	}
	total, err := b.store.CachedSizeBytes()
	if err != nil || total <= capBytes {
		return
	}
	// Evict oldest cached entries regardless of idle threshold until under cap.
	candidates, err := b.store.EvictCandidates(time.Now().Unix())
	if err != nil {
		return
	}
	for _, m := range candidates {
		if total <= capBytes {
			break
		}
		if b.journal.HasPending(m.Path) {
			continue
		}
		if err := os.Remove(b.cachePath(m.Path)); err != nil && !os.IsNotExist(err) {
			continue
		}
		_ = b.store.SetState(m.Path, StateGhost)
		total -= m.SizeBytes
		b.log.Debug("evicted under size cap", "rel", m.Path)
	}
}

// CacheUsage reports total materialized bytes (for `lg status`).
func (b *Backend) CacheUsage() (int64, error) { return b.store.CachedSizeBytes() }

// Stop signals background workers to exit.
func (b *Backend) Stop() { close(b.stop) }
