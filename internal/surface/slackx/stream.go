package slackx

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"

	"github.com/avarant/splitscreen/internal/surface"
)

// Slack's streaming methods render a message as it is written: prose types in,
// and task_update chunks become task cards that carry their own status. In
// plan display mode every card is grouped into ONE plan block — a single
// disclosure the reader opens if they care — rather than a timeline of
// interleaved cards, so a fifty-step turn reads as one card and an answer, not
// fifty cards and an answer.
//
// This is why the gateway no longer needs an activity cap or a transient mode:
// both existed to keep a rewritten message short.

// maxPlanTasks is Slack's cap on tasks in a plan block. One slot is reserved
// for the overflow card that absorbs everything past the cap, because a chunk
// that exceeds the cap fails the whole append and would demote the turn to the
// fallback path mid-flight.
const maxPlanTasks = 50

// overflowID is the shared task id every step past the cap collapses into.
const overflowID = "overflow"

// errStreamUnsupported is returned once the workspace has told us streaming is
// not available to this app, so the gateway stops asking.
var errStreamUnsupported = errors.New("slack: streaming not available for this app")

// streamDenied are the API errors that mean "never going to work", as opposed
// to a transient failure worth retrying on the next turn. Anything else leaves
// streaming enabled: a network blip must not permanently downgrade the fleet.
var streamDenied = []string{
	"missing_scope",
	"not_allowed_token_type",
	"method_not_supported_for_channel_type",
	"invalid_arguments",
	"unknown_method",
}

func deniedPermanently(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, code := range streamDenied {
		if strings.Contains(msg, code) {
			return true
		}
	}
	return false
}

// OpenStream starts a native streaming message.
//
// Slack requires the stream to be a reply, and requires a recipient when the
// stream lands in a channel rather than a DM. Both are already known: every
// splitscreen turn is threaded, and the turn carries the user who asked.
func (s *Surface) OpenStream(ctx context.Context, p surface.Post) (surface.Stream, error) {
	if s.noStream.Load() {
		return nil, errStreamUnsupported
	}
	if p.Thread == "" {
		return nil, errors.New("slack: streaming requires a thread")
	}

	opts := []slack.MsgOption{
		slack.MsgOptionTS(p.Thread),
		slack.MsgOptionTaskDisplayMode(slack.TaskDisplayModePlan),
	}
	if p.User != "" {
		opts = append(opts, slack.MsgOptionRecipientUserID(p.User))
	}
	if teamID := s.teamID; teamID != "" {
		opts = append(opts, slack.MsgOptionRecipientTeamID(teamID))
	}
	opts = append(opts, personaOptions(p.Persona)...)

	channel, ts, err := s.api.StartStreamContext(ctx, p.Channel, opts...)
	if err != nil {
		if deniedPermanently(err) {
			// Latched and logged once by the caller: every later turn takes the
			// post-and-edit path without a wasted round trip.
			s.noStream.Store(true)
		}
		return nil, fmt.Errorf("slack: start stream: %w", err)
	}
	return &stream{
		srf:      s,
		ref:      surface.Ref{Channel: channel, Thread: p.Thread, ID: ts},
		persona:  p.Persona,
		opened:   time.Now(),
		slots:    map[string]bool{},
		status:   map[string]surface.StepStatus{},
		overflow: map[string]surface.StepStatus{},
	}, nil
}

type stream struct {
	srf     *Surface
	ref     surface.Ref
	persona surface.Persona
	opened  time.Time

	mu     sync.Mutex
	closed bool
	// slots holds the step ids that own a task card of their own; once
	// maxPlanTasks-1 are taken, new ids collapse into the overflow card.
	slots map[string]bool
	// status is the last status seen per step id, overflow included, for the
	// closing plan title's counts.
	status map[string]surface.StepStatus
	// overflow is the same, for only the steps living in the overflow card,
	// whose displayed status is their aggregate.
	overflow map[string]surface.StepStatus
	// titled records that the plan has been given its provisional title.
	titled bool
}

func (st *stream) Ref() surface.Ref { return st.ref }

func (st *stream) Append(ctx context.Context, u surface.StreamUpdate) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		return errors.New("slack: stream already closed")
	}
	chunks := st.chunksLocked(u)
	if len(chunks) == 0 {
		return nil
	}
	_, _, err := st.srf.api.AppendStreamContext(ctx, st.ref.Channel, st.ref.ID,
		slack.MsgOptionChunks(chunks...))
	if err != nil {
		return fmt.Errorf("slack: append stream: %w", err)
	}
	return nil
}

