package gateway

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/avarant/splitscreen/config"
	"github.com/avarant/splitscreen/internal/surface"
)

// stream is a turn's output as the surface sees it.
//
// Two shapes, decided by what the surface can do. A surface implementing
// surface.Streamer gets deltas — prose as it is written, steps as their status
// changes — and renders progress itself. Everything else gets the original
// behaviour: one message, edited in place on a ticker, with a tail of tool
// lines that are stripped when the answer lands.
//
// Consolidating every runner onto one chat app means one rate-limit bucket, so
// the flush interval is a real control in both shapes, not a cosmetic one.
type stream struct {
	gw   *Gateway
	turn *turnContext

	mu   sync.Mutex
	body strings.Builder
	// sent is how much of body has reached the surface. The remainder is the
	// delta a streaming surface appends; the fallback path resends the whole
	// body every time and ignores it.
	sent int
	// pending is prose whose fate is not yet known. Text followed by more work
	// was narration ("let me check the disk…") and demotes into a step; text
	// still pending when the turn closes is the answer and lands in body. The
	// distinction only exists because a streamed message cannot be unsent: once
	// prose reaches the surface as the answer it is the answer forever.
	pending strings.Builder
	// synth numbers gateway-made steps (thoughts, demoted prose), which have no
	// harness call id to use.
	synth int

	order   []string
	steps   map[string]*surface.Step
	changed map[string]bool

	native surface.Stream
	// fellBack latches once a streaming surface refuses or fails. Retrying an
	// unsupported stream every tick would spend the turn's rate budget on
	// errors.
	fellBack bool

	posted bool
	ref    surface.Ref
	dirty  bool
	closed bool

	stop chan struct{}
	done chan struct{}
}

func (g *Gateway) streamFor(turn *turnContext) *stream {
	if v, ok := g.streams.Load(turn.TurnID); ok {
		return v.(*stream)
	}
	s := &stream{
		gw:      g,
		turn:    turn,
		steps:   map[string]*surface.Step{},
		changed: map[string]bool{},
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	actual, loaded := g.streams.LoadOrStore(turn.TurnID, s)
	if loaded {
		return actual.(*stream)
	}
	go s.loop(g.cfg.Load().Gateway.StreamInterval.Duration())
	return s
}

func (s *stream) loop(interval time.Duration) {
	defer close(s.done)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.flush(context.Background())
		}
	}
}

// AppendText adds assistant output. It is held in pending rather than sent,
// because whether it is narration or the answer is decided by what comes next.
func (s *stream) AppendText(text string) {
	if text == "" {
		return
	}
	s.mu.Lock()
	s.pending.WriteString(text)
	s.dirty = true
	s.mu.Unlock()
}

// demoteLocked turns pending prose into a finished step, called under the lock
// whenever new work begins: prose followed by more work was narration, not the
// answer. Callers that suppress steps must not call this — hidden activity
// keeps all prose in pending, so nothing the model said is lost.
func (s *stream) demoteLocked() {
	text := strings.TrimSpace(s.pending.String())
	s.pending.Reset()
	if text == "" {
		return
	}
	s.synth++
	id := "prose-" + strconv.Itoa(s.synth)
	title, detail := splitStepText(text)
	s.order = append(s.order, id)
	s.steps[id] = &surface.Step{ID: id, Title: title, Detail: detail, Status: surface.StepDone}
	s.changed[id] = true
	s.dirty = true
}

