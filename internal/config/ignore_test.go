package config

import "testing"

func TestMatcher(t *testing.T) {
	m := NewMatcher([]string{
		".venv/",
		"node_modules/",
		"*.pt",
		"/build",
		"!keep.pt",
		"a/b/*.log",
	})
	cases := []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{".venv", true, true},
		{".venv/lib/python", false, true}, // under an ignored dir
		{"node_modules/x/index.js", false, true},
		{"model.pt", false, true},
		{"weights/model.pt", false, true}, // *.pt is unanchored
		{"keep.pt", false, false},         // negated
		{"build", true, true},             // anchored at root
		{"src/build", true, false},        // anchored => only root build
		{"a/b/run.log", false, true},
		{"a/c/run.log", false, false},
		{"src/main.go", false, false},
	}
	for _, c := range cases {
		if got := m.Match(c.rel, c.isDir); got != c.want {
			t.Errorf("Match(%q,%v)=%v want %v", c.rel, c.isDir, got, c.want)
		}
	}
}

func TestMatcherDoubleStar(t *testing.T) {
	m := NewMatcher([]string{"**/dist", "logs/**/*.tmp"})
	cases := []struct {
		rel  string
		dir  bool
		want bool
	}{
		{"dist", true, true},
		{"a/dist", true, true},
		{"a/b/dist", true, true},
		{"logs/x/y/z.tmp", false, true},
		{"logs/z.tmp", false, true},
		{"other/z.tmp", false, false},
	}
	for _, c := range cases {
		if got := m.Match(c.rel, c.dir); got != c.want {
			t.Errorf("Match(%q)=%v want %v", c.rel, got, c.want)
		}
	}
}
