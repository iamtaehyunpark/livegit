package fuse

import (
	"context"
	"os"
	"testing"
	"time"
)

// A delete Source refuses forever (EACCES on a file owned by someone else)
// must not strand the queue behind it: the flush worker parks it and keeps
// draining. Found live on sclab 2026-08-25 — one such delete held 75k entries.
func TestFlushParksPermanentFailureAndDrainsRest(t *testing.T) {
	old := flushParkAfter
	flushParkAfter = 1 // one failure is enough here; 5 × the 2s retry is slow
	defer func() { flushParkAfter = old }()

	b, src := harness(t)
	src.put("locked.txt", "not yours")
	src.put("free.txt", "yours")
	src.delDeny = map[string]bool{"locked.txt": true}

	// Queue the undeletable one FIRST: it is the head that used to block.
	for _, rel := range []string{"locked.txt", "free.txt"} {
		if _, err := b.journal.Append(JournalEntry{Rel: rel, Op: OpDelete}); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.RunFlush(ctx)

	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, ok := src.get("free.txt"); !ok {
			break // the entry behind the wedge got through
		}
		if time.Now().After(deadline) {
			t.Fatal("free.txt still on source: the parked entry blocked the queue")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, ok := src.get("locked.txt"); !ok {
		t.Fatal("locked.txt was deleted on source; the fake should have refused")
	}
	if n := b.journal.ParkedCount(); n != 1 {
		t.Fatalf("parked = %d, want 1", n)
	}
	if n := b.journal.PendingCount(); n != 1 {
		t.Fatalf("pending = %d, want just the parked entry", n)
	}
}

// DropPending is the escape hatch: it clears queued entries without touching
// Source, forgets the parked flag, and throws away the cached copy of any
// abandoned local edit.
func TestDropPendingClearsQueueAndAbandonedEdits(t *testing.T) {
	b, _ := harness(t)

	for _, e := range []JournalEntry{
		{Rel: "dead/a.txt", Op: OpDelete},
		{Rel: "dead/sub/b.txt", Op: OpDelete},
		{Rel: "keep/c.txt", Op: OpWrite, Mode: 0o644},
	} {
		if _, err := b.journal.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	// The cached bytes of the pending write are the only copy of that edit.
	cp := b.cachePath("keep/c.txt")
	if err := os.MkdirAll(cp[:len(cp)-len("/c.txt")], 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cp, []byte("local edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	b.journal.Park(1)

	// A path takes its whole subtree, and leaves everything else alone.
	if n := b.DropPending([]string{"dead"}); n != 2 {
		t.Fatalf("dropped = %d, want 2", n)
	}
	if n := b.journal.PendingCount(); n != 1 {
		t.Fatalf("pending = %d, want 1 (keep/c.txt)", n)
	}
	if n := b.journal.ParkedCount(); n != 0 {
		t.Fatalf("parked = %d, want 0 after the parked entry was dropped", n)
	}
	if _, err := os.Stat(cp); err != nil {
		t.Fatalf("cache of an untouched pending write went missing: %v", err)
	}

	// Dropping the write abandons the edit, so its cache copy goes too —
	// keeping it would shadow Source's version on the next open.
	if n := b.DropPending(nil); n != 1 {
		t.Fatalf("drop-all dropped = %d, want 1", n)
	}
	if n := b.journal.PendingCount(); n != 0 {
		t.Fatalf("pending = %d, want 0", n)
	}
	if _, err := os.Stat(cp); !os.IsNotExist(err) {
		t.Fatalf("cached abandoned edit still present: %v", err)
	}

	// The drop must survive a restart: the on-disk log is compacted, not just
	// the in-memory list.
	if err := b.journal.Close(); err != nil {
		t.Fatal(err)
	}
	j2, err := OpenJournal(b.journal.path)
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	if n := j2.PendingCount(); n != 0 {
		t.Fatalf("pending after reopen = %d, want 0", n)
	}
}
