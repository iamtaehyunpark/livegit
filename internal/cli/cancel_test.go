package cli

import (
	"os"
	"path/filepath"
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
