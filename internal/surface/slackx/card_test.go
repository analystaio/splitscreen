package slackx

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/analystaio/splitscreen/internal/surface"
)

// fakeCardAPI records what a card asked Slack for.
type fakeCardAPI struct {
	mu        sync.Mutex
	posts     int
	updates   int
	deletes   int
	updateErr []error // consumed one per update
}

func (f *fakeCardAPI) PostMessageContext(context.Context, string, ...slack.MsgOption) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts++
	return "C1", "100.1", nil
}

func (f *fakeCardAPI) UpdateMessageContext(context.Context, string, string, ...slack.MsgOption) (string, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates++
	if len(f.updateErr) > 0 {
		err := f.updateErr[0]
		f.updateErr = f.updateErr[1:]
		return "", "", "", err
	}
	return "C1", "100.1", "", nil
}

func (f *fakeCardAPI) DeleteMessageContext(context.Context, string, string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	return "C1", "100.1", nil
}

func (f *fakeCardAPI) counts() (posts, updates, deletes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.posts, f.updates, f.deletes
}

func newCard(t *testing.T, api *fakeCardAPI) *card {
	t.Helper()
	c, err := openCard(context.Background(), api, surface.Post{Channel: "C1", Thread: "99.9", StopTurnID: "turn_1"})
	if err != nil {
		t.Fatal(err)
	}
	// Let the first rewrite through without waiting out the pacing.
	c.lastPush = time.Now().Add(-time.Minute)
	return c
}

