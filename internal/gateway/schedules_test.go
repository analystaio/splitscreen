package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/analystaio/splitscreen/config"
	"github.com/analystaio/splitscreen/internal/store"
	"github.com/analystaio/splitscreen/internal/surface"
	"github.com/analystaio/splitscreen/protocol"
)

// controlSurface adds buttons to the channel-capable test surface.
type controlSurface struct {
	*channelSurface
	mu      sync.Mutex
	cards   []surface.ControlMessage
	edits   []surface.ControlMessage
	editIDs []string
}

func (c *controlSurface) PostControls(_ context.Context, m surface.ControlMessage) (surface.Ref, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cards = append(c.cards, m)
	return surface.Ref{Channel: m.Channel, Thread: m.Thread, ID: "card" + itoa(len(c.cards))}, nil
}

func (c *controlSurface) UpdateControls(_ context.Context, ref surface.Ref, m surface.ControlMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.edits = append(c.edits, m)
	c.editIDs = append(c.editIDs, ref.ID)
	return nil
}

// lastCard is the card as it now reads: the latest edit, or the post.
func (c *controlSurface) lastCard(t *testing.T) surface.ControlMessage {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := len(c.edits); n > 0 {
		return c.edits[n-1]
	}
	if len(c.cards) == 0 {
		t.Fatal("no card posted")
	}
	return c.cards[len(c.cards)-1]
}

func buttonActions(m surface.ControlMessage) []string {
	var out []string
	for _, b := range m.Buttons {
		out = append(out, b.Action)
	}
	return out
}

type schedHarness struct {
	*harness
	ctl   *controlSurface
	clock time.Time
}

func newSchedHarness(t *testing.T) *schedHarness {
	t.Helper()
	h, cs := channelHarness(t)
	ctl := &controlSurface{channelSurface: cs}
	h.gw.surfaces["test"] = ctl
	h.gw.cfg.Load().Gateway.Timezone = "America/Los_Angeles"
	sh := &schedHarness{harness: h, ctl: ctl}
	// Thursday 2026-11-12 10:00 in Los Angeles.
	sh.clock = time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)
	h.gw.sched.now = func() time.Time { return sh.clock }
	return sh
}

func (sh *schedHarness) call(turn *turnContext, tool, args string) (string, error) {
	out, err := sh.gw.callSurfaceTool(context.Background(), "alpha", turn, &protocol.MCPCall{
		Server: "chat", Tool: tool, Args: json.RawMessage(args),
	})
	if err != nil {
		return "", err
	}
	return resultText(sh.t, out), nil
}

func (sh *schedHarness) create(t *testing.T, args string) *store.Schedule {
	t.Helper()
	if _, err := sh.call(memberTurn(), scheduleCreateTool, args); err != nil {
		t.Fatal(err)
	}
	all, err := sh.st.AllSchedules()
	if err != nil || len(all) == 0 {
		t.Fatalf("no schedule saved: %v", err)
	}
	return all[len(all)-1]
}

func (sh *schedHarness) press(action, id, user string) {
	sh.gw.OnControl(context.Background(), surface.Control{
		Surface: "test", Action: action, Value: id,
		User: surface.User{ID: user}, Ref: surface.Ref{Channel: "C1", Thread: "T1", ID: "card1"},
	})
}

