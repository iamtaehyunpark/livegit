package fuse

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	// ReadStream streams rel's content in order through sink and returns the
	// file's metadata (Exists=false with nil error when absent on Source).
	ReadStream(ctx context.Context, rel string, sink func(chunk []byte) error) (proto.FileStat, error)
	// Write applies a single-frame write (mkdir, or content that fits one
	// chunk). File flushes go through WriteFile, which streams from disk.
	Write(ctx context.Context, req proto.WriteReq) (proto.WriteAck, error)
	// WriteFile pushes the file at localPath to req.Rel, chunked + resumable;
	// req.Content is ignored.
	WriteFile(ctx context.Context, req proto.WriteReq, localPath string) (proto.WriteAck, error)
	Delete(ctx context.Context, req proto.DelReq) (proto.DelAck, error)
	Rename(ctx context.Context, req proto.RenameReq) (proto.RenameAck, error)
	// Tree fetches the full snapshot. haveDigest is the digest of the tree the
	// caller already applied; when Source's walk still matches it, Tree returns
	// unchanged=true (and nil entries) instead of re-shipping everything.
	Tree(ctx context.Context, haveDigest string) (entries []proto.TreeEntry, digest string, unchanged bool, err error)
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

	// xattrs holds Finder/macOS extended attributes in memory, per mount
	// session — never synced to Source. See xattr.go for why this exists.
	xattrs xattrStore

	// fetches tracks in-flight content downloads so concurrent opens share one
	// transfer and eviction skips files still being written (see fetch.go).
	fetchMu sync.Mutex
	fetches map[string]*fetchState

	// treeDigest identifies the last tree snapshot applied by SyncTree, echoed
	// to Source so an unchanged tree costs no transfer. Only the RunTreeSync
	// goroutine touches it.
	treeDigest string

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
		fetches:  map[string]*fetchState{},
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
	entries, digest, unchanged, err := b.source.Tree(ctx, b.treeDigest)
	if err != nil {
		return err
	}
	if unchanged {
		// Source's walk matched the tree we already applied: skip the Replace,
		// the stale-content scan, and the tree.json rewrite entirely. The
		// digest covers every entry's size/mtime, so any Source-side change —
		// including one the watcher missed — breaks it and forces a full fetch.
		b.log.Debug("tree unchanged")
		return nil
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

	b.treeDigest = digest
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
			// Generous deadline: the first sync of a 1M+ entry tree is a real
			// walk plus a paged transfer. A dead link doesn't hang this long —
			// connection loss aborts the in-flight call immediately.
			sctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
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

// Materialize ensures rel has COMPLETE local content, fetching from Source on
// demand, and returns the local cache path. This is the full-wait form used by
// writable opens (editing needs the whole file) and internal callers (rename,
// truncate); read-only opens go through OpenForRead instead and stream. Offline
// with no cached copy surfaces as an error so the OS reports "can't read, no
// connection". The fetch itself streams to disk in chunks (fetch.go) — no
// whole-file buffer, shared with any concurrent readers.
func (b *Backend) Materialize(ctx context.Context, rel string) (string, error) {
	rel = config.Rel(rel)
	path, st, err := b.StartFetch(ctx, rel)
	if err != nil {
		return "", err
	}
	if st == nil {
		return path, nil // already cached
	}
	if err := st.wait(ctx, -1); err != nil {
		return "", err
	}
	return b.cachePath(rel), nil
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
	// Move session-local xattrs along. Done up front: if the rename fails
	// mid-way, losing Finder bookkeeping attrs is harmless.
	b.xattrRename(oldRel, newRel)

	entry, _ := b.index.Get(oldRel)
	isDir := entry != nil && entry.IsDir

	// FAST PATH: everything under oldRel is already on Source (no unflushed
	// local truth) and Source is reachable — one RPC moves it server-side, no
	// content crosses the wire. Without this, dragging a multi-GB directory
	// downloaded and re-uploaded every byte and froze the mount for minutes.
	// A FILE with pending local truth skips this deliberately: that's the
	// editor atomic-save (tmp → target), whose bytes are already in the cache —
	// the journal path handles it instantly, no upload wait.
	if b.source.Online() {
		pend := b.journal.PendingForDir(oldRel)
		if pend && isDir {
			// Queued edits under the source dir would flush to the OLD names
			// after a server-side move (recreating them); give them a moment
			// to land, then re-check.
			_ = b.FlushBarrier(ctx, oldRel, 10*time.Second)
			pend = b.journal.PendingForDir(oldRel)
		}
		if !pend {
			ack, err := b.source.Rename(ctx, proto.RenameReq{OldRel: oldRel, NewRel: newRel})
			if err == nil && ack.OK {
				b.applyLocalRename(oldRel, newRel)
				return nil
			}
			if err == nil {
				b.log.Warn("source declined rename; using copy+delete",
					"old", oldRel, "new", newRel, "msg", ack.Message)
			} else {
				b.log.Warn("rename rpc failed; using copy+delete", "err", err)
			}
		}
	}

	// SLOW PATH (offline, unflushed edits under the source, or Source declined):
	// per-file copy+delete via the journal, as before.
	if isDir {
		return b.recordRenameDir(ctx, oldRel, newRel)
	}
	return b.recordRenameFile(ctx, oldRel, newRel)
}

// applyLocalRename mirrors a completed Source-side rename locally: cached
// bytes move with one os.Rename (file or whole subtree), and every index entry
// under oldRel re-registers at its new path with metadata intact — Hash and
// HaveContent preserved, so the watcher's echo of the remote rename compares
// equal and does not drop the just-moved cache.
func (b *Backend) applyLocalRename(oldRel, newRel string) {
	oldCache, newCache := b.cachePath(oldRel), b.cachePath(newRel)
	if err := os.MkdirAll(filepath.Dir(newCache), 0o755); err == nil {
		_ = os.Remove(newCache) // stale dest cache file, if any
		_ = os.Rename(oldCache, newCache)
	}
	// Snapshot the subtree before mutating the index.
	type moved struct{ old, new string }
	var list []moved
	var walk func(old, new string)
	walk = func(old, new string) {
		list = append(list, moved{old, new})
		for _, c := range b.index.Children(old) {
			walk(config.Rel(old+"/"+c.Name), config.Rel(new+"/"+c.Name))
		}
	}
	walk(oldRel, newRel)
	for _, m := range list {
		if e, ok := b.index.Get(m.old); ok {
			ne := *e
			ne.Rel = m.new
			b.index.Put(&ne)
		}
	}
	b.index.Remove(oldRel) // recursive: drops the whole old subtree
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
// individual move (last-write-wins per file), mirroring each subdirectory at
// the destination (so empty dirs survive the move), then deleting the old
// subtree — subdirs deepest-first, top dir last, so every rmdir reaching Source
// finds the dir already empty. Deleting only the top dir used to leave the old
// subdir skeleton behind on Source: its rmdir failed "not empty", the journal
// entry was dropped, and the next tree sync resurrected the old dir.
func (b *Backend) recordRenameDir(ctx context.Context, oldRel, newRel string) error {
	// Snapshot the descendant lists first, since each move mutates the index.
	// dirs is in pre-order (parent before child).
	var files, dirs []string
	dirMode := map[string]uint32{}
	var walk func(rel string)
	walk = func(rel string) {
		for _, c := range b.index.Children(rel) {
			childRel := config.Rel(rel + "/" + c.Name)
			if c.IsDir {
				dirs = append(dirs, childRel)
				dirMode[childRel] = c.Mode
				walk(childRel)
			} else {
				files = append(files, childRel)
			}
		}
	}
	walk(oldRel)

	b.markDir(newRel, 0o755)
	for _, d := range dirs {
		rest := strings.TrimPrefix(d, oldRel+"/")
		b.markDir(config.Rel(newRel+"/"+rest), dirMode[d])
	}
	for _, f := range files {
		rest := strings.TrimPrefix(f, oldRel+"/")
		if err := b.recordRenameFile(ctx, f, config.Rel(newRel+"/"+rest)); err != nil {
			return err
		}
	}
	// Drop the now-empty source subtree: children before parents.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := b.RecordDelete(dirs[i]); err != nil {
			return err
		}
	}
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
	b.xattrForget(rel)
	return nil
}

// Stop signals background workers to exit.
func (b *Backend) Stop() { close(b.stop) }
