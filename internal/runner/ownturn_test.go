package runner

import (
	"testing"
	"time"

	"github.com/analystaio/splitscreen/internal/harness"
	"github.com/analystaio/splitscreen/protocol"
)

// idleThread installs a session between turns, its last turn finished, and
// pumps its events to the gateway.
func idleThread(r *Runner, thread, lastTurn string, sess harness.Session) *threadSession {
	ts := &threadSession{threadID: thread, sess: sess, lastActivity: time.Now()}
	ts.setTurn(lastTurn)
	ts.endTurn()
	r.sessions.Store(thread, ts)
	go r.pumpEvents(ts, sess)
	return ts
}

// A background task finishing between turns makes the harness speak with no
// turn open. The runner opens one, so the gateway has somewhere to put it, and
// closes it when the harness is done.
func TestBackgroundOutputGetsATurnOfItsOwn(t *testing.T) {
	g := newFakeGateway(t)
	r, _ := connectedRunner(t, g)
	fs := newStoppable()
	ts := idleThread(r, "th1", "turn1", fs)

	fs.events <- harness.Event{Kind: harness.EventBackground}
	fs.events <- harness.Event{Kind: harness.EventText, Text: "finished"}

	start, ok := await[*protocol.TurnStart](g)
	if !ok {
		t.Fatal("no turn.start for unprompted output")
	}
	if start.ThreadID != "th1" || start.TurnID == "" || start.TurnID == "turn1" {
		t.Fatalf("turn.start = %+v", start)
	}
	text, ok := await[*protocol.TextDelta](g)
	if !ok || text.TurnID != start.TurnID || text.Text != "finished" {
		t.Fatalf("text = %+v, want it in turn %q", text, start.TurnID)
	}
	if !ts.isBusy() {
		t.Fatal("the session must count as busy while its own turn runs")
	}

	fs.events <- harness.Event{Kind: harness.EventBackgroundDone, SessionID: "sess", Usage: harness.Usage{Model: "m", Known: true, OutputTokens: 7}}
	usage, ok := await[*protocol.Usage](g)
	if !ok || usage.TurnID != start.TurnID || usage.OutputTokens != 7 {
		t.Fatalf("usage = %+v", usage)
	}
	done, ok := await[*protocol.Done](g)
	if !ok || done.TurnID != start.TurnID || done.Interrupted {
		t.Fatalf("done = %+v", done)
	}
	if ts.isBusy() {
		t.Fatal("the session is still busy after its own turn ended")
	}
}

// A stale task reported while a real turn runs must neither open a turn nor
// end the real one, and stray output between turns with no task behind it
// stays with the turn before.
func TestBackgroundReportDoesNotDisturbARealTurn(t *testing.T) {
	g := newFakeGateway(t)
	r, _ := connectedRunner(t, g)
	fs := newStoppable()
	ts := busyThread(r, "th1", "turn1", fs)

	fs.events <- harness.Event{Kind: harness.EventBackground}
	fs.events <- harness.Event{Kind: harness.EventBackgroundDone}
	fs.events <- harness.Event{Kind: harness.EventText, Text: "pong"}
	text, ok := await[*protocol.TextDelta](g)
	if !ok || text.TurnID != "turn1" {
		t.Fatalf("text = %+v, want it in turn1", text)
	}
	if !ts.runningTurn("turn1") {
		t.Fatal("the real turn was ended by a background report")
	}

	fs.events <- harness.Event{Kind: harness.EventDone}
	if done, ok := await[*protocol.Done](g); !ok || done.TurnID != "turn1" {
		t.Fatalf("done = %+v", done)
	}
	fs.events <- harness.Event{Kind: harness.EventText, Text: "straggler"}
	if text, ok := await[*protocol.TextDelta](g); !ok || text.TurnID != "turn1" {
		t.Fatalf("stray text = %+v, want it left in turn1", text)
	}
	if ts.isBusy() {
		t.Fatal("stray output opened a turn with no background task behind it")
	}
}
