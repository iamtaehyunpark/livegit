// Package transport multiplexes a single SSH connection into several
// logical yamux streams (PTY / file-RPC / notify). It also owns the single
// online/offline flag that FUSE write-through, the shell's SOURCE entry, and
// the journal flush worker all subscribe to.
//
// The SSH connection is established by exec'ing `lg serve` on Source through its
// existing sshd, then running yamux over that session's stdio. This reuses the
// firewall/auth problem sshd already solved — no extra daemon port.
package transport

import (
	"fmt"
	"io"

	"github.com/hashicorp/yamux"
)

// StreamKind is the first byte sent on every yamux stream so the accepting side
// knows how to handle it.
type StreamKind byte

const (
	StreamControl StreamKind = 1 // health ping/pong
	StreamFile    StreamKind = 2 // file RPC (Ghost initiates)
	StreamNotify  StreamKind = 3 // invalidation push (Source initiates)
	StreamPTY     StreamKind = 4 // raw PTY bytes for one remote command
	StreamPTYCtl  StreamKind = 5 // exec control (ExecReq/Resize/ExecExit) for that command
	StreamJobLog  StreamKind = 6 // tail a detached job's log file (Ghost initiates)
)

func (k StreamKind) String() string {
	switch k {
	case StreamControl:
		return "control"
	case StreamFile:
		return "file"
	case StreamNotify:
		return "notify"
	case StreamPTY:
		return "pty"
	case StreamPTYCtl:
		return "pty-ctl"
	case StreamJobLog:
		return "job-log"
	default:
		return fmt.Sprintf("kind(%d)", byte(k))
	}
}

// rwc adapts a separate reader and writer (e.g. an ssh session's stdout pipe and
// stdin pipe) into one io.ReadWriteCloser for yamux.
type rwc struct {
	r io.Reader
	w io.Writer
	c io.Closer
}

func (p *rwc) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *rwc) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *rwc) Close() error {
	if p.c != nil {
		return p.c.Close()
	}
	return nil
}

// NewClientSession wraps a transport pipe in a yamux client session (Ghost side).
func NewClientSession(conn io.ReadWriteCloser) (*yamux.Session, error) {
	return yamux.Client(conn, yamuxConfig())
}

// NewServerSession wraps a transport pipe in a yamux server session (Source side).
func NewServerSession(conn io.ReadWriteCloser) (*yamux.Session, error) {
	return yamux.Server(conn, yamuxConfig())
}

func yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	// yamux's own keepalive complements our application-level health ping; the
	// latter is what drives the online flag, the former just keeps NAT alive.
	cfg.EnableKeepAlive = true
	cfg.LogOutput = io.Discard
	return cfg
}

// OpenStream opens a yamux stream and writes its kind byte.
func OpenStream(sess *yamux.Session, kind StreamKind) (*yamux.Stream, error) {
	s, err := sess.OpenStream()
	if err != nil {
		return nil, err
	}
	if _, err := s.Write([]byte{byte(kind)}); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// AcceptStream accepts a yamux stream and reads its kind byte.
func AcceptStream(sess *yamux.Session) (*yamux.Stream, StreamKind, error) {
	s, err := sess.AcceptStream()
	if err != nil {
		return nil, 0, err
	}
	var b [1]byte
	if _, err := io.ReadFull(s, b[:]); err != nil {
		_ = s.Close()
		return nil, 0, err
	}
	return s, StreamKind(b[0]), nil
}
