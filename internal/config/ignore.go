package config

import (
	"bufio"
	"os"
	"path"
	"strings"
)

// Matcher is the single .lgignore / ignore-pattern implementation.
// It is shared by the FUSE layer (to skip ghost-mapping) and the Source watcher
// (to skip invalidation pushes), so both sides agree on exactly what is excluded.
//
// Supported gitignore-style syntax:
//   - blank lines and lines starting with '#' are ignored
//   - trailing '/' restricts the pattern to directories
//   - leading '/' anchors the pattern to the root
//   - '*' matches within a path segment, '**' spans segments
//   - leading '!' negates a previous match
type Matcher struct {
	rules []rule
}

type rule struct {
	pattern  string // normalized, no leading/trailing slash, no '!'
	negate   bool
	dirOnly  bool
	anchored bool
}

// NewMatcher compiles patterns (e.g. from config.Ignore) into a Matcher.
func NewMatcher(patterns []string) *Matcher {
	m := &Matcher{}
	for _, p := range patterns {
		m.add(p)
	}
	return m
}

// LoadIgnoreFile merges patterns from a .lgignore file (if present) into base.
// Missing file is not an error.
func LoadIgnoreFile(base []string, ignoreFilePath string) (*Matcher, error) {
	patterns := append([]string(nil), base...)
	f, err := os.Open(ignoreFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return NewMatcher(patterns), nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		patterns = append(patterns, sc.Text())
	}
	return NewMatcher(patterns), sc.Err()
}

func (m *Matcher) add(raw string) {
	line := strings.TrimSpace(raw)
	if line == "" || strings.HasPrefix(line, "#") {
		return
	}
	r := rule{}
	if strings.HasPrefix(line, "!") {
		r.negate = true
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		r.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}
	if strings.HasPrefix(line, "/") {
		r.anchored = true
		line = strings.TrimPrefix(line, "/")
	} else if strings.Contains(strings.TrimSuffix(line, "/"), "/") {
		// A pattern with an internal slash is anchored to the root in gitignore.
		r.anchored = true
	}
	r.pattern = line
	if r.pattern == "" {
		return
	}
	m.rules = append(m.rules, r)
}

// Match reports whether the given rel path is ignored. isDir indicates whether
// the path is a directory (affects dir-only patterns).
func (m *Matcher) Match(rel string, isDir bool) bool {
	rel = Rel(rel)
	if rel == "" {
		return false
	}
	ignored := false
	for _, r := range m.rules {
		if r.dirOnly && !isDir && !m.matchesAsParent(r, rel) {
			// A dir-only rule still ignores files *under* the matched dir.
			continue
		}
		if m.matches(r, rel) {
			ignored = !r.negate
		}
	}
	return ignored
}

// matches reports whether rule r matches rel (the path itself or an ancestor
// directory of it, which is how gitignore excludes whole subtrees).
func (m *Matcher) matches(r rule, rel string) bool {
	segs := strings.Split(rel, "/")
	if r.anchored {
		// Try matching the full path and every ancestor prefix.
		for i := 1; i <= len(segs); i++ {
			prefix := strings.Join(segs[:i], "/")
			if globMatch(r.pattern, prefix) {
				return true
			}
		}
		return false
	}
	// Unanchored: pattern may match any trailing sub-path.
	for i := 0; i < len(segs); i++ {
		for j := i + 1; j <= len(segs); j++ {
			sub := strings.Join(segs[i:j], "/")
			if globMatch(r.pattern, sub) {
				return true
			}
		}
	}
	return false
}

// matchesAsParent reports whether a dir-only rule matches some ancestor dir of
// rel (so files inside an ignored directory are themselves ignored).
func (m *Matcher) matchesAsParent(r rule, rel string) bool {
	segs := strings.Split(rel, "/")
	if len(segs) < 2 {
		return false
	}
	for i := 1; i < len(segs); i++ {
		if m.matches(r, strings.Join(segs[:i], "/")) {
			return true
		}
	}
	return false
}

// globMatch matches a single gitignore pattern against a path, honoring '**'.
func globMatch(pattern, name string) bool {
	// Fast path: no '**'.
	if !strings.Contains(pattern, "**") {
		ok, _ := path.Match(pattern, name)
		if ok {
			return true
		}
		// path.Match treats '*' as not crossing '/', matching gitignore segment
		// semantics, so this is sufficient for the non-'**' case.
		return false
	}
	return doubleStarMatch(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

// doubleStarMatch matches pattern segments against name segments where a "**"
// segment matches zero or more path segments.
func doubleStarMatch(pat, name []string) bool {
	if len(pat) == 0 {
		return len(name) == 0
	}
	if pat[0] == "**" {
		// Match '**' against zero segments, or consume one and retry.
		if doubleStarMatch(pat[1:], name) {
			return true
		}
		if len(name) > 0 {
			return doubleStarMatch(pat, name[1:])
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	if ok, _ := path.Match(pat[0], name[0]); !ok {
		return false
	}
	return doubleStarMatch(pat[1:], name[1:])
}
