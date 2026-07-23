package agent

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/iamtaehyunpark/livegit/internal/hashx"
	"github.com/iamtaehyunpark/livegit/internal/logx"
	"github.com/iamtaehyunpark/livegit/internal/proto"
)

// FileServer answers Ghost's file-RPC requests against Source's real disk
// rooted at remoteRoot. It is the Source half of the FUSE backend.
type FileServer struct {
	root    string // absolute remote_root
	mapper  *config.PathMapper
	matcher *config.Matcher
	log     *slog.Logger

	// Last full-tree walk, kept so Ghost can page through it (treePage).
	treeMu       sync.Mutex
	treeDigest   string
	treePages    [][]byte
	treePageSize int // entries per page; 0 = defaultTreePageSize (tests shrink it)
}

// NewFileServer builds a file server. remoteRoot overrides config when the
// agent is launched with --remote-root (the usual path).
func NewFileServer(remoteRoot string, matcher *config.Matcher) *FileServer {
	// Build a mapper whose local side is unused on Source; only RelToRemote matters.
	cfg := &config.Config{LocalRoot: remoteRoot}
	cfg.Source.RemoteRoot = remoteRoot
	return &FileServer{
		root:    filepath.Clean(remoteRoot),
		mapper:  config.NewPathMapper(cfg),
		matcher: matcher,
		log:     logx.For("fileserver"),
	}
}

// Handle dispatches one file-RPC request frame.
func (fs *FileServer) Handle(f proto.Frame) (proto.MsgType, any, bool, error) {
	switch f.Type {
	case proto.TypeStatReq:
		var req proto.StatReq
		_ = proto.Unmarshal(f.Body, &req)
		return proto.TypeStatResp, proto.StatResp{Stat: fs.stat(req.Rel)}, true, nil
	case proto.TypeReadReq:
		var req proto.ReadReq
		_ = proto.Unmarshal(f.Body, &req)
		resp, err := fs.read(req)
		return proto.TypeReadResp, resp, true, err
	case proto.TypeWriteReq:
		var req proto.WriteReq
		_ = proto.Unmarshal(f.Body, &req)
		ack, err := fs.write(req)
		return proto.TypeWriteAck, ack, true, err
	case proto.TypeDelReq:
		var req proto.DelReq
		_ = proto.Unmarshal(f.Body, &req)
		ack, err := fs.del(req)
		return proto.TypeDelAck, ack, true, err
	case proto.TypeRenameReq:
		var req proto.RenameReq
		_ = proto.Unmarshal(f.Body, &req)
		ack, err := fs.rename(req)
		return proto.TypeRenameAck, ack, true, err
	case proto.TypeListReq:
		var req proto.ListReq
		_ = proto.Unmarshal(f.Body, &req)
		resp, err := fs.list(req.Rel)
		return proto.TypeListResp, resp, true, err
	case proto.TypeTreeReq:
		var req proto.TreeReq
		_ = proto.Unmarshal(f.Body, &req)
		resp, err := fs.treePage(req)
		return proto.TypeTreeResp, resp, true, err
	default:
		return 0, nil, false, fmt.Errorf("fileserver: unexpected type %d", f.Type)
	}
}

func (fs *FileServer) abs(rel string) string { return fs.mapper.RelToRemote(rel) }

func (fs *FileServer) stat(rel string) proto.FileStat {
	st := proto.FileStat{Rel: config.Rel(rel)}
	info, err := os.Stat(fs.abs(rel))
	if err != nil {
		return st // Exists=false
	}
	st.Exists = true
	st.IsDir = info.IsDir()
	st.Size = info.Size()
	st.ModTime = info.ModTime().Unix()
	st.Mode = uint32(info.Mode().Perm())
	// Deliberately no content hash: hashing here read the ENTIRE file per stat
	// RPC (multi-second Getattr + full disk read for a 200MB+ file). Nothing
	// needs it — tree-sync entries carry no hash either, and conflict-detection
	// hashes come from the read/flush paths.
	return st
}

// readChunkMax caps how much a single ReadReq may ask for, whatever the peer
// sends — one chunk must always stay far under the codec's frame cap.
const readChunkMax = 8 << 20

