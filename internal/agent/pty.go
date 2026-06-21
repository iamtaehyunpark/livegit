package agent

import (
	"bufio"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/creack/pty"
	"github.com/taehyun/lg/internal/logx"
	"github.com/taehyun/lg/internal/proto"
	"github.com/taehyun/lg/internal/transport"
)

// ptyHub pairs a PTY control stream (framed: SessionReq/Resize) with its raw
// data stream and runs `tmux attach` inside a real pty, bridging pty<->stream.
//
// Pairing: Ghost first sends SessionReq on the control stream and learns the
// session name; it then opens the data stream and writes that name as a token
// line. The hub matches the two by token.
type ptyHub struct {
	tmux *tmuxManager
	mu   sync.Mutex
	wait map[string]*ptySession
}

func newPTYHub(t *tmuxManager) *ptyHub {
	return &ptyHub{tmux: t, wait: map[string]*ptySession{}}
}

type ptySession struct {
	name string
	cols uint16
	rows uint16
	pt   *os.File // pty master; set once the data stream attaches; guarded by hub.mu
}

// serveControl runs the framed control endpoint for one PTY session.
func (h *ptyHub) serveControl(stream io.ReadWriteCloser) {
	ep := transport.NewEndpoint(stream)
	var sessName string
	ep.SetHandler(func(f proto.Frame) (proto.MsgType, any, bool, error) {
		switch f.Type {
		case proto.TypeSessionReq:
			var req proto.SessionReq
			_ = proto.Unmarshal(f.Body, &req)
			name := sessionName(req.Project, req.TabID)
			created, err := h.tmux.ensure(name, req.Cols, req.Rows)
			if err != nil {
				return 0, nil, true, err
			}
			sessName = name
			h.mu.Lock()
			h.wait[name] = &ptySession{name: name, cols: req.Cols, rows: req.Rows}
			h.mu.Unlock()
			return proto.TypeSessionResp, proto.SessionResp{Name: name, Created: created}, true, nil
		case proto.TypeResize:
			var rz proto.Resize
			_ = proto.Unmarshal(f.Body, &rz)
			h.mu.Lock()
			s := h.wait[sessName]
			h.mu.Unlock()
			if s != nil && s.pt != nil {
				_ = pty.Setsize(s.pt, &pty.Winsize{Cols: rz.Cols, Rows: rz.Rows})
			}
			_ = h.tmux.resize(sessName, rz.Cols, rz.Rows)
			return 0, nil, false, nil
		default:
			return 0, nil, false, nil
		}
	})
	_ = ep.Serve()
}

// serveData reads the token line from the data stream, then bridges the pty.
func (h *ptyHub) serveData(stream io.ReadWriteCloser) {
	log := logx.For("pty")
	br := bufio.NewReader(stream)
	token, err := br.ReadString('\n')
	if err != nil {
		_ = stream.Close()
		return
	}
	token = strings.TrimSpace(token)

	h.mu.Lock()
	s := h.wait[token]
	h.mu.Unlock()
	if s == nil {
		log.Warn("pty data stream with unknown token", "token", token)
		_ = stream.Close()
		return
	}

	cmd := h.tmux.attachCmd(token)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		log.Error("pty start failed", "err", err)
		_ = stream.Close()
		return
	}
	if s.cols > 0 && s.rows > 0 {
		_ = pty.Setsize(ptmx, &pty.Winsize{Cols: s.cols, Rows: s.rows})
	}
	h.mu.Lock()
	s.pt = ptmx
	h.mu.Unlock()

	// Bridge: pty output -> stream, stream input -> pty. Any buffered bytes the
	// token reader already consumed past '\n' are forwarded first.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(stream, ptmx) // remote output to Ghost
	}()
	go func() {
		defer wg.Done()
		if n := br.Buffered(); n > 0 {
			peek, _ := br.Peek(n)
			_, _ = ptmx.Write(peek)
		}
		_, _ = io.Copy(ptmx, br) // Ghost input to remote
	}()

	_ = cmd.Wait()
	_ = ptmx.Close()
	_ = stream.Close()
	h.mu.Lock()
	delete(h.wait, token)
	h.mu.Unlock()
	wg.Wait()
	log.Debug("pty session detached", "session", token)
}

// sessionName builds the per-tab tmux session name (§5.3, §5.7).
func sessionName(project, tabID string) string {
	project = sanitize(project)
	tabID = sanitize(tabID)
	if project == "" {
		project = "lg"
	}
	return project + "-" + tabID
}

func sanitize(s string) string {
	r := strings.NewReplacer(".", "_", ":", "_", " ", "_", "/", "_")
	return r.Replace(s)
}
