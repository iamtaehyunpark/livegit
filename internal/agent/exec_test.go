package agent

import (
	"path/filepath"
	"testing"
)

// TestResolveDirClamp checks that a rel cwd is mapped under the remote root and
// that anything trying to escape it (via "..") is clamped back to the root —
// including the sibling-directory case a bare prefix check would wrongly accept.
func TestResolveDirClamp(t *testing.T) {
	root := "/home/user/repo"
	cases := []struct {
		rel, want string
	}{
		{"", root},
		{".", root},
		{"a/b", filepath.Join(root, "a/b")},
		{"../repo/x", filepath.Join(root, "x")}, // normalizes back inside
		{"../evil", root},                       // parent escape -> clamped
		{"../repo-evil", root},                  // sibling with shared prefix -> clamped
		{"../../etc/passwd", root},              // deep escape -> clamped
	}
	for _, c := range cases {
		if got := resolveDir(root, c.rel); got != c.want {
			t.Errorf("resolveDir(%q, %q) = %q, want %q", root, c.rel, got, c.want)
		}
	}
}