func (st *stream) Close(ctx context.Context, u surface.StreamUpdate) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		return nil
	}
	st.closed = true

	chunks := st.chunksLocked(u)
	if title := st.finalTitleLocked(); title != "" {
		chunks = append(chunks, slack.NewPlanUpdateChunk(title))
	}
	opts := []slack.MsgOption{}
	if len(chunks) > 0 {
		opts = append(opts, slack.MsgOptionChunks(chunks...))
	}
	_, _, err := st.srf.api.StopStreamContext(ctx, st.ref.Channel, st.ref.ID, opts...)
	if err != nil {
		return fmt.Errorf("slack: stop stream: %w", err)
	}
	return nil
}

// chunksLocked converts one update into the streaming protocol's chunks,
// tracking plan state as it goes. Called under the lock.
//
// Text goes first: the prose that explains a step reads better above it, and
// that is the order the harness produced them in.
func (st *stream) chunksLocked(u surface.StreamUpdate) []slack.StreamChunk {
	var chunks []slack.StreamChunk
	// The streaming API takes markdown, not Slack's mrkdwn dialect, so the
	// CommonMark the harness emits goes through untouched — unlike the
	// post-and-edit path, which has to convert.
	if text := u.Text; text != "" {
		chunks = append(chunks, slack.NewMarkdownTextChunk(text))
	}
	if len(u.Steps) > 0 && !st.titled {
		// The plan needs a title before its first card; the closing one
		// replaces it with the turn's summary.
		st.titled = true
		chunks = append(chunks, slack.NewPlanUpdateChunk("Working…"))
	}
	for _, step := range u.Steps {
		st.status[step.ID] = step.Status
		chunks = append(chunks, st.taskChunkLocked(step))
	}
	return chunks
}

// taskChunkLocked renders one step, giving it a card of its own while the plan
// has room and folding it into the shared overflow card after that.
func (st *stream) taskChunkLocked(step surface.Step) slack.TaskUpdateChunk {
	if !st.slots[step.ID] && len(st.slots) < maxPlanTasks-1 {
		st.slots[step.ID] = true
	}
	if st.slots[step.ID] {
		chunk := slack.NewTaskUpdateChunk(step.ID, truncateTitle(step.Title))
		chunk.Status = taskStatus(step.Status)
		chunk.Details = detailFor(step)
		if step.Output != "" {
			chunk.Output = truncate(step.Output, 2000)
		}
		return chunk
	}

	// Past the cap. The overflow card's title counts what it holds, its detail
	// shows the newest activity, and its status is the aggregate: failed if
	// anything failed, running if anything still is. Full detail is in the
	// audit log; a turn this long is skimmed, not read.
	st.overflow[step.ID] = step.Status
	var running, failed int
	for _, status := range st.overflow {
		switch status {
		case surface.StepRunning:
			running++
		case surface.StepFailed:
			failed++
		}
	}
	chunk := slack.NewTaskUpdateChunk(overflowID,
		fmt.Sprintf("…and %d more steps", len(st.overflow)))
	switch {
	case failed > 0:
		chunk.Status = slack.TaskCardStatusError
	case running > 0:
		chunk.Status = slack.TaskCardStatusInProgress
	default:
		chunk.Status = slack.TaskCardStatusComplete
	}
	detail := step.Title
	if step.Detail != "" {
		detail += ": " + step.Detail
	}
	chunk.Details = truncate(detail, 2000)
	return chunk
}

// finalTitleLocked summarizes the finished plan: step count, failures, and how
// long the turn ran. Empty when no steps were ever shown, since an untitled
// plan that never existed needs no title. Called under the lock.
func (st *stream) finalTitleLocked() string {
	if !st.titled {
		return ""
	}
	var failed int
	for _, status := range st.status {
		if status == surface.StepFailed {
			failed++
		}
	}
	title := fmt.Sprintf("%d steps", len(st.status))
	if len(st.status) == 1 {
		title = "1 step"
	}
	if failed > 0 {
		title += fmt.Sprintf(" · %d failed", failed)
	}
	if took := time.Since(st.opened).Round(time.Second); took > 0 {
		title += " · " + took.String()
	}
	return title
}

func taskStatus(s surface.StepStatus) slack.TaskCardStatus {
	switch s {
	case surface.StepDone:
		return slack.TaskCardStatusComplete
	case surface.StepFailed:
		return slack.TaskCardStatusError
	default:
		return slack.TaskCardStatusInProgress
	}
}

// detailFor puts the arguments and the elapsed time under the title. Duration
// only appears once the step has finished, because a running step's elapsed
// time would be frozen at whatever it was when the chunk was sent.
func detailFor(step surface.Step) string {
	detail := step.Detail
	if step.Status != surface.StepRunning && step.Elapsed >= 100*time.Millisecond {
		took := step.Elapsed.Round(100 * time.Millisecond).String()
		if detail == "" {
			detail = took
		} else {
			detail += "  ·  " + took
		}
	}
	return truncate(detail, 2000)
}

// truncateTitle keeps a card title inside Slack's 256-character limit.
func truncateTitle(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if s == "" {
		return "tool"
	}
	return truncate(s, 250)
}
