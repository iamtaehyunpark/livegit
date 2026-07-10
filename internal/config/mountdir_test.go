package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestMountDir covers both mountpoint modes: a stored local_root pins the
// path; empty derives a sibling of the project dir named after the Source
// repo, so the mount follows the project when it moves.
func TestMountDir(t *testing.T) {
	pinned := &Config{LocalRoot: "/pinned/mount/"}
	if got := pinned.MountDir(); got != "/pinned/mount" {
		t.Fatalf("pinned: got %q", got)
	}

	proj := t.TempDir()
	t.Setenv("LG_HOME", filepath.Join(proj, ".lg"))
	c := &Config{}
	c.Source.RemoteRoot = "/home/u/code/myrepo/"
	if got, want := c.MountDir(), filepath.Join(proj, "myrepo"); got != want {
		t.Fatalf("derived: got %q, want %q", got, want)
	}
}

// TestValidateLocalRoot: empty local_root is valid for a ghost (derived at
// runtime); a relative pin is still rejected.
func TestValidateLocalRoot(t *testing.T) {
	c := &Config{Role: RoleGhost}
	c.Source.Host = "h"
	c.Source.RemoteRoot = "/repo"
	if err := c.Validate(); err != nil {
		t.Fatalf("empty local_root should validate (derived at runtime): %v", err)
	}
	c.LocalRoot = "relative/path"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative local_root should fail validation, got %v", err)
	}
}