// Thought records a piece of the model's reasoning as a finished step.
func (s *stream) Thought(text string) {
	if s.turn.Activity == config.ActivityHidden {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.demoteLocked()
	s.synth++
	id := "thought-" + strconv.Itoa(s.synth)
	title, detail := splitStepText(text)
	s.order = append(s.order, id)
	s.steps[id] = &surface.Step{ID: id, Title: title, Detail: detail, Status: surface.StepDone}
	s.changed[id] = true
	s.dirty = true
}

// splitStepText shapes free text into a step: the first line carries as the
// title, the rest folds into the detail. Both are bounded here because they
// are progress indicators, not the transcript — the full text is in the audit
// log.
func splitStepText(text string) (title, detail string) {
	title, detail, _ = strings.Cut(text, "\n")
	title = truncateLine(title, 160)
	detail = truncateLine(strings.Join(strings.Fields(detail), " "), 200)
	return title, detail
}

// StartStep records a tool call beginning. The id is the harness's call id, so
// the matching StepEnd lands on the same step however many run concurrently.
func (s *stream) StartStep(id, title, detail string) {
	if s.turn.Activity == config.ActivityHidden || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.demoteLocked()
	if _, ok := s.steps[id]; !ok {
		s.order = append(s.order, id)
	}
	s.steps[id] = &surface.Step{
		ID:     id,
		Title:  title,
		Detail: detail,
		Status: surface.StepRunning,
	}
	s.changed[id] = true
	s.dirty = true
}

// EndStep closes a step out. A step that was never started still lands: losing
// a failure because its start was missed is the wrong failure direction.
func (s *stream) EndStep(id string, ok bool, errText string, durationMS int64) {
	if s.turn.Activity == config.ActivityHidden || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, exists := s.steps[id]
	if !exists {
		st = &surface.Step{ID: id, Title: "tool"}
		s.steps[id] = st
		s.order = append(s.order, id)
	}
	if ok {
		st.Status = surface.StepDone
	} else {
		st.Status = surface.StepFailed
		st.Output = errText
	}
	st.Elapsed = time.Duration(durationMS) * time.Millisecond
	s.changed[id] = true
	s.dirty = true
}

// NoteStep records something that is not a tool call but belongs in the run of
// steps anyway — a policy block, say, which is the reason a turn stopped short.
func (s *stream) NoteStep(id, title string, failed bool) {
	if s.turn.Activity == config.ActivityHidden {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.demoteLocked()
	if _, ok := s.steps[id]; !ok {
		s.order = append(s.order, id)
	}
	status := surface.StepDone
	if failed {
		status = surface.StepFailed
	}
	s.steps[id] = &surface.Step{ID: id, Title: title, Status: status}
	s.changed[id] = true
	s.dirty = true
}

// take collects everything not yet sent. Called under the lock.
func (s *stream) take() surface.StreamUpdate {
	u := surface.StreamUpdate{Text: s.body.String()[s.sent:]}
	for _, id := range s.order {
		if s.changed[id] {
			u.Steps = append(u.Steps, *s.steps[id])
		}
	}
	return u
}

// commit marks a successful send. Called under the lock.
func (s *stream) commit() {
	s.sent = s.body.Len()
	clear(s.changed)
}

// render composes the whole message for a surface that cannot stream. On the
// final pass activity lines are dropped unless the runner asked to keep them:
// they exist to show a long turn is alive, which stops being useful the moment
// the answer arrives.
func (s *stream) render(final bool) string {
	var b strings.Builder
	b.WriteString(s.body.String())
	showActivity := !final || s.turn.Activity == config.ActivityFull
	if len(s.order) > 0 && showActivity && s.turn.Activity != config.ActivityHidden {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		// Keep the tail: what a long turn is doing now matters more than what it
		// started with, and the full record is in the audit log regardless. A
		// streaming surface has no such limit — this is a message-length budget,
		// not a policy about how much detail is useful.
		lines := s.order
		if len(lines) > 12 {
			lines = lines[len(lines)-12:]
		}
		for _, id := range lines {
			b.WriteString("\n_")
			b.WriteString(activityLine(s.steps[id]))
			b.WriteString("_")
		}
	}
	return strings.TrimSpace(b.String())
}

func activityLine(st *surface.Step) string {
	line := st.Title
	if st.Detail != "" {
		line += ": " + st.Detail
	}
	if st.Status == surface.StepFailed {
		line = "failed — " + line
		if st.Output != "" {
			line += " (" + st.Output + ")"
		}
	}
	return line
}

func (s *stream) flush(ctx context.Context) { s.flushWith(ctx, false) }

func (s *stream) flushWith(ctx context.Context, final bool) {
	s.mu.Lock()
	if (!s.dirty && !final) || s.closed {
		s.mu.Unlock()
		return
	}
	if final {
		// Nothing else is coming, so whatever prose is pending is the answer.
		s.body.WriteString(s.pending.String())
		s.pending.Reset()
	}
	native, fellBack := s.native, s.fellBack
	// Whether there is anything worth sending: unsent prose (body past sent) or
	// any steps. A turn that produced neither has nothing to show.
	nothingToSend := s.body.Len() <= s.sent && len(s.order) == 0
	s.mu.Unlock()

	// A no-output turn must not open a native stream just to close it empty:
	// OpenStream posts a message and closing it with no chunks leaves a blank
	// bubble in the channel — the "sometimes Clank leaves an empty message" bug.
	// The edit path already self-guards (it posts only non-empty text), and
	// onDone reports "(no output)" for these, so silence is still handled.
	if native == nil && nothingToSend {
		return
	}

	srf, ok := s.gw.surfaceFor(s.turn.Surface)
	if !ok {
		return
	}

	if !fellBack {
		if native == nil {
			native = s.open(ctx, srf)
		}
		if native != nil {
			if s.pushNative(ctx, native, final) {
				return
			}
			// The stream broke mid-turn. Fall through rather than return: on a
			// final flush there is no later tick to recover on, and the answer
			// would be lost.
		}
	}
	s.pushEdit(ctx, srf, final)
}

// open starts a native stream on the first flush that has something to show.
// A surface that cannot stream, or refuses, latches the fallback for the turn.
func (s *stream) open(ctx context.Context, srf surface.Surface) surface.Stream {
	streamer, ok := srf.(surface.Streamer)
	if !ok {
		s.mu.Lock()
		s.fellBack = true
		s.mu.Unlock()
		return nil
	}
	st, err := streamer.OpenStream(ctx, surface.Post{
		Channel: s.turn.Channel,
		Thread:  s.turn.Thread,
		Persona: s.turn.Persona,
		User:    s.turn.User.ID,
	})
	if err != nil {
		if s.gw.streamWarned.CompareAndSwap(false, true) {
			// Once. A workspace without streaming would otherwise log this on
			// every turn for the life of the process.
			s.gw.log.Info("streaming unavailable, falling back to edits",
				"surface", s.turn.Surface, "err", err)
		}
		s.mu.Lock()
		s.fellBack = true
		s.mu.Unlock()
		return nil
	}
	s.mu.Lock()
	s.native = st
	s.ref = st.Ref()
	s.posted = true
	s.mu.Unlock()
	return st
}

// pushNative sends one increment. It reports false when the stream broke and
// the caller should take the fallback path instead.
func (s *stream) pushNative(ctx context.Context, st surface.Stream, final bool) bool {
	s.mu.Lock()
	u := s.take()
	s.dirty = false
	s.mu.Unlock()

	if !final && u.Text == "" && len(u.Steps) == 0 {
		return true
	}

	var err error
	if final {
		err = st.Close(ctx, u)
	} else {
		err = st.Append(ctx, u)
	}
	if err != nil {
		s.gw.log.Warn("stream append failed", "turn", s.turn.TurnID, "final", final, "err", err)
		s.mu.Lock()
		// Nothing was accepted, so nothing is committed and the whole body goes
		// out again through the fallback. The half-streamed message is left
		// where it is and a fresh one is posted: editing a message the
		// streaming API owns is not ours to do, and repeating some prose beats
		// dropping the answer.
		s.dirty = true
		s.fellBack = true
		s.native = nil
		s.posted = false
		s.ref = surface.Ref{}
		s.sent = 0
		s.mu.Unlock()
		return false
	}
	s.mu.Lock()
	s.commit()
	s.mu.Unlock()
	return true
}

func (s *stream) pushEdit(ctx context.Context, srf surface.Surface, final bool) {
	s.mu.Lock()
	text := s.render(final)
	posted, ref := s.posted, s.ref
	s.dirty = false
	s.mu.Unlock()

	if text == "" {
		return
	}

	post := surface.Post{
		Channel: s.turn.Channel,
		Thread:  s.turn.Thread,
		Text:    text,
		Persona: s.turn.Persona,
	}

	if !posted {
		newRef, err := srf.Post(ctx, post)
		if err != nil {
			s.gw.log.Warn("stream post failed", "turn", s.turn.TurnID, "err", err)
			s.mu.Lock()
			s.dirty = true // retry on the next tick rather than losing output
			s.mu.Unlock()
			return
		}
		s.mu.Lock()
		s.posted, s.ref = true, newRef
		s.commit()
		s.mu.Unlock()
		return
	}

	if err := srf.Update(ctx, ref, post); err != nil {
		s.gw.log.Warn("stream update failed", "turn", s.turn.TurnID, "err", err)
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.commit()
	s.mu.Unlock()
}

// Close performs a final flush and stops the ticker.
func (s *stream) Close(ctx context.Context) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = false // allow the final flush through
	s.mu.Unlock()

	close(s.stop)
	<-s.done
	// Final pass: closes the native stream, or strips the activity lines that
	// were only ever there to show the turn was alive.
	s.flushWith(ctx, true)

	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()

	s.gw.streams.Delete(s.turn.TurnID)
}

// HasOutput reports whether anything was rendered, so a turn that produced
// nothing can say so rather than leaving silence.
func (s *stream) HasOutput() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.Len() > 0 || s.pending.Len() > 0 || len(s.order) > 0
}