// read serves one chunk of a file. Ghost loops Offset until !More; only the
// requested window is read into memory, so a multi-GB file never builds a
// whole-file buffer (or frame) on the agent.
func (fs *FileServer) read(req proto.ReadReq) (proto.ReadResp, error) {
	abs := fs.abs(req.Rel)
	info, err := os.Stat(abs)
	if err != nil {
		return proto.ReadResp{Found: false}, nil
	}
	if info.IsDir() {
		return proto.ReadResp{Found: false}, fmt.Errorf("%s is a directory", req.Rel)
	}
	max := req.MaxLen
	if max <= 0 || max > readChunkMax {
		max = proto.ChunkSize
	}
	f, err := os.Open(abs)
	if err != nil {
		return proto.ReadResp{}, err
	}
	defer f.Close()
	buf := make([]byte, max)
	n, err := f.ReadAt(buf, req.Offset)
	if err != nil && err != io.EOF {
		return proto.ReadResp{}, err
	}
	return proto.ReadResp{
		Found:   true,
		Content: buf[:n],
		More:    req.Offset+int64(n) < info.Size(),
		Size:    info.Size(),
		ModTime: info.ModTime().Unix(),
		Mode:    uint32(info.Mode().Perm()),
	}, nil
}

// write applies a journal-flush from Ghost, with conflict detection:
// if Source's current content hash differs from the BaseHash Ghost synced from,
// the two sides diverged — back up Source's current version before overwriting.
func (fs *FileServer) write(req proto.WriteReq) (proto.WriteAck, error) {
	abs := fs.abs(req.Rel)
	if req.IsDir {
		// Journaled mkdir: empty directories sync too (before this, a local
		// mkdir only existed in Ghost's index and vanished on the next tree
		// sync). No content, no conflict check — mkdir is idempotent.
		perm := os.FileMode(req.Mode & 0o777)
		if perm == 0 {
			perm = 0o755
		}
		if err := os.MkdirAll(abs, perm); err != nil {
			return proto.WriteAck{}, err
		}
		return proto.WriteAck{OK: true}, nil
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return proto.WriteAck{}, err
	}

	// Chunked upload (big files arrive in ChunkSize pieces): accumulate into a
	// sidecar staging file; only the final chunk falls through to commit below,
	// so a half-uploaded file is never visible at the real path. An `.id` file
	// records the upload identity so an interrupted upload can RESUME: the
	// Ghost probes for the staged size and continues from there instead of
	// re-sending everything (a dropped link at minute 4 of a 900MB push used
	// to mean starting over).
	part := abs + ".lg-part"
	idf := part + ".id"
	if req.Probe {
		var at int64
		if id, err := os.ReadFile(idf); err == nil && req.StageID != "" && string(id) == req.StageID {
			if st, err := os.Stat(part); err == nil {
				at = st.Size()
			}
		}
		return proto.WriteAck{OK: true, StagedAt: at}, nil
	}
	staged := req.Offset > 0 || req.More
	if staged {
		flags := os.O_WRONLY | os.O_CREATE | os.O_APPEND
		if req.Offset == 0 {
			flags |= os.O_TRUNC // fresh upload; also clears any abandoned staging
			if err := os.WriteFile(idf, []byte(req.StageID), 0o600); err != nil {
				return proto.WriteAck{}, err
			}
		} else {
			if id, err := os.ReadFile(idf); err != nil || string(id) != req.StageID {
				// Different upload than the staged one: restart from scratch.
				os.Remove(part)
				os.Remove(idf)
				return proto.WriteAck{}, fmt.Errorf("stale staging for %s; restart upload", req.Rel)
			}
		}
		f, err := os.OpenFile(part, flags, 0o600)
		if err != nil {
			return proto.WriteAck{}, err
		}
		st, err := f.Stat()
		if err == nil && st.Size() != req.Offset {
			// A gap means a lost/replayed chunk. Keep the staging — the Ghost's
			// retry probes StagedAt and realigns instead of re-uploading.
			f.Close()
			return proto.WriteAck{}, fmt.Errorf("write chunk gap for %s: staged %d bytes, chunk offset %d", req.Rel, st.Size(), req.Offset)
		}
		if _, err := f.Write(req.Content); err != nil {
			f.Close()
			return proto.WriteAck{}, err
		}
		if err := f.Close(); err != nil {
			return proto.WriteAck{}, err
		}
		if req.More {
			return proto.WriteAck{OK: true}, nil
		}
		defer os.Remove(idf) // commit (or its failure) ends this staged upload
	}
	ack := proto.WriteAck{}

	current, err := hashx.File(abs)
	if err != nil {
		return proto.WriteAck{}, err
	}
	// current=="" means file doesn't exist on Source -> no conflict possible.
	if current != "" && req.BaseHash != "" && current != req.BaseHash {
		// Divergence: Source changed since Ghost's last sync point.
		backupRel := fmt.Sprintf("%s.lg-conflict-%d", req.Rel, time.Now().Unix())
		if err := copyFile(abs, fs.abs(backupRel)); err != nil {
			return proto.WriteAck{}, fmt.Errorf("conflict backup failed: %w", err)
		}
		ack.Conflict = true
		ack.BackupRel = backupRel
		fs.log.Warn("write conflict; backed up source copy", "rel", req.Rel, "backup", backupRel)
	}

	mode := os.FileMode(req.Mode)
	if mode == 0 {
		mode = 0o644
	}
	if staged {
		// Atomic commit of the accumulated chunks (rename replaces even a
		// read-only target — permission lives on the directory).
		if err := os.Rename(part, abs); err != nil {
			os.Remove(part)
			return proto.WriteAck{}, err
		}
	} else if err := os.WriteFile(abs, req.Content, mode); err != nil {
		// Last-write-wins must also beat a read-only target: an existing file
		// without the owner write bit (git pack files are 0444) makes the
		// O_WRONLY open fail EACCES — and since the flush queue drains strictly
		// in order, that one entry would wedge the whole journal. Make it
		// writable, retry, and let the chmod below set the final mode.
		if !os.IsPermission(err) {
			return proto.WriteAck{}, err
		}
		if chErr := os.Chmod(abs, mode|0o200); chErr != nil {
			return proto.WriteAck{}, err
		}
		if err := os.WriteFile(abs, req.Content, mode); err != nil {
			return proto.WriteAck{}, err
		}
	}
	// os.WriteFile applies mode only when it creates the file; chmod explicitly
	// so a mode change journaled for an existing file (chmod +x, Finder's
	// permission pass) actually lands on Source.
	if err := os.Chmod(abs, mode); err != nil {
		return proto.WriteAck{}, err
	}
	if req.ModTime > 0 {
		t := time.Unix(req.ModTime, 0)
		_ = os.Chtimes(abs, t, t)
	}
	ack.OK = true
	if staged {
		ack.NewHash, _ = hashx.File(abs)
	} else {
		ack.NewHash = hashx.Bytes(req.Content)
	}
	if info, err := os.Stat(abs); err == nil {
		ack.SourceMod = info.ModTime().Unix()
	}
	return ack, nil
}

