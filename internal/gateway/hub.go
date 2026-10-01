package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/analystaio/splitscreen/protocol"
)

// ConnState is a runner connection's liveness.
type ConnState string

const (
	StateConnected    ConnState = "connected"
	StateDegraded     ConnState = "degraded"
	StateDisconnected ConnState = "disconnected"
)

// Hub is the registry of live runner connections.
type Hub struct {
	mu    sync.RWMutex
	conns map[string]*Conn
}

func NewHub() *Hub { return &Hub{conns: map[string]*Conn{}} }

// Register adds a connection, displacing any existing one for the same runner.
// Displacing rather than refusing is deliberate: after an ungraceful restart the
// old connection is a zombie the gateway has no way to distinguish from a live
// one, and refusing would leave the runner permanently locked out.
func (h *Hub) Register(c *Conn) *Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	old := h.conns[c.runner]
	h.conns[c.runner] = c
	return old
}

// Unregister removes a connection, but only if it is still the current one.
func (h *Hub) Unregister(c *Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur, ok := h.conns[c.runner]; ok && cur == c {
		delete(h.conns, c.runner)
	}
}

func (h *Hub) Get(runner string) (*Conn, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	c, ok := h.conns[runner]
	return c, ok
}

func (h *Hub) List() []*Conn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*Conn, 0, len(h.conns))
	for _, c := range h.conns {
		out = append(out, c)
	}
	return out
}

// ---------------------------------------------------------------------------

type outFrame struct {
	typ  websocket.MessageType
	data []byte
}

// Conn is one runner's connection.
type Conn struct {
	runner  string
	gw      *Gateway
	ws      *websocket.Conn
	hello   *protocol.Hello
	peerVer protocol.SemVer

	out    chan outFrame
	done   chan struct{}
	closed atomic.Bool
	once   sync.Once

	connectedAt time.Time
	lastSeen    atomic.Int64 // unix nanos

	// blobs being assembled from this runner, keyed by blob id.
	blobs sync.Map

	bundleVersion atomic.Int64
	bundleDigest  atomic.Value // string: what the runner currently has
}

func newConn(gw *Gateway, ws *websocket.Conn, hello *protocol.Hello, ver protocol.SemVer) *Conn {
	c := &Conn{
		runner:      hello.Runner,
		gw:          gw,
		ws:          ws,
		hello:       hello,
		peerVer:     ver,
		out:         make(chan outFrame, 256),
		done:        make(chan struct{}),
		connectedAt: time.Now(),
	}
	c.lastSeen.Store(time.Now().UnixNano())
	c.bundleDigest.Store("")
	if hello.Bundle != nil {
		c.bundleDigest.Store(hello.Bundle.Digest)
		c.bundleVersion.Store(int64(hello.Bundle.Version))
	}
	return c
}

// BundleDigest is the configuration the runner reports having materialized.
func (c *Conn) BundleDigest() string {
	s, _ := c.bundleDigest.Load().(string)
	return s
}

func (c *Conn) setBundle(version int, digest string) {
	c.bundleVersion.Store(int64(version))
	c.bundleDigest.Store(digest)
}

func (c *Conn) Runner() string         { return c.runner }
func (c *Conn) ConnectedAt() time.Time { return c.connectedAt }
func (c *Conn) Harness() protocol.HarnessInfo {
	if c.hello == nil {
		return protocol.HarnessInfo{}
	}
	return c.hello.Harness
}
func (c *Conn) BundleVersion() int { return int(c.bundleVersion.Load()) }

// State derives liveness from the heartbeat: degraded after two missed pings,
// disconnected after five. Silence is reported, not assumed benign.
func (c *Conn) State(heartbeat time.Duration) ConnState {
	if c.closed.Load() {
		return StateDisconnected
	}
	silence := time.Since(time.Unix(0, c.lastSeen.Load()))
	switch {
	case silence > 5*heartbeat:
		return StateDisconnected
	case silence > 2*heartbeat:
		return StateDegraded
	default:
		return StateConnected
	}
}

var errConnClosed = errors.New("gateway: runner connection is closed")

// Send queues a control frame. It never blocks the caller on a slow runner: a
// full queue closes the connection, which the runner recovers from by
// reconnecting and draining its persisted queue.
func (c *Conn) Send(f protocol.Frame) error {
	data, err := protocol.Encode(f)
	if err != nil {
		return err
	}
	return c.sendRaw(outFrame{typ: websocket.MessageText, data: data})
}

