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

// fakeSource is an in-memory Source implementing SourceRPC. Writes are last-
// write-wins (no conflict apparatus), matching the new model.
type fakeSource struct {
	mu     sync.Mutex
	files  map[string]fakeFile
	online bool
}

type fakeFile struct {
	content []byte
	mode    uint32
	mod     int64
}

func newFakeSource() *fakeSource {
	return &fakeSource{files: map[string]fakeFile{}, online: true}
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
	s.files[req.Rel] = fakeFile{content: req.Content, mode: req.Mode, mod: req.ModTime}
	return proto.WriteAck{OK: true, NewHash: hashx.Bytes(req.Content), SourceMod: req.ModTime}, nil
}

func (s *fakeSource) Delete(_ context.Context, req proto.DelReq) (proto.DelAck, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.files, req.Rel)
	return proto.DelAck{OK: true}, nil
}

func (s *fakeSource) Tree(_ context.Context) ([]proto.TreeEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []proto.TreeEntry
	for rel, f := range s.files {
		out = append(out, proto.TreeEntry{
			Rel: rel, Size: int64(len(f.content)), ModTime: f.mod,
			Mode: f.mode, Hash: hashx.Bytes(f.content),
		})
	}
	return out, nil
}

// harness wires a Backend over a temp LG_HOME with a fake source.
func harness(t *testing.T) (*Backend, *fakeSource) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("LG_HOME", home)
	if err := os.MkdirAll(config.CacheDir(), 0o755); err != nil {
		t.Fatal(err)
	}
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
	b := NewBackend(cfg, journal, src, config.NewMatcher(nil))
	return b, src
}

func TestMaterializeFetchesContent(t *testing.T) {
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
	if e, ok := b.index.Get("a/x.go"); !ok || !e.HaveContent {
		t.Fatalf("expected indexed with content, got %+v ok=%v", e, ok)
	}
}

func TestMaterializeOfflineUncachedErrors(t *testing.T) {
	b, src := harness(t)
	src.put("z.txt", "hi")
	src.setOnline(false)
	if _, err := b.Materialize(context.Background(), "z.txt"); err == nil {
		t.Fatal("expected error reading uncached file while offline")
	}
}