// rename moves a file or directory in place on Source. Declines (OK=false, no
// error — an error would look like a transport failure to Ghost) when the move
// can't apply, e.g. the source is gone or the destination is a non-empty dir.
func (fs *FileServer) rename(req proto.RenameReq) (proto.RenameAck, error) {
	oldAbs, newAbs := fs.abs(req.OldRel), fs.abs(req.NewRel)
	if _, err := os.Lstat(oldAbs); err != nil {
		return proto.RenameAck{Message: fmt.Sprintf("source missing: %v", err)}, nil
	}
	if err := os.MkdirAll(filepath.Dir(newAbs), 0o755); err != nil {
		return proto.RenameAck{}, err
	}
	if err := os.Rename(oldAbs, newAbs); err != nil {
		return proto.RenameAck{Message: err.Error()}, nil
	}
	fs.log.Info("renamed", "old", req.OldRel, "new", req.NewRel)
	return proto.RenameAck{OK: true}, nil
}

func (fs *FileServer) del(req proto.DelReq) (proto.DelAck, error) {
	abs := fs.abs(req.Rel)
	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return proto.DelAck{OK: true}, nil // already gone
		}
		return proto.DelAck{}, err
	}

	if info.IsDir() {
		// Journaled rmdir. Directories have no content hash, so skip the
		// conflict check (hashing a dir errors with EISDIR — that used to fail
		// the RPC forever and block the whole flush queue behind it). Ghost
		// unlinks every child it has synced before this rmdir arrives, so a
		// non-empty dir here holds only things Ghost never showed the user:
		// leftover empty subdirs, macOS junk, or ignore-matched content. If
		// that's all that remains, the user deleted a fully-synced dir —
		// finish the job recursively. A real non-ignored file means content
		// that never synced to Ghost: keep the dir and report a conflict ack —
		// Ghost drops the entry and the next tree sync resurfaces the dir,
		// instead of wedging the queue.
		err := os.Remove(abs)
		if err != nil && !os.IsNotExist(err) {
			if blocker := fs.unsyncedLeft(abs); blocker != "" {
				fs.log.Warn("dir delete skipped: unsynced content on source",
					"rel", req.Rel, "blocker", blocker)
				return proto.DelAck{OK: false, Conflict: true,
					Message: "directory not removed: holds unsynced " + blocker}, nil
			}
			if err := os.RemoveAll(abs); err != nil {
				fs.log.Warn("dir delete skipped", "rel", req.Rel, "err", err)
				return proto.DelAck{OK: false, Conflict: true,
					Message: "directory not removed: " + err.Error()}, nil
			}
		}
		return proto.DelAck{OK: true}, nil
	}

	// Only regular files have a meaningful content hash; for symlinks/fifos
	// just remove (hashing would follow the link or block).
	current := ""
	if info.Mode().IsRegular() {
		if current, err = hashx.File(abs); err != nil {
			return proto.DelAck{}, err
		}
	}
	if current != "" && req.BaseHash != "" && current != req.BaseHash {
		// Source changed since the delete was journaled; surface as a conflict
		// rather than destroying newer content.
		return proto.DelAck{OK: false, Conflict: true,
			Message: "source modified since delete was journaled"}, nil
	}
	if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
		return proto.DelAck{}, err
	}
	return proto.DelAck{OK: true}, nil
}