func (c *Conn) SendRawJSON(data []byte) error {
	return c.sendRaw(outFrame{typ: websocket.MessageText, data: data})
}

func (c *Conn) sendBinary(data []byte) error {
	return c.sendRaw(outFrame{typ: websocket.MessageBinary, data: data})
}

func (c *Conn) sendRaw(f outFrame) error {
	// The queue is never closed, only abandoned: closing it would let a
	// concurrent send panic, and senders here are spread across goroutines the
	// closer knows nothing about.
	select {
	case <-c.done:
		return errConnClosed
	default:
	}
	select {
	case c.out <- f:
		return nil
	case <-c.done:
		return errConnClosed
	default:
		c.gw.log.Error("runner send queue full; dropping connection", "runner", c.runner)
		c.CloseWith("send queue overflow")
		return fmt.Errorf("gateway: send queue full for runner %s", c.runner)
	}
}

// CloseWith shuts the connection down once, with a reason the runner logs.
//
// The socket close runs in the background on purpose. A WebSocket close is a
// handshake: it writes a close frame and waits for the peer to answer. A
// displaced connection is very often a zombie whose peer will never answer, and
// blocking here would stall the replacement connection's own handshake for the
// full close timeout — exactly the case displacement exists to recover from.
func (c *Conn) CloseWith(reason string) {
	c.once.Do(func() {
		c.closed.Store(true)
		close(c.done)
		go func() {
			_ = c.ws.Close(websocket.StatusNormalClosure, truncateReason(reason))
		}()
	})
}

func truncateReason(s string) string {
	// Close reasons are capped by the protocol.
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

// writeLoop drains the outbound queue.
func (c *Conn) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case f := <-c.out:
			wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := c.ws.Write(wctx, f.typ, f.data)
			cancel()
			if err != nil {
				c.gw.log.Warn("runner write failed", "runner", c.runner, "err", err)
				c.closed.Store(true)
				return
			}
		}
	}
}

// heartbeat pings on an interval so silence is detectable in bounded time,
// and acts on that silence: five unanswered periods close the connection.
//
// Closing is what makes the offline queue reachable. A host that hangs (rather
// than exits) leaves the TCP session half-open, so writes into it keep
// "succeeding" — pings included — and without this the connection stays in the
// hub looking online while every message sent to it evaporates. Killing the
// socket unblocks the read loop, which unregisters the runner, and from then
// on messages queue with a visible in-thread notice instead.
func (c *Conn) heartbeat(ctx context.Context, period time.Duration) {
	t := time.NewTicker(period)
	defer t.Stop()
	var nonce int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if c.closed.Load() {
				return
			}
			if silence := time.Since(time.Unix(0, c.lastSeen.Load())); silence > 5*period {
				c.gw.log.Warn("runner heartbeat timeout; dropping connection",
					"runner", c.runner, "silence", silence.Round(time.Second))
				c.CloseWith(fmt.Sprintf("heartbeat timeout: no traffic for %s", silence.Round(time.Second)))
				return
			}
			nonce++
			if err := c.Send(&protocol.Ping{Nonce: nonce}); err != nil {
				return
			}
		}
	}
}

// readLoop dispatches inbound frames until the connection ends.
func (c *Conn) readLoop(ctx context.Context) error {
	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			return err
		}
		c.lastSeen.Store(time.Now().UnixNano())

		switch typ {
		case websocket.MessageText:
			frame, err := protocol.Decode(data, protocol.DirUp)
			if err != nil {
				// An unknown frame from a newer minor is ignorable by design;
				// anything else is a protocol violation worth surfacing.
				if errors.Is(err, protocol.ErrUnknownFrame) {
					c.gw.log.Debug("ignoring unknown frame", "runner", c.runner, "err", err)
					continue
				}
				c.gw.log.Warn("bad frame from runner", "runner", c.runner, "err", err)
				_ = c.Send(&protocol.Error{Code: "bad_frame", Message: err.Error()})
				continue
			}
			c.gw.dispatch(ctx, c, frame)

		case websocket.MessageBinary:
			hdr, payload, err := protocol.DecodeChunk(data)
			if err != nil {
				c.gw.log.Warn("bad chunk from runner", "runner", c.runner, "err", err)
				continue
			}
			c.gw.onChunk(c, hdr, payload)
		}
	}
}
