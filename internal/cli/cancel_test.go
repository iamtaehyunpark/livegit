package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iamtaehyunpark/livegit/internal/config"
)

// clearCache empties the content cache but never touches a file whose journal
// entry is still pending (only copy of the edit), and prunes emptied dirs.
func TestClearCacheKeepsPending(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LG_HOME", home)
	cdir := config.CacheDir()
	for _, p := range []string{"a/one.txt", "a/two.txt", "b/edit.txt", "c/part.bin.lg-tmp"} {
		abs := filepath.Join(cdir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte("xx"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	removed, freed, kept := clearCache(map[string]bool{"b/edit.txt": true})
	if removed != 3 || kept != 1 || freed != 6 {
		t.Fatalf("removed=%d freed=%d kept=%d, want 3/6/1", removed, freed, kept)
	}
	if _, err := os.Stat(filepath.Join(cdir, "b/edit.txt")); err != nil {
		t.Fatal("pending file must survive")
	}
	if _, err := os.Stat(filepath.Join(cdir, "a")); !os.IsNotExist(err) {
		t.Fatal("emptied dir must be pruned")
	}

	// sweepStaging removes only .lg-tmp leftovers.
	tmp := filepath.Join(cdir, "d", "x.lg-tmp")
	os.MkdirAll(filepath.Dir(tmp), 0o755)
	os.WriteFile(tmp, []byte("yyy"), 0o644)
	n, bytes := sweepStaging()
	if n != 1 || bytes != 3 {
		t.Fatalf("sweep n=%d bytes=%d, want 1/3", n, bytes)
	}
	if _, err := os.Stat(filepath.Join(cdir, "b/edit.txt")); err != nil {
		t.Fatal("sweep must not touch real cache content")
	}
}

func TestGaugeAndETA(t *testing.T) {
	if g := gauge(0.5, 10); g != "[█████░░░░░]" {
		t.Fatalf("gauge(0.5)=%q", g)
	}
	if g := gauge(0, 10); g != "[░░░░░░░░░░]" {
		t.Fatalf("gauge(0)=%q", g)
	}
	if g := gauge(1.7, 10); g != "[██████████]" {
		t.Fatalf("gauge(clamped)=%q", g)
	}
	for in, want := range map[float64]string{75: "1:15", 3725: "1:02:05", -1: "--:--"} {
		if got := fmtETA(in); got != want {
			t.Fatalf("fmtETA(%v)=%q want %q", in, got, want)
		}
	}
}

func TestResolveDownloadArgs(t *testing.T) {
	dls := []inflight{
		{rel: "a/deep/path/file1.jsonl"},
		{rel: "b/other/file2.jsonl"},
		{rel: "c/dup/name.bin"},
		{rel: "d/dup/name.bin"},
	}
	// Exact rel and filename fragment both resolve.
	rels, err := resolveDownloadArgs([]string{"a/deep/path/file1.jsonl", "file2.jsonl"}, dls, "/mnt")
	if err != nil || len(rels) != 2 || rels[0] != "a/deep/path/file1.jsonl" || rels[1] != "b/other/file2.jsonl" {
		t.Fatalf("rels=%v err=%v", rels, err)
	}
	// Absolute path under the mount resolves.
	rels, err = resolveDownloadArgs([]string{"/mnt/b/other/file2.jsonl"}, dls, "/mnt")
	if err != nil || len(rels) != 1 || rels[0] != "b/other/file2.jsonl" {
		t.Fatalf("abs: rels=%v err=%v", rels, err)
	}
	// Ambiguous fragment errors and names both.
	if _, err := resolveDownloadArgs([]string{"name.bin"}, dls, "/mnt"); err == nil || !strings.Contains(err.Error(), "c/dup/name.bin") {
		t.Fatalf("ambiguous should error with candidates, got %v", err)
	}
	// No match errors.
	if _, err := resolveDownloadArgs([]string{"zzz.txt"}, dls, "/mnt"); err == nil {
		t.Fatal("no match must error")
	}
}
