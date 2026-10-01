package gateway

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/analystaio/splitscreen/config"
	"github.com/analystaio/splitscreen/internal/surface"
	"github.com/analystaio/splitscreen/internal/wake"
	"github.com/analystaio/splitscreen/protocol"
)

// fakeStarter stands in for EC2. Each call pops the next scripted outcome; the
// last one repeats.
type fakeStarter struct {
	mu      sync.Mutex
	calls   []string
	results []fakeStart
}

type fakeStart struct {
	prev string
	err  error
}

func (f *fakeStarter) Start(_ context.Context, region, id string) (wake.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, region+"/"+id)
	r := f.results[0]
	if len(f.results) > 1 {
		f.results = f.results[1:]
	}
	if r.err != nil {
		return wake.Result{}, r.err
	}
	return wake.Result{PreviousState: r.prev, CurrentState: "pending"}, nil
}

func (f *fakeStarter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

const testInstance = "i-0123456789abcdef0"

func wakeHarness(t *testing.T, results ...fakeStart) (*harness, *fakeStarter) {
	t.Helper()
	h := newHarness(t)
	rc := h.gw.cfg.Load().Runners["alpha"]
	rc.Wake = &config.Wake{EC2Instance: testInstance, Region: "us-east-2"}
	f := &fakeStarter{results: results}
	h.gw.waker = f
	return h, f
}

func offlineMsg(thread, text string) surface.Inbound {
	return surface.Inbound{
		Surface: "test", Channel: "C1", Thread: thread,
		User: surface.User{ID: "U1"}, Text: text, Addressed: true,
	}
}

func TestWakeOnMessageStartsOnceAndAnnounces(t *testing.T) {
	h, f := wakeHarness(t, fakeStart{prev: "stopped"})
	ctx := context.Background()

	h.gw.OnMessage(ctx, offlineMsg("T1", "hello"))
	h.gw.OnMessage(ctx, offlineMsg("T2", "and another"))

	if n := f.count(); n != 1 {
		t.Fatalf("StartInstances calls = %d, want 1 within the rate-limit window", n)
	}
	if f.calls[0] != "us-east-2/"+testInstance {
		t.Errorf("started %q", f.calls[0])
	}
	posts := h.srf.postsCopy()
	if len(posts) != 2 {
		t.Fatalf("want 2 wake notices, got %d: %q", len(posts), h.srf.allText())
	}
	if !strings.Contains(posts[0].Text, "asleep") || !strings.Contains(posts[0].Text, "starting it now") {
		t.Errorf("first notice = %q", posts[0].Text)
	}
	if !strings.Contains(posts[1].Text, "starting up") {
		t.Errorf("second notice = %q", posts[1].Text)
	}
	// Visible to the channel, and posted as the runner.
	if posts[0].Ephemeral || posts[0].Persona.Name != "Alpha" {
		t.Errorf("wake notice should be a visible persona post: %+v", posts[0])
	}
	if d, _ := h.st.QueueDepth("alpha"); d != 2 {
		t.Fatalf("queue depth = %d, want 2", d)
	}

	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)
	readFrame[*protocol.Message](t, ws)
	eventually(t, "wake notices closed out", func() bool {
		return strings.Count(h.srf.updatesText(), "is awake") == 2
	})
}

func TestWakeFailureIsReportedAndMessageStaysQueued(t *testing.T) {
	h, _ := wakeHarness(t, fakeStart{err: errors.New("the gateway is not allowed to start it")})

	h.gw.OnMessage(context.Background(), offlineMsg("T1", "hello"))

	text := h.srf.allText()
	if !strings.Contains(text, "could not be started") || !strings.Contains(text, "not allowed") {
		t.Fatalf("failure not reported in-thread: %q", text)
	}
	if d, _ := h.st.QueueDepth("alpha"); d != 1 {
		t.Fatalf("queue depth = %d; a failed wake must not drop the message", d)
	}
}

// A host that has just idled out is still stopping, which EC2 refuses to start.
// The gateway retries until it can, then edits the notice.
func TestWakeRetriesWhileStopping(t *testing.T) {
	h, f := wakeHarness(t,
		fakeStart{err: wake.ErrStopping},
		fakeStart{err: wake.ErrStopping},
		fakeStart{prev: "stopped"})
	h.gw.wakeRetryEvery = 5 * time.Millisecond
	h.gw.wakeRetryFor = 5 * time.Second

	h.gw.OnMessage(context.Background(), offlineMsg("T1", "hello"))
	if !strings.Contains(h.srf.allText(), "still shutting down") {
		t.Fatalf("notice = %q", h.srf.allText())
	}
	eventually(t, "retry started the machine", func() bool {
		return f.count() == 3 && strings.Contains(h.srf.updatesText(), "starting it now")
	})

	// While a retry loop runs, further messages do not add calls of their own.
	h.gw.wakeMu.Lock()
	h.gw.wakeStateFor("alpha").retrying = true
	h.gw.wakeMu.Unlock()
	h.gw.OnMessage(context.Background(), offlineMsg("T2", "again"))
	if f.count() != 3 {
		t.Errorf("a message during the retry loop made its own call")
	}
}

func TestNonWakeableRunnerDoesNotWake(t *testing.T) {
	h := newHarness(t)
	f := &fakeStarter{results: []fakeStart{{prev: "stopped"}}}
	h.gw.waker = f

	h.gw.OnMessage(context.Background(), offlineMsg("T1", "hello"))
	if f.count() != 0 {
		t.Fatal("a runner without wake was started")
	}
	if !strings.Contains(h.srf.allText(), "offline — queued") {
		t.Errorf("notice = %q", h.srf.allText())
	}
}

// Attachments sent while the runner is away are held and relayed on drain,
// rather than dropped.
func TestAttachmentsHeldWhileOffline(t *testing.T) {
	h := newHarness(t)
	in := offlineMsg("T1", "see attached")
	in.Files = []surface.File{{
		Name: "notes.txt", Mime: "text/plain", Size: 5,
		Open: func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("hello")), nil
		},
	}}
	h.gw.OnMessage(context.Background(), in)
	if !strings.Contains(h.srf.allText(), "Attachments are held") {
		t.Fatalf("notice = %q", h.srf.allText())
	}

	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)
	begin := readFrame[*protocol.BlobBegin](t, ws)
	if begin.Name != "notes.txt" {
		t.Errorf("blob = %q", begin.Name)
	}
	msg := readFrame[*protocol.Message](t, ws)
	if len(msg.Attachments) != 1 || msg.Attachments[0].BlobID != begin.BlobID {
		t.Fatalf("drained message attachments = %+v", msg.Attachments)
	}
}

// A queued turn is waiting, not stuck: neither the stranded sweep nor the
// disconnect reconciler may finalize it, or the message it later delivers would
// run under a turn the gateway has forgotten.
func TestQueuedTurnSurvivesSweepAndReconcile(t *testing.T) {
	h := newHarness(t)
	h.gw.OnMessage(context.Background(), offlineMsg("T1", "hello"))

	if n := h.gw.sweepStrandedOnce(context.Background(), 0); n != 0 {
		t.Fatalf("sweep finalized %d queued turn(s)", n)
	}
	h.gw.reconcileTurns("alpha", 0)
	var live int
	h.gw.turns.Range(func(_, _ any) bool { live++; return true })
	if live != 1 {
		t.Fatalf("live turns = %d, want the queued one to survive", live)
	}
}
