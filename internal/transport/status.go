package transport

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/taehyun/lg/internal/logx"
	"github.com/taehyun/lg/internal/proto"
)

// Status is the single online/offline source of truth (spec §6.2, §7). The
// transport owns it; FUSE write-through, the shell's SOURCE entry, and the
// journal flush worker all read it (via Online) and may subscribe for edge
// notifications (via Subscribe) rather than each polling independently.
type Status struct {
	online atomic.Bool

	mu   sync.Mutex
	subs []chan bool
}

// NewStatus starts offline.
func NewStatus() *Status { return &Status{} }

// Online reports the current state.
func (s *Status) Online() bool { return s.online.Load() }

// set updates state and fans out edge events to subscribers.
func (s *Status) set(v bool) {
	if s.online.Swap(v) == v {
		return // no edge
	}
	logx.For("transport").Info("online state changed", "online", v)
	s.mu.Lock()
	subs := append([]chan bool(nil), s.subs...)
	s.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- v:
		default: // non-blocking; subscribers coalesce
		}
	}
}

// Subscribe returns a channel that receives true/false on each state edge.
func (s *Status) Subscribe() <-chan bool {
	ch := make(chan bool, 1)
	s.mu.Lock()
	s.subs = append(s.subs, ch)
	s.mu.Unlock()
	return ch
}

// HealthChecker pings over the control endpoint every interval and drives the
// Status flag. A failed ping (or ctx timeout) flips offline; a success flips
// online. The reconnect loop lives one level up in the Client.
func HealthChecker(ctx context.Context, ctrl *Endpoint, st *Status, interval time.Duration) {
	log := logx.For("health")
	var nonce uint64
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		// Ping immediately, then on each tick.
		nonce++
		pingCtx, cancel := context.WithTimeout(ctx, interval)
		_, err := ctrl.Call(pingCtx, proto.TypePing, proto.Ping{Nonce: nonce})
		cancel()
		if err != nil {
			st.set(false)
			log.Debug("health ping failed", "err", err)
		} else {
			st.set(true)
		}
		select {
		case <-ctx.Done():
			return
		case <-ctrl.Done():
			st.set(false)
			return
		case <-tick.C:
		}
	}
}