func (sh *schedHarness) reload(t *testing.T, id string) *store.Schedule {
	t.Helper()
	sc, err := sh.st.Schedule(id)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// queuedMessages drains the offline queue the runs land in (alpha is never
// connected in these tests).
func (sh *schedHarness) queuedMessages(t *testing.T) []*protocol.Message {
	t.Helper()
	qs, err := sh.st.Dequeue("alpha", 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []*protocol.Message
	for _, q := range qs {
		f, err := protocol.Decode(q.Frame, protocol.DirDown)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, f.(*protocol.Message))
		_ = sh.st.DeleteQueued(q.ID)
	}
	return out
}

const weekdayNine = `{"name":"stale-prs","cron":"0 9 * * 1-5","prompt":"List PRs in acme/widgets open more than 3 days."}`

func TestCreateOnlyProposes(t *testing.T) {
	sh := newSchedHarness(t)
	text, err := sh.call(memberTurn(), scheduleCreateTool, weekdayNine)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "NOT active") || !strings.Contains(text, "Fri Nov 13 09:00") {
		t.Errorf("tool result = %q", text)
	}
	sc := sh.create(t, weekdayNine)
	if sc.State != store.SchedulePending || !sc.NextRunAt.IsZero() {
		t.Fatalf("new schedule = %+v", sc)
	}
	if sc.Timezone != "America/Los_Angeles" || sc.Mode != store.ScheduleModeChannel || sc.Creator != "UMEMBER" {
		t.Errorf("defaults = %+v", sc)
	}

	card := sh.ctl.lastCard(t)
	if card.Channel != "C1" || card.Thread != "T1" {
		t.Errorf("card posted to %s/%s", card.Channel, card.Thread)
	}
	if got := buttonActions(card); strings.Join(got, ",") != "sched_confirm,sched_run,sched_cancel" {
		t.Errorf("pending buttons = %v", got)
	}
	if !strings.Contains(card.Text, "Fri Nov 13 09:00 · Mon Nov 16 09:00") {
		t.Errorf("card does not show upcoming runs:\n%s", card.Text)
	}

	// Long past its first run time, and still nothing happens.
	sh.clock = sh.clock.Add(7 * 24 * time.Hour)
	if n := sh.gw.runDueSchedules(context.Background()); n != 0 {
		t.Fatalf("an unconfirmed schedule fired %d times", n)
	}
}

func TestOnlyTheCreatorConfirms(t *testing.T) {
	sh := newSchedHarness(t)
	sc := sh.create(t, weekdayNine)

	sh.press(actScheduleConfirm, sc.ID, "USOMEONE")
	if got := sh.reload(t, sc.ID); got.State != store.SchedulePending {
		t.Fatalf("someone else confirmed it: %s", got.State)
	}
	if !strings.Contains(sh.ctl.allText(), "Only <@UMEMBER>") {
		t.Errorf("no refusal sent: %s", sh.ctl.allText())
	}

	sh.press(actScheduleConfirm, sc.ID, "UMEMBER")
	got := sh.reload(t, sc.ID)
	if got.State != store.ScheduleActive {
		t.Fatalf("state = %s", got.State)
	}
	want := time.Date(2026, 11, 13, 17, 0, 0, 0, time.UTC) // Fri 09:00 PST
	if !got.NextRunAt.Equal(want) {
		t.Errorf("next = %s, want %s", got.NextRunAt, want)
	}
	if acts := buttonActions(sh.ctl.lastCard(t)); strings.Join(acts, ",") != "sched_pause,sched_run,sched_delete" {
		t.Errorf("active buttons = %v", acts)
	}
}

func TestDueScheduleRunsInANewThreadAsItsCreator(t *testing.T) {
	sh := newSchedHarness(t)
	sc := sh.create(t, weekdayNine)
	sh.press(actScheduleConfirm, sc.ID, "UMEMBER")

	sh.clock = time.Date(2026, 11, 13, 17, 0, 30, 0, time.UTC)
	if n := sh.gw.runDueSchedules(context.Background()); n != 1 {
		t.Fatalf("fired %d", n)
	}

	msgs := sh.queuedMessages(t)
	if len(msgs) != 1 {
		t.Fatalf("dispatched %d messages", len(msgs))
	}
	m := msgs[0]
	if m.User.ID != "UMEMBER" {
		t.Errorf("run acts as %q", m.User.ID)
	}
	if !strings.Contains(m.Text, "List PRs in acme/widgets") || !strings.Contains(m.Text, "Scheduled run of \"stale-prs\"") {
		t.Errorf("run text = %q", m.Text)
	}
	if strings.HasSuffix(m.ThreadID, ":T1") {
		t.Errorf("channel-mode run reused the creation thread: %s", m.ThreadID)
	}

	// The header that roots the new thread was posted top-level.
	var header *surface.Post
	for _, p := range sh.srf.posts {
		if strings.Contains(p.Text, "scheduled run") {
			header = &p
		}
	}
	if header == nil || header.Thread != "" || header.Channel != "C1" {
		t.Fatalf("header = %+v", header)
	}

	got := sh.reload(t, sc.ID)
	if want := time.Date(2026, 11, 16, 17, 0, 0, 0, time.UTC); !got.NextRunAt.Equal(want) {
		t.Errorf("next after Friday = %s, want Monday %s", got.NextRunAt, want)
	}
	if got.LastResult != "running" || got.LastRunAt.IsZero() {
		t.Errorf("last run not recorded: %+v", got)
	}
}

