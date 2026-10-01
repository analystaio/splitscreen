package gateway

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/analystaio/splitscreen/config"
	"github.com/analystaio/splitscreen/internal/surface"
	"github.com/analystaio/splitscreen/protocol"
)

// streamingSurface is a fakeSurface that can also stream, so the two paths can
// be exercised against the same gateway.
type streamingSurface struct {
	fakeSurface

	smu      sync.Mutex
	opened   int
	appends  []surface.StreamUpdate
	closes   []surface.StreamUpdate
	openErr  error
	sendErr  error // returned by the first Append once set
	sendOnce bool
}

func (f *streamingSurface) OpenStream(_ context.Context, p surface.Post) (surface.Stream, error) {
	f.smu.Lock()
	defer f.smu.Unlock()
	if f.openErr != nil {
		return nil, f.openErr
	}
	f.opened++
	return &fakeStream{srf: f, ref: surface.Ref{Channel: p.Channel, Thread: p.Thread, ID: "stream1"}}, nil
}

func (f *streamingSurface) record(u surface.StreamUpdate, final bool) error {
	f.smu.Lock()
	defer f.smu.Unlock()
	if f.sendErr != nil && !f.sendOnce {
		f.sendOnce = true
		return f.sendErr
	}
	if final {
		f.closes = append(f.closes, u)
	} else {
		f.appends = append(f.appends, u)
	}
	return nil
}

// steps returns every step state seen, in order, as "id:status" pairs.
func (f *streamingSurface) steps() []string {
	f.smu.Lock()
	defer f.smu.Unlock()
	var out []string
	for _, u := range append(append([]surface.StreamUpdate{}, f.appends...), f.closes...) {
		for _, s := range u.Steps {
			out = append(out, s.ID+":"+string(s.Status))
		}
	}
	return out
}

func (f *streamingSurface) streamedText() string {
	f.smu.Lock()
	defer f.smu.Unlock()
	var b strings.Builder
	for _, u := range append(append([]surface.StreamUpdate{}, f.appends...), f.closes...) {
		b.WriteString(u.Text)
	}
	return b.String()
}

type fakeStream struct {
	srf *streamingSurface
	ref surface.Ref
}

func (s *fakeStream) Ref() surface.Ref { return s.ref }
func (s *fakeStream) Append(_ context.Context, u surface.StreamUpdate) error {
	return s.srf.record(u, false)
}
func (s *fakeStream) Close(_ context.Context, u surface.StreamUpdate) error {
	return s.srf.record(u, true)
}

func newStreamingHarness(t *testing.T) (*harness, *streamingSurface) {
	t.Helper()
	h := newHarness(t)
	srf := &streamingSurface{}
	srf.channels = h.srf.channels
	h.gw.surfaces["test"] = srf
	return h, srf
}

