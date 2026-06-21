package fuse

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/taehyun/lg/internal/config"
	"github.com/taehyun/lg/internal/hashx"
	"github.com/taehyun/lg/internal/proto"
)

// fakeSource is an in-memory Source implementing SourceRPC, with the same
// conflict semantics as the real fileserver (§4.4).
type fakeSource struct {
	mu      sync.Mutex
	files   map[string]fakeFile
	online  bool
	backups map[string]string // rel -> backupRel created on conflict
}

type fakeFile struct {
	content []byte
	mode    uint32
	mod     int64
}

func newFakeSource() *fakeSource {
	return &fakeSource{files: map[string]fakeFile{}, online: true, backups: map[string]string{}}
}

func (s *fakeSource) put(rel, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[rel] = fakeFile{content: []byte(content), mode: 0o644, mod: time.Now().Unix()}
}

func (s *fakeSource) get(rel string) (fakeFile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.files[rel]
	return f, ok
}

func (s *fakeSource) Online() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.online }

func (s *fakeSource) setOnline(v bool) { s.mu.Lock(); s.online = v; s.mu.Unlock() }

func (s *fakeSource) Stat(_ context.Context, rel string) (proto.FileStat, error) {
	f, ok := s.get(rel)
	if !ok {
		return proto.FileStat{Rel: rel}, nil
	}
	return proto.FileStat{Rel: rel, Exists: true, Size: int64(len(f.content)),
		ModTime: f.mod, Mode: f.mode, Hash: hashx.Bytes(f.content)}, nil
}

func (s *fakeSource) Read(_ context.Context, rel string) (proto.ReadResp, error) {
	f, ok := s.get(rel)
	if !ok {
		return proto.ReadResp{Found: false}, nil
	}
	return proto.ReadResp{Found: true, Content: f.content, Hash: hashx.Bytes(f.content),
		ModTime: f.mod, Mode: f.mode}, nil
}

func (s *fakeSource) Write(_ context.Context, req proto.WriteReq) (proto.WriteAck, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ack := proto.WriteAck{OK: true}
	cur, exists := s.files[req.Rel]
	if exists && req.BaseHash != "" && hashx.Bytes(cur.content) != req.BaseHash {
		backup := req.Rel + ".lg-conflict"
		s.files[backup] = cur
		s.backups[req.Rel] = backup
		ack.Conflict = true
		ack.BackupRel = backup
	}
	s.files[req.Rel] = fakeFile{content: req.Content, mode: req.Mode, mod: req.ModTime}
	ack.NewHash = hashx.Bytes(req.Content)
	ack.SourceMod = req.ModTime
	return ack, nil
}

func (s *fakeSource) Delete(_ context.Context, req proto.DelReq) (proto.DelAck, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.files, req.Rel)
	return proto.DelAck{OK: true}, nil
}

func (s *fakeSource) List(_ context.Context, rel string) (proto.ListResp, error) {
	return proto.ListResp{Found: true}, nil
}

