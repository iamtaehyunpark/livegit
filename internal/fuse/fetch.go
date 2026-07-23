package fuse

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/iamtaehyunpark/livegit/internal/hashx"
)

// fetchState tracks one in-flight content download. Concurrent opens of the
// same file share a single transfer, and read-only handles consume bytes as
// they arrive instead of blocking until the whole file lands (a multi-minute
// freeze for a 200 MB+ file over a slow WAN — the "download looks hung" bug).
type fetchState struct {
	mu       sync.Mutex
	progress int64 // bytes written to the staging file so far
	done     bool
	err      error
	ch       chan struct{} // closed + replaced on every state change (broadcast)
}

func newFetchState() *fetchState { return &fetchState{ch: make(chan struct{})} }

func (f *fetchState) broadcastLocked() {
	close(f.ch)
	f.ch = make(chan struct{})
}

func (f *fetchState) advance(n int64) {
	f.mu.Lock()
	f.progress += n
	f.broadcastLocked()
	f.mu.Unlock()
}

func (f *fetchState) finish(err error) {
	f.mu.Lock()
	f.done = true
	f.err = err
	f.broadcastLocked()
	f.mu.Unlock()
}

// wait blocks until at least `need` bytes are available, or the fetch is done
// (need < 0 waits for completion). Returns the fetch error, if any; a fetch
// that finished short of `need` returns nil — the reader just hits EOF.
func (f *fetchState) wait(ctx context.Context, need int64) error {
	for {
		f.mu.Lock()
		progress, done, err, ch := f.progress, f.done, f.err, f.ch
		f.mu.Unlock()
		if done {
			return err
		}
		if need >= 0 && progress >= need {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}

// stopCtx derives a context that is additionally canceled when the backend
// stops (unmount). Without this, an in-flight fetch runs on and the kernel
// unmount waits behind it — `lg unmount` hanging mid-download.
func (b *Backend) stopCtx(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		select {
		case <-b.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// StartFetch ensures rel's content is cached or downloading. It returns the
// path to read from — the final cache file when content is already present
// (st == nil), or the growing staging file plus its fetchState while a
// download is in flight.
func (b *Backend) StartFetch(ctx context.Context, rel string) (string, *fetchState, error) {
	rel = config.Rel(rel)
	cp := b.cachePath(rel)
	if cacheFileExists(cp) {
		return cp, nil, nil
	}
	tmp := cp + fetchTmpSuffix

	b.fetchMu.Lock()
	if st, ok := b.fetches[rel]; ok {
		b.fetchMu.Unlock()
		return tmp, st, nil
	}
	if !b.source.Online() {
		b.fetchMu.Unlock()
		return "", nil, fmt.Errorf("offline: cannot fetch %s", rel)
	}
	// Create the staging file before registering, so a joining reader can
	// always open the path this returns.
	if err := os.MkdirAll(filepath.Dir(cp), 0o755); err != nil {
		b.fetchMu.Unlock()
		return "", nil, err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		b.fetchMu.Unlock()
		return "", nil, err
	}
	st := newFetchState()
	b.fetches[rel] = st
	b.fetchMu.Unlock()

	go b.runFetch(rel, cp, tmp, f, st)
	return tmp, st, nil
}

// fetchTmpSuffix marks a partially-downloaded staging file (skipped by
// eviction scans; removed on fetch failure).
const fetchTmpSuffix = ".lg-tmp"

func (b *Backend) runFetch(rel, cp, tmp string, f *os.File, st *fetchState) {
	err := b.doFetch(rel, cp, tmp, f, st)
	b.fetchMu.Lock()
	delete(b.fetches, rel)
	b.fetchMu.Unlock()
	if err != nil {
		_ = os.Remove(tmp)
	}
	st.finish(err)
}

func (b *Backend) doFetch(rel, cp, tmp string, f *os.File, st *fetchState) error {
	// Tied to b.stop, NOT to the opener's kernel context: several readers may
	// share this fetch, and it completes the cache even if the first reader
	// gives up. Unmount (Stop) aborts it immediately.
	ctx, cancel := b.stopCtx(context.Background())
	defer cancel()

	h := hashx.New()
	meta, err := b.source.ReadStream(ctx, rel, func(chunk []byte) error {
		if _, werr := f.Write(chunk); werr != nil {
			return werr
		}
		_, _ = h.Write(chunk)
		st.advance(int64(len(chunk)))
		return nil
	})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if !meta.Exists {
		return os.ErrNotExist
	}
	// Preserve Source's mtime on the cache file so Getattr (which reads the
	// cache file's mtime) returns the correct remote timestamp, not "now".
	if meta.ModTime > 0 {
		t := time.Unix(meta.ModTime, 0)
		_ = os.Chtimes(tmp, t, t)
	}
	// Readers holding the staging fd keep reading seamlessly — rename moves
	// the inode, not the bytes.
	if err := os.Rename(tmp, cp); err != nil {
		return err
	}
	st.mu.Lock()
	size := st.progress
	st.mu.Unlock()
	b.index.Put(&Entry{
		Rel: rel, Size: size, ModTime: meta.ModTime,
		Mode: meta.Mode, Hash: hashx.Sum(h), HaveContent: true,
	})
	b.log.Debug("materialized", "rel", rel, "bytes", size)
	return nil
}

// OpenForRead opens rel's content for a read-only handle: the finished cache
// file, or the growing staging file (with its fetchState) while downloading.
// The tiny retry absorbs the race where the fetch completes (staging renamed
// away) between StartFetch and the open.
func (b *Backend) OpenForRead(ctx context.Context, rel string) (*os.File, *fetchState, error) {
	for attempt := 0; attempt < 3; attempt++ {
		path, st, err := b.StartFetch(ctx, rel)
		if err != nil {
			return nil, nil, err
		}
		f, oerr := os.Open(path)
		if oerr == nil {
			return f, st, nil
		}
		if st != nil && os.IsNotExist(oerr) {
			continue
		}
		return nil, nil, oerr
	}
	return nil, nil, fmt.Errorf("open %s: fetch completed underneath every attempt", rel)
}

// fetchActive reports whether rel is downloading right now (eviction guard).
func (b *Backend) fetchActive(rel string) bool {
	b.fetchMu.Lock()
	_, ok := b.fetches[rel]
	b.fetchMu.Unlock()
	return ok
}
