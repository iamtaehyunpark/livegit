package transport

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// wedgedPipe models the real failure mode of the ssh transport: a network
// black hole. Reads block forever — they unblock ONLY when the connection's
// closer runs (killing ssh EOFs the stdout pipe). Writes are swallowed.
type wedgedPipe struct {
	unblock chan struct{}
}

func (p *wedgedPipe) Read(b []byte) (int, error) {
	<-p.unblock
	return 0, errors.New("connection torn down")
}

func (p *wedgedPipe) Write(b []byte) (int, error) { return len(b), nil }

// TestSessionCloseUnblocksWedgedRead is the regression test for the 2026-07-08
// hang: yamux's Session.Close() calls conn.Close() and then waits for its
// recvLoop to exit, but the recvLoop only exits when the underlying transport
// dies. With rwc.c unwired (nil), Close hung forever on a wedged connection —
// wedging the reconnect supervisor and spinning any blocked stream readers at
// 100% CPU on the closed shutdownCh. Wiring the conn's closer into the rwc
// (as both dial paths now do) lets Close complete and the readers exit.
func TestSessionCloseUnblocksWedgedRead(t *testing.T) {
	pipe := &wedgedPipe{unblock: make(chan struct{})}
	var closed atomic.Bool
	closer := newConnCloser(func() error {
		closed.Store(true)
		close(pipe.unblock)
		return nil
	})
	conn := &sshConn{
		pipe:   &rwc{r: pipe, w: pipe, c: closerFunc(closer)},
		closer: closer,
	}

	sess, err := NewClientSession(conn.pipe)
	if err != nil {
		t.Fatalf("NewClientSession: %v", err)
	}

	// A blocked stream reader, like Endpoint.Serve on the control stream.
	stream, err := sess.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	readerDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, rerr := stream.Read(buf)
		readerDone <- rerr
	}()

	// Session.Close must complete promptly: it closes the conn (unblocking the
	// recvLoop via the wired closer) before waiting for it.
	closeDone := make(chan struct{})
	go func() {
		_ = sess.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Session.Close hung on a wedged connection (rwc closer not wired?)")
	}
	if !closed.Load() {
		t.Fatal("Session.Close did not tear down the underlying connection")
	}

	// The blocked reader must exit with an error, not spin forever.
	select {
	case rerr := <-readerDone:
		if rerr == nil {
			t.Fatal("stream read returned nil error after session close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream reader still blocked (or spinning) after Session.Close")
	}
}

// TestConnCloserIdempotent: the same closer is reachable from yamux's
// Session.Close, connectOnce, and Client.Close — it must run exactly once.
func TestConnCloserIdempotent(t *testing.T) {
	var n atomic.Int32
	c := newConnCloser(func() error {
		n.Add(1)
		return errors.New("boom")
	})
	for i := 0; i < 3; i++ {
		if err := c(); err == nil || err.Error() != "boom" {
			t.Fatalf("call %d: want cached error, got %v", i, err)
		}
	}
	if n.Load() != 1 {
		t.Fatalf("teardown ran %d times, want 1", n.Load())
	}
}
