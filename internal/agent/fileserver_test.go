package agent

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/iamtaehyunpark/livegit/internal/proto"
)

// del() must handle every path kind without erroring the RPC: an error return
// re-queues the journal entry forever and blocks the whole flush queue behind
// it (the src/__pycache__ bug — hashing a directory returned EISDIR).
func TestDelDirectory(t *testing.T) {
	root := t.TempDir()
	fs := NewFileServer(root, nil)

	// Empty dir: removed, OK.
	if err := os.Mkdir(filepath.Join(root, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}
	ack, err := fs.del(proto.DelReq{Rel: "emptydir"})
	if err != nil || !ack.OK {
		t.Fatalf("empty dir delete: ack=%+v err=%v", ack, err)
	}
	if _, err := os.Stat(filepath.Join(root, "emptydir")); !os.IsNotExist(err) {
		t.Fatal("empty dir should be gone")
	}

	// Non-empty dir: kept, conflict ack, NO error (an error would wedge the queue).
	if err := os.MkdirAll(filepath.Join(root, "fulldir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "fulldir", "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ack, err = fs.del(proto.DelReq{Rel: "fulldir"})
	if err != nil {
		t.Fatalf("non-empty dir delete must not error: %v", err)
	}
	if ack.OK || !ack.Conflict {
		t.Fatalf("non-empty dir delete: want conflict ack, got %+v", ack)
	}
	if _, err := os.Stat(filepath.Join(root, "fulldir", "keep.txt")); err != nil {
		t.Fatal("non-empty dir contents must be preserved")
	}
}

// A dir delete arrives after Ghost unlinked everything it had synced, so
// leftovers that could never sync (empty subdir skeletons, macOS junk,
// ignore-matched content) must not block it — the user deleted a fully-synced
// dir and expects it gone, not resurrected by the next tree sync. Real
// non-ignored files still decline.
func TestDelDirectoryUnsyncedLeftovers(t *testing.T) {
	root := t.TempDir()
	fs := NewFileServer(root, config.NewMatcher([]string{"*.pt", ".venv/"}))

	// Only unsyncable leftovers: delete completes recursively.
	gone := filepath.Join(root, "gone")
	if err := os.MkdirAll(filepath.Join(gone, "__pycache__"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(gone, ".venv", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{".DS_Store", "model.pt", ".venv/lib/site.py"} {
		if err := os.WriteFile(filepath.Join(gone, filepath.FromSlash(f)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ack, err := fs.del(proto.DelReq{Rel: "gone"})
	if err != nil || !ack.OK {
		t.Fatalf("junk-only dir delete: ack=%+v err=%v", ack, err)
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Fatal("dir with only unsyncable leftovers should be gone")
	}

	// One real (synced-class) file among the junk: kept, conflict ack.
	keep := filepath.Join(root, "keep")
	if err := os.MkdirAll(filepath.Join(keep, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keep, "sub", "data.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ack, err = fs.del(proto.DelReq{Rel: "keep"})
	if err != nil {
		t.Fatalf("unsynced-content dir delete must not error: %v", err)
	}
	if ack.OK || !ack.Conflict {
		t.Fatalf("want conflict ack, got %+v", ack)
	}
	if _, err := os.Stat(filepath.Join(keep, "sub", "data.txt")); err != nil {
		t.Fatal("unsynced file must be preserved")
	}
}

func TestDelMissingAndSymlink(t *testing.T) {
	root := t.TempDir()
	fs := NewFileServer(root, nil)

	// Already gone: OK.
	ack, err := fs.del(proto.DelReq{Rel: "nope.txt"})
	if err != nil || !ack.OK {
		t.Fatalf("missing path delete: ack=%+v err=%v", ack, err)
	}

	// Symlink (even to a directory): the link is removed, the target survives.
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	ack, err = fs.del(proto.DelReq{Rel: "link"})
	if err != nil || !ack.OK {
		t.Fatalf("symlink delete: ack=%+v err=%v", ack, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "link")); !os.IsNotExist(err) {
		t.Fatal("symlink should be gone")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal("symlink target must survive")
	}
}

func TestDelFileConflictStillDetected(t *testing.T) {
	root := t.TempDir()
	fs := NewFileServer(root, nil)
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("newer"), 0o644); err != nil {
		t.Fatal(err)
	}
	// BaseHash from some older content: conflict, file kept.
	ack, err := fs.del(proto.DelReq{Rel: "f.txt", BaseHash: "deadbeef"})
	if err != nil {
		t.Fatal(err)
	}
	if ack.OK || !ack.Conflict {
		t.Fatalf("want conflict ack, got %+v", ack)
	}
	if _, err := os.Stat(filepath.Join(root, "f.txt")); err != nil {
		t.Fatal("conflicting file must be preserved")
	}

	// Matching/empty BaseHash: last-write-wins delete goes through.
	ack, err = fs.del(proto.DelReq{Rel: "f.txt"})
	if err != nil || !ack.OK {
		t.Fatalf("plain delete: ack=%+v err=%v", ack, err)
	}
}

// write() must overwrite a read-only existing file (git pack files are 0444):
// os.WriteFile applies mode only at creation, so without the chmod-retry the
// open fails EACCES and that one entry wedges the whole flush queue behind it
// (the EngramTrace/.git pack bug). A mode-only change to an existing file must
// also actually land on Source.
func TestWriteReadOnlyTargetAndModeChange(t *testing.T) {
	root := t.TempDir()
	fs := NewFileServer(root, nil)
	abs := filepath.Join(root, "pack.idx")

	if err := os.WriteFile(abs, []byte("old"), 0o444); err != nil {
		t.Fatal(err)
	}
	ack, err := fs.write(proto.WriteReq{Rel: "pack.idx", Content: []byte("new"), Mode: 0o444})
	if err != nil || !ack.OK {
		t.Fatalf("write over read-only file: ack=%+v err=%v", ack, err)
	}
	b, err := os.ReadFile(abs)
	if err != nil || string(b) != "new" {
		t.Fatalf("content = %q err=%v, want %q", b, err, "new")
	}
	if info, _ := os.Stat(abs); info.Mode().Perm() != 0o444 {
		t.Fatalf("mode = %o, want 0444", info.Mode().Perm())
	}

	// chmod journaled as a write against an existing file: mode must change.
	ack, err = fs.write(proto.WriteReq{Rel: "pack.idx", Content: []byte("new"), Mode: 0o644})
	if err != nil || !ack.OK {
		t.Fatalf("mode-change write: ack=%+v err=%v", ack, err)
	}
	if info, _ := os.Stat(abs); info.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %o, want 0644", info.Mode().Perm())
	}
}

// decodeTreePageT gunzips + decodes one TreeResp page (test helper, shared
// with the integration test).
func decodeTreePageT(t *testing.T, gz []byte) []proto.TreeEntry {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var out []proto.TreeEntry
	if err := json.NewDecoder(zr).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Reads are served in bounded chunks: offsets walk the file, More flags the
// tail, and the concatenation reproduces the exact content.
func TestReadChunked(t *testing.T) {
	root := t.TempDir()
	fs := NewFileServer(root, nil)
	content := []byte("0123456789") // 10 bytes, MaxLen 4 -> chunks of 4/4/2
	if err := os.WriteFile(filepath.Join(root, "f.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	var got []byte
	off := int64(0)
	for i := 0; ; i++ {
		resp, err := fs.read(proto.ReadReq{Rel: "f.bin", Offset: off, MaxLen: 4})
		if err != nil || !resp.Found {
			t.Fatalf("chunk %d: resp=%+v err=%v", i, resp, err)
		}
		if resp.Size != int64(len(content)) {
			t.Fatalf("chunk %d: size=%d want %d", i, resp.Size, len(content))
		}
		got = append(got, resp.Content...)
		off += int64(len(resp.Content))
		if !resp.More {
			break
		}
		if i > 5 {
			t.Fatal("chunk loop did not terminate")
		}
	}
	if string(got) != string(content) {
		t.Fatalf("reassembled=%q want %q", got, content)
	}

	// Offset past EOF (file shrank between chunks): empty final chunk, no error.
	resp, err := fs.read(proto.ReadReq{Rel: "f.bin", Offset: 99, MaxLen: 4})
	if err != nil || resp.More || len(resp.Content) != 0 {
		t.Fatalf("past-EOF read: resp=%+v err=%v", resp, err)
	}
}

// A chunked write stages into a sidecar (never a half-written real file) and
// the final chunk commits atomically with the usual conflict/mode/mtime logic.
func TestWriteChunked(t *testing.T) {
	root := t.TempDir()
	fs := NewFileServer(root, nil)

	chunks := []proto.WriteReq{
		{Rel: "big.bin", Content: []byte("aaaa"), Offset: 0, More: true, Mode: 0o644},
		{Rel: "big.bin", Content: []byte("bbbb"), Offset: 4, More: true, Mode: 0o644},
		{Rel: "big.bin", Content: []byte("cc"), Offset: 8, More: false, Mode: 0o644, ModTime: 1700000000},
	}
	for i, c := range chunks {
		ack, err := fs.write(c)
		if err != nil || !ack.OK {
			t.Fatalf("chunk %d: ack=%+v err=%v", i, ack, err)
		}
		if c.More {
			if _, err := os.Stat(filepath.Join(root, "big.bin")); !os.IsNotExist(err) {
				t.Fatalf("chunk %d: real file must not exist before commit", i)
			}
		}
	}
	got, err := os.ReadFile(filepath.Join(root, "big.bin"))
	if err != nil || string(got) != "aaaabbbbcc" {
		t.Fatalf("committed=%q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, "big.bin.lg-part")); !os.IsNotExist(err) {
		t.Fatal("staging sidecar must be gone after commit")
	}

	// A chunk that skips ahead (lost predecessor) must fail the write so the
	// Ghost restarts it, never silently commit a file with a hole.
	if _, err := fs.write(proto.WriteReq{Rel: "gap.bin", Content: []byte("x"), Offset: 0, More: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.write(proto.WriteReq{Rel: "gap.bin", Content: []byte("z"), Offset: 5, More: false}); err == nil {
		t.Fatal("gapped chunk must error")
	}
	if _, err := os.Stat(filepath.Join(root, "gap.bin")); !os.IsNotExist(err) {
		t.Fatal("gapped write must not commit")
	}
}

// The tree snapshot pages correctly, honors ignores, answers Unchanged for a
// matching digest, and rejects pages of an expired snapshot.
func TestTreePagingAndDigest(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"a.txt", "b.txt", "sub/c.txt", "sub/d.txt", ".venv/skip.txt"} {
		abs := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fs := NewFileServer(root, config.NewMatcher([]string{".venv/"}))
	fs.treePageSize = 2

	first, err := fs.treePage(proto.TreeReq{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Unchanged || first.Digest == "" || first.Pages < 2 {
		t.Fatalf("first page resp=%+v", first)
	}
	entries := decodeTreePageT(t, first.Gz)
	for cur := 1; cur < first.Pages; cur++ {
		page, err := fs.treePage(proto.TreeReq{Digest: first.Digest, Cursor: cur})
		if err != nil {
			t.Fatalf("page %d: %v", cur, err)
		}
		entries = append(entries, decodeTreePageT(t, page.Gz)...)
	}
	seen := map[string]bool{}
	for i, e := range entries {
		seen[e.Rel] = true
		if i > 0 && entries[i-1].Rel >= e.Rel {
			t.Fatalf("entries not sorted: %q then %q", entries[i-1].Rel, e.Rel)
		}
	}
	for _, want := range []string{"a.txt", "b.txt", "sub", "sub/c.txt", "sub/d.txt"} {
		if !seen[want] {
			t.Fatalf("missing %q in %+v", want, entries)
		}
	}
	if seen[".venv/skip.txt"] || seen[".venv"] {
		t.Fatal("ignored path leaked into the tree")
	}

	// Same tree, digest echoed back: Unchanged, no page data.
	again, err := fs.treePage(proto.TreeReq{Digest: first.Digest})
	if err != nil || !again.Unchanged || len(again.Gz) != 0 {
		t.Fatalf("unchanged resp=%+v err=%v", again, err)
	}

	// A change breaks the digest and produces a fresh snapshot; pages of the
	// old snapshot are then refused.
	if err := os.WriteFile(filepath.Join(root, "e.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	fresh, err := fs.treePage(proto.TreeReq{Digest: first.Digest})
	if err != nil || fresh.Unchanged || fresh.Digest == first.Digest {
		t.Fatalf("post-change resp=%+v err=%v", fresh, err)
	}
	if _, err := fs.treePage(proto.TreeReq{Digest: first.Digest, Cursor: 1}); err == nil {
		t.Fatal("stale-digest page fetch must error")
	}
}

// rename moves files and whole directories in place; a missing source or an
// occupied destination declines with OK=false (no error — an error would read
// as a transport failure and tear down the ghost's fallback logic).
func TestRenameInPlace(t *testing.T) {
	root := t.TempDir()
	fs := NewFileServer(root, nil)
	for _, p := range []string{"d/a.txt", "d/sub/b.txt", "solo.txt"} {
		abs := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(p), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ack, err := fs.rename(proto.RenameReq{OldRel: "d", NewRel: "moved/d2"})
	if err != nil || !ack.OK {
		t.Fatalf("dir rename ack=%+v err=%v", ack, err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "moved/d2/sub/b.txt")); err != nil || string(got) != "d/sub/b.txt" {
		t.Fatalf("moved content=%q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, "d")); !os.IsNotExist(err) {
		t.Fatal("old dir must be gone")
	}

	ack, err = fs.rename(proto.RenameReq{OldRel: "ghost.txt", NewRel: "x.txt"})
	if err != nil || ack.OK {
		t.Fatalf("missing source must decline, ack=%+v err=%v", ack, err)
	}

	// Renaming onto a non-empty directory declines rather than clobbering it.
	ack, err = fs.rename(proto.RenameReq{OldRel: "solo.txt", NewRel: "moved/d2"})
	if err != nil || ack.OK {
		t.Fatalf("occupied destination must decline, ack=%+v err=%v", ack, err)
	}
}
