package fuse

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
)

// JournalOp is the kind of write event recorded.
type JournalOp string

const (
	OpWrite  JournalOp = "write"
	OpDelete JournalOp = "delete"
	OpCreate JournalOp = "create"
	OpMkdir  JournalOp = "mkdir" // directory create; flushed as WriteReq{IsDir}
)

// JournalEntry is one record. Content is not stored inline: it is read
// from the live cache file at flush time, which naturally coalesces repeated
// edits to the same path into the latest bytes.
type JournalEntry struct {
	Seq      uint64    `json:"seq"`
	Rel      string    `json:"rel"`
	Op       JournalOp `json:"op"`
	BaseHash string    `json:"base_hash"` // sync-point hash, for conflict detection
	ModTime  int64     `json:"mod_time"`
	Mode     uint32    `json:"mode"`
	Ts       int64     `json:"ts"`
}

// Journal is the append-only write log. It is the single path for all writes —
// online or offline. Online, the flush worker drains it within ms;
// offline, entries accumulate until reconnect. Durable across restarts.
type Journal struct {
	path string

	mu      sync.Mutex
	f       *os.File
	pending []JournalEntry
	nextSeq uint64
	notify  chan struct{} // signals the flush worker that work exists
}

// OpenJournal opens/replays the journal at path.
func OpenJournal(path string) (*Journal, error) {
	j := &Journal{path: path, notify: make(chan struct{}, 1)}
	if err := j.load(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	j.f = f
	return j, nil
}

func (j *Journal) load() error {
	f, err := os.Open(j.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e JournalEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // tolerate a torn final line
		}
		j.coalesceLocked(e)
		j.pending = append(j.pending, e)
		if e.Seq > j.nextSeq {
			j.nextSeq = e.Seq
		}
	}
	return sc.Err()
}

// Append durably records an entry and wakes the flush worker.
func (j *Journal) Append(e JournalEntry) (uint64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.nextSeq++
	e.Seq = j.nextSeq
	line, err := json.Marshal(e)
	if err != nil {
		return 0, err
	}
	if _, err := j.f.Write(append(line, '\n')); err != nil {
		return 0, err
	}
	if err := j.f.Sync(); err != nil {
		return 0, err
	}
	j.coalesceLocked(e)
	j.pending = append(j.pending, e)
	j.signal()
	return e.Seq, nil
}

// coalesceLocked drops pending write/create entries superseded by a new
// write/create for the same path. Content is read from the cache at flush time
// and pushes are last-write-wins, so only the newest such entry matters — a
// Finder copy closes + chmods each file several times, and without this every
// one of those events re-uploads the whole file (a big folder moved ~10x its
// size). Deletes and mkdirs keep their place; only stale writes are dropped.
// The superseded on-disk lines are left behind (append-only) — the next Ack
// compaction clears them, and load() applies the same coalescing on replay.
func (j *Journal) coalesceLocked(e JournalEntry) {
	if e.Op != OpWrite && e.Op != OpCreate {
		return
	}
	out := j.pending[:0]
	for _, p := range j.pending {
		if p.Rel == e.Rel && (p.Op == OpWrite || p.Op == OpCreate) {
			continue
		}
		out = append(out, p)
	}
	j.pending = out
}

func (j *Journal) signal() {
	select {
	case j.notify <- struct{}{}:
	default:
	}
}

// Notify returns the channel pulsed when new work is appended or on Wake.
func (j *Journal) Notify() <-chan struct{} { return j.notify }

// Wake forces the flush worker to re-check (used on reconnect).
func (j *Journal) Wake() { j.mu.Lock(); j.signal(); j.mu.Unlock() }

// Peek returns the oldest pending entry, or false if empty.
func (j *Journal) Peek() (JournalEntry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.pending) == 0 {
		return JournalEntry{}, false
	}
	return j.pending[0], true
}

// Ack removes a flushed entry (by seq) and compacts the on-disk log.
func (j *Journal) Ack(seq uint64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := j.pending[:0]
	for _, e := range j.pending {
		if e.Seq != seq {
			out = append(out, e)
		}
	}
	j.pending = out
	return j.compactLocked()
}

// PendingForDir reports whether any pending entry is at/under the rel dir.
// Used by the SOURCE-entry flush barrier.
func (j *Journal) PendingForDir(relDir string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, e := range j.pending {
		if relDir == "" || e.Rel == relDir || hasPathPrefix(e.Rel, relDir) {
			return true
		}
	}
	return false
}

// PendingSnapshot returns a copy of all unflushed entries (oldest first). Used
// by SyncTree to keep local-truth index entries alive across a tree Replace.
func (j *Journal) PendingSnapshot() []JournalEntry {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]JournalEntry, len(j.pending))
	copy(out, j.pending)
	return out
}

// PendingCount returns the number of unflushed entries (for `lg status`).
func (j *Journal) PendingCount() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.pending)
}

// HasPending reports whether a specific path has unflushed entries (eviction
// guard — lg must not evict a path with pending journal entries).
func (j *Journal) HasPending(rel string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, e := range j.pending {
		if e.Rel == rel {
			return true
		}
	}
	return false
}

func (j *Journal) compactLocked() error {
	tmp := j.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, e := range j.pending {
		line, _ := json.Marshal(e)
		_, _ = w.Write(append(line, '\n'))
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	_ = f.Close()
	if j.f != nil {
		_ = j.f.Close()
	}
	if err := os.Rename(tmp, j.path); err != nil {
		return err
	}
	j.f, err = os.OpenFile(j.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	return err
}

// Close closes the journal file.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f != nil {
		return j.f.Close()
	}
	return nil
}

// ScanPendingDir reads a journal file read-only and reports whether any entry
// targets relDir or its subtree. Used by the `lg run` process (which does not
// own the in-memory Journal) to implement the pre-run flush barrier by polling —
// so a remote command sees the latest local edits.
func ScanPendingDir(journalPath, relDir string) (bool, error) {
	f, err := os.Open(journalPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e JournalEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if relDir == "" || e.Rel == relDir || hasPathPrefix(e.Rel, relDir) {
			return true, nil
		}
	}
	return false, sc.Err()
}

// hasPathPrefix reports whether rel is under dir (slash-aware, no false
// positive on sibling names that share a textual prefix).
func hasPathPrefix(rel, dir string) bool {
	if dir == "" {
		return true
	}
	return len(rel) > len(dir) && rel[:len(dir)] == dir && rel[len(dir)] == '/'
}
