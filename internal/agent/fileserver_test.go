package agent

import (
	"os"
	"path/filepath"
	"testing"

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
