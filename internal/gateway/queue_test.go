package gateway

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/avarant/splitscreen/config"
	"github.com/avarant/splitscreen/internal/store"
	"github.com/avarant/splitscreen/internal/surface"
)

func qInbound(thread, text string) surface.Inbound {
	return surface.Inbound{
		Surface: "test", Channel: "C1", Thread: thread,
		User: surface.User{ID: "U1"}, Text: text,
	}
}

func (f *fakeSurface) postsCopy() []surface.Post {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]surface.Post(nil), f.posts...)
}

func (f *fakeSurface) updatesText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, u := range f.updates {
		b.WriteString(u.Text)
		b.WriteString("\n")
	}
	return b.String()
}

// TestConcurrencyQueuePositions checks the whole admit/advance cycle: slots up
// to the cap dispatch immediately, the overflow queues FIFO with visible
// positions, and freeing a slot promotes the head and advances the rest.
func TestConcurrencyQueuePositions(t *testing.T) {
	h := newHarness(t)
	g := h.gw
	rc := g.cfg.Load().Runners["alpha"]
	rc.MaxConcurrent = 2
	ctx := context.Background()

	for _, th := range []string{"t1", "t2"} {
		if g.admit(ctx, qInbound(th, "hi"), "alpha", rc, surface.Persona{}) {
			t.Fatalf("thread %s should have taken a free slot, not queued", th)
		}
	}
	for _, th := range []string{"t3", "t4"} {
		if !g.admit(ctx, qInbound(th, "hi"), "alpha", rc, surface.Persona{}) {
			t.Fatalf("thread %s should have queued past the cap", th)
		}
	}

	posts := h.srf.postsCopy()
	if len(posts) != 2 {
		t.Fatalf("want 2 queue notices, got %d: %q", len(posts), h.srf.allText())
	}
	if !strings.Contains(posts[0].Text, "#1") || !strings.Contains(posts[1].Text, "#2") {
		t.Fatalf("queue notices should read #1 then #2, got %q / %q", posts[0].Text, posts[1].Text)
	}

	// A slot frees: t3 (head) is admitted and starts, t4 advances to #1.
	g.turnSlotFreed(ctx, "alpha")
	ups := h.srf.updatesText()
	if !strings.Contains(ups, "starting") {
		t.Fatalf("dequeued turn should get a 'starting' edit, updates were: %q", ups)
	}
	if !strings.Contains(ups, "#1") {
		t.Fatalf("remaining turn should advance to #1, updates were: %q", ups)
	}
}

// TestQueueFullRejects confirms the queue is bounded and a rejection is told to
// the user rather than silently accepted.
func TestQueueFullRejects(t *testing.T) {
	h := newHarness(t)
	g := h.gw
	rc := g.cfg.Load().Runners["alpha"]
	rc.MaxConcurrent = 1
	ctx := context.Background()

	// One takes the slot; fill the queue to the brim.
	g.admit(ctx, qInbound("t0", "x"), "alpha", rc, surface.Persona{})
	for i := 0; i < maxQueueDepth; i++ {
		g.admit(ctx, qInbound("q"+string(rune('a'+i)), "x"), "alpha", rc, surface.Persona{})
	}
	// The next one is refused.
	if !g.admit(ctx, qInbound("overflow", "x"), "alpha", rc, surface.Persona{}) {
		t.Fatal("an over-full queue should still return true (handled)")
	}
	if !strings.Contains(h.srf.allText(), "queue is full") {
		t.Fatalf("expected a queue-full notice, got: %q", h.srf.allText())
	}
}

// --- empty-message fix (native streaming path) ---

type recordingStreamer struct {
	*fakeSurface
	mu        sync.Mutex
	openCalls int
}

type recordingStream struct {
	ref surface.Ref
}

func (s *recordingStream) Ref() surface.Ref                                   { return s.ref }
func (s *recordingStream) Append(context.Context, surface.StreamUpdate) error { return nil }
func (s *recordingStream) Close(context.Context, surface.StreamUpdate) error  { return nil }

func (r *recordingStreamer) OpenStream(_ context.Context, p surface.Post) (surface.Stream, error) {
	r.mu.Lock()
	r.openCalls++
	r.mu.Unlock()
	return &recordingStream{ref: surface.Ref{Channel: p.Channel, Thread: p.Thread, ID: "s1"}}, nil
}

// TestEmptyTurnDoesNotOpenStream is the regression for blank Slack messages: a
// turn that produces no text and no steps must not open a native stream, since
// closing an empty stream leaves a blank bubble in the channel.
func TestEmptyTurnDoesNotOpenStream(t *testing.T) {
	cfg, err := config.Parse([]byte(testConfig))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	srf := &recordingStreamer{fakeSurface: &fakeSurface{}}
	gw, err := New(Options{
		Config: cfg, Store: st, Surfaces: map[string]surface.Surface{"test": srf},
		Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}

	turn := &turnContext{TurnID: "turnE", Surface: "test", Channel: "C1", Thread: "t1"}
	s := gw.streamFor(turn)
	s.Close(context.Background()) // no text, no steps

	srf.mu.Lock()
	opened := srf.openCalls
	srf.mu.Unlock()
	if opened != 0 {
		t.Fatalf("empty turn opened a native stream %d times; should be 0", opened)
	}
	if posts := srf.postsCopy(); len(posts) != 0 {
		t.Fatalf("empty turn posted %d messages via edit path; should be 0", len(posts))
	}
}

// TestStreamWithTextStillOpens guards against the fix over-reaching: a turn with
// real output must still open the stream and deliver it.
func TestStreamWithTextStillOpens(t *testing.T) {
	cfg, _ := config.Parse([]byte(testConfig))
	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	srf := &recordingStreamer{fakeSurface: &fakeSurface{}}
	gw, _ := New(Options{Config: cfg, Store: st, Surfaces: map[string]surface.Surface{"test": srf}, Logger: testLogger()})

	turn := &turnContext{TurnID: "turnT", Surface: "test", Channel: "C1", Thread: "t1"}
	s := gw.streamFor(turn)
	s.AppendText("here is the answer")
	s.Close(context.Background())

	srf.mu.Lock()
	opened := srf.openCalls
	srf.mu.Unlock()
	if opened != 1 {
		t.Fatalf("a turn with output should open the stream once, got %d", opened)
	}
}
