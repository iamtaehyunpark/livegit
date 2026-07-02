// Package proto defines the control + file-RPC message schema and the
// hand-rolled length-prefixed framing. Rather than depend on a
// protoc toolchain, messages are plain structs encoded as a compact tagged
// payload; the framing on the wire is exactly "uvarint length + bytes" as the framing
// contract mandates, so a real protobuf payload can be swapped in later without
// touching the framing or dispatch loop.
package proto

// MsgType tags each frame so the dispatch loop can route it.
type MsgType uint8

const (
	// Control / health.
	TypePing MsgType = 1
	TypePong MsgType = 2
	TypeErr  MsgType = 3 // generic error response

	// File RPC (Ghost -> Source over the file-RPC stream).
	TypeStatReq  MsgType = 10
	TypeStatResp MsgType = 11
	TypeReadReq  MsgType = 12 // fetch full file content (ghost -> cached)
	TypeReadResp MsgType = 13
	TypeWriteReq MsgType = 14 // journal flush: push content to Source
	TypeWriteAck MsgType = 15
	TypeDelReq   MsgType = 16
	TypeDelAck   MsgType = 17
	TypeListReq  MsgType = 18 // directory listing
	TypeListResp MsgType = 19

	// Full-tree metadata sync (Ghost -> Source over the file-RPC stream).
	TypeTreeReq  MsgType = 20 // request the entire remote tree's metadata
	TypeTreeResp MsgType = 21

	// Change notification (Source -> Ghost over the notify stream).
	TypeInvalidate MsgType = 30

	// Command runner (Ghost -> Source over a PTY-control + PTY-data stream pair).
	// One remote process per invocation, run inside a real PTY.
	TypeExecReq  MsgType = 40 // start a command in a PTY
	TypeExecResp MsgType = 41 // reply with the token that pairs the data stream
	TypeExecExit MsgType = 42 // pushed when the remote process exits (carries code)
	TypeResize   MsgType = 44 // terminal resize (SIGWINCH) for the active PTY

	// Detached jobs (Ghost -> Source over the control stream). The async sibling
	// of the command runner: launch a command that outlives the `lg run` that
	// started it (and the ghost disconnecting) by escaping the ssh session's
	// systemd scope. See internal/agent/jobs.go.
	TypeJobStartReq  MsgType = 50 // launch a detached job; returns an id
	TypeJobStartResp MsgType = 51
	TypeJobListReq   MsgType = 52 // list known jobs and their state
	TypeJobListResp  MsgType = 53
	TypeJobActReq    MsgType = 54 // act on a job: kill | rm
	TypeJobActResp   MsgType = 55
)

// Frame is the envelope every message travels in.
type Frame struct {
	Type MsgType
	// ReqID correlates a response with its request on a stream that may have
	// several in flight. Zero for unsolicited pushes (invalidations).
	ReqID uint64
	Body  []byte // type-specific payload, encoded by the structs below
}

// --- Control ---

type Ping struct{ Nonce uint64 }
type Pong struct{ Nonce uint64 }
type ErrResp struct{ Message string }

// --- File RPC ---

// FileStat is the metadata Source reports for a path.
type FileStat struct {
	Rel     string
	Exists  bool
	IsDir   bool
	Size    int64
	ModTime int64 // unix seconds
	Mode    uint32
	Hash    string // content hash (empty for dirs)
}

type StatReq struct{ Rel string }
type StatResp struct{ Stat FileStat }

type ReadReq struct{ Rel string }
type ReadResp struct {
	Found   bool
	Content []byte
	Hash    string
	ModTime int64
	Mode    uint32
}

// WriteReq is a journal entry being flushed to Source. BaseHash is the content
// hash Ghost last synced for this path; Source uses it for conflict detection
// — if Source's current content differs from BaseHash, the two
// sides diverged and Source backs up before applying.
type WriteReq struct {
	Rel      string
	Content  []byte
	BaseHash string
	ModTime  int64
	Mode     uint32
}
type WriteAck struct {
	OK        bool
	Conflict  bool   // Source detected divergence and made a backup
	BackupRel string // path of the .lg-conflict backup, if any
	NewHash   string // resulting content hash on Source
	SourceMod int64  // Source's modtime after apply
	Message   string
}

type DelReq struct {
	Rel      string
	BaseHash string
}
type DelAck struct {
	OK       bool
	Conflict bool
	Message  string
}

type DirEntry struct {
	Name  string
	IsDir bool
	Size  int64
	Mode  uint32
}
type ListReq struct{ Rel string }
type ListResp struct {
	Found   bool
	Entries []DirEntry
}

// --- Full-tree metadata sync ---

// TreeEntry is one node in Source's full directory tree. Sent eagerly so Ghost
// can render the entire mount (names, sizes, types at all depths) without a
// round-trip per `ls`, OneDrive-style. Hash may be empty (filled on read).
type TreeEntry struct {
	Rel     string
	IsDir   bool
	Size    int64
	ModTime int64 // unix seconds
	Mode    uint32
	Hash    string
}
type TreeReq struct{}
type TreeResp struct{ Entries []TreeEntry }

// --- Change notification ---

// Invalidate tells Ghost that Source's copy of Rel changed. It carries enough
// metadata to update the full-tree index in place (not just mark content
// stale): create/delete/rename/size-change all flow through here.
type Invalidate struct {
	Rel     string
	Deleted bool
	IsDir   bool
	Size    int64
	Mode    uint32
	Hash    string
	ModTime int64
}

// --- Command runner ---

// ExecReq starts one command in a PTY on Source. Cwd is a rel path (mapped to
// the remote root); empty means the remote root itself.
type ExecReq struct {
	Cmd  string
	Cwd  string
	Cols uint16
	Rows uint16
	Term string // Ghost's $TERM, so the remote PTY initialises the terminal
}

// ExecResp returns the token Ghost writes on the data stream to pair it.
type ExecResp struct{ Token string }

// ExecExit is pushed on the control stream when the remote process exits, so
// Ghost can propagate the exit code locally.
type ExecExit struct{ Code int }

// Resize carries a SIGWINCH from Ghost to Source for the active PTY.
type Resize struct {
	Cols uint16
	Rows uint16
}

// --- Detached jobs ---

// JobStartReq launches a fire-and-forget command on Source. Cwd is a rel path
// (mapped to the remote root) exactly like ExecReq.
type JobStartReq struct {
	Cmd string
	Cwd string
}

// JobStartResp returns the new job's id. Mode is how it was launched
// ("systemd" | "nohup"); Warn is a non-fatal caveat to surface to the user
// (e.g. systemd --user unavailable, so durability across full logout needs
// linger).
type JobStartResp struct {
	ID   string
	Mode string
	Warn string
}

// JobInfo is one row of `lg jobs`.
type JobInfo struct {
	ID      string
	Cmd     string
	State   string // "running" | "done" | "dead" (exited without recording a code)
	Code    int    // exit code, valid when State=="done"
	Started int64  // unix seconds
	Mode    string // "systemd" | "nohup"
}

type JobListReq struct{}
type JobListResp struct{ Jobs []JobInfo }

// JobActReq acts on an existing job. Action is "kill" (stop it) or "rm" (forget
// a finished job and delete its logs).
type JobActReq struct {
	ID     string
	Action string
}
type JobActResp struct {
	OK      bool
	Message string
}

// JobLogReq is the JSON header line Ghost writes first on a StreamJobLog stream
// (mirroring the exec data stream's token line). The agent then streams the
// job's log file back, tailing it live when Follow is set.
type JobLogReq struct {
	ID     string
	Follow bool
}
