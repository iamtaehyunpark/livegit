package transport

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/iamtaehyunpark/livegit/internal/logx"
	"github.com/iamtaehyunpark/livegit/internal/proto"
)

// ErrOffline is returned by RPC helpers when the link is down.
var ErrOffline = errors.New("transport: offline")

// Client is the Ghost-side handle to Source. It owns the SSH connection, the
// yamux session, the control + file endpoints, the online flag, and the
// reconnect supervisor. Higher layers (FUSE, shell) talk to Source only through
// this handle so there is exactly one connection and one online flag.
type Client struct {
	cfg       *config.Config
	remoteBin string
	status    *Status

	ctx    context.Context
	cancel context.CancelFunc

	mu   sync.RWMutex
	conn *sshConn
	sess *yamux.Session
	ctrl *Endpoint
	file *Endpoint

	invalidate  func(proto.Invalidate) // registered by FUSE layer
	healthEvery time.Duration
}

// NewClient builds a client (not yet connected).
func NewClient(cfg *config.Config, remoteBin string) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	return &Client{
		cfg:         cfg,
		remoteBin:   remoteBin,
		status:      NewStatus(),
		ctx:         ctx,
		cancel:      cancel,
		healthEvery: 5 * time.Second,
	}
}

// Status returns the shared online flag.
func (c *Client) Status() *Status { return c.status }

// OnInvalidate registers the handler for Source->Ghost change pushes.
func (c *Client) OnInvalidate(fn func(proto.Invalidate)) { c.invalidate = fn }

// Start launches the reconnect supervisor in the background.
func (c *Client) Start() { go c.supervise() }

// supervise keeps a connection alive, reconnecting with exponential backoff.
// To avoid flooding logs while a host is unreachable, the failure is logged once
// (WARN) on the first attempt of an outage; subsequent retries log at DEBUG.
func (c *Client) supervise() {
	log := logx.For("client")
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	// A connection that dies sooner than this is treated as a failed attempt
	// (e.g. the remote agent is missing and `lg serve` exits immediately). Only a
	// session that stayed up longer resets the backoff — otherwise a misconfig
	// would reconnect in a tight loop and hammer the server.
	const minHealthy = 5 * time.Second
	loggedOutage := false
	for c.ctx.Err() == nil {
		start := time.Now()
		err := c.connectOnce()
		lasted := time.Since(start)

		if err == nil && lasted >= minHealthy {
			backoff = time.Second // genuinely healthy session that has now ended
			loggedOutage = false
			continue
		}
		if !loggedOutage {
			log.Warn("not connected (agent missing, or link dropped immediately); retrying with backoff",
				"err", err, "lasted", lasted.Round(time.Millisecond))
			loggedOutage = true
		} else {
			log.Debug("connect retry failed", "err", err, "lasted", lasted, "retry_in", backoff)
		}
		c.status.set(false)
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}

// connectOnce establishes one connection and blocks until it fails.
func (c *Client) connectOnce() error {
	conn, err := dialSSH(c.cfg, c.remoteBin)
	if err != nil {
		return err
	}
	sess, err := NewClientSession(conn.pipe)
	if err != nil {
		_ = conn.Close()
		return err
	}
	ctrlStream, err := OpenStream(sess, StreamControl)
	if err != nil {
		_ = sess.Close()
		_ = conn.Close()
		return err
	}
	fileStream, err := OpenStream(sess, StreamFile)
	if err != nil {
		_ = sess.Close()
		_ = conn.Close()
		return err
	}
	ctrl := NewEndpoint(ctrlStream)
	file := NewEndpoint(fileStream)
	go ctrl.Serve()
	go file.Serve()

	c.mu.Lock()
	c.conn, c.sess, c.ctrl, c.file = conn, sess, ctrl, file
	c.mu.Unlock()

	connCtx, connCancel := context.WithCancel(c.ctx)
	defer connCancel()
	go HealthChecker(connCtx, ctrl, c.status, c.healthEvery)
	go c.acceptServerStreams(connCtx, sess)

	logx.For("client").Info("connected to source", "host", c.cfg.Source.Host)

	// Block until either endpoint or the session dies.
	select {
	case <-ctrl.Done():
	case <-file.Done():
	case <-c.ctx.Done():
	}
	c.status.set(false)
	_ = sess.Close()
	_ = conn.Close()
	logx.For("client").Warn("connection lost")
	return nil
}

// acceptServerStreams handles streams Source opens toward Ghost (notify).
func (c *Client) acceptServerStreams(ctx context.Context, sess *yamux.Session) {
	for ctx.Err() == nil {
		stream, kind, err := AcceptStream(sess)
		if err != nil {
			return
		}
		switch kind {
		case StreamNotify:
			go c.serveNotify(stream)
		default:
			_ = stream.Close()
		}
	}
}

func (c *Client) serveNotify(stream *yamux.Stream) {
	ep := NewEndpoint(stream)
	ep.SetHandler(func(f proto.Frame) (proto.MsgType, any, bool, error) {
		if f.Type == proto.TypeInvalidate {
			var inv proto.Invalidate
			if err := proto.Unmarshal(f.Body, &inv); err == nil && c.invalidate != nil {
				c.invalidate(inv)
			}
		}
		return 0, nil, false, nil // one-way push, no reply
	})
	_ = ep.Serve()
}

// ControlCall issues a request on the control stream (health/session-list).
func (c *Client) ControlCall(ctx context.Context, t proto.MsgType, body any) (proto.Frame, error) {
	c.mu.RLock()
	ep := c.ctrl
	c.mu.RUnlock()
	if ep == nil || !c.status.Online() {
		return proto.Frame{}, ErrOffline
	}
	return ep.Call(ctx, t, body)
}

// FileCall issues a file-RPC request on the current connection.
func (c *Client) FileCall(ctx context.Context, t proto.MsgType, body any) (proto.Frame, error) {
	c.mu.RLock()
	ep := c.file
	c.mu.RUnlock()
	if ep == nil || !c.status.Online() {
		return proto.Frame{}, ErrOffline
	}
	return ep.Call(ctx, t, body)
}

// OpenPTYStreams opens a paired control+data stream for a SOURCE-mode session.
func (c *Client) OpenPTYStreams() (ctl, data *yamux.Stream, err error) {
	c.mu.RLock()
	sess := c.sess
	online := c.status.Online()
	c.mu.RUnlock()
	if sess == nil || !online {
		return nil, nil, ErrOffline
	}
	ctl, err = OpenStream(sess, StreamPTYCtl)
	if err != nil {
		return nil, nil, err
	}
	data, err = OpenStream(sess, StreamPTY)
	if err != nil {
		_ = ctl.Close()
		return nil, nil, err
	}
	return ctl, data, nil
}

// OpenJobLogStream opens a stream to tail a detached job's log file. The caller
// writes a JSON proto.JobLogReq line first (mirroring the exec token line).
func (c *Client) OpenJobLogStream() (*yamux.Stream, error) {
	c.mu.RLock()
	sess := c.sess
	online := c.status.Online()
	c.mu.RUnlock()
	if sess == nil || !online {
		return nil, ErrOffline
	}
	return OpenStream(sess, StreamJobLog)
}

// Close tears the client down.
func (c *Client) Close() error {
	c.cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	var err error
	if c.conn != nil {
		err = c.conn.Close()
	}
	if c.sess != nil {
		_ = c.sess.Close()
	}
	return err
}