// A tool call is a step with a lifecycle, not a line of text. The surface must
// see it start and then finish, so it can render progress that resolves rather
// than a log that accumulates.
func TestToolCallsStreamAsStepsWithALifecycle(t *testing.T) {
	h, srf := newStreamingHarness(t)
	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)

	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U1"}, Addressed: true, Text: "check the disk",
	})
	msg := readFrame[*protocol.Message](t, ws)

	send(t, ws, &protocol.TextDelta{ThreadID: msg.ThreadID, TurnID: msg.TurnID, Text: "Checking. "})
	send(t, ws, &protocol.ToolStart{
		ThreadID: msg.ThreadID, TurnID: msg.TurnID,
		CallID: "c1", Tool: "Bash", Summary: "df -h /",
	})
	eventually(t, "step reported running", func() bool {
		return contains(srf.steps(), "c1:running")
	})

	send(t, ws, &protocol.ToolEnd{
		ThreadID: msg.ThreadID, TurnID: msg.TurnID,
		CallID: "c1", OK: true, DurationMS: 240,
	})
	send(t, ws, &protocol.TextDelta{ThreadID: msg.ThreadID, TurnID: msg.TurnID, Text: "52% used."})
	send(t, ws, &protocol.Done{ThreadID: msg.ThreadID, TurnID: msg.TurnID})

	eventually(t, "stream closed", func() bool {
		srf.smu.Lock()
		defer srf.smu.Unlock()
		return len(srf.closes) == 1
	})

	srf.smu.Lock()
	opened := srf.opened
	srf.smu.Unlock()
	if opened != 1 {
		t.Fatalf("expected exactly one stream for the turn, got %d", opened)
	}

	got := srf.steps()
	if !contains(got, "c1:running") || !contains(got, "c1:done") {
		t.Fatalf("step never completed its lifecycle: %v", got)
	}

	// Prose followed by a tool call was narration, not the answer: it demotes
	// into a step, and only the text still unclaimed when the turn closes is
	// streamed as the message body.
	if !contains(got, "prose-1:done") {
		t.Fatalf("narration before the tool call did not become a step: %v", got)
	}
	if text := srf.streamedText(); text != "52% used." {
		t.Fatalf("only the final response should stream as text, got %q", text)
	}

	// Streaming replaces the message entirely: nothing goes through post/edit.
	srf.mu.Lock()
	defer srf.mu.Unlock()
	if len(srf.posts) != 0 || len(srf.updates) != 0 {
		t.Fatalf("streaming surface also got %d posts and %d edits",
			len(srf.posts), len(srf.updates))
	}
}

// A failed tool has to reach the surface as a failed step, not vanish because
// its start was the only thing rendered.
func TestFailedToolBecomesAFailedStep(t *testing.T) {
	h, srf := newStreamingHarness(t)
	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)

	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U1"}, Addressed: true, Text: "read the file",
	})
	msg := readFrame[*protocol.Message](t, ws)

	send(t, ws, &protocol.ToolStart{
		ThreadID: msg.ThreadID, TurnID: msg.TurnID,
		CallID: "c9", Tool: "Read", Summary: "/etc/shadow",
	})
	send(t, ws, &protocol.ToolEnd{
		ThreadID: msg.ThreadID, TurnID: msg.TurnID,
		CallID: "c9", OK: false, Error: "permission denied",
	})
	send(t, ws, &protocol.Done{ThreadID: msg.ThreadID, TurnID: msg.TurnID})

	eventually(t, "failure reported", func() bool {
		return contains(srf.steps(), "c9:failed")
	})

	srf.smu.Lock()
	defer srf.smu.Unlock()
	for _, u := range append(append([]surface.StreamUpdate{}, srf.appends...), srf.closes...) {
		for _, s := range u.Steps {
			if s.ID == "c9" && s.Status == surface.StepFailed && s.Output == "permission denied" {
				return
			}
		}
	}
	t.Fatal("the failure reached the surface without its reason")
}

