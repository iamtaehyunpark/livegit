package agent

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
		resp, err := fs.read(req.Rel)
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
	case proto.TypeListReq:
		var req proto.ListReq
		_ = proto.Unmarshal(f.Body, &req)
		resp, err := fs.list(req.Rel)
		return proto.TypeListResp, resp, true, err
	case proto.TypeTreeReq:
		resp, err := fs.tree()
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
	if !info.IsDir() {
		if h, err := hashx.File(fs.abs(rel)); err == nil {
			st.Hash = h
		}
	}
	return st
}

func (fs *FileServer) read(rel string) (proto.ReadResp, error) {
	abs := fs.abs(rel)
	info, err := os.Stat(abs)
	if err != nil {
		return proto.ReadResp{Found: false}, nil
	}
	if info.IsDir() {
		return proto.ReadResp{Found: false}, fmt.Errorf("%s is a directory", rel)
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return proto.ReadResp{}, err
	}
	return proto.ReadResp{
		Found:   true,
		Content: b,
		Hash:    hashx.Bytes(b),
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
	if err := os.WriteFile(abs, req.Content, mode); err != nil {
		return proto.WriteAck{}, err
	}
	if req.ModTime > 0 {
		t := time.Unix(req.ModTime, 0)
		_ = os.Chtimes(abs, t, t)
	}
	ack.OK = true
	ack.NewHash = hashx.Bytes(req.Content)
	if info, err := os.Stat(abs); err == nil {
		ack.SourceMod = info.ModTime().Unix()
	}
	return ack, nil
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

// tree walks the entire remote root and returns one TreeEntry per file/dir
// (honoring .lgignore), so Ghost can render the whole mount eagerly. Content
// hashes are left empty here — walking 50k files to hash them all would be slow;
// Ghost fills the hash lazily on first read (OneDrive-style).
func (fs *FileServer) tree() (proto.TreeResp, error) {
	var out proto.TreeResp
	err := filepath.WalkDir(fs.root, func(abs string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable paths rather than aborting the whole walk
		}
		rel, rerr := fs.mapper.RemoteToRel(abs)
		if rerr != nil {
			return nil
		}
		if rel == "" || rel == "." {
			return nil // don't emit the root itself
		}
		if fs.matcher != nil && fs.matcher.Match(rel, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		out.Entries = append(out.Entries, proto.TreeEntry{
			Rel:     rel,
			IsDir:   d.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime().Unix(),
			Mode:    uint32(info.Mode().Perm()),
		})
		return nil
	})
	return out, err
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}
