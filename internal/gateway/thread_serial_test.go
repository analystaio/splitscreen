package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/avarant/splitscreen/internal/surface"
)

// TestThreadSerialization: one thread runs one turn at a time; a second message
// on the same thread parks behind it and dispatches when the first ends, while
// a different thread is unaffected.
func TestThreadSerialization(t *testing.T) {
	h := newHarness(t)
	g := h.gw
	rc := g.cfg.Load().Runners["alpha"]
	rc.MaxConcurrent = 4 // don't let the runner cap interfere
	ctx := context.Background()
	tA := "slack:C1:threadA"
	tB := "slack:C1:threadB"

	// First message on thread A: thread was free -> proceed.
	if !g.beginThreadTurn(ctx, qInbound("threadA", "one"), tA, "alpha", rc, surface.Persona{}) {
		t.Fatal("first message on a free thread should proceed")
	}
	// A different thread is independent.
	if !g.beginThreadTurn(ctx, qInbound("threadB", "x"), tB, "alpha", rc, surface.Persona{}) {
		t.Fatal("a different thread should proceed independently")
	}
	// Second and third on thread A: parked.
	if g.beginThreadTurn(ctx, qInbound("threadA", "two"), tA, "alpha", rc, surface.Persona{}) {
		t.Fatal("second message on a busy thread must park, not proceed")
	}
	if g.beginThreadTurn(ctx, qInbound("threadA", "three"), tA, "alpha", rc, surface.Persona{}) {
		t.Fatal("third message on a busy thread must park")
	}

	g.queuesMu.Lock()
	waiting := len(g.threadWaiting[tA])
	active := g.threadActive[tA]
	g.queuesMu.Unlock()
	if waiting != 2 || !active {
		t.Fatalf("thread A should have 2 parked and be active; got waiting=%d active=%v", waiting, active)
	}
	// Two queue notices posted, positions #1 and #2.
	posts := h.srf.postsCopy()
	if len(posts) != 2 || !strings.Contains(posts[0].Text, "#1") || !strings.Contains(posts[1].Text, "#2") {
		t.Fatalf("expected #1,#2 thread notices, got %d: %q", len(posts), h.srf.allText())
	}

	// First turn ends -> "two" dispatches, "three" advances to #1.
	g.endThreadTurn(ctx, tA)
	g.queuesMu.Lock()
	waiting = len(g.threadWaiting[tA])
	active = g.threadActive[tA]
	g.queuesMu.Unlock()
	if waiting != 1 || !active {
		t.Fatalf("after one end, thread A should have 1 parked and stay active; waiting=%d active=%v", waiting, active)
	}
	ups := h.srf.updatesText()
	if !strings.Contains(ups, "starting") || !strings.Contains(ups, "#1") {
		t.Fatalf("expected a 'starting' edit and a #1 advance, got: %q", ups)
	}

	// Drain the rest.
	g.endThreadTurn(ctx, tA) // dispatches "three"
	g.endThreadTurn(ctx, tA) // nothing left -> thread freed
	g.queuesMu.Lock()
	_, stillActive := g.threadActive[tA]
	g.queuesMu.Unlock()
	if stillActive {
		t.Fatal("thread A should be free after draining its queue")
	}
}

// TestStrandedSweep: a turn silent past the timeout is finalized and its slot
// released; a fresh turn is left alone.
func TestStrandedSweep(t *testing.T) {
	h := newHarness(t)
	g := h.gw
	ctx := context.Background()

	stale := &turnContext{TurnID: "turn_stale", ThreadID: "slack:C1:t1", Runner: "alpha", StartedAt: time.Now()}
	stale.lastActivity.Store(time.Now().Add(-90 * time.Minute).UnixNano())
	fresh := &turnContext{TurnID: "turn_fresh", ThreadID: "slack:C1:t2", Runner: "alpha", StartedAt: time.Now()}
	fresh.touch()
	g.turns.Store(stale.TurnID, stale)
	g.turns.Store(fresh.TurnID, fresh)
	// Mark both threads active as a live dispatch would have.
	g.queuesMu.Lock()
	g.threadActive[stale.ThreadID] = true
	g.threadActive[fresh.ThreadID] = true
	g.queuesMu.Unlock()

	n := g.sweepStrandedOnce(ctx, 60*time.Minute)
	if n != 1 {
		t.Fatalf("sweep should finalize exactly the 1 stale turn, got %d", n)
	}
	if _, ok := g.turns.Load("turn_stale"); ok {
		t.Fatal("stale turn should be removed from the turn map")
	}
	if _, ok := g.turns.Load("turn_fresh"); !ok {
		t.Fatal("fresh turn must be left alone")
	}
	// The stale turn's thread slot is released; the fresh one still held.
	g.queuesMu.Lock()
	_, staleActive := g.threadActive["slack:C1:t1"]
	_, freshActive := g.threadActive["slack:C1:t2"]
	g.queuesMu.Unlock()
	if staleActive {
		t.Fatal("stale turn's thread should be released")
	}
	if !freshActive {
		t.Fatal("fresh turn's thread should stay held")
	}
}
