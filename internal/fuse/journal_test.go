package fuse

import (
	"path/filepath"
	"testing"
)

// Repeated writes to the same path must coalesce to the newest entry (content
// is read from the cache at flush time, so older entries are pure duplicate
// uploads — a Finder folder copy journals ~10 events per file and used to
// re-upload every file ~10x). Deletes keep their place in the order.
func TestJournalCoalescesDuplicateWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.log")
	j, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}

	must := func(e JournalEntry) {
		t.Helper()
		if _, err := j.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	must(JournalEntry{Rel: "a.txt", Op: OpCreate, Mode: 0o600})
	must(JournalEntry{Rel: "a.txt", Op: OpWrite, Mode: 0o644})
	must(JournalEntry{Rel: "b.txt", Op: OpWrite, Mode: 0o644})
	must(JournalEntry{Rel: "a.txt", Op: OpDelete})
	must(JournalEntry{Rel: "a.txt", Op: OpWrite, Mode: 0o444})

	check := func(j *Journal) {
		t.Helper()
		p := j.PendingSnapshot()
		if len(p) != 3 {
			t.Fatalf("pending = %+v, want 3 entries", p)
		}
		if p[0].Rel != "b.txt" || p[0].Op != OpWrite {
			t.Fatalf("p[0] = %+v, want b.txt write", p[0])
		}
		if p[1].Rel != "a.txt" || p[1].Op != OpDelete {
			t.Fatalf("p[1] = %+v, want a.txt delete", p[1])
		}
		if p[2].Rel != "a.txt" || p[2].Op != OpWrite || p[2].Mode != 0o444 {
			t.Fatalf("p[2] = %+v, want a.txt write mode 0444", p[2])
		}
	}
	check(j)
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	// The on-disk log still holds the superseded lines (append-only); replay
	// must coalesce them the same way.
	j2, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	check(j2)
}
