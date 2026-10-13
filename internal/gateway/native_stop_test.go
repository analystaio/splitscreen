package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/analystaio/splitscreen/internal/surface"
	"github.com/analystaio/splitscreen/protocol"
)

// Slack's own stop button names a thread, not a turn. When Slack has already
// ended the streaming message, the gateway must not post the answer again.
func TestNativeStopCancelsAndDoesNotRepostTheAnswer(t *testing.T) {
	h, srf := newStreamingHarness(t)
	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)

	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U1"}, Addressed: true, Text: "long job",
	})
	msg := readFrame[*protocol.Message](t, ws)
	send(t, ws, &protocol.ToolStart{ThreadID: msg.ThreadID, TurnID: msg.TurnID, CallID: "c1", Tool: "Bash"})
	eventually(t, "stream opened", func() bool {
		srf.smu.Lock()
		defer srf.smu.Unlock()
		return srf.opened == 1
	})

	h.gw.OnStop(context.Background(), surface.StopRequest{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U2"}, StreamStopped: true,
	})
	if c := readFrame[*protocol.TurnCancel](t, ws); c.TurnID != msg.TurnID || c.By.ID != "U2" {
		t.Fatalf("cancel = %+v", c)
	}

	// Slack refuses writes to a stream it stopped.
	srf.smu.Lock()
	srf.sendErr = errors.New("message_not_in_streaming_state")
	srf.smu.Unlock()
	send(t, ws, &protocol.TextDelta{ThreadID: msg.ThreadID, TurnID: msg.TurnID, Text: "partial answer"})
	send(t, ws, &protocol.Done{ThreadID: msg.ThreadID, TurnID: msg.TurnID, Interrupted: true})

	eventually(t, "stop noted", func() bool {
		return strings.Contains(srf.allText(), "Stopped by <@U2>")
	})
	if strings.Contains(srf.allText(), "partial answer") {
		t.Fatal("the answer was re-posted after the surface stopped the stream")
	}
}

func TestNativeStopWithNothingRunningClearsWorking(t *testing.T) {
	h := newHarness(t)
	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)
	h.gw.OnStop(context.Background(), surface.StopRequest{
		Surface: "test", Channel: "C1", Thread: "T9", User: surface.User{ID: "U1"},
	})
	if strings.Contains(h.srf.allText(), "Stopping") {
		t.Fatal("a stop with nothing running posted something")
	}
}
