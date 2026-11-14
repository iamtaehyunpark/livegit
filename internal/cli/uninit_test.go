package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iamtaehyunpark/livegit/internal/config"
)

func TestUninitLocal_CleansManagedStateOnly(t *testing.T) {
	proj := t.TempDir()
	lgDir := filepath.Join(proj, ".lg")
	mountDir := filepath.Join(proj, "repo")
	if err := os.MkdirAll(lgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lgDir, "config.yaml"), []byte("role: ghost\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Stamped GUIDE.md should be removed.
	guidePath := filepath.Join(proj, "GUIDE.md")
	if err := os.WriteFile(guidePath, []byte(docStamp("v1.5.4")+"guide\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &config.Config{Role: config.RoleGhost}
	c.Source.Host = "gpu-box"
	c.Source.RemoteRoot = "/home/u/repo"
	c.Source.SSHMode = "native" // avoid invoking ssh during unit test

	if err := uninitLocal(c, lgDir); err != nil {
		t.Fatalf("uninitLocal: %v", err)
	}

	if _, err := os.Stat(lgDir); !os.IsNotExist(err) {
		t.Errorf(".lg dir still exists")
	}
	if _, err := os.Stat(mountDir); !os.IsNotExist(err) {
		t.Errorf("empty mount dir still exists")
	}
	if _, err := os.Stat(guidePath); !os.IsNotExist(err) {
		t.Errorf("stamped GUIDE.md still exists")
	}
}

func TestUninitLocal_PreservesUserFiles(t *testing.T) {
	proj := t.TempDir()
	lgDir := filepath.Join(proj, ".lg")
	mountDir := filepath.Join(proj, "repo")
	if err := os.MkdirAll(lgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		t.Fatal(err)
	}
	userFile := filepath.Join(mountDir, "keep.txt")
	if err := os.WriteFile(userFile, []byte("important"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Unmarked GUIDE.md is user-owned and must be preserved.
	guidePath := filepath.Join(proj, "GUIDE.md")
	if err := os.WriteFile(guidePath, []byte("my own guide\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &config.Config{Role: config.RoleGhost}
	c.Source.Host = "gpu-box"
	c.Source.RemoteRoot = "/home/u/repo"
	c.Source.SSHMode = "native"

	if err := uninitLocal(c, lgDir); err != nil {
		t.Fatalf("uninitLocal: %v", err)
	}

	if _, err := os.Stat(lgDir); !os.IsNotExist(err) {
		t.Errorf(".lg dir still exists")
	}
	if _, err := os.Stat(userFile); err != nil {
		t.Errorf("user file in non-empty mount dir was removed: %v", err)
	}
	if _, err := os.Stat(guidePath); err != nil {
		t.Errorf("user-owned GUIDE.md was removed: %v", err)
	}
}