// unsyncedLeft walks a directory Ghost asked to delete and returns the rel of
// the first entry that could NOT have been deleted through the mount: a regular
// file that is neither macOS junk nor ignore-matched (ignored paths never sync,
// so their presence doesn't mean the user's view was incomplete). Returns ""
// when everything left is deletable — i.e. Ghost's view of the dir was complete
// and the whole delete should go through.
func (fs *FileServer) unsyncedLeft(absDir string) string {
	blocker := ""
	_ = filepath.WalkDir(absDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			blocker = p // unreadable: assume unsynced, keep the dir
			return filepath.SkipAll
		}
		if p == absDir {
			return nil
		}
		rel, rerr := fs.mapper.RemoteToRel(p)
		if rerr != nil {
			blocker = p
			return filepath.SkipAll
		}
		if fs.matcher != nil && fs.matcher.Match(rel, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir // ignored subtree: deletable wholesale
			}
			return nil
		}
		base := filepath.Base(p)
		if base == ".DS_Store" || strings.HasPrefix(base, "._") {
			return nil // macOS junk never syncs regardless of config
		}
		if d.IsDir() {
			return nil // empty dirs are fine; a file below would flag itself
		}
		blocker = rel // real content Ghost never synced (incl. symlinks/fifos)
		return filepath.SkipAll
	})
	return blocker
}

