package slackx

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/analystaio/splitscreen/internal/surface"
)

func newPlanStream() *stream {
	return &stream{
		opened:   time.Now(),
		slots:    map[string]bool{},
		status:   map[string]surface.StepStatus{},
		overflow: map[string]surface.StepStatus{},
	}
}

func taskChunks(chunks []slack.StreamChunk) []slack.TaskUpdateChunk {
	var out []slack.TaskUpdateChunk
	for _, c := range chunks {
		if t, ok := c.(slack.TaskUpdateChunk); ok {
			out = append(out, t)
		}
	}
	return out
}

func planTitles(chunks []slack.StreamChunk) []string {
	var out []string
	for _, c := range chunks {
		if p, ok := c.(slack.PlanUpdateChunk); ok {
			out = append(out, p.Title)
		}
	}
	return out
}

// The first update carrying a step titles the plan, and a step maps onto one
// card that keeps its id across status changes.
func TestPlanIsTitledOnceAndStepsKeepTheirCard(t *testing.T) {
	st := newPlanStream()

	first := st.chunksLocked(surface.StreamUpdate{Steps: []surface.Step{
		{ID: "c1", Title: "Bash", Detail: "df -h", Status: surface.StepRunning},
	}})
	if got := planTitles(first); len(got) != 1 || got[0] != "Working…" {
		t.Fatalf("expected the provisional plan title, got %v", got)
	}

	second := st.chunksLocked(surface.StreamUpdate{Steps: []surface.Step{
		{ID: "c1", Title: "Bash", Detail: "df -h", Status: surface.StepDone, Elapsed: 250 * time.Millisecond},
	}})
	if got := planTitles(second); len(got) != 0 {
		t.Fatalf("plan was re-titled on a later update: %v", got)
	}
	tasks := taskChunks(second)
	if len(tasks) != 1 || tasks[0].ID != "c1" || tasks[0].Status != slack.TaskCardStatusComplete {
		t.Fatalf("step did not update its own card: %+v", tasks)
	}
}

// Slack caps a plan at 50 tasks and rejects the whole append past it. Steps
// beyond the reserve collapse into one shared card that counts them, shows the
// newest, and aggregates status.
func TestOverflowCollapsesIntoOneCard(t *testing.T) {
	st := newPlanStream()

	var update surface.StreamUpdate
	for i := 0; i < 60; i++ {
		update.Steps = append(update.Steps, surface.Step{
			ID: fmt.Sprintf("c%d", i), Title: "Bash", Detail: fmt.Sprintf("step %d", i),
			Status: surface.StepDone,
		})
	}
	tasks := taskChunks(st.chunksLocked(update))

	ids := map[string]bool{}
	for _, task := range tasks {
		ids[task.ID] = true
	}
	if len(ids) != maxPlanTasks {
		t.Fatalf("expected exactly %d distinct task ids, got %d", maxPlanTasks, len(ids))
	}
	if !ids[overflowID] {
		t.Fatal("no overflow card was emitted")
	}

	last := tasks[len(tasks)-1]
	if last.ID != overflowID {
		t.Fatalf("last chunk should be the overflow card, got %q", last.ID)
	}
	if last.Title != "…and 11 more steps" {
		t.Fatalf("overflow title = %q", last.Title)
	}
	if !strings.Contains(last.Details, "step 59") {
		t.Fatalf("overflow detail should show the newest step, got %q", last.Details)
	}
	if last.Status != slack.TaskCardStatusComplete {
		t.Fatalf("all-done overflow should read complete, got %q", last.Status)
	}

	// A failure inside the overflow surfaces on the shared card.
	failed := taskChunks(st.chunksLocked(surface.StreamUpdate{Steps: []surface.Step{
		{ID: "c99", Title: "Bash", Status: surface.StepFailed, Output: "boom"},
	}}))
	if len(failed) != 1 || failed[0].ID != overflowID || failed[0].Status != slack.TaskCardStatusError {
		t.Fatalf("overflow did not aggregate the failure: %+v", failed)
	}
}

// The closing title summarizes the plan; a turn that never showed a step
// leaves the plan untitled because no plan was ever rendered.
func TestFinalPlanTitle(t *testing.T) {
	st := newPlanStream()
	if got := st.finalTitleLocked(); got != "" {
		t.Fatalf("a stepless turn produced a plan title: %q", got)
	}

	st.chunksLocked(surface.StreamUpdate{Steps: []surface.Step{
		{ID: "c1", Title: "Bash", Status: surface.StepDone},
		{ID: "c2", Title: "Read", Status: surface.StepFailed},
	}})
	got := st.finalTitleLocked()
	if !strings.HasPrefix(got, "2 steps · 1 failed") {
		t.Fatalf("final title = %q", got)
	}
}

// Prose rides through untouched as a markdown chunk: the streaming API takes
// CommonMark, not mrkdwn.
func TestTextBecomesAMarkdownChunk(t *testing.T) {
	st := newPlanStream()
	chunks := st.chunksLocked(surface.StreamUpdate{Text: "**done**"})
	if len(chunks) != 1 {
		t.Fatalf("expected one chunk, got %d", len(chunks))
	}
	md, ok := chunks[0].(slack.MarkdownTextChunk)
	if !ok || md.Text != "**done**" {
		t.Fatalf("text did not become a markdown chunk: %+v", chunks[0])
	}
}
