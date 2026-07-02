package transport

import (
	"bufio"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/iamtaehyunpark/livegit/internal/proto"
)

// Endpoint runs the framed request/response protocol over one yamux stream.
//
// Usage on a given stream is asymmetric: one side initiates Calls, the other
// registers a Handler and only responds. Incoming frames whose ReqID matches a
// pending local call are delivered as responses; all others are dispatched to
// the Handler (and its result returned with the same ReqID). This keeps ReqID
// spaces from colliding without per-frame role tagging.
type Endpoint struct {
	w   io.Writer
	br  *bufio.Reader
	cl  io.Closer
	wmu sync.Mutex // serializes frame writes

	nextID  atomic.Uint64
	mu      sync.Mutex
	pending map[uint64]chan proto.Frame

	handler Handler

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  atomic.Value // error
}

// Handler processes an inbound request frame and returns a response frame body.
// Returning an error sends a TypeErr response. A nil response (ok=false) means
// the frame was a one-way notification needing no reply.
type Handler func(f proto.Frame) (respType proto.MsgType, body any, reply bool, err error)

// NewEndpoint builds an endpoint over a stream.
func NewEndpoint(stream io.ReadWriteCloser) *Endpoint {
	e := &Endpoint{
		w:       stream,
		br:      bufio.NewReaderSize(stream, 64<<10),
		cl:      stream,
		pending: make(map[uint64]chan proto.Frame),
		closed:  make(chan struct{}),
	}
	return e
}

// SetHandler registers the inbound request handler (server side).
func (e *Endpoint) SetHandler(h Handler) { e.handler = h }

// Serve runs the read loop until the stream closes. Run it in its own goroutine.
func (e *Endpoint) Serve() error {
	for {
		f, err := proto.ReadFrame(e.br)
		if err != nil {
			e.shutdown(err)
			return err
		}
		e.mu.Lock()
		ch, isResp := e.pending[f.ReqID]
		if isResp {
			delete(e.pending, f.ReqID)
		}
		e.mu.Unlock()

		if isResp && f.ReqID != 0 {
			ch <- f
			continue
		}
		// Inbound request / notification.
		go e.dispatch(f)
	}
}

func (e *Endpoint) dispatch(f proto.Frame) {
	if e.handler == nil {
		return
	}
	respType, body, reply, err := e.handler(f)
	if !reply {
		return
	}
	if err != nil {
		_ = e.send(proto.MsgType(proto.TypeErr), f.ReqID, proto.ErrResp{Message: err.Error()})
		return
	}
	_ = e.send(respType, f.ReqID, body)
}

func (e *Endpoint) send(t proto.MsgType, reqID uint64, body any) error {
	fr, err := proto.NewFrame(t, reqID, body)
	if err != nil {
		return err
	}
	e.wmu.Lock()
	defer e.wmu.Unlock()
	return proto.WriteFrame(e.w, fr)
}

// Notify sends a one-way frame (ReqID 0), e.g. an invalidation push.
func (e *Endpoint) Notify(t proto.MsgType, body any) error {
	return e.send(t, 0, body)
}

// ErrClosed is returned when an endpoint's stream has shut down.
var ErrClosed = errors.New("transport: endpoint closed")

// Call sends a request and waits for the matching response or ctx cancellation.
func (e *Endpoint) Call(ctx context.Context, t proto.MsgType, body any) (proto.Frame, error) {
	id := e.nextID.Add(1)
	ch := make(chan proto.Frame, 1)

	e.mu.Lock()
	e.pending[id] = ch
	e.mu.Unlock()

	if err := e.send(t, id, body); err != nil {
		e.mu.Lock()
		delete(e.pending, id)
		e.mu.Unlock()
		return proto.Frame{}, err
	}

	select {
	case <-ctx.Done():
		e.mu.Lock()
		delete(e.pending, id)
		e.mu.Unlock()
		return proto.Frame{}, ctx.Err()
	case <-e.closed:
		return proto.Frame{}, e.closedError()
	case f := <-ch:
		if f.Type == proto.TypeErr {
			var er proto.ErrResp
			_ = proto.Unmarshal(f.Body, &er)
			return f, errors.New(er.Message)
		}
		return f, nil
	}
}

func (e *Endpoint) shutdown(err error) {
	e.closeOnce.Do(func() {
		if err != nil {
			e.closeErr.Store(err)
		}
		close(e.closed)
		e.mu.Lock()
		for id, ch := range e.pending {
			close(ch)
			delete(e.pending, id)
		}
		e.mu.Unlock()
	})
}

func (e *Endpoint) closedError() error {
	if v := e.closeErr.Load(); v != nil {
		return v.(error)
	}
	return ErrClosed
}

// Close shuts the endpoint and its stream.
func (e *Endpoint) Close() error {
	e.shutdown(nil)
	return e.cl.Close()
}

// Done is closed when the endpoint shuts down.
func (e *Endpoint) Done() <-chan struct{} { return e.closed }
