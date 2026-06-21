// Package proto defines the control + file-RPC message schema and the
// hand-rolled length-prefixed framing chosen in D3. Rather than depend on a
// protoc toolchain, messages are plain structs encoded as a compact tagged
// payload; the framing on the wire is exactly "uvarint length + bytes" as the
// spec mandates, so a real protobuf payload can be swapped in later without
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

	// Change notification (Source -> Ghost over the notify stream).
	TypeInvalidate MsgType = 30

	// tmux session control (Ghost -> Source over a PTY-control stream).
	TypeSessionReq  MsgType = 40 // create/attach a session
	TypeSessionResp MsgType = 41
	TypeSessionList MsgType = 42
	TypeSessionsRsp MsgType = 43
	TypeResize      MsgType = 44 // terminal resize (SIGWINCH) for a PTY session
	TypeExit        MsgType = 45 // remote tmux attach detached/exited
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
// (spec §4.4) — if Source's current content differs from BaseHash, the two
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

// --- Change notification ---

// Invalidate tells Ghost that Source's copy of Rel changed (spec §4.3). No
// content is sent; Ghost decides lazily whether to refetch.
type Invalidate struct {
	Rel     string
	Deleted bool
	Hash    string
	ModTime int64
}

// --- tmux session control ---

type SessionReq struct {
	Project string
	TabID   string
	Cols    uint16
	Rows    uint16
	Term    string // Ghost's $TERM, so the remote tmux client can init the terminal
}
type SessionResp struct {
	Name    string
	Created bool // true if a new session was made, false if attached existing
	Message string
}

type SessionInfo struct {
	Name     string
	Attached bool
	Windows  int
	Created  int64
}
type SessionListReq struct{}
type SessionsResp struct{ Sessions []SessionInfo }

// Resize carries a SIGWINCH from Ghost to Source for the active PTY session.
type Resize struct {
	Cols uint16
	Rows uint16
}

// ExitNote signals the remote attach ended (session detached or tmux exited).
type ExitNote struct{ Message string }
