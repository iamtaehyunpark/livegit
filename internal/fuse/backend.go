package fuse

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/taehyun/lg/internal/config"
	"github.com/taehyun/lg/internal/logx"
	"github.com/taehyun/lg/internal/proto"
)

// SourceRPC is the subset of the transport the Backend needs. An interface so
// the whole state machine is unit-testable against a fake Source.
type SourceRPC interface {
	Stat(ctx context.Context, rel string) (proto.FileStat, error)
	Read(ctx context.Context, rel string) (proto.ReadResp, error)
	Write(ctx context.Context, req proto.WriteReq) (proto.WriteAck, error)
	Delete(ctx context.Context, req proto.DelReq) (proto.DelAck, error)
	List(ctx context.Context, rel string) (proto.ListResp, error)
	Online() bool
}

// Conflict is a recorded divergence surfaced via `lg status` (§4.4).
type Conflict struct {
	Rel       string
	BackupRel string
	At        time.Time
	Detail    string
}

// Backend implements the ghost/cached/live state machine, journal-first write-
// through, eviction, and conflict handling. node.go/mount.go drive it from
// syscalls; everything here is plain Go.
type Backend struct {
	store    *StateStore
	journal  *Journal
	cacheDir string
	mapper   *config.PathMapper
	matcher  *config.Matcher
	source   SourceRPC
	cfg      *config.Config
	log      *slog.Logger

	mu        sync.Mutex
	conflicts []Conflict

	stop chan struct{}
}

// NewBackend assembles the Ghost FUSE backend.
func NewBackend(cfg *config.Config, store *StateStore, journal *Journal, source SourceRPC, matcher *config.Matcher) *Backend {
	return &Backend{
		store:    store,
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

// cachePath maps a rel to its materialized content file under ~/.lg/cache.
func (b *Backend) cachePath(rel string) string {
	return filepath.Join(b.cacheDir, filepath.FromSlash(config.Rel(rel)))
}

func cacheFileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// Materialize ensures rel has real local content, fetching from Source if it is
// in ghost state (the open() hook, §4.2). Returns the local cache path.
func (b *Backend) Materialize(ctx context.Context, rel string) (string, error) {
	rel = config.Rel(rel)
	cp := b.cachePath(rel)
	meta, err := b.store.Get(rel)
	if err != nil {
		return "", err
	}
	if meta != nil && (meta.State == StateCached || meta.State == StateLive) && cacheFileExists(cp) {
		_ = b.store.Touch(rel)
		return cp, nil
	}
	// Ghost (or unknown / missing local content): synchronous fetch from Source.
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
	if err := os.WriteFile(cp, resp.Content, mode); err != nil {
		return "", err
	}
	now := time.Now().Unix()
	if err := b.store.Upsert(&Meta{
		Path:           rel,
		State:          StateCached,
		ContentHash:    resp.Hash,
		LastModifiedBy: "source",
		LastModifiedAt: resp.ModTime,
		LastAccessedAt: now,
		SizeBytes:      int64(len(resp.Content)),
	}); err != nil {
		return "", err
	}
	b.log.Debug("materialized", "rel", rel, "bytes", len(resp.Content))
	return cp, nil
}

// RecordWrite is called on close() of a dirty file. It appends a journal entry
// (write-through always goes through the journal, §4.2) and marks the file live.
// The actual push to Source happens asynchronously in the flush worker.
func (b *Backend) RecordWrite(rel string) error {
	rel = config.Rel(rel)
	cp := b.cachePath(rel)
	info, err := os.Stat(cp)
	if err != nil {
		return err
	}
	meta, _ := b.store.Get(rel)
	base := ""
	op := OpWrite
	if meta != nil {
		base = meta.ContentHash
	} else {
		op = OpCreate
	}
	now := time.Now()
	if _, err := b.journal.Append(JournalEntry{
		Rel:      rel,
		Op:       op,
		BaseHash: base,
		ModTime:  now.Unix(),
		Mode:     uint32(info.Mode().Perm()),
		Ts:       now.Unix(),
	}); err != nil {
		return err
	}
	return b.store.Upsert(&Meta{
		Path:           rel,
		State:          StateLive,
		ContentHash:    base, // stays at sync point until flush acks
		LastModifiedBy: "ghost",
		LastModifiedAt: now.Unix(),
		LastAccessedAt: now.Unix(),
		SizeBytes:      info.Size(),
	})
}

// markLiveNew registers a freshly created file as live (called from Create
// before any bytes are written, so even an empty new file is tracked/flushed).
func (b *Backend) markLiveNew(rel string, mode uint32) error {
	rel = config.Rel(rel)
	perm := mode & 0o777
	if perm == 0 {
		perm = 0o644
	}
	now := time.Now().Unix()
	return b.store.Upsert(&Meta{
		Path:           rel,
		State:          StateLive,
		ContentHash:    "", // brand new: no sync point yet
		LastModifiedBy: "ghost",
		LastModifiedAt: now,
		LastAccessedAt: now,
		SizeBytes:      0,
	})
}

// RecordDelete journals a delete.
func (b *Backend) RecordDelete(rel string) error {
	rel = config.Rel(rel)
	meta, _ := b.store.Get(rel)
	base := ""
	if meta != nil {
		base = meta.ContentHash
	}
	now := time.Now()
	if _, err := b.journal.Append(JournalEntry{
		Rel: rel, Op: OpDelete, BaseHash: base, Ts: now.Unix(), ModTime: now.Unix(),
	}); err != nil {
		return err
	}
	_ = os.Remove(b.cachePath(rel))
	return b.store.SetState(rel, StateLive) // pending delete is still "dirty"
}

// recordConflict stores a conflict in memory and appends it to the conflicts
// log so `lg status` (a separate process) can surface it (§4.4).
func (b *Backend) recordConflict(c Conflict) {
	b.mu.Lock()
	b.conflicts = append(b.conflicts, c)
	b.mu.Unlock()
	b.log.Warn("conflict recorded", "rel", c.Rel, "backup", c.BackupRel)
	if f, err := os.OpenFile(config.ConflictsPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		line, _ := json.Marshal(c)
		_, _ = f.Write(append(line, '\n'))
		_ = f.Close()
	}
}

// Conflicts returns a snapshot of recorded conflicts.
func (b *Backend) Conflicts() []Conflict {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Conflict(nil), b.conflicts...)
}
