package fuse

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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

// EvictOnce performs one size-cap sweep. Exposed for tests.
func (b *Backend) EvictOnce() {
	capBytes := b.capBytes()
	if capBytes <= 0 {
		return
	}
	files, total := b.scanCache()
	if total <= capBytes {
		return
	}
	// Evict least-recently-accessed content first, skipping files with unflushed
	// edits (their bytes are the only copy until the flush worker pushes them).
	sort.Slice(files, func(i, j int) bool { return files[i].atime.Before(files[j].atime) })
	for _, f := range files {
		if total <= capBytes {
			break
		}
		if b.journal.HasPending(f.rel) {
			continue
		}
		if err := os.Remove(b.cachePath(f.rel)); err != nil && !os.IsNotExist(err) {
			continue
		}
		b.index.SetHaveContent(f.rel, false)
		total -= f.size
		b.log.Debug("evicted cached content", "rel", f.rel, "bytes", f.size)
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
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		relPath, rerr := filepath.Rel(b.cacheDir, p)
		if rerr != nil {
			return nil
		}
		files = append(files, cachedFile{
			rel:   config.Rel(filepath.ToSlash(relPath)),
			size:  info.Size(),
			atime: info.ModTime(),
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
