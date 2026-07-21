package agent

import (
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
