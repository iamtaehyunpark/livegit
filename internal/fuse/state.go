// Package fuse implements the Ghost-side virtual filesystem: the ghost/cached/
// live state machine (§4.1–4.2), journal-first async write-through (§4.2, §4.5),
// LRU eviction (§4.2), and conflict backup (§4.4). The pure logic lives in
// Backend (testable without a real mount); node.go/mount.go are the thin
// go-fuse adapter.
package fuse

import (
	"database/sql"
	"time"

	_ "modernc.org/sqlite" // pure-Go sqlite driver (no cgo)
)

// FileState is the ghost/cached/live tristate from §4.1.
type FileState string

const (
	StateGhost  FileState = "ghost"  // metadata only, content on Source
	StateCached FileState = "cached" // real local copy, in sync
	StateLive   FileState = "live"   // locally modified, pending flush
)

// Meta is one row of the state table (§4.1).
type Meta struct {
	Path           string // rel path (canonical identity)
	State          FileState
	ContentHash    string
	LastModifiedBy string // 'source' | 'ghost'
	LastModifiedAt int64
	LastAccessedAt int64
	SizeBytes      int64
}

// StateStore is the SQLite-backed metadata DB (~/.lg/state.db).
type StateStore struct {
	db *sql.DB
}

// OpenState opens (and migrates) the state DB at path.
func OpenState(path string) (*StateStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// Single connection avoids "database is locked" under the pure-Go driver.
	db.SetMaxOpenConns(1)
	s := &StateStore{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *StateStore) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS files (
  path             TEXT PRIMARY KEY,
  state            TEXT NOT NULL,
  content_hash     TEXT NOT NULL DEFAULT '',
  last_modified_by TEXT NOT NULL DEFAULT '',
  last_modified_at INTEGER NOT NULL DEFAULT 0,
  last_accessed_at INTEGER NOT NULL DEFAULT 0,
  size_bytes       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_files_accessed ON files(last_accessed_at);
CREATE INDEX IF NOT EXISTS idx_files_state ON files(state);`)
	return err
}

// Close closes the DB.
func (s *StateStore) Close() error { return s.db.Close() }

// Get returns the row for rel, or (nil,nil) if absent.
func (s *StateStore) Get(rel string) (*Meta, error) {
	row := s.db.QueryRow(`SELECT path,state,content_hash,last_modified_by,
		last_modified_at,last_accessed_at,size_bytes FROM files WHERE path=?`, rel)
	var m Meta
	err := row.Scan(&m.Path, &m.State, &m.ContentHash, &m.LastModifiedBy,
		&m.LastModifiedAt, &m.LastAccessedAt, &m.SizeBytes)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// Upsert writes a full row.
func (s *StateStore) Upsert(m *Meta) error {
	_, err := s.db.Exec(`INSERT INTO files
		(path,state,content_hash,last_modified_by,last_modified_at,last_accessed_at,size_bytes)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET
		  state=excluded.state,
		  content_hash=excluded.content_hash,
		  last_modified_by=excluded.last_modified_by,
		  last_modified_at=excluded.last_modified_at,
		  last_accessed_at=excluded.last_accessed_at,
		  size_bytes=excluded.size_bytes`,
		m.Path, m.State, m.ContentHash, m.LastModifiedBy,
		m.LastModifiedAt, m.LastAccessedAt, m.SizeBytes)
	return err
}

// SetState updates just the state column.
func (s *StateStore) SetState(rel string, st FileState) error {
	_, err := s.db.Exec(`UPDATE files SET state=? WHERE path=?`, st, rel)
	return err
}

// Touch updates last_accessed_at to now (for LRU).
func (s *StateStore) Touch(rel string) error {
	_, err := s.db.Exec(`UPDATE files SET last_accessed_at=? WHERE path=?`, time.Now().Unix(), rel)
	return err
}

// Delete removes a row.
func (s *StateStore) Delete(rel string) error {
	_, err := s.db.Exec(`DELETE FROM files WHERE path=?`, rel)
	return err
}

// EvictCandidates returns cached (not live) rows idle since before cutoff,
// oldest-accessed first (§4.2 LRU).
func (s *StateStore) EvictCandidates(cutoff int64) ([]Meta, error) {
	rows, err := s.db.Query(`SELECT path,state,content_hash,last_modified_by,
		last_modified_at,last_accessed_at,size_bytes FROM files
		WHERE state=? AND last_accessed_at < ? ORDER BY last_accessed_at ASC`,
		StateCached, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Meta
	for rows.Next() {
		var m Meta
		if err := rows.Scan(&m.Path, &m.State, &m.ContentHash, &m.LastModifiedBy,
			&m.LastModifiedAt, &m.LastAccessedAt, &m.SizeBytes); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CachedSizeBytes totals materialized (cached+live) content size.
func (s *StateStore) CachedSizeBytes() (int64, error) {
	row := s.db.QueryRow(`SELECT COALESCE(SUM(size_bytes),0) FROM files WHERE state IN (?,?)`,
		StateCached, StateLive)
	var total int64
	err := row.Scan(&total)
	return total, err
}

// Counts returns the number of files in each state (for `lg status`).
func (s *StateStore) Counts() (ghost, cached, live int, err error) {
	rows, err := s.db.Query(`SELECT state, COUNT(*) FROM files GROUP BY state`)
	if err != nil {
		return 0, 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return 0, 0, 0, err
		}
		switch FileState(st) {
		case StateGhost:
			ghost = n
		case StateCached:
			cached = n
		case StateLive:
			live = n
		}
	}
	return ghost, cached, live, rows.Err()
}