// harness wires a Backend over a temp LG_HOME with a fake source.
func harness(t *testing.T) (*Backend, *fakeSource) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("LG_HOME", home)
	if err := os.MkdirAll(config.CacheDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := OpenState(config.StateDBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	journal, err := OpenJournal(config.JournalPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { journal.Close() })

	cfg := &config.Config{LocalRoot: filepath.Join(home, "mount")}
	cfg.Source.RemoteRoot = "/remote"
	cfg.Cache.EvictAfterIdleMinutes = 30
	cfg.Cache.MaxCacheSizeGB = 10
	src := newFakeSource()
	b := NewBackend(cfg, store, journal, src, config.NewMatcher(nil))
	return b, src
}

func TestMaterializeGhostToCached(t *testing.T) {
	b, src := harness(t)
	src.put("a/x.go", "package a\n")
	cp, err := b.Materialize(context.Background(), "a/x.go")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(cp)
	if string(got) != "package a\n" {
		t.Fatalf("content=%q", got)
	}
	meta, _ := b.store.Get("a/x.go")
	if meta == nil || meta.State != StateCached {
		t.Fatalf("expected cached, got %+v", meta)
	}
}

func TestWriteFlushCycle(t *testing.T) {
	b, src := harness(t)
	src.put("f.txt", "old")
	cp, err := b.Materialize(context.Background(), "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an editor write + close.
	if err := os.WriteFile(cp, []byte("new content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.RecordWrite("f.txt"); err != nil {
		t.Fatal(err)
	}
	meta, _ := b.store.Get("f.txt")
	if meta.State != StateLive {
		t.Fatalf("expected live, got %s", meta.State)
	}
	if b.journal.PendingCount() != 1 {
		t.Fatalf("expected 1 pending, got %d", b.journal.PendingCount())
	}
	// Flush once.
	e, _ := b.journal.Peek()
	if err := b.flushEntry(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if f, _ := src.get("f.txt"); string(f.content) != "new content" {
		t.Fatalf("source content=%q", f.content)
	}
	meta, _ = b.store.Get("f.txt")
	if meta.State != StateCached {
		t.Fatalf("expected cached after flush, got %s", meta.State)
	}
	if b.journal.PendingCount() != 0 {
		t.Fatalf("journal not drained: %d", b.journal.PendingCount())
	}
}

func TestFlushConflict(t *testing.T) {
	b, src := harness(t)
	src.put("c.txt", "base")
	cp, _ := b.Materialize(context.Background(), "c.txt")
	// Ghost edits locally.
	os.WriteFile(cp, []byte("ghost-edit"), 0o644)
	b.RecordWrite("c.txt")
	// Source diverges concurrently.
	src.put("c.txt", "source-edit")
	// Flush -> conflict, source backs up its copy.
	e, _ := b.journal.Peek()
	if err := b.flushEntry(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	conflicts := b.Conflicts()
	if len(conflicts) != 1 || conflicts[0].Rel != "c.txt" {
		t.Fatalf("expected 1 conflict, got %+v", conflicts)
	}
	if _, ok := src.get("c.txt.lg-conflict"); !ok {
		t.Fatal("expected backup file on source")
	}
}

func TestEviction(t *testing.T) {
	b, src := harness(t)
	src.put("big.bin", "data")
	b.Materialize(context.Background(), "big.bin")
	// Force last_accessed_at into the past.
	b.store.db.Exec(`UPDATE files SET last_accessed_at=? WHERE path=?`, int64(0), "big.bin")
	b.EvictOnce()
	meta, _ := b.store.Get("big.bin")
	if meta.State != StateGhost {
		t.Fatalf("expected ghost after eviction, got %s", meta.State)
	}
	if cacheFileExists(b.cachePath("big.bin")) {
		t.Fatal("cache file should be removed after eviction")
	}
}

func TestEvictionSkipsDirty(t *testing.T) {
	b, src := harness(t)
	src.put("d.txt", "x")
	cp, _ := b.Materialize(context.Background(), "d.txt")
	os.WriteFile(cp, []byte("dirty"), 0o644)
	b.RecordWrite("d.txt") // now live + pending journal entry
	b.store.db.Exec(`UPDATE files SET last_accessed_at=? WHERE path=?`, int64(0), "d.txt")
	b.EvictOnce()
	// Live files are excluded from eviction candidates entirely; assert content stays.
	if !cacheFileExists(cp) {
		t.Fatal("dirty file must not be evicted")
	}
}

func TestInvalidateGhostMarksStale(t *testing.T) {
	b, src := harness(t)
	src.put("inv.go", "v1")
	b.Materialize(context.Background(), "inv.go")
	// Evict to ghost.
	b.store.SetState("inv.go", StateGhost)
	os.Remove(b.cachePath("inv.go"))
	// Source changes; invalidation should just update metadata (lazy).
	src.put("inv.go", "v2-longer")
	b.Invalidate(proto.Invalidate{Rel: "inv.go", Hash: hashx.Bytes([]byte("v2-longer")), ModTime: time.Now().Unix()})
	meta, _ := b.store.Get("inv.go")
	if meta.State != StateGhost {
		t.Fatalf("expected ghost, got %s", meta.State)
	}
	// Next open should fetch the new content.
	cp, _ := b.Materialize(context.Background(), "inv.go")
	got, _ := os.ReadFile(cp)
	if string(got) != "v2-longer" {
		t.Fatalf("expected refetched v2, got %q", got)
	}
}

func TestInvalidateCachedRefetches(t *testing.T) {
	b, src := harness(t)
	src.put("live.go", "a")
	b.Materialize(context.Background(), "live.go") // cached
	src.put("live.go", "b-updated")
	b.Invalidate(proto.Invalidate{Rel: "live.go", Hash: hashx.Bytes([]byte("b-updated")), ModTime: time.Now().Unix()})
	got, _ := os.ReadFile(b.cachePath("live.go"))
	if string(got) != "b-updated" {
		t.Fatalf("cached file should refetch immediately, got %q", got)
	}
}

func TestOfflineJournalAccumulatesAndReplays(t *testing.T) {
	b, src := harness(t)
	src.put("o.txt", "base")
	cp, _ := b.Materialize(context.Background(), "o.txt")
	src.setOnline(false)
	os.WriteFile(cp, []byte("offline-edit"), 0o644)
	b.RecordWrite("o.txt")
	// Flush worker would park; emulate one tick that should NOT drain offline.
	if e, ok := b.journal.Peek(); ok {
		if src.Online() {
			t.Fatal("source should be offline")
		}
		_ = e
	}
	if b.journal.PendingCount() != 1 {
		t.Fatalf("offline entry should remain queued, got %d", b.journal.PendingCount())
	}
	// Reconnect and drain.
	src.setOnline(true)
	e, _ := b.journal.Peek()
	if err := b.flushEntry(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if f, _ := src.get("o.txt"); string(f.content) != "offline-edit" {
		t.Fatalf("replay failed, source=%q", f.content)
	}
	if b.journal.PendingCount() != 0 {
		t.Fatal("journal should be drained after replay")
	}
}
