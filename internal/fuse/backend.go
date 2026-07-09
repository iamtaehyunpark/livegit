package fuse

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/iamtaehyunpark/livegit/internal/logx"
	"github.com/iamtaehyunpark/livegit/internal/proto"
)

// SourceRPC is the subset of the transport the Backend needs. An interface so
// the backend is unit-testable against a fake Source.
type SourceRPC interface {
	Stat(ctx context.Context, rel string) (proto.FileStat, error)
	Read(ctx context.Context, rel string) (proto.ReadResp, error)
	Write(ctx context.Context, req proto.WriteReq) (proto.WriteAck, error)
	Delete(ctx context.Context, req proto.DelReq) (proto.DelAck, error)
	Tree(ctx context.Context) ([]proto.TreeEntry, error)
	Online() bool
}

// Backend is the Ghost FUSE backend: a full-tree metadata index synced from
// Source, lazy content-fetch on open, and journal-based write-through. Source is
// the single source of truth for content; local edits flow back last-write-wins.
type Backend struct {
	index    *Index
	journal  *Journal
	cacheDir string
	mapper   *config.PathMapper
	matcher  *config.Matcher
	source   SourceRPC
	cfg      *config.Config
	log      *slog.Logger

	testCapBytes int64 // test-only content-cache cap override (0 = use config GB)

	// synced flips true after the first successful full-tree sync. From then on
	// the index is authoritative for negatives: Getattr answers ENOENT locally
	// instead of paying a per-lookup remote Stat (macOS probes nonexistent names
	// — ._*, .DS_Store — constantly; each miss used to cost a WAN round trip).
	synced atomic.Bool

	stop chan struct{}
}

// capBytes is the content-cache size cap in bytes (tests can override it).
func (b *Backend) capBytes() int64 {
	if b.testCapBytes > 0 {
		return b.testCapBytes
	}
	return int64(b.cfg.Cache.MaxCacheSizeGB) << 30
}

// NewBackend assembles the Ghost FUSE backend.
func NewBackend(cfg *config.Config, journal *Journal, source SourceRPC, matcher *config.Matcher) *Backend {
	return &Backend{
		index:    NewIndex(filepath.Join(config.Dir(), "tree.json")),
		journal:  journal,
		cacheDir: config.CacheDir(),
		mapper:   config.NewPathMapper(cfg),
		matcher:  matcher,
		source:   source,
		cfg:      cfg,
		log:      logx.For("fuse"),
		stop:     make(chan struct{}),
	}
}

// Index exposes the metadata index (for the mount's snapshot saver + status).
func (b *Backend) Index() *Index { return b.index }

// cachePath maps a rel to its materialized content file under ~/.lg/cache.
func (b *Backend) cachePath(rel string) string {
	return filepath.Join(b.cacheDir, filepath.FromSlash(config.Rel(rel)))
}

func cacheFileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// SyncTree fetches Source's entire tree and replaces the index. Called once the
// connection is up and again on each reconnect (mount.go), keeping the full-tree
// view eager and current; the watcher applies incremental updates in between.
func (b *Backend) SyncTree(ctx context.Context) error {
	if !b.source.Online() {
		return fmt.Errorf("offline")
	}
	// Snapshot pending BEFORE the fetch as well as after: an entry that flushes
	// while Tree() is in flight (say a delete) is gone from the post-fetch
	// snapshot but still present in the fetched tree — without the union it
	// would be resurrected from the stale tree until the next refresh.
	pend := b.journal.PendingSnapshot()
	entries, err := b.source.Tree(ctx)
	if err != nil {
		return err
	}
	pend = append(pend, b.journal.PendingSnapshot()...)

	// A walk that returned nothing for a populated index is a Source-side
	// failure (tree() skips unreadable paths silently), not a real empty repo —
	// wiping the local view would be worse than staying stale. Retry next tick.
	if len(entries) == 0 && b.index.Len() > 0 {
		return fmt.Errorf("empty tree for a non-empty index; keeping local view")
	}

	// Local truth not yet on Source must survive the wholesale Replace: an entry
	// with an unflushed write/create/mkdir would vanish from the mount until its
	// flush lands, and an unflushed delete would resurrect the remote copy.
	kept := map[string]*Entry{}
	for _, je := range pend {
		if je.Op != OpDelete {
			if e, ok := b.index.Get(je.Rel); ok {
				kept[je.Rel] = e
			}
		}
	}

	// Content backstop: repairing lost watcher pushes includes *content*. A file
	// whose size/mtime changed on Source while we hold cached bytes (and no
	// unflushed local edit) gets its cache dropped so the next open refetches —
	// Invalidate's policy, keyed on size/mtime because tree entries carry no
	// hash. No self-echo: the agent applies our pushed ModTime on flush, so a
	// flushed local edit compares equal.
	var stale []string
	for _, e := range entries {
		if e.IsDir {
			continue
		}
		rel := config.Rel(e.Rel)
		if old, ok := b.index.Get(rel); ok && old.HaveContent &&
			(old.Size != e.Size || old.ModTime != e.ModTime) && !b.journal.HasPending(rel) {
			stale = append(stale, rel)
		}
	}

	b.index.Replace(entries)
	for _, je := range pend {
		if je.Op == OpDelete {
			b.index.Remove(je.Rel)
		} else if e := kept[je.Rel]; e != nil {
			b.index.Put(e)
		}
	}
	for _, rel := range stale {
		_ = os.Remove(b.cachePath(rel))
		b.index.SetHaveContent(rel, false)
		b.log.Debug("refresh dropped stale cached content", "rel", rel)
	}

	if b.synced.CompareAndSwap(false, true) {
		b.log.Info("tree synced", "entries", len(entries))
	} else {
		// Periodic refresh — Debug so lg.log isn't a line-a-minute.
		b.log.Debug("tree refreshed", "entries", len(entries))
	}
	return nil
}

// treeRefreshInterval is the periodic full re-sync cadence. The watcher's
// invalidations keep the index current in real time; this is the backstop that
// bounds staleness when pushes are lost (offline edits, inotify queue overflow,
// the new-dir watch race) — it also replaced the old per-lookup remote-stat
// fallback as the recovery path for missed events.
const treeRefreshInterval = 60 * time.Second

