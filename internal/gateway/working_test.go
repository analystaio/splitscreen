package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/avarant/splitscreen/internal/surface"
	"github.com/avarant/splitscreen/protocol"
)

func statusesEqual(h *harness, want ...string) func() bool {
	return func() bool {
		return strings.Join(h.srf.statusTexts(), "|") == strings.Join(want, "|")
	}
}

// The indicator appears when a turn is dispatched, as the runner, and is
// cleared when the turn ends.
func TestWorkingIndicatorFollowsTheTurn(t *testing.T) {
	h := newHarness(t)
	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)

	h.gw.OnMessage(context.Background(), offlineMsg("T1", "hello"))
	msg := readFrame[*protocol.Message](t, ws)
	eventually(t, "status set", statusesEqual(h, "C1:is working…"))
	h.srf.mu.Lock()
	st := h.srf.statuses[0]
	h.srf.mu.Unlock()
	if st.Thread != "T1" || st.Persona.Name != "Alpha" {
		t.Errorf("status = %+v", st)
	}

	send(t, ws, &protocol.Done{ThreadID: msg.ThreadID, TurnID: msg.TurnID})
	eventually(t, "status cleared", statusesEqual(h, "C1:is working…", "C1:"))
}

func TestWorkingIndicatorOffPerRunner(t *testing.T) {
	h := newHarness(t)
	off := ""
	h.gw.cfg.Load().Runners["alpha"].WorkingStatus = &off
	custom := h.gw.cfg.Load().Runners["alpha"]
	if custom.WorkingText() != "" {
		t.Fatal("explicit empty working_status should disable")
	}
	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)
	h.gw.OnMessage(context.Background(), offlineMsg("T1", "hello"))
	readFrame[*protocol.Message](t, ws)
	time.Sleep(50 * time.Millisecond)
	if n := len(h.srf.statusTexts()); n != 0 {
		t.Fatalf("%d status calls with the indicator off", n)
	}
}

// A permanent failure is tried once per channel and then latched off; a turn
// is never affected by it.
func TestWorkingIndicatorLatchesOffOnPermanentFailure(t *testing.T) {
	h := newHarness(t)
	h.srf.statusErr = func(st surface.Status) error {
		if st.Channel == "C1" {
			return fmt.Errorf("%w: missing_scope", surface.ErrStatusUnavailable)
		}
		return nil
	}
	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)

	for i := 0; i < 3; i++ {
		h.gw.OnMessage(context.Background(), offlineMsg(fmt.Sprintf("T%d", i), "hello"))
		msg := readFrame[*protocol.Message](t, ws)
		send(t, ws, &protocol.Done{ThreadID: msg.ThreadID, TurnID: msg.TurnID})
	}
	time.Sleep(100 * time.Millisecond)
	if got := h.srf.statusTexts(); len(got) != 1 {
		t.Fatalf("status calls after a permanent failure: %v, want exactly the first", got)
	}

	// A transient error is not latched.
	h2 := newHarness(t)
	h2.srf.statusErr = func(surface.Status) error { return errors.New("ratelimited") }
	h2.gw.setWorking("k1", "test", "C9", "T1", surface.Persona{}, "is working…", true)
	h2.gw.setWorking("k2", "test", "C9", "T2", surface.Persona{}, "is working…", true)
	eventually(t, "both transient attempts made", func() bool { return len(h2.srf.statusTexts()) == 2 })
}

// A message waiting on a sleeping machine says so; once the runner connects and
// the queue drains, the indicator becomes the working one.
func TestWorkingIndicatorWhileWaking(t *testing.T) {
	h, _ := wakeHarness(t, fakeStart{prev: "stopped"})
	h.gw.OnMessage(context.Background(), offlineMsg("T1", "hello"))
	eventually(t, "starting status", statusesEqual(h, "C1:"+workingStartingText))

	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)
	readFrame[*protocol.Message](t, ws)
	eventually(t, "working status after drain",
		statusesEqual(h, "C1:"+workingStartingText, "C1:is working…"))
}

func TestWorkingIndicatorRefresh(t *testing.T) {
	h := newHarness(t)
	g := h.gw
	g.setWorking("run", "test", "C1", "T1", surface.Persona{}, "is working…", true)
	g.setWorking("wait", "test", "C1", "T2", surface.Persona{}, workingQueuedText, false)
	eventually(t, "initial sets", func() bool { return len(h.srf.statusTexts()) == 2 })

	// Activity inside the rate limit sends nothing; outside it, one re-set.
	g.touchWorking("run")
	g.working.mu.Lock()
	g.working.entries["run"].lastSent = time.Now().Add(-workingRefreshMin - time.Second)
	g.working.mu.Unlock()
	g.touchWorking("run")
	g.touchWorking("run")
	eventually(t, "one activity re-set", func() bool { return len(h.srf.statusTexts()) == 3 })

	// The ticker re-sets a quiet running turn, and forgets a waiting state past
	// its window instead of keeping it alive forever.
	g.working.mu.Lock()
	g.working.entries["run"].lastSent = time.Now().Add(-workingTick)
	g.working.entries["wait"].setAt = time.Now().Add(-workingWaitFor - time.Second)
	g.working.entries["wait"].lastSent = time.Now().Add(-workingTick)
	g.working.mu.Unlock()
	g.refreshWorking()
	eventually(t, "tick re-set", func() bool { return len(h.srf.statusTexts()) == 4 })
	g.working.mu.Lock()
	_, waiting := g.working.entries["wait"]
	g.working.mu.Unlock()
	if waiting {
		t.Error("an expired waiting indicator was kept")
	}
}