// Thoughts are steps, and narration followed by more work demotes into one:
// the reader gets a run of intermediate steps and only the final response as
// prose.
func TestThoughtsAndNarrationBecomeSteps(t *testing.T) {
	h, srf := newStreamingHarness(t)
	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)

	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U1"}, Addressed: true, Text: "think it over",
	})
	msg := readFrame[*protocol.Message](t, ws)

	send(t, ws, &protocol.Thought{
		ThreadID: msg.ThreadID, TurnID: msg.TurnID,
		Text: "The disk is the likely culprit.\nUsage first, then inodes.",
	})
	send(t, ws, &protocol.TextDelta{ThreadID: msg.ThreadID, TurnID: msg.TurnID, Text: "Let me look."})
	send(t, ws, &protocol.ToolStart{
		ThreadID: msg.ThreadID, TurnID: msg.TurnID,
		CallID: "c1", Tool: "Bash", Summary: "df -h",
	})
	send(t, ws, &protocol.ToolEnd{ThreadID: msg.ThreadID, TurnID: msg.TurnID, CallID: "c1", OK: true})
	send(t, ws, &protocol.TextDelta{ThreadID: msg.ThreadID, TurnID: msg.TurnID, Text: "All clear."})
	send(t, ws, &protocol.Done{ThreadID: msg.ThreadID, TurnID: msg.TurnID})

	eventually(t, "stream closed", func() bool {
		srf.smu.Lock()
		defer srf.smu.Unlock()
		return len(srf.closes) == 1
	})

	got := srf.steps()
	for _, want := range []string{"thought-1:done", "prose-2:done", "c1:done"} {
		if !contains(got, want) {
			t.Fatalf("missing step %q in %v", want, got)
		}
	}
	if text := srf.streamedText(); text != "All clear." {
		t.Fatalf("only the final response should stream as text, got %q", text)
	}

	// The thought's first line titles its step; the rest folds into the detail.
	srf.smu.Lock()
	defer srf.smu.Unlock()
	for _, u := range append(append([]surface.StreamUpdate{}, srf.appends...), srf.closes...) {
		for _, s := range u.Steps {
			if s.ID == "thought-1" && s.Title == "The disk is the likely culprit." &&
				s.Detail == "Usage first, then inodes." {
				return
			}
		}
	}
	t.Fatal("the thought step did not carry its text as title and detail")
}

// Hidden activity suppresses steps, but it must not lose prose: with no step
// to demote onto, narration stays pending and all of it lands as the answer.
func TestHiddenActivityKeepsAllProse(t *testing.T) {
	s := &stream{
		turn:    &turnContext{Activity: config.ActivityHidden},
		steps:   map[string]*surface.Step{},
		changed: map[string]bool{},
	}
	s.AppendText("first. ")
	s.Thought("private reasoning")
	s.StartStep("c1", "Bash", "ls")
	s.EndStep("c1", true, "", 5)
	s.AppendText("second.")

	if len(s.order) != 0 {
		t.Fatalf("hidden activity still produced steps: %v", s.order)
	}
	if got := s.pending.String(); got != "first. second." {
		t.Fatalf("prose was lost under hidden activity: %q", got)
	}
}

// A surface that cannot stream must still get a whole message. The gateway
// decides once, per turn, and never leaves the answer stranded.
func TestStreamRefusedFallsBackToPostAndEdit(t *testing.T) {
	h, srf := newStreamingHarness(t)
	srf.openErr = errors.New("missing_scope")

	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)

	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U1"}, Addressed: true, Text: "still answer me",
	})
	msg := readFrame[*protocol.Message](t, ws)

	send(t, ws, &protocol.ToolStart{
		ThreadID: msg.ThreadID, TurnID: msg.TurnID,
		CallID: "c1", Tool: "Bash", Summary: "uptime",
	})
	send(t, ws, &protocol.TextDelta{
		ThreadID: msg.ThreadID, TurnID: msg.TurnID, Text: "up 8 days.",
	})
	send(t, ws, &protocol.Done{ThreadID: msg.ThreadID, TurnID: msg.TurnID})

	eventually(t, "answer posted the old way", func() bool {
		return strings.Contains(srf.allText(), "up 8 days.")
	})
}

// The nastiest case: Slack accepts the stream, then rejects an append. The
// half-written message is left alone and the answer is posted fresh — the one
// outcome that must never happen is silence.
func TestStreamBreakingMidTurnStillDeliversTheAnswer(t *testing.T) {
	h, srf := newStreamingHarness(t)
	srf.sendErr = errors.New("stopped_by_user")

	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)

	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U1"}, Addressed: true, Text: "answer under failure",
	})
	msg := readFrame[*protocol.Message](t, ws)

	send(t, ws, &protocol.TextDelta{
		ThreadID: msg.ThreadID, TurnID: msg.TurnID, Text: "the answer survives.",
	})
	send(t, ws, &protocol.Done{ThreadID: msg.ThreadID, TurnID: msg.TurnID})

	eventually(t, "answer delivered by fallback", func() bool {
		return strings.Contains(srf.allText(), "the answer survives.")
	})
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
