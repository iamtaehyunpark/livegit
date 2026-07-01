// Package fuse implements the Ghost-side virtual filesystem: a full-tree
// metadata index synced eagerly from Source (OneDrive-style), with file content
// fetched lazily on open and written back through a journal. The pure logic
// lives in Backend (testable without a real mount); node.go/mount.go are the
// thin go-fuse adapter.
package fuse

import (
	"encoding/json"
	"os"
	"path"
	"sort"
	"sync"

	"github.com/taehyun/lg/internal/config"
	"github.com/taehyun/lg/internal/proto"
)

// Entry is one node in the full-tree metadata index. Every file and directory
// on Source has an Entry the moment the tree is synced — so the whole mount is
// browsable immediately (names, real sizes, types), online or offline, before
// any content is fetched (Pivot Directive §2).
type Entry struct {
	Rel         string `json:"rel"`
	IsDir       bool   `json:"dir,omitempty"`
	Size        int64  `json:"size,omitempty"`
	ModTime     int64  `json:"mtime,omitempty"`
	Mode        uint32 `json:"mode,omitempty"`
	Hash        string `json:"hash,omitempty"`
	HaveContent bool   `json:"-"` // local cache holds the bytes (not persisted)
}

// Index is the in-memory full-tree metadata mirror, persisted to a snapshot file
// so browsing survives restarts/offline. It is safe for concurrent use.
type Index struct {
	mu       sync.RWMutex
	entries  map[string]*Entry          // rel -> entry (root "" is implicit)
	children map[string]map[string]bool // parent rel -> set of child rels
	path     string                     // snapshot file
	dirty    bool
}

// NewIndex creates an index and loads the on-disk snapshot if present.
func NewIndex(snapshotPath string) *Index {
	ix := &Index{
		entries:  map[string]*Entry{},
		children: map[string]map[string]bool{},
		path:     snapshotPath,
	}
	ix.loadSnapshot()
	return ix
}

// Replace rebuilds the whole index from a full Source snapshot (TreeResp). Any
// locally-created entries not yet on Source are preserved if still in the cache;
// callers re-add those via Put after a Replace if needed. Persists the snapshot.
func (ix *Index) Replace(entries []proto.TreeEntry) {
	ix.mu.Lock()
	ix.entries = map[string]*Entry{}
	ix.children = map[string]map[string]bool{}
	for _, e := range entries {
		ix.putLocked(&Entry{
			Rel: config.Rel(e.Rel), IsDir: e.IsDir, Size: e.Size,
			ModTime: e.ModTime, Mode: e.Mode, Hash: e.Hash,
		})
	}
	ix.dirty = true
	ix.mu.Unlock()
	ix.SaveSnapshot()
}

// Put inserts or updates a single entry (synthesizing missing ancestors).
func (ix *Index) Put(e *Entry) {
	ix.mu.Lock()
	ix.putLocked(e)
	ix.dirty = true
	ix.mu.Unlock()
}

func (ix *Index) putLocked(e *Entry) {
	e.Rel = config.Rel(e.Rel)
	if e.Rel == "" {
		return
	}
	// Preserve a known HaveContent flag across metadata updates.
	if prev, ok := ix.entries[e.Rel]; ok {
		e.HaveContent = prev.HaveContent
	}
	ix.entries[e.Rel] = e
	ix.ensureAncestors(e.Rel)
	p := parentRel(e.Rel)
	if ix.children[p] == nil {
		ix.children[p] = map[string]bool{}
	}
	ix.children[p][e.Rel] = true
}

// ensureAncestors synthesizes directory entries for any missing parent dirs so
// a deep path is always reachable from the root.
func (ix *Index) ensureAncestors(rel string) {
	for p := parentRel(rel); p != ""; p = parentRel(p) {
		if _, ok := ix.entries[p]; !ok {
			ix.entries[p] = &Entry{Rel: p, IsDir: true, Mode: 0o755}
		}
		gp := parentRel(p)
		if ix.children[gp] == nil {
			ix.children[gp] = map[string]bool{}
		}
		ix.children[gp][p] = true
	}
}

// Remove deletes an entry and (recursively) its subtree from the index.
func (ix *Index) Remove(rel string) {
	rel = config.Rel(rel)
	ix.mu.Lock()
	ix.removeLocked(rel)
	ix.dirty = true
	ix.mu.Unlock()
}

func (ix *Index) removeLocked(rel string) {
	if kids := ix.children[rel]; kids != nil {
		for c := range kids {
			ix.removeLocked(c)
		}
		delete(ix.children, rel)
	}
	delete(ix.entries, rel)
	if set := ix.children[parentRel(rel)]; set != nil {
		delete(set, rel)
	}
}

// Get returns the entry for rel (a copy), or (nil,false).
func (ix *Index) Get(rel string) (*Entry, bool) {
	rel = config.Rel(rel)
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	e, ok := ix.entries[rel]
	if !ok {
		return nil, false
	}
	cp := *e
	return &cp, true
}

// SetHaveContent marks whether local cache holds rel's bytes.
func (ix *Index) SetHaveContent(rel string, have bool) {
	rel = config.Rel(rel)
	ix.mu.Lock()
	if e, ok := ix.entries[rel]; ok {
		e.HaveContent = have
	}
	ix.mu.Unlock()
}

// Children returns the immediate children of rel as directory entries, sorted.
func (ix *Index) Children(rel string) []proto.DirEntry {
	rel = config.Rel(rel)
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	set := ix.children[rel]
	out := make([]proto.DirEntry, 0, len(set))
	for childRel := range set {
		e := ix.entries[childRel]
		if e == nil {
			continue
		}
		out = append(out, proto.DirEntry{
			Name: path.Base(childRel), IsDir: e.IsDir, Size: e.Size, Mode: e.Mode,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Len reports the number of indexed entries (for `lg status`).
func (ix *Index) Len() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.entries)
}

// SaveSnapshot persists the index to disk if it changed since the last save.
func (ix *Index) SaveSnapshot() {
	ix.mu.Lock()
	if !ix.dirty {
		ix.mu.Unlock()
		return
	}
	list := make([]*Entry, 0, len(ix.entries))
	for _, e := range ix.entries {
		list = append(list, e)
	}
	ix.dirty = false
	ix.mu.Unlock()

	b, err := json.Marshal(list)
	if err != nil {
		return
	}
	tmp := ix.path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, ix.path)
	}
}

func (ix *Index) loadSnapshot() {
	b, err := os.ReadFile(ix.path)
	if err != nil {
		return
	}
	var list []*Entry
	if json.Unmarshal(b, &list) != nil {
		return
	}
	ix.mu.Lock()
	for _, e := range list {
		ix.putLocked(e)
	}
	ix.dirty = false
	ix.mu.Unlock()
}

// parentRel returns the parent directory rel of a canonical rel path ("" for a
// top-level entry).
func parentRel(rel string) string {
	d := path.Dir(rel)
	if d == "." || d == "/" {
		return ""
	}
	return d
}