func TestMissedRunsFireOnce(t *testing.T) {
	sh := newSchedHarness(t)
	sc := sh.create(t, `{"name":"hourly","cron":"0 * * * *","prompt":"ping"}`)
	sh.press(actScheduleConfirm, sc.ID, "UMEMBER")

	sh.clock = sh.clock.Add(3*24*time.Hour + 10*time.Minute) // a gateway down for three days
	if n := sh.gw.runDueSchedules(context.Background()); n != 1 {
		t.Fatalf("fired %d, want 1", n)
	}
	if n := sh.gw.runDueSchedules(context.Background()); n != 0 {
		t.Fatalf("fired again on the next tick: %d", n)
	}
	if next := sh.reload(t, sc.ID).NextRunAt; !next.After(sh.clock) {
		t.Errorf("next run %s is not in the future", next)
	}
}

func TestOverlappingRunIsSkipped(t *testing.T) {
	sh := newSchedHarness(t)
	sc := sh.create(t, `{"name":"hourly","cron":"0 * * * *","prompt":"ping"}`)
	sh.press(actScheduleConfirm, sc.ID, "UMEMBER")

	sh.clock = sh.clock.Add(time.Hour)
	if n := sh.gw.runDueSchedules(context.Background()); n != 1 {
		t.Fatalf("first run: fired %d", n)
	}
	// The runner is offline, so the first run is still queued an hour later.
	sh.clock = sh.clock.Add(time.Hour)
	if n := sh.gw.runDueSchedules(context.Background()); n != 0 {
		t.Fatalf("second run started on top of the first")
	}
	if len(sh.queuedMessages(t)) != 1 {
		t.Fatal("expected only the first run queued")
	}
}

func TestThreadModeContinuesTheThread(t *testing.T) {
	sh := newSchedHarness(t)
	sc := sh.create(t, `{"name":"standup","cron":"0 9 * * *","prompt":"Daily standup.","post_to":"thread"}`)
	sh.press(actScheduleConfirm, sc.ID, "UMEMBER")
	sh.clock = sh.clock.Add(24 * time.Hour)
	if n := sh.gw.runDueSchedules(context.Background()); n != 1 {
		t.Fatalf("fired %d", n)
	}
	msgs := sh.queuedMessages(t)
	if len(msgs) != 1 || msgs[0].ThreadID != threadKey("test", "C1", "T1") {
		t.Fatalf("thread-mode run went to %v", msgs)
	}
}

func TestRepeatedFailuresPause(t *testing.T) {
	sh := newSchedHarness(t)
	sc := sh.create(t, weekdayNine)
	sh.press(actScheduleConfirm, sc.ID, "UMEMBER")
	ctx := context.Background()

	sh.gw.recordScheduleResult(ctx, sc.ID, store.TurnError, "boom")
	sh.gw.recordScheduleResult(ctx, sc.ID, store.TurnDone, "")
	sh.gw.recordScheduleResult(ctx, sc.ID, store.TurnError, "boom")
	sh.gw.recordScheduleResult(ctx, sc.ID, store.TurnError, "boom")
	if got := sh.reload(t, sc.ID); got.State != store.ScheduleActive {
		t.Fatalf("paused after a success reset the count: %+v", got)
	}
	sh.gw.recordScheduleResult(ctx, sc.ID, store.TurnError, "boom")
	got := sh.reload(t, sc.ID)
	if got.State != store.SchedulePaused || !got.NextRunAt.IsZero() {
		t.Fatalf("not paused after 3 failures: %+v", got)
	}
	if !strings.Contains(sh.srf.allText(), "I paused schedule **stale-prs**") {
		t.Errorf("creator not told:\n%s", sh.srf.allText())
	}
	if acts := buttonActions(sh.ctl.lastCard(t)); acts[0] != actScheduleResume {
		t.Errorf("paused card buttons = %v", acts)
	}
	sh.press(actScheduleResume, sc.ID, "UMEMBER")
	if got := sh.reload(t, sc.ID); got.State != store.ScheduleActive || got.Failures != 0 {
		t.Errorf("resume = %+v", got)
	}
}

