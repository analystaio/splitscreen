package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/analystaio/splitscreen/internal/surface"
	"github.com/analystaio/splitscreen/protocol"
)

// The fake surface offers stop controls, so every gateway test runs with them
// armed; the default delay outlives the tests that do not care.
func (f *fakeSurface) PostStop(_ context.Context, c surface.StopControl) (surface.Ref, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopControls = append(f.stopControls, c)
	f.seq++
	return surface.Ref{Channel: c.Channel, Thread: c.Thread, ID: "s" + itoa(f.seq)}, nil
}

func (f *fakeSurface) Delete(_ context.Context, ref surface.Ref) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, ref.ID)
	return nil
}

func (f *fakeSurface) stopState() (controls []surface.StopControl, deleted, resolved []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append(controls, f.stopControls...), append(deleted, f.deleted...), append(resolved, f.resolved...)
}

func quickStopControl(t *testing.T) {
	old := stopControlDelay
	stopControlDelay = 20 * time.Millisecond
	t.Cleanup(func() { stopControlDelay = old })
}

func TestStopButtonCancelsTheTurn(t *testing.T) {
	quickStopControl(t)
	h := newHarness(t)
	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)

	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U1"}, Text: "do a long thing", Addressed: true,
	})
	msg := readFrame[*protocol.Message](t, ws)

	var control surface.StopControl
	eventually(t, "stop control", func() bool {
		cs, _, _ := h.srf.stopState()
		if len(cs) == 1 {
			control = cs[0]
			return true
		}
		return false
	})
	if control.TurnID != msg.TurnID || control.Thread != "T1" {
		t.Fatalf("control = %+v", control)
	}

	h.gw.OnStop(context.Background(), surface.StopRequest{
		Surface: "test", TurnID: msg.TurnID, User: surface.User{ID: "U2"},
		Ref: surface.Ref{Channel: "C1", Thread: "T1", ID: "s1"},
	})
	cancel := readFrame[*protocol.TurnCancel](t, ws)
	if cancel.TurnID != msg.TurnID || cancel.By == nil || cancel.By.ID != "U2" {
		t.Fatalf("cancel = %+v", cancel)
	}

	send(t, ws, &protocol.Done{ThreadID: msg.ThreadID, TurnID: msg.TurnID, Interrupted: true})
	eventually(t, "stopped outcome", func() bool {
		_, _, resolved := h.srf.stopState()
		return len(resolved) > 0 && strings.Contains(resolved[len(resolved)-1], "Stopped by <@U2>")
	})
	if strings.Contains(h.srf.allText(), "no output") {
		t.Fatal("a stopped turn was reported as having no output")
	}

	// The thread is free again: the next message dispatches.
	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U1"}, Text: "carry on",
	})
	if next := readFrame[*protocol.Message](t, ws); next.Text != "carry on" {
		t.Fatalf("next = %+v", next)
	}
}

func TestUnpressedStopControlIsDeleted(t *testing.T) {
	quickStopControl(t)
	h := newHarness(t)
	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)

	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U1"}, Text: "hi", Addressed: true,
	})
	msg := readFrame[*protocol.Message](t, ws)
	eventually(t, "stop control", func() bool {
		cs, _, _ := h.srf.stopState()
		return len(cs) == 1
	})
	send(t, ws, &protocol.Done{ThreadID: msg.ThreadID, TurnID: msg.TurnID})
	eventually(t, "control deleted", func() bool {
		_, deleted, _ := h.srf.stopState()
		return len(deleted) == 1
	})
}

func TestBangStopCancelsAndDropsTheQueue(t *testing.T) {
	h := newHarness(t)
	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)

	in := surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U1"}, Addressed: true,
	}
	first := in
	first.Text = "first"
	h.gw.OnMessage(context.Background(), first)
	msg := readFrame[*protocol.Message](t, ws)

	parked := in
	parked.Text = "second"
	h.gw.OnMessage(context.Background(), parked)

	stop := in
	stop.Text = "!stop"
	h.gw.OnMessage(context.Background(), stop)
	if c := readFrame[*protocol.TurnCancel](t, ws); c.TurnID != msg.TurnID {
		t.Fatalf("cancel = %+v", c)
	}
	eventually(t, "parked message dropped", func() bool {
		return strings.Contains(h.srf.updatesText(), "Dropped")
	})

	send(t, ws, &protocol.Done{ThreadID: msg.ThreadID, TurnID: msg.TurnID, Interrupted: true})
	eventually(t, "stop recorded in thread", func() bool {
		return strings.Contains(h.srf.allText(), "Stopped by <@U1>")
	})

	// The dropped message must not run once the stopped turn ends.
	again := in
	again.Text = "third"
	h.gw.OnMessage(context.Background(), again)
	if next := readFrame[*protocol.Message](t, ws); next.Text != "third" {
		t.Fatalf("next dispatched = %q, want third (second was dropped)", next.Text)
	}
}

func TestBangStopWithNothingRunning(t *testing.T) {
	h := newHarness(t)
	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)
	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U1"}, Text: "!stop", Addressed: true,
	})
	eventually(t, "nothing-running notice", func() bool {
		return strings.Contains(h.srf.allText(), "Nothing is running")
	})
}
