package runner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/analystaio/splitscreen/internal/harness"
	"github.com/analystaio/splitscreen/protocol"
)

// stoppableSession stands in for a harness. With interruptible set, Interrupt
// ends the turn the way Claude Code does — an error result — and the process
// stays up; with stuck set, Interrupt is accepted but nothing ever happens.
// Close ends the event stream, as a killed process does.
type stoppableSession struct {
	events      chan harness.Event
	interrupted chan struct{}
	stuck       bool

	mu     sync.Mutex
	closed bool
}

func newStoppable() *stoppableSession {
	return &stoppableSession{
		events:      make(chan harness.Event, 8),
		interrupted: make(chan struct{}, 1),
	}
}

func (s *stoppableSession) Send(context.Context, harness.Input) error { return nil }
func (s *stoppableSession) Events() <-chan harness.Event              { return s.events }
func (s *stoppableSession) SessionID() string                         { return "sess" }
func (s *stoppableSession) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed
}

func (s *stoppableSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.events)
	}
	return nil
}

func (s *stoppableSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

type interruptible struct{ *stoppableSession }

func (s interruptible) Interrupt(context.Context) error {
	s.interrupted <- struct{}{}
	if !s.stuck {
		s.events <- harness.Event{Kind: harness.EventError, Error: "error_during_execution"}
	}
	return nil
}

// busyThread installs a session mid-turn and pumps its events to the gateway.
func busyThread(r *Runner, thread, turn string, sess harness.Session) *threadSession {
	ts := &threadSession{threadID: thread, sess: sess, lastActivity: time.Now()}
	ts.setTurn(turn)
	r.sessions.Store(thread, ts)
	go r.pumpEvents(ts, sess)
	return ts
}

func TestCancelInterruptsAndKeepsTheSession(t *testing.T) {
	g := newFakeGateway(t)
	r, ws := connectedRunner(t, g)
	fs := newStoppable()
	busyThread(r, "th1", "turn1", interruptible{fs})

	g.reply(ws, &protocol.TurnCancel{ThreadID: "th1", TurnID: "turn1", By: &protocol.UserRef{ID: "U1"}})

	done, ok := await[*protocol.Done](g)
	if !ok {
		t.Fatal("no done after cancel")
	}
	if done.TurnID != "turn1" || !done.Interrupted {
		t.Fatalf("done = %+v, want turn1 interrupted", done)
	}
	select {
	case <-fs.interrupted:
	default:
		t.Fatal("the harness was never interrupted")
	}
	if fs.isClosed() {
		t.Fatal("an interrupt that landed must not end the session")
	}
}

func TestCancelKillsWhenTheHarnessCannotInterrupt(t *testing.T) {
	g := newFakeGateway(t)
	r, ws := connectedRunner(t, g)
	fs := newStoppable()
	busyThread(r, "th1", "turn1", fs)

	g.reply(ws, &protocol.TurnCancel{ThreadID: "th1", TurnID: "turn1"})

	done, ok := await[*protocol.Done](g)
	if !ok {
		t.Fatal("no done after cancel")
	}
	if !done.Interrupted {
		t.Fatalf("done = %+v, want interrupted", done)
	}
	if !fs.isClosed() {
		t.Fatal("the session was not ended")
	}
}

func TestCancelIgnoresATurnThatIsNotRunning(t *testing.T) {
	r := capRunner(t, 0)
	fs := newStoppable()
	ts := busyThread(r, "th1", "turn2", interruptible{fs})

	r.cancelTurn(t.Context(), &protocol.TurnCancel{ThreadID: "th1", TurnID: "turn1"})

	select {
	case <-fs.interrupted:
		t.Fatal("interrupted a turn nobody asked to stop")
	default:
	}
	if ts.wasCancelled() || fs.isClosed() {
		t.Fatal("the running turn was disturbed")
	}
}

func TestCancelKillsWhenTheInterruptNeverLands(t *testing.T) {
	old := interruptGrace
	interruptGrace = 200 * time.Millisecond
	t.Cleanup(func() { interruptGrace = old })

	g := newFakeGateway(t)
	r, ws := connectedRunner(t, g)
	fs := newStoppable()
	fs.stuck = true
	busyThread(r, "th1", "turn1", interruptible{fs})

	g.reply(ws, &protocol.TurnCancel{ThreadID: "th1", TurnID: "turn1"})

	done, ok := await[*protocol.Done](g)
	if !ok {
		t.Fatal("no done after a stuck interrupt")
	}
	if !done.Interrupted || !fs.isClosed() {
		t.Fatalf("done = %+v closed = %v, want interrupted and killed", done, fs.isClosed())
	}
}