func TestEditingSendsItBackForConfirmation(t *testing.T) {
	sh := newSchedHarness(t)
	sc := sh.create(t, weekdayNine)
	sh.press(actScheduleConfirm, sc.ID, "UMEMBER")

	if _, err := sh.call(memberTurn(), scheduleUpdateTool, `{"id":"`+sc.ID+`","cron":"0 10 * * 1-5"}`); err != nil {
		t.Fatal(err)
	}
	got := sh.reload(t, sc.ID)
	if got.State != store.SchedulePending || got.Cron != "0 10 * * 1-5" {
		t.Fatalf("after edit: %+v", got)
	}
	// And the agent cannot confirm it for them.
	if _, err := sh.call(memberTurn(), scheduleUpdateTool, `{"id":"`+sc.ID+`","state":"active"}`); err == nil {
		t.Fatal("agent activated an unconfirmed schedule")
	}
}

func TestPauseAndResumeByTool(t *testing.T) {
	sh := newSchedHarness(t)
	sc := sh.create(t, weekdayNine)
	sh.press(actScheduleConfirm, sc.ID, "UMEMBER")
	if _, err := sh.call(memberTurn(), scheduleUpdateTool, `{"id":"`+sc.ID+`","state":"paused"}`); err != nil {
		t.Fatal(err)
	}
	if got := sh.reload(t, sc.ID); got.State != store.SchedulePaused {
		t.Fatalf("state = %s", got.State)
	}
	if _, err := sh.call(memberTurn(), scheduleUpdateTool, `{"id":"`+sc.ID+`","state":"active"}`); err != nil {
		t.Fatal(err)
	}
	if got := sh.reload(t, sc.ID); got.State != store.ScheduleActive || got.NextRunAt.IsZero() {
		t.Fatalf("resumed = %+v", got)
	}
}

func TestOthersCannotChangeOrDelete(t *testing.T) {
	sh := newSchedHarness(t)
	sc := sh.create(t, weekdayNine)
	sh.ctl.members["UOTHER"] = true
	other := memberTurn()
	other.User = surface.User{ID: "UOTHER"}
	if _, err := sh.call(other, scheduleDeleteTool, `{"id":"`+sc.ID+`"}`); err == nil {
		t.Fatal("another member deleted someone's schedule")
	}
	sh.press(actScheduleDelete, sc.ID, "UOTHER")
	if _, err := sh.st.Schedule(sc.ID); err != nil {
		t.Fatal("button delete by another member went through")
	}

	// A runner approver may manage it.
	sh.gw.cfg.Load().Runners["alpha"].Policy.Approvers = []string{"UOTHER"}
	if _, err := sh.call(other, scheduleDeleteTool, `{"id":"`+sc.ID+`"}`); err != nil {
		t.Fatalf("approver refused: %v", err)
	}
	if !strings.Contains(sh.ctl.lastCard(t).Text, "deleted by <@UOTHER>") || len(sh.ctl.lastCard(t).Buttons) != 0 {
		t.Errorf("card not retired: %+v", sh.ctl.lastCard(t))
	}
}

func TestCancelRemovesAProposal(t *testing.T) {
	sh := newSchedHarness(t)
	sc := sh.create(t, weekdayNine)
	sh.press(actScheduleCancel, sc.ID, "UMEMBER")
	if _, err := sh.st.Schedule(sc.ID); err == nil {
		t.Fatal("cancelled schedule still exists")
	}
}

func TestRunNowFiresWithoutMovingTheSchedule(t *testing.T) {
	sh := newSchedHarness(t)
	sc := sh.create(t, weekdayNine)
	sh.press(actScheduleRun, sc.ID, "UMEMBER") // trying it before confirming
	msgs := sh.queuedMessages(t)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "pressed Run now") {
		t.Fatalf("run now dispatched %v", msgs)
	}
	if got := sh.reload(t, sc.ID); got.State != store.SchedulePending {
		t.Errorf("run now changed state to %s", got.State)
	}
}