func (fs *FileServer) list(rel string) (proto.ListResp, error) {
	abs := fs.abs(rel)
	entries, err := os.ReadDir(abs)
	if err != nil {
		return proto.ListResp{Found: false}, nil
	}
	out := proto.ListResp{Found: true}
	for _, e := range entries {
		childRel := config.Rel(config.Rel(rel) + "/" + e.Name())
		if fs.matcher != nil && fs.matcher.Match(childRel, e.IsDir()) {
			continue // honor .lgignore on the watcher/list side too
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out.Entries = append(out.Entries, proto.DirEntry{
			Name:  e.Name(),
			IsDir: e.IsDir(),
			Size:  info.Size(),
			Mode:  uint32(info.Mode().Perm()),
		})
	}
	sort.Slice(out.Entries, func(i, j int) bool { return out.Entries[i].Name < out.Entries[j].Name })
	return out, nil
}

// treeWalkWorkers bounds concurrent directory listings during the full walk.
// The walk is NFS-latency-bound (~1ms per stat serially — 100k+ entries took
// minutes), so a modest pool gives a near-linear speedup without hammering
// the file server.
const treeWalkWorkers = 16

// defaultTreePageSize is entries per TreeResp page: ~25k entries is a few MB
// of JSON, well under 1 MB gzipped — comfortably inside the frame cap however
// big the tree grows.
const defaultTreePageSize = 25000

// treePage serves one page of the full-tree snapshot (see proto.TreeReq).
// Cursor 0 walks fresh, digests the result, and answers Unchanged when Ghost
// already holds this exact tree; later cursors serve the pages that walk
// saved. Content hashes are left empty — hashing 50k files would be slow;
// Ghost fills the hash lazily on first read (OneDrive-style).
func (fs *FileServer) treePage(req proto.TreeReq) (proto.TreeResp, error) {
	fs.treeMu.Lock()
	defer fs.treeMu.Unlock()

	if req.Cursor > 0 {
		if req.Digest != fs.treeDigest {
			// A newer walk replaced the snapshot mid-download (or the agent
			// restarted); Ghost restarts the sync from cursor 0 on its next tick.
			return proto.TreeResp{}, fmt.Errorf("tree snapshot expired; resync from cursor 0")
		}
		if req.Cursor >= len(fs.treePages) {
			return proto.TreeResp{}, fmt.Errorf("tree page %d out of range (%d pages)", req.Cursor, len(fs.treePages))
		}
		return proto.TreeResp{Digest: fs.treeDigest, Pages: len(fs.treePages), Gz: fs.treePages[req.Cursor]}, nil
	}

	entries := fs.walkTree()

	// Digest the walked entries (not their gzip encoding) so identity is
	// stable. Any metadata change anywhere in the tree changes the digest.
	h := sha256.New()
	henc := json.NewEncoder(h)
	for i := range entries {
		_ = henc.Encode(&entries[i])
	}
	digest := hex.EncodeToString(h.Sum(nil))
	if digest == req.Digest {
		return proto.TreeResp{Unchanged: true, Digest: digest}, nil
	}

	pageSize := fs.treePageSize
	if pageSize <= 0 {
		pageSize = defaultTreePageSize
	}
	var pages [][]byte
	for start := 0; ; start += pageSize {
		end := min(start+pageSize, len(entries))
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if err := json.NewEncoder(zw).Encode(entries[start:end]); err != nil {
			return proto.TreeResp{}, err
		}
		if err := zw.Close(); err != nil {
			return proto.TreeResp{}, err
		}
		pages = append(pages, buf.Bytes())
		if end == len(entries) {
			break
		}
	}
	fs.treeDigest, fs.treePages = digest, pages
	return proto.TreeResp{Digest: digest, Pages: len(pages), Gz: pages[0]}, nil
}

// walkTree lists the entire remote root (honoring .lgignore) with parallel
// per-directory workers, sorted by Rel so the digest is stable across walks.
// Unreadable paths are skipped rather than aborting the walk, as before.
func (fs *FileServer) walkTree() []proto.TreeEntry {
	var (
		mu      sync.Mutex
		entries []proto.TreeEntry
		wg      sync.WaitGroup
		sem     = make(chan struct{}, treeWalkWorkers)
	)
	var walk func(dir string)
	walk = func(dir string) {
		defer wg.Done()
		ents, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		local := make([]proto.TreeEntry, 0, len(ents))
		for _, d := range ents {
			if n := d.Name(); strings.HasSuffix(n, ".lg-part") || strings.HasSuffix(n, ".lg-part.id") {
				continue // transient upload staging; never part of the tree
			}
			abs := filepath.Join(dir, d.Name())
			rel, rerr := fs.mapper.RemoteToRel(abs)
			if rerr != nil || rel == "" || rel == "." {
				continue
			}
			if fs.matcher != nil && fs.matcher.Match(rel, d.IsDir()) {
				continue // ignored: don't emit, don't descend
			}
			info, ierr := d.Info()
			if ierr != nil {
				continue
			}
			local = append(local, proto.TreeEntry{
				Rel:     rel,
				IsDir:   d.IsDir(),
				Size:    info.Size(),
				ModTime: info.ModTime().Unix(),
				Mode:    uint32(info.Mode().Perm()),
			})
			if d.IsDir() {
				wg.Add(1)
				select {
				case sem <- struct{}{}: // worker slot free: descend concurrently
					go func(p string) { defer func() { <-sem }(); walk(p) }(abs)
				default: // pool saturated: descend inline
					walk(abs)
				}
			}
		}
		mu.Lock()
		entries = append(entries, local...)
		mu.Unlock()
	}
	wg.Add(1)
	walk(fs.root)
	wg.Wait()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Rel < entries[j].Rel })
	return entries
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}
