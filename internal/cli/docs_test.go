package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The doc sync must: write missing files stamped, refresh only marker-carrying
// files whose version differs, never churn on dev builds, and never touch an
// unmarked (user-owned) file.
func TestSyncDocFile(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "AGENTS.md")
	content := []byte("# agents\nbody v1\n")

	if got := syncDocFile(dst, content, "v1.2.2"); got != "written" {
		t.Fatalf("missing file: got %q, want written", got)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if v := docVersion(b); v != "v1.2.2" {
		t.Errorf("stamped version = %q, want v1.2.2", v)
	}
	if !bytes.HasSuffix(b, content) {
		t.Errorf("content not preserved after stamp:\n%s", b)
	}

	if got := syncDocFile(dst, content, "v1.2.2"); got != "" {
		t.Errorf("same version: got %q, want no-op", got)
	}

	newContent := []byte("# agents\nbody v2\n")
	if got := syncDocFile(dst, newContent, "v1.3.0"); got != "refreshed" {
		t.Fatalf("newer version: got %q, want refreshed", got)
	}
	b, _ = os.ReadFile(dst)
	if v := docVersion(b); v != "v1.3.0" {
		t.Errorf("refreshed version = %q, want v1.3.0", v)
	}
	if !bytes.Contains(b, []byte("body v2")) {
		t.Errorf("refreshed content missing:\n%s", b)
	}

	// dev builds carry no comparable version: stamped files are never churned.
	if got := syncDocFile(dst, content, "dev"); got != "" {
		t.Errorf("dev build: got %q, want no-op", got)
	}

	// An unmarked file is user-owned (pre-existing, marker removed, or written
	// by a pre-marker lg) — never overwritten, whatever the version says.
	own := filepath.Join(dir, "GUIDE.md")
	if err := os.WriteFile(own, []byte("my own guide\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := syncDocFile(own, content, "v9.9.9"); got != "" {
		t.Errorf("unmarked file: got %q, want no-op", got)
	}
	b, _ = os.ReadFile(own)
	if string(b) != "my own guide\n" {
		t.Errorf("unmarked file was modified:\n%s", b)
	}
}