func blockTypes(blocks []slack.Block) []string {
	var out []string
	for _, b := range blocks {
		out = append(out, string(b.BlockType()))
	}
	return out
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A card with nothing to show yet still says the turn is working and offers
// Stop, in the one message.
func TestEmptyCardShowsWorkingAndStop(t *testing.T) {
	c := newCard(t, &fakeCardAPI{})
	blocks := c.blocks(false, c.shownText(), "")
	if got := blockTypes(blocks); len(got) != 2 || got[0] != "context" || got[1] != "actions" {
		t.Fatalf("blocks = %v, want context then actions", got)
	}
	if js := asJSON(t, blocks); !strings.Contains(js, `"value":"turn_1"`) || !strings.Contains(js, stopInlineBlock) {
		t.Fatalf("stop button missing or not marked inline: %s", js)
	}
}

// While running: the latest steps as plain lines, then the draft, then Stop,
// and no plan to fold shut on every rewrite. Finished: the steps under their
// summary, the answer, the footer, and no button.
func TestCardLayoutRunningAndFinished(t *testing.T) {
	c := newCard(t, &fakeCardAPI{})
	c.absorb(surface.StreamUpdate{
		Draft: "Looking at the disk.",
		Steps: []surface.Step{{ID: "c1", Title: "Bash", Detail: "df -h *", Status: surface.StepRunning}},
	})
	running := c.blocks(false, c.shownText(), "")
	if got := blockTypes(running); strings.Join(got, ",") != "context,markdown,actions" {
		t.Fatalf("running blocks = %v", got)
	}
	if js := asJSON(t, running); !strings.Contains(js, `"type":"plain_text","text":"⏳ Bash · df -h *"`) {
		t.Fatalf("running steps = %s", js)
	}

	c.absorb(surface.StreamUpdate{
		Text:  "Disk is fine.",
		Steps: []surface.Step{{ID: "c1", Title: "Bash", Detail: "df -h *", Status: surface.StepDone}},
	})
	final := c.blocks(true, "Disk is fine.", "Stopped by <@U1> after 3s.")
	if got := blockTypes(final); strings.Join(got, ",") != "plan,markdown,context" {
		t.Fatalf("final blocks = %v", got)
	}
	js := asJSON(t, final)
	if !strings.Contains(js, `"title":"1 step`) || strings.Contains(js, "Looking at the disk") || strings.Contains(js, stopAction) {
		t.Fatalf("final card = %s", js)
	}
}

// What the model said before its first step stays text at the top of the
// message, running and finished; what it said between steps is a step.
func TestOpeningProseStaysAboveTheSteps(t *testing.T) {
	c := newCard(t, &fakeCardAPI{})
	c.absorb(surface.StreamUpdate{Steps: []surface.Step{
		{ID: "prose-1", Title: "Checking the disk first.", Status: surface.StepDone, Prose: true},
		{ID: "c1", Title: "Bash", Detail: "df -h", Status: surface.StepDone},
		{ID: "prose-2", Title: "Now the load.", Status: surface.StepDone, Prose: true},
		{ID: "c2", Title: "Bash", Detail: "uptime", Status: surface.StepRunning},
	}})
	running := c.blocks(false, c.shownText(), "")
	if got := blockTypes(running); strings.Join(got, ",") != "markdown,context,actions" {
		t.Fatalf("running blocks = %v", got)
	}
	if js := asJSON(t, running); !strings.Contains(js, `"text":"✓ Bash · df -h\n✓ Now the load.\n⏳ Bash · uptime"`) {
		t.Fatalf("running card = %s", js)
	}
	final := c.blocks(true, "All good.", "")
	if got := blockTypes(final); strings.Join(got, ",") != "markdown,plan,markdown" {
		t.Fatalf("final blocks = %v", got)
	}
	js := asJSON(t, final)
	if !strings.Contains(js, `"title":"3 steps`) || strings.Count(js, "Checking the disk first.") != 1 {
		t.Fatalf("final card = %s", js)
	}
}

// A running card lists only the newest steps, under a count of the rest.
func TestRunningCardShowsOnlyRecentSteps(t *testing.T) {
	c := newCard(t, &fakeCardAPI{})
	var u surface.StreamUpdate
	for i := 0; i < 8; i++ {
		u.Steps = append(u.Steps, surface.Step{ID: "c" + string(rune('A'+i)), Title: "Bash", Detail: "step " + string(rune('A'+i)), Status: surface.StepDone})
	}
	c.absorb(u)
	js := asJSON(t, c.blocks(false, "", ""))
	if !strings.Contains(js, `"text":"3 earlier steps\n✓ Bash · step D\n`) || strings.Contains(js, "step C") || !strings.Contains(js, "step H") {
		t.Fatalf("running card = %s", js)
	}
}

// Once a stop has been asked for, the button gives way to the line saying so.
func TestStoppingReplacesTheButton(t *testing.T) {
	c := newCard(t, &fakeCardAPI{})
	c.absorb(surface.StreamUpdate{Stopping: "Stopping… — requested by <@U1>"})
	blocks := c.blocks(false, c.shownText(), "")
	if js := asJSON(t, blocks); strings.Contains(js, stopAction) || !strings.Contains(js, "Stopping") {
		t.Fatalf("blocks = %s", js)
	}
}

// Slack caps a plan at 50 tasks; the steps past the reserve share one card.
func TestCardPlanOverflow(t *testing.T) {
	c := newCard(t, &fakeCardAPI{})
	var u surface.StreamUpdate
	for i := 0; i < 70; i++ {
		status := surface.StepDone
		if i == 69 {
			status = surface.StepFailed
		}
		u.Steps = append(u.Steps, surface.Step{ID: "c" + string(rune('A'+i)), Title: "Bash", Status: status})
	}
	c.absorb(u)
	plan := c.plan(c.order)
	if len(plan.Tasks) != maxPlanTasks {
		t.Fatalf("plan has %d tasks, want %d", len(plan.Tasks), maxPlanTasks)
	}
	last := plan.Tasks[len(plan.Tasks)-1]
	if last.TaskID != overflowID || last.Title != "…and 21 more steps" || last.Status != slack.TaskCardStatusError {
		t.Fatalf("overflow card = %+v", last)
	}
}

// A finished plan never spins. A step whose end was lost stands at running in
// the card's state; the message is closed and will not be rewritten again, so
// a task card left in progress would spin under a finished answer forever.
func TestFinishedPlanHasNoRunningTask(t *testing.T) {
	c := newCard(t, &fakeCardAPI{})
	c.absorb(surface.StreamUpdate{Steps: []surface.Step{
		{ID: "c1", Title: "Read", Status: surface.StepDone},
		{ID: "c2", Title: "Bash", Status: surface.StepRunning}, // its end never arrived
		{ID: "c3", Title: "Bash", Status: surface.StepFailed},
	}})
	plan := c.plan(c.order)
	for _, task := range plan.Tasks {
		if task.Status == slack.TaskCardStatusInProgress || task.Status == slack.TaskCardStatusPending {
			t.Fatalf("task %s is %q in a finished plan", task.TaskID, task.Status)
		}
	}
	if plan.Tasks[1].Status != slack.TaskCardStatusComplete {
		t.Fatalf("unfinished step rendered %q, want complete", plan.Tasks[1].Status)
	}
	// A real failure still reads as one.
	if plan.Tasks[2].Status != slack.TaskCardStatusError {
		t.Fatalf("failed step rendered %q, want error", plan.Tasks[2].Status)
	}
	if !strings.Contains(plan.Title, "1 failed") {
		t.Fatalf("plan title = %q, want one failure counted", plan.Title)
	}
}

// Updates are paced: one that arrives too soon is carried by a timer rather
// than sent at once or dropped.
func TestCardPacesUpdatesAndCarriesTheLastState(t *testing.T) {
	api := &fakeCardAPI{}
	c := newCard(t, api)
	ctx := context.Background()
	if err := c.Append(ctx, surface.StreamUpdate{Draft: "one"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Append(ctx, surface.StreamUpdate{Draft: "two"}); err != nil {
		t.Fatal(err)
	}
	if _, updates, _ := api.counts(); updates != 1 {
		t.Fatalf("updates = %d right after two appends, want 1", updates)
	}
	deadline := time.Now().Add(3 * cardMinInterval)
	for time.Now().Before(deadline) {
		if _, updates, _ := api.counts(); updates == 2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the delayed state was never sent")
}

// A rate limit or a passing failure does not break the turn's message; only a
// run of failures hands the turn to the fallback.
func TestCardToleratesFailures(t *testing.T) {
	api := &fakeCardAPI{updateErr: []error{&slack.RateLimitedError{RetryAfter: time.Hour}}}
	c := newCard(t, api)
	if err := c.Append(context.Background(), surface.StreamUpdate{Draft: "x"}); err != nil {
		t.Fatalf("rate limit surfaced as a failure: %v", err)
	}
	c.mu.Lock()
	c.timer.Stop()
	c.mu.Unlock()

	api = &fakeCardAPI{updateErr: []error{errors.New("a"), errors.New("b"), errors.New("c")}}
	c = newCard(t, api)
	var err error
	for i := 0; i < cardMaxFailures; i++ {
		c.mu.Lock()
		if c.timer != nil {
			c.timer.Stop()
			c.timer = nil
		}
		c.lastPush = time.Now().Add(-time.Minute)
		c.mu.Unlock()
		err = c.Append(context.Background(), surface.StreamUpdate{Draft: "x"})
	}
	if err == nil {
		t.Fatal("a run of failures did not report the message as broken")
	}
}

// A turn that showed nothing leaves no message behind.
func TestSilentTurnDeletesItsCard(t *testing.T) {
	api := &fakeCardAPI{}
	c := newCard(t, api)
	if err := c.Close(context.Background(), surface.StreamUpdate{}); err != nil {
		t.Fatal(err)
	}
	if _, updates, deletes := api.counts(); deletes != 1 || updates != 0 {
		t.Fatalf("updates = %d, deletes = %d; want the card deleted", updates, deletes)
	}
}

// An answer too long for one message continues in further ones.
func TestLongAnswerContinuesInMoreMessages(t *testing.T) {
	api := &fakeCardAPI{}
	c := newCard(t, api)
	para := strings.Repeat("word ", 400) // 2000 bytes
	text := strings.TrimSpace(strings.Repeat(para+"\n\n", 12))
	if err := c.Close(context.Background(), surface.StreamUpdate{Text: text, Footer: "f"}); err != nil {
		t.Fatal(err)
	}
	posts, updates, _ := api.counts()
	if updates != 1 || posts != 1+2 {
		t.Fatalf("posts = %d, updates = %d; want the card rewritten once and 2 continuations", posts, updates)
	}
}

func TestSplitTextKeepsCodeFencesBalanced(t *testing.T) {
	text := "intro\n\n```\n" + strings.Repeat("line of code\n", 60) + "```\n\noutro"
	parts := splitText(text, 300)
	if len(parts) < 2 {
		t.Fatalf("not split: %d parts", len(parts))
	}
	for i, p := range parts {
		if len(p) > 300+8 {
			t.Fatalf("part %d is %d bytes", i, len(p))
		}
		if strings.Count(p, "```")%2 != 0 {
			t.Fatalf("part %d leaves a code fence open:\n%s", i, p)
		}
	}
	if !strings.Contains(strings.Join(parts, "\n"), "outro") {
		t.Fatal("text lost")
	}
}
