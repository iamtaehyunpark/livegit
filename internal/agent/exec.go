package agent

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/creack/pty"
	"github.com/taehyun/lg/internal/logx"
	"github.com/taehyun/lg/internal/proto"
	"github.com/taehyun/lg/internal/transport"
)

// execHub runs one command per invocation inside a real PTY, bridging the
// pty<->yamux streams. It replaces the old tmux session-pairing model: there is
// no persistent named session, just "run this command, stream it, return the
// exit code" — the Pivot Directive's command runner (§1).
//
// Pairing mirrors the old protocol: Ghost sends ExecReq on the control stream
// and gets back a token; it then opens the data stream and writes that token as
// a line. The hub matches the two by token, starts the command, and pushes
// ExecExit (with the process exit code) back on the control endpoint when done.
type execHub struct {
	remoteRoot string
	mu         sync.Mutex
	wait       map[string]*execSession
	seq        atomic.Uint64
}

func newExecHub(remoteRoot string) *execHub {
	return &execHub{remoteRoot: remoteRoot, wait: map[string]*execSession{}}
}

type execSession struct {
	token string
	cmd   string
	cwd   string // rel path; empty = remote root
	cols  uint16
	rows  uint16
	term  string
	ctl   *transport.Endpoint // control endpoint, used to push ExecExit
	pt    *os.File            // pty master; set once the data stream attaches
}

// serveControl runs the framed control endpoint for one exec invocation.
func (h *execHub) serveControl(stream io.ReadWriteCloser) {
	ep := transport.NewEndpoint(stream)
	var token string
	ep.SetHandler(func(f proto.Frame) (proto.MsgType, any, bool, error) {
		switch f.Type {
		case proto.TypeExecReq:
			var req proto.ExecReq
			_ = proto.Unmarshal(f.Body, &req)
			token = fmt.Sprintf("exec-%d-%d", os.Getpid(), h.seq.Add(1))
			term := req.Term
			if term == "" {
				term = "xterm-256color"
			}
			h.mu.Lock()
			h.wait[token] = &execSession{
				token: token, cmd: req.Cmd, cwd: req.Cwd,
				cols: req.Cols, rows: req.Rows, term: term, ctl: ep,
			}
			h.mu.Unlock()
			return proto.TypeExecResp, proto.ExecResp{Token: token}, true, nil
		case proto.TypeResize:
			var rz proto.Resize
			_ = proto.Unmarshal(f.Body, &rz)
			h.mu.Lock()
			s := h.wait[token]
			h.mu.Unlock()
			if s != nil && s.pt != nil {
				_ = pty.Setsize(s.pt, &pty.Winsize{Cols: rz.Cols, Rows: rz.Rows})
			}
			return 0, nil, false, nil
		default:
			return 0, nil, false, nil
		}
	})
	_ = ep.Serve()
}

// serveData reads the token line, starts the command in a pty, and bridges it.
func (h *execHub) serveData(stream io.ReadWriteCloser) {
	log := logx.For("exec")
	br := bufio.NewReader(stream)
	tokenLine, err := br.ReadString('\n')
	if err != nil {
		_ = stream.Close()
		return
	}
	token := strings.TrimSpace(tokenLine)

	h.mu.Lock()
	s := h.wait[token]
	h.mu.Unlock()
	if s == nil {
		log.Warn("exec data stream with unknown token", "token", token)
		_ = stream.Close()
		return
	}

	// A login shell so the remote PATH/conda/venv resolve exactly as they would
	// for an interactive ssh command — `sh -lc "<cmd>"`.
	cmd := exec.Command("/bin/sh", "-lc", s.cmd)
	cmd.Dir = h.resolveDir(s.cwd)
	cmd.Env = append(os.Environ(), "TERM="+s.term)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		log.Error("pty start failed", "err", err)
		_ = stream.Close()
		h.finish(token, 127, nil)
		return
	}
	if s.cols > 0 && s.rows > 0 {
		_ = pty.Setsize(ptmx, &pty.Winsize{Cols: s.cols, Rows: s.rows})
	}
	h.mu.Lock()
	s.pt = ptmx
	h.mu.Unlock()

	// Bridge. The input direction (Ghost stdin -> pty) is fire-and-forget: it
	// blocks reading the data stream and only ends when the stream closes, so we
	// must NOT wait on it — otherwise we'd deadlock (the client keeps the stream
	// open until it sees ExecExit). The exit is driven by the output side: when
	// the process dies the pty EOFs and the output copy returns.
	go func() {
		if n := br.Buffered(); n > 0 {
			peek, _ := br.Peek(n)
			_, _ = ptmx.Write(peek)
		}
		_, _ = io.Copy(ptmx, br) // Ghost input (incl. in-band Ctrl-C) to remote
	}()
	_, _ = io.Copy(stream, ptmx) // remote output to Ghost; returns on process exit

	werr := cmd.Wait()
	h.finish(token, exitCode(werr), s.ctl) // push ExecExit before closing
	_ = ptmx.Close()
	_ = stream.Close()
	log.Debug("exec finished", "token", token, "code", exitCode(werr))
}

// finish removes the session and pushes the exit code to Ghost so it can
// propagate `$?`.
func (h *execHub) finish(token string, code int, ctl *transport.Endpoint) {
	h.mu.Lock()
	delete(h.wait, token)
	h.mu.Unlock()
	if ctl != nil {
		_ = ctl.Notify(proto.TypeExecExit, proto.ExecExit{Code: code})
	}
}

// resolveDir maps a rel cwd to an absolute remote path under the root. A rel
// that escapes the root (or is empty) falls back to the root itself.
func (h *execHub) resolveDir(rel string) string {
	if rel == "" || rel == "." {
		return h.remoteRoot
	}
	abs := filepath.Join(h.remoteRoot, filepath.FromSlash(rel))
	if !strings.HasPrefix(abs, h.remoteRoot) {
		return h.remoteRoot
	}
	return abs
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return 1
}
