package runner

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/avarant/splitscreen/internal/harness"
)

type fakeSession struct {
	running bool
	closed  bool
	events  chan harness.Event
}

func (f *fakeSession) Send(context.Context, harness.Input) error { return nil }
func (f *fakeSession) Events() <-chan harness.Event              { return f.events }
func (f *fakeSession) SessionID() string                         { return "fake" }
func (f *fakeSession) Running() bool                             { return f.running && !f.closed }
func (f *fakeSession) Close() error {
	f.closed = true
	return nil
}

func capRunner(t *testing.T, max int) *Runner {
	t.Helper()
	return &Runner{
		opts: Options{Name: "t", MaxSessions: max},
		log:  slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}
}

func addSession(r *Runner, thread string, busy bool, idle time.Duration) *fakeSession {
	fs := &fakeSession{running: true, events: make(chan harness.Event)}
	ts := &threadSession{
		threadID:     thread,
		sess:         fs,
		busy:         busy,
		lastActivity: time.Now().Add(-idle),
	}
	r.sessions.Store(thread, ts)
	return fs
}

func TestMakeRoomUnlimitedByDefault(t *testing.T) {
	r := capRunner(t, 0)
	for i := 0; i < 50; i++ {
		addSession(r, string(rune('a'+i)), true, 0)
	}
	if err := r.makeRoom("new"); err != nil {
		t.Fatalf("unlimited runner refused a session: %v", err)
	}
}

func TestMakeRoomUnderCap(t *testing.T) {
	r := capRunner(t, 3)
	a := addSession(r, "a", false, time.Hour)
	b := addSession(r, "b", true, 0)
	if err := r.makeRoom("new"); err != nil {
		t.Fatalf("under-cap runner refused a session: %v", err)
	}
	if a.closed || b.closed {
		t.Fatal("under-cap makeRoom evicted a session")
	}
}

func TestMakeRoomEvictsLongestIdle(t *testing.T) {
	r := capRunner(t, 3)
	oldest := addSession(r, "oldest", false, 2*time.Hour)
	newer := addSession(r, "newer", false, time.Minute)
	working := addSession(r, "working", true, 3*time.Hour)
	if err := r.makeRoom("new"); err != nil {
		t.Fatalf("makeRoom failed with an evictable session: %v", err)
	}
	if !oldest.closed {
		t.Fatal("longest-idle session was not evicted")
	}
	if newer.closed {
		t.Fatal("evicted more sessions than needed")
	}
	if working.closed {
		t.Fatal("evicted a session that was mid-turn")
	}
}

func TestMakeRoomRefusesWhenAllBusy(t *testing.T) {
	r := capRunner(t, 2)
	a := addSession(r, "a", true, time.Hour)
	b := addSession(r, "b", true, time.Hour)
	err := r.makeRoom("new")
	if err == nil {
		t.Fatal("expected capacity refusal when every session is mid-turn")
	}
	if !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.closed || b.closed {
		t.Fatal("a mid-turn session was killed")
	}
}

func TestMakeRoomIgnoresDeadSessions(t *testing.T) {
	r := capRunner(t, 2)
	dead := addSession(r, "dead", false, time.Hour)
	dead.running = false
	live := addSession(r, "live", true, 0)
	if err := r.makeRoom("new"); err != nil {
		t.Fatalf("dead session counted against the cap: %v", err)
	}
	if live.closed {
		t.Fatal("live session evicted while under the real cap")
	}
}

func TestMakeRoomExcludesOwnThread(t *testing.T) {
	// The thread asking for a session is restarting itself: its own (dead)
	// entry must not count toward the cap.
	r := capRunner(t, 1)
	addSession(r, "self", false, time.Hour)
	if err := r.makeRoom("self"); err != nil {
		t.Fatalf("a thread's own entry counted against it: %v", err)
	}
}

func TestEndTurnClearsBusy(t *testing.T) {
	ts := &threadSession{}
	ts.setTurn("turn_1")
	if !ts.isBusy() {
		t.Fatal("setTurn did not mark the session busy")
	}
	ts.endTurn()
	if ts.isBusy() {
		t.Fatal("endTurn did not clear busy")
	}
	if ts.turn() != "turn_1" {
		t.Fatal("endTurn dropped the turn id needed for late-event attribution")
	}
}