func TestCreateValidation(t *testing.T) {
	sh := newSchedHarness(t)
	for _, bad := range []string{
		`{"name":"x","cron":"*/5 * * * *","prompt":"p"}`,                 // too frequent
		`{"name":"x","cron":"0 9 * * *","prompt":"p","timezone":"Mars"}`, // bad zone
		`{"name":"x","cron":"9am daily","prompt":"p"}`,                   // not cron
		`{"name":"","cron":"0 9 * * *","prompt":"p"}`,                    // no name
		`{"name":"x","cron":"0 9 * * *","prompt":"  "}`,                  // no prompt
		`{"name":"x","cron":"0 9 * * *","prompt":"p","post_to":"#eng"}`,  // other channel
		`{"name":"x","cron":"0 9 * * *","prompt":"p","extra":1}`,         // unknown field
	} {
		if _, err := sh.call(memberTurn(), scheduleCreateTool, bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if all, _ := sh.st.AllSchedules(); len(all) != 0 {
		t.Fatalf("invalid calls saved %d schedules", len(all))
	}
}

func TestCreateRefusals(t *testing.T) {
	sh := newSchedHarness(t)

	guest := memberTurn()
	guest.User = surface.User{ID: "UGUEST"}
	if _, err := sh.call(guest, scheduleCreateTool, weekdayNine); err == nil {
		t.Error("a non-member created a schedule")
	}

	scheduled := memberTurn()
	scheduled.ScheduleID = "sch_x"
	if _, err := sh.call(scheduled, scheduleCreateTool, weekdayNine); err == nil {
		t.Error("a scheduled run created a schedule")
	}

	for i := 0; i < maxSchedulesPerUser; i++ {
		sh.create(t, weekdayNine)
	}
	if _, err := sh.call(memberTurn(), scheduleCreateTool, weekdayNine); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("cap not enforced: %v", err)
	}
}

func TestSurfaceToolDenyRule(t *testing.T) {
	sh := newSchedHarness(t)
	sh.gw.cfg.Load().MCP["chat"] = &config.MCPServer{Kind: config.MCPSurface, Deny: []string{"schedule_*"}}
	if _, err := sh.call(memberTurn(), scheduleCreateTool, weekdayNine); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("deny rule ignored: %v", err)
	}
	if _, err := sh.call(memberTurn(), createChannelTool, `{"name":"ok-channel"}`); err != nil {
		t.Fatalf("deny rule too broad: %v", err)
	}
}

func TestSchedulesCommand(t *testing.T) {
	sh := newSchedHarness(t)
	sc := sh.create(t, weekdayNine)
	sh.press(actScheduleConfirm, sc.ID, "UMEMBER")
	sh.gw.OnMessage(context.Background(), msg("C1", "T9", "UMEMBER", "!schedules", true))
	text := sh.srf.allText()
	if !strings.Contains(text, "**stale-prs**") || !strings.Contains(text, "active") {
		t.Errorf("!schedules output:\n%s", text)
	}
}

func TestDueQueryComparesTimestampsCorrectly(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	next := time.Date(2026, 11, 13, 17, 0, 0, 0, time.UTC)
	if err := st.CreateSchedule(&store.Schedule{
		ID: "s1", Name: "n", Surface: "test", Channel: "C", Thread: "T", Mode: "channel",
		Runner: "r", Creator: "U", Cron: "0 9 * * *", Timezone: "UTC", Prompt: "p",
		State: store.ScheduleActive, NextRunAt: next,
	}); err != nil {
		t.Fatal(err)
	}
	if due, _ := st.DueSchedules(next.Add(-time.Second)); len(due) != 0 {
		t.Error("due a second early")
	}
	// A clock with a fractional second must still see it as due.
	if due, _ := st.DueSchedules(next.Add(123 * time.Millisecond)); len(due) != 1 {
		t.Error("not due 123ms after its time")
	}
}

func TestConnectedRunRecordsItsOutcome(t *testing.T) {
	sh := newSchedHarness(t)
	ws := sh.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)
	sc := sh.create(t, weekdayNine)
	sh.press(actScheduleConfirm, sc.ID, "UMEMBER")

	sh.press(actScheduleRun, sc.ID, "UMEMBER")
	m := readFrame[*protocol.Message](t, ws)
	send(t, ws, &protocol.TextDelta{ThreadID: m.ThreadID, TurnID: m.TurnID, Text: "3 stale PRs"})
	send(t, ws, &protocol.Done{ThreadID: m.ThreadID, TurnID: m.TurnID})
	eventually(t, "run recorded ok", func() bool {
		return sh.reload(t, sc.ID).LastResult == "ok"
	})

	sh.press(actScheduleRun, sc.ID, "UMEMBER")
	m = readFrame[*protocol.Message](t, ws)
	send(t, ws, &protocol.Error{ThreadID: m.ThreadID, TurnID: m.TurnID, Code: "harness_error", Message: "boom"})
	eventually(t, "run recorded failed", func() bool {
		got := sh.reload(t, sc.ID)
		return got.Failures == 1 && strings.Contains(got.LastResult, "boom")
	})
}
