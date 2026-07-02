package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

// PathMapper is the single source of truth for translating between the Ghost
// FUSE mount (local_root) and Source's real tree (remote_root). It is a
// cross-cutting helper shared by FUSE, watcher, and journal: get it
// right once here so the three never drift.
//
// Internally everything is keyed by a "rel" path: slash-separated, relative to
// the root, with no leading slash and no "." — the stable identity of a file
// regardless of which side is talking about it.
type PathMapper struct {
	localRoot  string
	remoteRoot string
}

// NewPathMapper builds a mapper from a config.
func NewPathMapper(c *Config) *PathMapper {
	return &PathMapper{
		localRoot:  filepath.Clean(c.LocalRoot),
		remoteRoot: cleanSlash(c.Source.RemoteRoot),
	}
}

// cleanSlash normalizes a remote (always slash-style) path.
func cleanSlash(p string) string {
	p = strings.TrimRight(p, "/")
	if p == "" {
		return "/"
	}
	return p
}

// Rel normalizes any path-fragment into the canonical rel form.
// Accepts "", "/", "foo/bar", "./foo" and returns "" or "foo/bar".
func Rel(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.Trim(p, "/")
	if p == "" || p == "." {
		return ""
	}
	// Clean without reintroducing a leading slash.
	cleaned := filepath.ToSlash(filepath.Clean(p))
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "." {
		return ""
	}
	return cleaned
}

// LocalToRel maps an absolute local path under local_root to a rel path.
func (m *PathMapper) LocalToRel(abs string) (string, error) {
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(m.localRoot, abs)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes local_root %q", abs, m.localRoot)
	}
	return Rel(rel), nil
}

// RemoteToRel maps an absolute remote path under remote_root to a rel path.
func (m *PathMapper) RemoteToRel(abs string) (string, error) {
	abs = cleanSlash(abs)
	if !strings.HasPrefix(abs+"/", m.remoteRoot+"/") {
		return "", fmt.Errorf("path %q escapes remote_root %q", abs, m.remoteRoot)
	}
	rest := strings.TrimPrefix(abs, m.remoteRoot)
	return Rel(rest), nil
}

// RelToLocal builds the absolute local path for a rel path.
func (m *PathMapper) RelToLocal(rel string) string {
	rel = Rel(rel)
	if rel == "" {
		return m.localRoot
	}
	return filepath.Join(m.localRoot, filepath.FromSlash(rel))
}

// RelToRemote builds the absolute remote path for a rel path.
func (m *PathMapper) RelToRemote(rel string) string {
	rel = Rel(rel)
	if rel == "" {
		return m.remoteRoot
	}
	if m.remoteRoot == "/" {
		return "/" + rel
	}
	return m.remoteRoot + "/" + rel
}

// LocalRoot / RemoteRoot expose the normalized roots.
func (m *PathMapper) LocalRoot() string  { return m.localRoot }
func (m *PathMapper) RemoteRoot() string { return m.remoteRoot }