// RunTreeSync keeps the full-tree index eager: it syncs once the connection is
// up, re-syncs on every reconnect, and refreshes periodically while online.
// One field carries all the state: a zero lastSync means "sync ASAP" (startup
// and reconnect), a failed sync leaves it unadvanced so the next tick retries.
func (b *Backend) RunTreeSync(ctx context.Context) {
	var lastSync time.Time
	interval := treeRefreshInterval
	for {
		if !b.source.Online() {
			lastSync = time.Time{} // offline: re-sync immediately on reconnect
		} else if time.Since(lastSync) >= interval {
			sctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			start := time.Now()
			err := b.SyncTree(sctx)
			cancel()
			if err != nil {
				b.log.Warn("tree sync failed", "err", err)
			} else {
				lastSync = time.Now()
				// Adaptive cadence: a full walk of a huge tree must not dominate
				// Source (a 30s walk every 60s is a 50% duty cycle). Cap the
				// refresh at ~10% duty, clamped to [treeRefreshInterval, 10m].
				interval = 10 * time.Since(start)
				if interval < treeRefreshInterval {
					interval = treeRefreshInterval
				}
				if interval > 10*time.Minute {
					interval = 10 * time.Minute
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-b.stop:
			return
		case <-time.After(time.Second):
		}
	}
}

// Materialize ensures rel has real local content, fetching from Source on demand
// (the open() hook). Returns the local cache path. Offline with no cached copy
// surfaces as an error so the OS reports "can't read, no connection".
func (b *Backend) Materialize(ctx context.Context, rel string) (string, error) {
	rel = config.Rel(rel)
	cp := b.cachePath(rel)
	if cacheFileExists(cp) {
		return cp, nil
	}
	if !b.source.Online() {
		return "", fmt.Errorf("offline: cannot fetch %s", rel)
	}
	resp, err := b.source.Read(ctx, rel)
	if err != nil {
		return "", err
	}
	if !resp.Found {
		return "", os.ErrNotExist
	}
	if err := os.MkdirAll(filepath.Dir(cp), 0o755); err != nil {
		return "", err
	}
	mode := os.FileMode(resp.Mode)
	if mode == 0 {
		mode = 0o644
	}
	// Write via temp + rename: a crash or full disk mid-write must never leave a
	// truncated file at cp — cacheFileExists would treat it as fully materialized
	// and serve the partial bytes as the real content forever after.
	tmp := cp + ".lg-tmp"
	if err := os.WriteFile(tmp, resp.Content, mode); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	// Preserve Source's mtime on the cache file so Getattr (which reads the
	// cache file's mtime) returns the correct remote timestamp, not "now".
	if resp.ModTime > 0 {
		t := time.Unix(resp.ModTime, 0)
		_ = os.Chtimes(tmp, t, t)
	}
	if err := os.Rename(tmp, cp); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	b.index.Put(&Entry{
		Rel: rel, Size: int64(len(resp.Content)), ModTime: resp.ModTime,
		Mode: resp.Mode, Hash: resp.Hash, HaveContent: true,
	})
	b.log.Debug("materialized", "rel", rel, "bytes", len(resp.Content))
	return cp, nil
}

// ignored reports whether rel should never sync up to Source: the ignore matcher
// (.venv, etc.), plus `.DS_Store` always (universal macOS junk — guarded here so
// even a config that predates the default ignore never leaks it to the server).
func (b *Backend) ignored(rel string) bool {
	if filepath.Base(rel) == ".DS_Store" {
		return true
	}
	return b.matcher != nil && b.matcher.Match(config.Rel(rel), false)
}

// RecordWrite is called on close() of a dirty file. It journals a last-write-
// wins push (empty BaseHash: Source overwrites) and updates the index from the
// local file. The actual push to Source happens in the flush worker.
func (b *Backend) RecordWrite(rel string) error {
	rel = config.Rel(rel)
	if b.ignored(rel) {
		return nil // ignored junk (e.g. .DS_Store): keep local, never sync up
	}
	cp := b.cachePath(rel)
	info, err := os.Stat(cp)
	if err != nil {
		return err
	}
	op := OpWrite
	if _, ok := b.index.Get(rel); !ok {
		op = OpCreate
	}
	now := time.Now()
	if _, err := b.journal.Append(JournalEntry{
		Rel:      rel,
		Op:       op,
		BaseHash: "", // last-write-wins
		ModTime:  now.Unix(),
		Mode:     uint32(info.Mode().Perm()),
		Ts:       now.Unix(),
	}); err != nil {
		return err
	}
	b.index.Put(&Entry{
		Rel: rel, Size: info.Size(), ModTime: now.Unix(),
		Mode: uint32(info.Mode().Perm()), HaveContent: true,
	})
	return nil
}

// markLiveNew registers a freshly created file in the index (called from Create
// before any bytes are written, so an empty new file is still visible).
func (b *Backend) markLiveNew(rel string, mode uint32) error {
	rel = config.Rel(rel)
	if b.ignored(rel) {
		return nil // don't surface ignored junk in the tree at all
	}
	perm := mode & 0o777
	if perm == 0 {
		perm = 0o644
	}
	b.index.Put(&Entry{
		Rel: rel, Size: 0, ModTime: time.Now().Unix(), Mode: perm, HaveContent: true,
	})
	return nil
}

// markDir registers a locally-created directory in the index AND journals a
// mkdir push. Without the journal entry an empty dir existed only in Ghost's
// index — never on Source — so every tree sync erased it from the mount.
func (b *Backend) markDir(rel string, mode uint32) {
	rel = config.Rel(rel)
	if b.ignored(rel) {
		return
	}
	perm := mode & 0o777
	if perm == 0 {
		perm = 0o755
	}
	now := time.Now()
	b.index.Put(&Entry{Rel: rel, IsDir: true, Mode: perm, ModTime: now.Unix()})
	if _, err := b.journal.Append(JournalEntry{
		Rel: rel, Op: OpMkdir, ModTime: now.Unix(), Mode: perm, Ts: now.Unix(),
	}); err != nil {
		b.log.Warn("mkdir journal append failed", "rel", rel, "err", err)
	}
}

// RecordRename moves oldRel to newRel through the journal (last-write-wins).
// Editors save atomically — write a tmp file, then rename(2) it over the target
// — so without this every editor save (and git, vim, etc.) fails on the mount
// with ENOSYS, which macFUSE surfaces as ENOTSUP. There is no rename RPC: a move
// is modeled as a write at the destination plus a delete at the source, which
// the flush worker already knows how to push.
func (b *Backend) RecordRename(ctx context.Context, oldRel, newRel string) error {
	oldRel = config.Rel(oldRel)
	newRel = config.Rel(newRel)
	if oldRel == "" || oldRel == newRel {
		return nil // renaming a thing onto itself: nothing to do
	}
	if e, ok := b.index.Get(oldRel); ok && e.IsDir {
		return b.recordRenameDir(ctx, oldRel, newRel)
	}
	return b.recordRenameFile(ctx, oldRel, newRel)
}

// recordRenameFile moves a single file's bytes to the destination and journals
// the move as write(new)+delete(old).
func (b *Backend) recordRenameFile(ctx context.Context, oldRel, newRel string) error {
	oldCache := b.cachePath(oldRel)
	newCache := b.cachePath(newRel)

	// The moved bytes must be in the local cache to push to the destination. The
	// editor atomic-save case renames a freshly written tmp file, so its content
	// is already live; Materialize is the fallback for renaming a Source file
	// that was synced but never opened (fails cleanly if offline & uncached).
	if !cacheFileExists(oldCache) {
		if _, err := b.Materialize(ctx, oldRel); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(newCache), 0o755); err != nil {
		return err
	}
	_ = os.Remove(newCache) // overwrite any stale dest cache
	if err := os.Rename(oldCache, newCache); err != nil {
		return err
	}
	// Journal + index the destination as a last-write-wins write.
	if !b.ignored(newRel) {
		info, err := os.Stat(newCache)
		if err != nil {
			return err
		}
		now := time.Now()
		if _, err := b.journal.Append(JournalEntry{
			Rel:      newRel,
			Op:       OpWrite,
			BaseHash: "",
			ModTime:  now.Unix(),
			Mode:     uint32(info.Mode().Perm()),
			Ts:       now.Unix(),
		}); err != nil {
			return err
		}
		b.index.Put(&Entry{
			Rel: newRel, Size: info.Size(), ModTime: now.Unix(),
			Mode: uint32(info.Mode().Perm()), HaveContent: true,
		})
	}
	// Remove the source name (local drop + journaled delete for Source).
	return b.RecordDelete(oldRel)
}

// recordRenameDir moves a directory by re-journaling each descendant file as an
// individual move (last-write-wins per file), then deletes the old subtree. The
// destination directories materialize on Source as a side effect of the file
// writes (the file server MkdirAll's parents).
func (b *Backend) recordRenameDir(ctx context.Context, oldRel, newRel string) error {
	// Snapshot the descendant file list first, since each move mutates the index.
	var files []string
	var walk func(rel string)
	walk = func(rel string) {
		for _, c := range b.index.Children(rel) {
			childRel := config.Rel(rel + "/" + c.Name)
			if c.IsDir {
				walk(childRel)
			} else {
				files = append(files, childRel)
			}
		}
	}
	walk(oldRel)

	b.markDir(newRel, 0o755)
	for _, f := range files {
		rest := strings.TrimPrefix(f, oldRel+"/")
		if err := b.recordRenameFile(ctx, f, config.Rel(newRel+"/"+rest)); err != nil {
			return err
		}
	}
	// Drop the now-empty source subtree locally and journal a delete so Source
	// removes the old (now-empty) directory too.
	return b.RecordDelete(oldRel)
}

// RecordSetattr applies a truncate/chmod/utimes to rel. The only write primitive
// is a whole-file push, so a size or mode change is journaled as a write (which
// reads the cache file); a bare mtime touch only updates the index rather than
// re-uploading the file just to bump a timestamp. Without a Setattr node op these
// all fail with ENOTSUP, so `chmod +x`, `truncate`, and `touch` break on the
// mount. size/mode/mtime are nil when that attribute isn't being changed.
func (b *Backend) RecordSetattr(ctx context.Context, rel string, size *int64, mode *uint32, mtime *int64) error {
	rel = config.Rel(rel)
	if b.ignored(rel) {
		return nil
	}
	// Directories carry no content: reflect mode/time in the index only.
	if e, ok := b.index.Get(rel); ok && e.IsDir {
		if mode != nil {
			e.Mode = *mode & 0o777
		}
		if mtime != nil {
			e.ModTime = *mtime
		}
		b.index.Put(e)
		if mode != nil {
			_ = os.Chmod(b.cachePath(rel), os.FileMode(*mode&0o777))
		}
		return nil
	}

	cp := b.cachePath(rel)
	cached := cacheFileExists(cp)
	// Truncate needs the real bytes present; materialize on demand.
	if size != nil && !cached {
		if _, err := b.Materialize(ctx, rel); err != nil {
			return err
		}
		cached = true
	}
	if size != nil {
		if err := os.Truncate(cp, *size); err != nil {
			return err
		}
	}
	if mode != nil && cached {
		if err := os.Chmod(cp, os.FileMode(*mode&0o777)); err != nil {
			return err
		}
	}

	// Size or mode is content/perm state Source must reflect -> journal a write.
	if (size != nil || mode != nil) && cached {
		info, err := os.Stat(cp)
		if err != nil {
			return err
		}
		perm := uint32(info.Mode().Perm())
		mt := info.ModTime().Unix()
		if mtime != nil {
			mt = *mtime
		}
		if _, err := b.journal.Append(JournalEntry{
			Rel: rel, Op: OpWrite, BaseHash: "", ModTime: mt, Mode: perm, Ts: time.Now().Unix(),
		}); err != nil {
			return err
		}
		b.index.Put(&Entry{Rel: rel, Size: info.Size(), ModTime: mt, Mode: perm, HaveContent: true})
		return nil
	}

	// Nothing pushable (chmod/touch on an unopened file, or mtime-only): reflect
	// it in the index best-effort so the change is at least visible locally.
	if e, ok := b.index.Get(rel); ok {
		if mode != nil {
			e.Mode = *mode & 0o777
		}
		if mtime != nil {
			e.ModTime = *mtime
		}
		if size != nil {
			e.Size = *size
		}
		b.index.Put(e)
	}
	return nil
}

// RecordDelete journals a delete (last-write-wins) and removes local state.
func (b *Backend) RecordDelete(rel string) error {
	rel = config.Rel(rel)
	now := time.Now()
	if _, err := b.journal.Append(JournalEntry{
		Rel: rel, Op: OpDelete, BaseHash: "", Ts: now.Unix(), ModTime: now.Unix(),
	}); err != nil {
		return err
	}
	_ = os.Remove(b.cachePath(rel))
	b.index.Remove(rel)
	return nil
}

// Stop signals background workers to exit.
func (b *Backend) Stop() { close(b.stop) }
