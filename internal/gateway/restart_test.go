package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/analystaio/splitscreen/internal/surface"
	"github.com/analystaio/splitscreen/protocol"
)

// connectInstance performs the handshake as a particular runner process.
func (h *harness) connectInstance(t *testing.T, instance string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { ws.Close(websocket.StatusNormalClosure, "") })
	send(t, ws, &protocol.Hello{
		Protocol: protocol.Version,
		Runner:   "alpha",
		Auth:     protocol.Auth{Mode: protocol.AuthToken, Value: "s3cret"},
		Host:     protocol.Host{OS: "linux", Arch: "amd64"},
		Harness:  protocol.HarnessInfo{Adapter: "claude-code"},
		Instance: instance,
	})
	readFrame[*protocol.HelloAck](t, ws)
	return ws
}

func startRunningTurn(t *testing.T, h *harness, ws *websocket.Conn, text string) *protocol.Message {
	t.Helper()
	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U1"}, Text: text, Addressed: true,
	})
	return readFrame[*protocol.Message](t, ws)
}

// A runner that comes back as a different process cannot finish what the old
// one was running; its turns end at once instead of blocking the thread until
// the stranded-turn sweep.
func TestRestartedRunnerFailsItsStaleTurns(t *testing.T) {
	h := newHarness(t)
	ws := h.connectInstance(t, "proc-1")
	msg := startRunningTurn(t, h, ws, "first")

	// A message behind it parks on the thread.
	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U2"}, Text: "second",
	})

	ws.Close(websocket.StatusNormalClosure, "")
	ws2 := h.connectInstance(t, "proc-2")

	eventually(t, "restart notice", func() bool {
		return strings.Contains(h.srf.allText(), "restarted mid-turn")
	})
	if _, live := h.gw.turns.Load(msg.TurnID); live {
		t.Fatal("the stale turn is still live")
	}
	// The thread is free: the parked message dispatches to the new process.
	if next := readFrame[*protocol.Message](t, ws2); next.Text != "second" {
		t.Fatalf("next = %q, want the parked message", next.Text)
	}
}

// The same process reconnecting is a network blip: its turns carry on.
func TestReconnectingSameProcessKeepsTurns(t *testing.T) {
	h := newHarness(t)
	ws := h.connectInstance(t, "proc-1")
	msg := startRunningTurn(t, h, ws, "first")

	ws.Close(websocket.StatusNormalClosure, "")
	ws2 := h.connectInstance(t, "proc-1")

	send(t, ws2, &protocol.Done{ThreadID: msg.ThreadID, TurnID: msg.TurnID})
	eventually(t, "turn finished normally", func() bool {
		_, live := h.gw.turns.Load(msg.TurnID)
		return !live
	})
	if strings.Contains(h.srf.allText(), "restarted mid-turn") {
		t.Fatal("a reconnect of the same process was treated as a restart")
	}
}
