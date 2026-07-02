package cli

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// mkProject writes a stub .lg/config.yaml under root/rel so the walk has
// something to find (contents don't matter to findProjectConfigs).
func mkProject(t *testing.T, root, rel string) {
	t.Helper()
	dir := filepath.Join(root, rel, ".lg")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("role: ghost\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindProjectConfigs(t *testing.T) {
	root := t.TempDir()
	mkProject(t, root, "top")            // depth 1
	mkProject(t, root, "a/b/deep")       // deeper
	mkProject(t, root, "node_modules/x") // must be skipped (noise dir)
	mkProject(t, root, ".hidden/y")      // must be skipped (dotdir)

	got := findProjectConfigs(root, 6)
	// Normalize to project-relative dirs for a stable assertion.
	var rels []string
	for _, p := range got {
		rel, _ := filepath.Rel(root, filepath.Dir(filepath.Dir(p)))
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	want := []string{"a/b/deep", "top"}
	if len(rels) != len(want) {
		t.Fatalf("found %v, want %v", rels, want)
	}
	for i := range want {
		if rels[i] != want[i] {
			t.Errorf("found %v, want %v", rels, want)
			break
		}
	}
}

func TestFindProjectConfigsDepthCap(t *testing.T) {
	root := t.TempDir()
	mkProject(t, root, "d1/d2/d3/d4/deep") // .lg is several levels down

	if got := findProjectConfigs(root, 2); len(got) != 0 {
		t.Errorf("max-depth 2 should skip the deep project, got %v", got)
	}
	if got := findProjectConfigs(root, 8); len(got) != 1 {
		t.Errorf("max-depth 8 should find the deep project, got %v", got)
	}
}

func TestFindProjectConfigsDoesNotRecurseIntoLg(t *testing.T) {
	root := t.TempDir()
	mkProject(t, root, "proj")
	// A nested .lg-looking path *inside* another project's .lg must not be found
	// (we SkipDir on .lg), and stray files shouldn't confuse the walk.
	inner := filepath.Join(root, "proj", ".lg", "run", ".lg")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(inner, "config.yaml"), []byte("x"), 0o644)

	if got := findProjectConfigs(root, 6); len(got) != 1 {
		t.Errorf("expected exactly the top-level project, got %v", got)
	}
}

func TestShortHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home dir")
	}
	if got := shortHome(home); got != "~" {
		t.Errorf("shortHome(home) = %q, want ~", got)
	}
	if got := shortHome(filepath.Join(home, "proj")); got != "~/proj" {
		t.Errorf("shortHome = %q, want ~/proj", got)
	}
	if got := shortHome("/tmp/x"); got != "/tmp/x" {
		t.Errorf("shortHome(/tmp/x) = %q, want unchanged", got)
	}
}
