package agent

import (
	"io"
	"path/filepath"

	"github.com/hashicorp/yamux"
	"github.com/taehyun/lg/internal/config"
	"github.com/taehyun/lg/internal/logx"
	"github.com/taehyun/lg/internal/proto"
	"github.com/taehyun/lg/internal/transport"
)

// Server is the Source-side daemon (`lg serve`). It runs over the stdio pipe of
// the ssh exec session: a yamux server multiplexing file-RPC, control, notify,
// and PTY streams (§3.1, D1).
type Server struct {
	remoteRoot string
	matcher    *config.Matcher
	file       *FileServer
	tmux       *tmuxManager
	pty        *ptyHub
}

// NewServer builds the Source daemon for a given remote root.
func NewServer(remoteRoot string) (*Server, error) {
	remoteRoot = filepath.Clean(remoteRoot)
	matcher, err := config.LoadIgnoreFile(nil, filepath.Join(remoteRoot, ".lgignore"))
	if err != nil {
		return nil, err
	}
	tmuxSock := filepath.Join(config.Dir(), "tmux.sock")
	return &Server{
		remoteRoot: remoteRoot,
		matcher:    matcher,
		file:       NewFileServer(remoteRoot, matcher),
		tmux:       newTmuxManager(tmuxSock),
		pty:        newPTYHub(newTmuxManager(tmuxSock)),
	}, nil
}

// Serve runs until the transport pipe closes. conn is the ssh session stdio.
func (s *Server) Serve(conn io.ReadWriteCloser) error {
	log := logx.For("agent")
	sess, err := transport.NewServerSession(conn)
	if err != nil {
		return err
	}
	defer sess.Close()

	// Open a notify stream back to Ghost and start the watcher on it.
	if notifyStream, err := transport.OpenStream(sess, transport.StreamNotify); err == nil {
		notifyEp := transport.NewEndpoint(notifyStream)
		go notifyEp.Serve()
		watcher, werr := NewWatcher(s.remoteRoot, s.matcher, func(inv proto.Invalidate) {
			_ = notifyEp.Notify(proto.TypeInvalidate, inv)
		})
		if werr == nil {
			go watcher.Run()
			defer watcher.Close()
		} else {
			log.Warn("watcher disabled", "err", werr)
		}
	} else {
		log.Warn("could not open notify stream", "err", err)
	}

	log.Info("agent serving", "remote_root", s.remoteRoot)
	for {
		stream, kind, err := transport.AcceptStream(sess)
		if err != nil {
			log.Info("transport closed", "err", err)
			return err
		}
		s.dispatch(stream, kind)
	}
}

func (s *Server) dispatch(stream *yamux.Stream, kind transport.StreamKind) {
	switch kind {
	case transport.StreamControl:
		ep := transport.NewEndpoint(stream)
		ep.SetHandler(s.handleControl)
		go ep.Serve()
	case transport.StreamFile:
		ep := transport.NewEndpoint(stream)
		ep.SetHandler(s.file.Handle)
		go ep.Serve()
	case transport.StreamPTYCtl:
		go s.pty.serveControl(stream)
	case transport.StreamPTY:
		go s.pty.serveData(stream)
	default:
		logx.For("agent").Warn("unknown stream kind", "kind", kind)
		_ = stream.Close()
	}
}

// handleControl answers health pings and session-list queries.
func (s *Server) handleControl(f proto.Frame) (proto.MsgType, any, bool, error) {
	switch f.Type {
	case proto.TypePing:
		var p proto.Ping
		_ = proto.Unmarshal(f.Body, &p)
		return proto.TypePong, proto.Pong{Nonce: p.Nonce}, true, nil
	case proto.TypeSessionList:
		sessions, err := s.tmux.list()
		if err != nil {
			return 0, nil, true, err
		}
		return proto.TypeSessionsRsp, proto.SessionsResp{Sessions: sessions}, true, nil
	default:
		return 0, nil, false, nil
	}
}