func TestFullTreeReaddirAndAttr(t *testing.T) {
	b, src := harness(t)
	src.put("dir/a.txt", "hello")
	src.put("dir/b.txt", "worldwide")
	src.put("top.txt", "x")
	if err := b.SyncTree(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Whole tree browsable without opening anything.
	rootKids, _ := b.Readdir(context.Background(), "")
	names := map[string]bool{}
	for _, e := range rootKids {
		names[e.Name] = true
	}
	if !names["dir"] || !names["top.txt"] {
		t.Fatalf("root listing missing entries: %+v", rootKids)
	}
	dirKids, _ := b.Readdir(context.Background(), "dir")
	if len(dirKids) != 2 {
		t.Fatalf("expected 2 children under dir, got %+v", dirKids)
	}
	// Unopened file shows its real size (OneDrive-style), not 0.
	a, err := b.Getattr(context.Background(), "dir/b.txt")
	if err != nil || !a.Exists || a.Size != int64(len("worldwide")) {
		t.Fatalf("expected real size for unopened file, got %+v err=%v", a, err)
	}
}

func TestWriteFlushLastWriteWins(t *testing.T) {
	b, src := harness(t)
	src.put("f.txt", "old")
	cp, err := b.Materialize(context.Background(), "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	// Source diverges after we cached it; last-write-wins means our edit still
	// overwrites it on flush (no conflict, no backup).
	src.put("f.txt", "source-changed")
	if err := os.WriteFile(cp, []byte("new content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.RecordWrite("f.txt"); err != nil {
		t.Fatal(err)
	}
	if b.journal.PendingCount() != 1 {
		t.Fatalf("expected 1 pending, got %d", b.journal.PendingCount())
	}
	e, _ := b.journal.Peek()
	if err := b.flushEntry(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if f, _ := src.get("f.txt"); string(f.content) != "new content" {
		t.Fatalf("source content=%q", f.content)
	}
	if b.journal.PendingCount() != 0 {
		t.Fatalf("journal not drained: %d", b.journal.PendingCount())
	}
}

func TestRenameAtomicSave(t *testing.T) {
	b, src := harness(t)
	src.put("main.go", "package main // old\n")
	// Editor atomic-save: write a fresh tmp file in the cache, then rename it
	// over the target. This is exactly the pattern that failed with ENOTSUP.
	tmpRel := "main.go.tmp.123"
	tmpCache := b.cachePath(tmpRel)
	if err := os.WriteFile(tmpCache, []byte("package main // new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b.markLiveNew(tmpRel, 0o644)
	if err := b.RecordRename(context.Background(), tmpRel, "main.go"); err != nil {
		t.Fatal(err)
	}
	// The tmp name is gone; the target holds the new bytes.
	if _, ok := b.index.Get(tmpRel); ok {
		t.Fatal("tmp name should be gone after rename")
	}
	if cacheFileExists(tmpCache) {
		t.Fatal("tmp cache file should have been moved, not left behind")
	}
	got, err := os.ReadFile(b.cachePath("main.go"))
	if err != nil || string(got) != "package main // new\n" {
		t.Fatalf("target content=%q err=%v", got, err)
	}
	// Draining the journal pushes the new content and removes the tmp on Source.
	for {
		e, ok := b.journal.Peek()
		if !ok {
			break
		}
		if err := b.flushEntry(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	if f, _ := src.get("main.go"); string(f.content) != "package main // new\n" {
		t.Fatalf("source main.go=%q after flush", f.content)
	}
	if _, ok := src.get(tmpRel); ok {
		t.Fatal("tmp file should not exist on source after flush")
	}
}

func TestRenameUnopenedFileMaterializes(t *testing.T) {
	b, src := harness(t)
	src.put("README.md", "# docs\n")
	if err := b.SyncTree(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Rename a file that was synced from Source but never opened (no cached bytes).
	if err := b.RecordRename(context.Background(), "README.md", "README2.md"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(b.cachePath("README2.md"))
	if err != nil || string(got) != "# docs\n" {
		t.Fatalf("renamed content=%q err=%v", got, err)
	}
	for {
		e, ok := b.journal.Peek()
		if !ok {
			break
		}
		if err := b.flushEntry(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := src.get("README.md"); ok {
		t.Fatal("old name should be deleted on source")
	}
	if f, _ := src.get("README2.md"); string(f.content) != "# docs\n" {
		t.Fatalf("source README2.md=%q", f.content)
	}
}

func TestRenameDirMovesSubtree(t *testing.T) {
	b, src := harness(t)
	src.put("old/a.txt", "aaa")
	src.put("old/sub/b.txt", "bbb")
	if err := b.SyncTree(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.RecordRename(context.Background(), "old", "new"); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.index.Get("old"); ok {
		t.Fatal("old dir should be gone from index")
	}
	if _, ok := b.index.Get("new/sub/b.txt"); !ok {
		t.Fatal("nested file should have moved under new/")
	}
	for {
		e, ok := b.journal.Peek()
		if !ok {
			break
		}
		if err := b.flushEntry(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	if f, _ := src.get("new/sub/b.txt"); string(f.content) != "bbb" {
		t.Fatalf("source new/sub/b.txt=%q", f.content)
	}
	if _, ok := src.get("old/a.txt"); ok {
		t.Fatal("old subtree should be deleted on source")
	}
}

func TestSetattrTruncateSyncs(t *testing.T) {
	b, src := harness(t)
	src.put("log.txt", "aaaaaaaaaa") // 10 bytes
	cp, err := b.Materialize(context.Background(), "log.txt")
	if err != nil {
		t.Fatal(err)
	}
	size := int64(4)
	if err := b.RecordSetattr(context.Background(), "log.txt", &size, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(cp)
	if string(got) != "aaaa" {
		t.Fatalf("truncated cache=%q", got)
	}
	for {
		e, ok := b.journal.Peek()
		if !ok {
			break
		}
		if err := b.flushEntry(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	if f, _ := src.get("log.txt"); string(f.content) != "aaaa" {
		t.Fatalf("source not truncated: %q", f.content)
	}
}

func TestSetattrChmodSyncs(t *testing.T) {
	b, src := harness(t)
	src.put("run.sh", "#!/bin/sh\n")
	if _, err := b.Materialize(context.Background(), "run.sh"); err != nil {
		t.Fatal(err)
	}
	mode := uint32(0o755)
	if err := b.RecordSetattr(context.Background(), "run.sh", nil, &mode, nil); err != nil {
		t.Fatal(err)
	}
	for {
		e, ok := b.journal.Peek()
		if !ok {
			break
		}
		if err := b.flushEntry(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	if f, _ := src.get("run.sh"); f.mode&0o777 != 0o755 {
		t.Fatalf("source mode=%o, want 0755", f.mode&0o777)
	}
}

func TestRmdirJournalsDelete(t *testing.T) {
	b, src := harness(t)
	src.put("gone/keep.txt", "x")
	if err := b.SyncTree(context.Background()); err != nil {
		t.Fatal(err)
	}
	// rm -r removes children first (Unlink), then the dir (Rmdir == RecordDelete).
	if err := b.RecordDelete("gone/keep.txt"); err != nil {
		t.Fatal(err)
	}
	if err := b.RecordDelete("gone"); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.index.Get("gone"); ok {
		t.Fatal("dir should be gone from index")
	}
	for {
		e, ok := b.journal.Peek()
		if !ok {
			break
		}
		if err := b.flushEntry(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := src.get("gone/keep.txt"); ok {
		t.Fatal("source dir contents should be deleted")
	}
}

func TestEvictionUnderSizeCap(t *testing.T) {
	b, src := harness(t)
	src.put("big.bin", "0123456789")
	if _, err := b.Materialize(context.Background(), "big.bin"); err != nil {
		t.Fatal(err)
	}
	b.testCapBytes = 1 // force over-cap immediately
	b.EvictOnce()
	if cacheFileExists(b.cachePath("big.bin")) {
		t.Fatal("cached content should be evicted under cap")
	}
	// Metadata still present (full tree never loses entries).
	if _, ok := b.index.Get("big.bin"); !ok {
		t.Fatal("index entry must survive content eviction")
	}
}

func TestEvictionSkipsPending(t *testing.T) {
	b, src := harness(t)
	src.put("d.txt", "x")
	cp, _ := b.Materialize(context.Background(), "d.txt")
	os.WriteFile(cp, []byte("dirty"), 0o644)
	b.RecordWrite("d.txt") // pending journal entry
	b.testCapBytes = 1
	b.EvictOnce()
	if !cacheFileExists(cp) {
		t.Fatal("file with pending write must not be evicted")
	}
}

func TestInvalidateUpdatesIndexAndDropsContent(t *testing.T) {
	b, src := harness(t)
	src.put("inv.go", "v1")
	b.Materialize(context.Background(), "inv.go") // cached locally
	// Source changes; invalidation updates metadata and drops stale content.
	src.put("inv.go", "v2-longer")
	b.Invalidate(proto.Invalidate{Rel: "inv.go", Size: int64(len("v2-longer")),
		Hash: hashx.Bytes([]byte("v2-longer")), ModTime: time.Now().Unix()})
	if cacheFileExists(b.cachePath("inv.go")) {
		t.Fatal("stale cached content should be dropped on invalidate")
	}
	a, _ := b.Getattr(context.Background(), "inv.go")
	if a.Size != int64(len("v2-longer")) {
		t.Fatalf("index size not updated: %+v", a)
	}
	// Next open refetches the new content.
	cp, _ := b.Materialize(context.Background(), "inv.go")
	got, _ := os.ReadFile(cp)
	if string(got) != "v2-longer" {
		t.Fatalf("expected refetched v2, got %q", got)
	}
}

func TestInvalidateKeepsUnflushedLocalEdit(t *testing.T) {
	b, src := harness(t)
	src.put("p.txt", "base")
	cp, _ := b.Materialize(context.Background(), "p.txt")
	os.WriteFile(cp, []byte("local-edit"), 0o644)
	b.RecordWrite("p.txt") // pending
	// Source push arrives while our edit is unflushed: must NOT clobber it.
	b.Invalidate(proto.Invalidate{Rel: "p.txt", Hash: "different", ModTime: time.Now().Unix()})
	got, _ := os.ReadFile(cp)
	if string(got) != "local-edit" {
		t.Fatalf("unflushed local edit was clobbered: %q", got)
	}
}

func TestOfflineJournalAccumulatesAndReplays(t *testing.T) {
	b, src := harness(t)
	src.put("o.txt", "base")
	cp, _ := b.Materialize(context.Background(), "o.txt")
	src.setOnline(false)
	os.WriteFile(cp, []byte("offline-edit"), 0o644)
	b.RecordWrite("o.txt")
	if b.journal.PendingCount() != 1 {
		t.Fatalf("offline entry should remain queued, got %d", b.journal.PendingCount())
	}
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

func TestDeleteJournalsAndRemoves(t *testing.T) {
	b, src := harness(t)
	src.put("del.txt", "bye")
	b.Materialize(context.Background(), "del.txt")
	if err := b.RecordDelete("del.txt"); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.index.Get("del.txt"); ok {
		t.Fatal("index entry should be gone after delete")
	}
	e, _ := b.journal.Peek()
	if err := b.flushEntry(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if _, ok := src.get("del.txt"); ok {
		t.Fatal("source file should be deleted after flush")
	}
}
