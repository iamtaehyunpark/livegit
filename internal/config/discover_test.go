package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindProjectDir(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "myproj")
	lg := filepath.Join(proj, ".lg")
	if err := os.MkdirAll(lg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lg, "config.yaml"), []byte("role: ghost\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(proj, "src", "models")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	// From a deep subdirectory, discovery walks up to the project's .lg.
	if got, ok := findProjectDir(sub); !ok || got != lg {
		t.Fatalf("from subdir: got %q ok=%v, want %q", got, ok, lg)
	}
	// From the project root itself.
	if got, ok := findProjectDir(proj); !ok || got != lg {
		t.Fatalf("from root: got %q ok=%v, want %q", got, ok, lg)
	}
	// Outside any project: not found.
	other := t.TempDir()
	if got, ok := findProjectDir(other); ok {
		t.Fatalf("unrelated dir should not resolve, got %q", got)
	}
}

func TestDirHonorsLGHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LG_HOME", home)
	if Dir() != home {
		t.Fatalf("Dir()=%q, want %q (LG_HOME override)", Dir(), home)
	}
}
