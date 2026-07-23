package fuse

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/iamtaehyunpark/livegit/internal/config"
)

// Barrier sentinel errors.
var (
	ErrBarrierTimeout = errors.New("flush barrier: timed out")
	ErrBarrierOffline = errors.New("flush barrier: went offline")
)

// cachedFile is one materialized content file on local disk.
type cachedFile struct {
	rel   string
	size  int64
	atime time.Time
}

// RunEviction periodically keeps the on-disk content cache under the configured
// size cap. Unlike the old model, eviction only removes *content* bytes — the
// full-tree metadata index always stays complete, so an evicted file is still
// listed with its real size and simply refetches on next open.
func (b *Backend) RunEviction(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.stop:
			return
		case <-time.After(time.Minute):
			b.EvictOnce()
		}
	}
}

// EvictOnce performs one eviction sweep: first drops content idle longer than
// cache.evict_after_idle_minutes (so the cache doesn't grow without bound while
// under the size cap — it used to), then enforces the size cap LRU-first.
// Files with unflushed edits (their bytes are the only copy until the flush
// worker pushes them) and in-flight downloads are never touched.
func (b *Backend) EvictOnce() {
	files, total := b.scanCache()

	evict := func(f cachedFile) bool {
		if b.journal.HasPending(f.rel) || b.fetchActive(f.rel) {
			return false
		}
		if err := os.Remove(b.cachePath(f.rel)); err != nil && !os.IsNotExist(err) {
			return false
		}
		b.index.SetHaveContent(f.rel, false)
		b.log.Debug("evicted cached content", "rel", f.rel, "bytes", f.size)
		return true
	}

	if idleMin := b.cfg.Cache.EvictAfterIdleMinutes; idleMin > 0 {
		cutoff := time.Now().Add(-time.Duration(idleMin) * time.Minute)
		kept := files[:0]
		for _, f := range files {
			if f.atime.Before(cutoff) && evict(f) {
				total -= f.size
				continue
			}
			kept = append(kept, f)
		}
		files = kept
	}

	capBytes := b.capBytes()
	if capBytes <= 0 || total <= capBytes {
		return
	}
	// Least-recently-accessed first.
	sort.Slice(files, func(i, j int) bool { return files[i].atime.Before(files[j].atime) })
	for _, f := range files {
		if total <= capBytes {
			break
		}
		if evict(f) {
			total -= f.size
		}
	}
}

// scanCache walks the content cache directory and returns each file plus the
// total bytes used.
func (b *Backend) scanCache() ([]cachedFile, int64) {
	var files []cachedFile
	var total int64
	_ = filepath.WalkDir(b.cacheDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(p, fetchTmpSuffix) {
			return nil // in-flight download staging; not evictable content
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		relPath, rerr := filepath.Rel(b.cacheDir, p)
		if rerr != nil {
			return nil
		}
		files = append(files, cachedFile{
			rel:  config.Rel(filepath.ToSlash(relPath)),
			size: info.Size(),
			// Real access time, not ModTime: the cache file's mtime is set to
			// the SOURCE file's mtime (for Getattr), so using it here made
			// "LRU" mean "oldest remote file" and idle eviction impossible.
			atime: atimeOf(info),
		})
		total += info.Size()
		return nil
	})
	return files, total
}

// CacheUsage reports total materialized bytes (for `lg status`).
func (b *Backend) CacheUsage() (int64, error) {
	_, total := b.scanCache()
	return total, nil
}
