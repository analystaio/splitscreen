package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/analystaio/splitscreen/internal/cron"
	"github.com/analystaio/splitscreen/internal/store"
	"github.com/analystaio/splitscreen/internal/surface"
)

// Schedules run a prompt on a cron expression.
//
// A person asks for one in plain language; the agent turns that into a cron
// expression and calls schedule_create. That only proposes it: the gateway
// posts a card with the concrete next run times, and nothing runs until the
// person who asked presses Confirm. The card stays as the schedule's controls.
//
// A run is a message nobody typed. The gateway builds an Inbound on the
// creator's behalf and sends it down the ordinary path — thread serialization,
// the runner's concurrency cap, the offline queue, waking a stopped host — so
// scheduled work gets every guarantee a typed message does and runners need no
// change.

const (
	scheduleCreateTool = "schedule_create"
	scheduleListTool   = "schedule_list"
	scheduleUpdateTool = "schedule_update"
	scheduleDeleteTool = "schedule_delete"

	// maxSchedulesPerUser counts every state, pending included: an unconfirmed
	// proposal is still something to clean up.
	maxSchedulesPerUser = 10
	// minScheduleInterval keeps a schedule from becoming a poll loop.
	minScheduleInterval = 15 * time.Minute
	// maxScheduleFailures in a row pauses a schedule, so a broken one stops
	// spending and says so instead of failing forever.
	maxScheduleFailures = 3
	scheduleTick        = 30 * time.Second

	maxScheduleName   = 80
	maxSchedulePrompt = 4000
)

// Control actions on a schedule card.
const (
	actScheduleConfirm = "sched_confirm"
	actScheduleCancel  = "sched_cancel"
	actSchedulePause   = "sched_pause"
	actScheduleResume  = "sched_resume"
	actScheduleRun     = "sched_run"
	actScheduleDelete  = "sched_delete"
)

// scheduleState is the gateway's in-memory side of scheduling.
type scheduleState struct {
	// mu serializes every read-modify-write of a schedule row: the ticker, a
	// button press and an agent tool can all touch one at once.
	mu sync.Mutex
	// lastRun is the thread key of each schedule's most recent run, so a run
	// still going (or parked behind one) makes the next firing skip rather than
	// pile up. In memory only: a gateway restart fails every open turn anyway.
	lastRun map[string]string
	now     func() time.Time
}

func scheduleToolDefs(defaultTZ string) string {
	return `{
  "name":"schedule_create",
  "description":"Propose a recurring scheduled run of a prompt, from what the person asked for in plain language. This does not start it: a card with the next run times is posted in this thread and the schedule only runs once the person who asked presses Confirm, so tell them to check the card and confirm. Each run is a new message to you, sent on their behalf with no memory of this conversation, so the prompt must stand on its own: name repos, channels, people and criteria explicitly instead of saying 'those' or 'the same as before'. Runs at most every 15 minutes.",
  "inputSchema":{"type":"object","required":["name","cron","prompt"],"additionalProperties":false,"properties":{
    "name":{"type":"string","maxLength":80,"description":"Short label, e.g. 'stale-prs' or 'Monday standup'."},
    "cron":{"type":"string","description":"Standard 5-field cron: minute hour day-of-month month day-of-week. Lists, ranges, steps and jan-dec / sun-sat names are allowed; no seconds field and no @daily. Example: '0 9 * * 1-5' is 09:00 on weekdays."},
    "timezone":{"type":"string","description":"IANA zone the cron is read in, e.g. 'America/Los_Angeles'. Defaults to ` + defaultTZ + `. Use the person's zone when you know it."},
    "prompt":{"type":"string","maxLength":4000,"description":"The complete, self-contained instructions for each run."},
    "post_to":{"type":"string","enum":["channel","thread"],"description":"'channel' (default): each run starts a new thread in this channel on a fresh session. 'thread': every run posts in this thread and continues this conversation."}
  }}
},{
  "name":"schedule_list",
  "description":"List the schedules the person asking created, and those in this channel, with their ids, state and next run.",
  "inputSchema":{"type":"object","additionalProperties":false,"properties":{}}
},{
  "name":"schedule_update",
  "description":"Change, pause or resume a schedule. Only its creator (or a runner approver) can. Changing name, cron, timezone, prompt or post_to sends it back for the creator to confirm on its card before it runs again. state 'paused' stops runs; 'active' resumes a paused schedule that was confirmed before.",
  "inputSchema":{"type":"object","required":["id"],"additionalProperties":false,"properties":{
    "id":{"type":"string"},
    "name":{"type":"string","maxLength":80},
    "cron":{"type":"string"},
    "timezone":{"type":"string"},
    "prompt":{"type":"string","maxLength":4000},
    "post_to":{"type":"string","enum":["channel","thread"]},
    "state":{"type":"string","enum":["active","paused"]}
  }}
},{
  "name":"schedule_delete",
  "description":"Delete a schedule for good. Only its creator (or a runner approver) can. Prefer schedule_update with state 'paused' if they may want it back.",
  "inputSchema":{"type":"object","required":["id"],"additionalProperties":false,"properties":{
    "id":{"type":"string"}
  }}
}`
}

func (g *Gateway) scheduleNow() time.Time {
	if g.sched.now != nil {
		return g.sched.now()
	}
	return time.Now()
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

// parsedTiming is a validated cron expression in its zone.
type parsedTiming struct {
	spec *cron.Spec
	loc  *time.Location
}

func (g *Gateway) parseTiming(expr, tz string) (parsedTiming, error) {
	if tz == "" {
		tz = g.cfg.Load().Gateway.Timezone
	}
	if tz == "" || tz == "Local" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return parsedTiming{}, fmt.Errorf("%q is not an IANA time zone such as America/Los_Angeles", tz)
	}
	spec, err := cron.Parse(expr)
	if err != nil {
		return parsedTiming{}, err
	}
	if err := spec.CheckInterval(g.scheduleNow().In(loc), minScheduleInterval); err != nil {
		return parsedTiming{}, err
	}
	return parsedTiming{spec: spec, loc: loc}, nil
}

func (p parsedTiming) next(after time.Time) time.Time { return p.spec.Next(after.In(p.loc)) }

func (g *Gateway) timingOf(sc *store.Schedule) (parsedTiming, error) {
	loc, err := time.LoadLocation(sc.Timezone)
	if err != nil {
		return parsedTiming{}, err
	}
	spec, err := cron.Parse(sc.Cron)
	if err != nil {
		return parsedTiming{}, err
	}
	return parsedTiming{spec: spec, loc: loc}, nil
}

func checkName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxScheduleName || strings.ContainsAny(name, "\n\r") {
		return "", fmt.Errorf("name must be one line of 1-%d characters", maxScheduleName)
	}
	return name, nil
}

func checkPrompt(prompt string) (string, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" || utf8.RuneCountInString(prompt) > maxSchedulePrompt {
		return "", fmt.Errorf("prompt must be 1-%d characters", maxSchedulePrompt)
	}
	return prompt, nil
}

func checkMode(postTo string) (string, error) {
	switch postTo {
	case "", store.ScheduleModeChannel:
		return store.ScheduleModeChannel, nil
	case store.ScheduleModeThread:
		return store.ScheduleModeThread, nil
	}
	return "", fmt.Errorf("post_to must be %q or %q", store.ScheduleModeChannel, store.ScheduleModeThread)
}

func decodeArgs(tool string, raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: bad arguments: %w", tool, err)
	}
	return nil
}

// scheduleCaller checks the common preconditions of every schedule tool and
// returns the surface to act on.
func (g *Gateway) scheduleCaller(ctx context.Context, tool, runner string, turn *turnContext) (surface.Controls, error) {
	if turn == nil || turn.Runner != runner {
		return nil, fmt.Errorf("%s needs a running turn: ask from a chat thread", tool)
	}
	// A schedule acts as its creator with nobody watching. Letting one run
	// manage schedules would let it reproduce, or undo what a person set.
	if turn.ScheduleID != "" {
		return nil, fmt.Errorf("%s: a scheduled run cannot manage schedules", tool)
	}
	srf := g.surfaces[turn.Surface]
	ctl, ok := srf.(surface.Controls)
	if !ok {
		return nil, fmt.Errorf("%s: surface %q cannot post the buttons a schedule needs", tool, turn.Surface)
	}
	// As for create_channel: recurring work in the workspace is a member's call,
	// whatever a route admits.
	v, ok := srf.(surface.AudienceVerifier)
	if !ok || !v.SenderIsMember(ctx, turn.User.ID) {
		return nil, fmt.Errorf("%s: only a verified full member can manage schedules", tool)
	}
	return ctl, nil
}

// canManage reports whether a person may change a schedule: its creator, or an
// approver of its runner when the runner names approvers.
func (g *Gateway) canManage(sc *store.Schedule, userID string) bool {
	if userID == sc.Creator {
		return true
	}
	rc, ok := g.cfg.Load().Runners[sc.Runner]
	return ok && len(rc.Policy.Approvers) > 0 && slices.Contains(rc.Policy.Approvers, userID)
}

// ---------------------------------------------------------------------------
// Tools
// ---------------------------------------------------------------------------

type scheduleCreateArgs struct {
	Name     string `json:"name"`
	Cron     string `json:"cron"`
	Timezone string `json:"timezone"`
	Prompt   string `json:"prompt"`
	PostTo   string `json:"post_to"`
}

func (g *Gateway) scheduleCreate(ctx context.Context, runner string, turn *turnContext, raw json.RawMessage) (json.RawMessage, error) {
	ctl, err := g.scheduleCaller(ctx, scheduleCreateTool, runner, turn)
	if err != nil {
		return nil, err
	}
	var args scheduleCreateArgs
	if err := decodeArgs(scheduleCreateTool, raw, &args); err != nil {
		return nil, err
	}
	name, err := checkName(args.Name)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", scheduleCreateTool, err)
	}
	prompt, err := checkPrompt(args.Prompt)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", scheduleCreateTool, err)
	}
	mode, err := checkMode(args.PostTo)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", scheduleCreateTool, err)
	}
	timing, err := g.parseTiming(args.Cron, args.Timezone)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", scheduleCreateTool, err)
	}

	g.sched.mu.Lock()
	defer g.sched.mu.Unlock()
	n, err := g.store.CountSchedulesBy(turn.Surface, turn.User.ID)
	if err != nil {
		return nil, err
	}
	if n >= maxSchedulesPerUser {
		return nil, fmt.Errorf("%s: %s already has %d schedules, the limit; delete one first (schedule_list shows them)",
			scheduleCreateTool, turn.User.ID, n)
	}

	sc := &store.Schedule{
		ID:             newID("sch"),
		Name:           name,
		Surface:        turn.Surface,
		Channel:        turn.Channel,
		Thread:         turn.Thread,
		IsDM:           turn.IsDM,
		Mode:           mode,
		Runner:         runner,
		Creator:        turn.User.ID,
		CreatorDisplay: turn.User.Display,
		Cron:           timing.spec.String(),
		Timezone:       timing.loc.String(),
		Prompt:         prompt,
		State:          store.SchedulePending,
	}
	ref, err := ctl.PostControls(ctx, g.scheduleCard(sc, ""))
	if err != nil {
		return nil, fmt.Errorf("%s: could not post the confirmation card: %w", scheduleCreateTool, err)
	}
	sc.CardID = ref.ID
	if err := g.store.CreateSchedule(sc); err != nil {
		g.retireCard(ctx, sc, "Could not save this schedule: "+err.Error())
		return nil, err
	}
	g.auditSchedule("schedule.proposed", sc, turn.User.ID, nil)

	upcoming := formatUpcoming(timing.spec.Upcoming(g.scheduleNow().In(timing.loc), 3))
	return toolText(fmt.Sprintf(
		"Proposed schedule %s (%q): `%s` %s, next %s. It is NOT active yet: a card was posted in this thread, and nothing runs until %s presses Confirm on it. Tell them that, and that the card also has Run now to try it once.",
		sc.ID, sc.Name, sc.Cron, sc.Timezone, upcoming, mention(sc.Creator))), nil
}

func (g *Gateway) scheduleList(ctx context.Context, runner string, turn *turnContext) (json.RawMessage, error) {
	if _, err := g.scheduleCaller(ctx, scheduleListTool, runner, turn); err != nil {
		return nil, err
	}
	list, err := g.store.SchedulesFor(turn.Surface, turn.User.ID, turn.Channel)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return toolText("No schedules here, and none created by the person asking."), nil
	}
	var b strings.Builder
	for _, sc := range list {
		fmt.Fprintf(&b, "- %s %q: %s, `%s` %s, posts to %s, created by %s in %s",
			sc.ID, sc.Name, sc.State, sc.Cron, sc.Timezone, sc.Mode, sc.Creator, sc.Channel)
		if !sc.NextRunAt.IsZero() {
			fmt.Fprintf(&b, ", next %s", formatWhen(sc.NextRunAt, sc.Timezone))
		}
		if sc.LastResult != "" {
			fmt.Fprintf(&b, ", last run %s", sc.LastResult)
		}
		fmt.Fprintf(&b, "\n  prompt: %s\n", truncateLine(sc.Prompt, 300))
	}
	return toolText(b.String()), nil
}

type scheduleUpdateArgs struct {
	ID       string  `json:"id"`
	Name     *string `json:"name"`
	Cron     *string `json:"cron"`
	Timezone *string `json:"timezone"`
	Prompt   *string `json:"prompt"`
	PostTo   *string `json:"post_to"`
	State    *string `json:"state"`
}

func (g *Gateway) scheduleUpdate(ctx context.Context, runner string, turn *turnContext, raw json.RawMessage) (json.RawMessage, error) {
	if _, err := g.scheduleCaller(ctx, scheduleUpdateTool, runner, turn); err != nil {
		return nil, err
	}
	var args scheduleUpdateArgs
	if err := decodeArgs(scheduleUpdateTool, raw, &args); err != nil {
		return nil, err
	}

	g.sched.mu.Lock()
	defer g.sched.mu.Unlock()
	sc, err := g.managedSchedule(scheduleUpdateTool, args.ID, turn)
	if err != nil {
		return nil, err
	}

	edited := false
	if args.Name != nil {
		if sc.Name, err = checkName(*args.Name); err != nil {
			return nil, fmt.Errorf("%s: %w", scheduleUpdateTool, err)
		}
		edited = true
	}
	if args.Prompt != nil {
		if sc.Prompt, err = checkPrompt(*args.Prompt); err != nil {
			return nil, fmt.Errorf("%s: %w", scheduleUpdateTool, err)
		}
		edited = true
	}
	if args.PostTo != nil {
		if sc.Mode, err = checkMode(*args.PostTo); err != nil {
			return nil, fmt.Errorf("%s: %w", scheduleUpdateTool, err)
		}
		edited = true
	}
	if args.Cron != nil || args.Timezone != nil {
		expr, tz := sc.Cron, sc.Timezone
		if args.Cron != nil {
			expr = *args.Cron
		}
		if args.Timezone != nil {
			tz = *args.Timezone
		}
		timing, err := g.parseTiming(expr, tz)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", scheduleUpdateTool, err)
		}
		sc.Cron, sc.Timezone = timing.spec.String(), timing.loc.String()
		edited = true
	}

	var result string
	switch {
	case edited:
		// What runs unattended changed, so the person it acts for looks again.
		sc.State = store.SchedulePending
		sc.NextRunAt = time.Time{}
		result = fmt.Sprintf("Updated %s. It is paused until %s presses Confirm on its card again; tell them.", sc.ID, mention(sc.Creator))
	case args.State == nil:
		return nil, fmt.Errorf("%s: nothing to change", scheduleUpdateTool)
	case *args.State == store.SchedulePaused:
		if sc.State == store.SchedulePending {
			return nil, fmt.Errorf("%s: %s is not confirmed yet, so it is not running; delete it instead", scheduleUpdateTool, sc.ID)
		}
		sc.State = store.SchedulePaused
		sc.NextRunAt = time.Time{}
		result = "Paused " + sc.ID + "."
	case *args.State == store.ScheduleActive:
		if sc.State == store.SchedulePending {
			return nil, fmt.Errorf("%s: %s has never been confirmed; only %s can, with the Confirm button on its card",
				scheduleUpdateTool, sc.ID, mention(sc.Creator))
		}
		if err := g.activate(sc); err != nil {
			return nil, err
		}
		result = fmt.Sprintf("Resumed %s; next run %s.", sc.ID, formatWhen(sc.NextRunAt, sc.Timezone))
	default:
		return nil, fmt.Errorf("%s: state must be %q or %q", scheduleUpdateTool, store.ScheduleActive, store.SchedulePaused)
	}

	if err := g.store.UpdateSchedule(sc); err != nil {
		return nil, err
	}
	g.auditSchedule("schedule.updated", sc, turn.User.ID, map[string]any{"edited": edited})
	g.refreshCard(ctx, sc, "")
	return toolText(result), nil
}

type scheduleIDArgs struct {
	ID string `json:"id"`
}

func (g *Gateway) scheduleDelete(ctx context.Context, runner string, turn *turnContext, raw json.RawMessage) (json.RawMessage, error) {
	if _, err := g.scheduleCaller(ctx, scheduleDeleteTool, runner, turn); err != nil {
		return nil, err
	}
	var args scheduleIDArgs
	if err := decodeArgs(scheduleDeleteTool, raw, &args); err != nil {
		return nil, err
	}
	g.sched.mu.Lock()
	defer g.sched.mu.Unlock()
	sc, err := g.managedSchedule(scheduleDeleteTool, args.ID, turn)
	if err != nil {
		return nil, err
	}
	if err := g.store.DeleteSchedule(sc.ID); err != nil {
		return nil, err
	}
	g.auditSchedule("schedule.deleted", sc, turn.User.ID, nil)
	g.retireCard(ctx, sc, fmt.Sprintf(":wastebasket: Schedule **%s** was deleted by %s.", sc.Name, mention(turn.User.ID)))
	return toolText("Deleted " + sc.ID + "."), nil
}

// managedSchedule loads a schedule the turn's person may change.
func (g *Gateway) managedSchedule(tool, id string, turn *turnContext) (*store.Schedule, error) {
	sc, err := g.store.Schedule(id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && sc.Surface != turn.Surface) {
		return nil, fmt.Errorf("%s: no schedule %q", tool, id)
	}
	if err != nil {
		return nil, err
	}
	if !g.canManage(sc, turn.User.ID) {
		return nil, fmt.Errorf("%s: only %s, who created %s, can change it", tool, mention(sc.Creator), sc.ID)
	}
	return sc, nil
}

// activate makes a schedule active from now: runs it missed while inactive are
// not owed.
func (g *Gateway) activate(sc *store.Schedule) error {
	timing, err := g.timingOf(sc)
	if err != nil {
		return fmt.Errorf("schedule %s: %w", sc.ID, err)
	}
	sc.State = store.ScheduleActive
	sc.Failures = 0
	sc.NextRunAt = timing.next(g.scheduleNow())
	return nil
}

// ---------------------------------------------------------------------------
// Cards
// ---------------------------------------------------------------------------

func mention(userID string) string { return "<@" + userID + ">" }

func formatWhen(t time.Time, tz string) string {
	if loc, err := time.LoadLocation(tz); err == nil {
		t = t.In(loc)
	}
	return t.Format("Mon Jan 2 15:04")
}

func formatUpcoming(ts []time.Time) string {
	parts := make([]string, len(ts))
	for i, t := range ts {
		parts[i] = t.Format("Mon Jan 2 15:04")
	}
	return strings.Join(parts, " · ")
}

// scheduleCard renders a schedule's card. note is an extra line, such as why
// it was paused.
func (g *Gateway) scheduleCard(sc *store.Schedule, note string) surface.ControlMessage {
	var b strings.Builder
	fmt.Fprintf(&b, ":alarm_clock: **Schedule: %s** · `%s`\n", sc.Name, sc.ID)

	var buttons []surface.Button
	btn := func(label, action string, style surface.ButtonStyle) {
		buttons = append(buttons, surface.Button{Label: label, Action: action, Value: sc.ID, Style: style})
	}
	switch sc.State {
	case store.SchedulePending:
		fmt.Fprintf(&b, "**Status:** waiting for %s to confirm. Nothing runs until then.\n", mention(sc.Creator))
		btn("Confirm", actScheduleConfirm, surface.ButtonPrimary)
		btn("Run now", actScheduleRun, surface.ButtonDefault)
		btn("Cancel", actScheduleCancel, surface.ButtonDanger)
	case store.ScheduleActive:
		b.WriteString("**Status:** active\n")
		btn("Pause", actSchedulePause, surface.ButtonDefault)
		btn("Run now", actScheduleRun, surface.ButtonDefault)
		btn("Delete", actScheduleDelete, surface.ButtonDanger)
	case store.SchedulePaused:
		b.WriteString("**Status:** paused\n")
		btn("Resume", actScheduleResume, surface.ButtonPrimary)
		btn("Run now", actScheduleRun, surface.ButtonDefault)
		btn("Delete", actScheduleDelete, surface.ButtonDanger)
	}
	if note != "" {
		b.WriteString(note + "\n")
	}

	fmt.Fprintf(&b, "**When:** `%s` (%s)\n", sc.Cron, sc.Timezone)
	if timing, err := g.timingOf(sc); err == nil {
		label := "Next"
		if sc.State != store.ScheduleActive {
			label = "Would run"
		}
		fmt.Fprintf(&b, "**%s:** %s\n", label, formatUpcoming(timing.spec.Upcoming(g.scheduleNow().In(timing.loc), 3)))
	}
	if sc.Mode == store.ScheduleModeThread {
		b.WriteString("**Posts:** in this thread, continuing this conversation\n")
	} else {
		where := "this channel"
		if sc.IsDM {
			where = "this DM"
		}
		fmt.Fprintf(&b, "**Posts:** a new thread in %s for each run, on a fresh session\n", where)
	}
	fmt.Fprintf(&b, "**Runs as:** %s on `%s`\n", mention(sc.Creator), sc.Runner)
	if !sc.LastRunAt.IsZero() {
		fmt.Fprintf(&b, "**Last run:** %s", formatWhen(sc.LastRunAt, sc.Timezone))
		if sc.LastResult != "" {
			b.WriteString(" — " + sc.LastResult)
		}
		b.WriteString("\n")
	}
	for _, line := range strings.Split(excerpt(sc.Prompt, 600, 12), "\n") {
		b.WriteString("> " + line + "\n")
	}

	return surface.ControlMessage{
		Channel: sc.Channel, Thread: sc.Thread, Persona: g.schedulePersona(sc),
		Text: strings.TrimRight(b.String(), "\n"), Buttons: buttons,
	}
}

// excerpt shortens multi-line text to at most n runes and maxLines lines.
func excerpt(s string, n, maxLines int) string {
	s = strings.TrimSpace(s)
	cut := false
	if lines := strings.Split(s, "\n"); len(lines) > maxLines {
		s, cut = strings.Join(lines[:maxLines], "\n"), true
	}
	if r := []rune(s); len(r) > n {
		s, cut = string(r[:n]), true
	}
	if cut {
		s += "…"
	}
	return s
}

func (g *Gateway) schedulePersona(sc *store.Schedule) surface.Persona {
	rc, _ := g.runnerConfig(sc.Runner)
	return personaFor(rc)
}

func (g *Gateway) cardRef(sc *store.Schedule) surface.Ref {
	return surface.Ref{Channel: sc.Channel, Thread: sc.Thread, ID: sc.CardID}
}

// refreshCard re-renders a schedule's card from its current row.
func (g *Gateway) refreshCard(ctx context.Context, sc *store.Schedule, note string) {
	ctl, ok := g.surfaces[sc.Surface].(surface.Controls)
	if !ok || sc.CardID == "" {
		return
	}
	if err := ctl.UpdateControls(ctx, g.cardRef(sc), g.scheduleCard(sc, note)); err != nil {
		g.log.Warn("schedule card update failed", "schedule", sc.ID, "err", err)
	}
}

// retireCard replaces a card with a line of text and no buttons.
func (g *Gateway) retireCard(ctx context.Context, sc *store.Schedule, text string) {
	ctl, ok := g.surfaces[sc.Surface].(surface.Controls)
	if !ok || sc.CardID == "" {
		return
	}
	if err := ctl.UpdateControls(ctx, g.cardRef(sc), surface.ControlMessage{
		Channel: sc.Channel, Thread: sc.Thread, Persona: g.schedulePersona(sc), Text: text,
	}); err != nil {
		g.log.Warn("schedule card retire failed", "schedule", sc.ID, "err", err)
	}
}

func (g *Gateway) auditSchedule(kind string, sc *store.Schedule, by string, extra map[string]any) {
	detail := map[string]any{
		"schedule": sc.ID, "name": sc.Name, "cron": sc.Cron, "timezone": sc.Timezone,
		"channel": sc.Channel, "state": sc.State, "creator": sc.Creator,
	}
	for k, v := range extra {
		detail[k] = v
	}
	if err := g.store.Log(store.Event{
		Kind: kind, Runner: sc.Runner, SurfaceUser: by, Detail: detail,
	}); err != nil {
		g.log.Error("audit write failed", "err", err)
	}
}

// ---------------------------------------------------------------------------
// Buttons
// ---------------------------------------------------------------------------

// OnControl implements surface.ControlHandler.
func (g *Gateway) OnControl(ctx context.Context, c surface.Control) {
	if !strings.HasPrefix(c.Action, "sched_") {
		return
	}
	reply := func(text string) {
		if srf, ok := g.surfaceFor(c.Surface); ok {
			_, _ = srf.Post(ctx, surface.Post{
				Channel: c.Ref.Channel, Thread: c.Ref.Thread, Text: text, Ephemeral: true, User: c.User.ID,
			})
		}
	}

	g.sched.mu.Lock()
	sc, err := g.store.Schedule(c.Value)
	if err != nil || sc.Surface != c.Surface {
		g.sched.mu.Unlock()
		reply("That schedule no longer exists.")
		return
	}
	// Confirming and cancelling are the creator's alone: confirming is them
	// agreeing to work done in their name.
	creatorOnly := c.Action == actScheduleConfirm || c.Action == actScheduleCancel
	if (creatorOnly && c.User.ID != sc.Creator) || (!creatorOnly && !g.canManage(sc, c.User.ID)) {
		g.sched.mu.Unlock()
		reply(fmt.Sprintf("Only %s, who asked for this schedule, can do that.", mention(sc.Creator)))
		return
	}

	switch c.Action {
	case actScheduleConfirm, actScheduleResume:
		if c.Action == actScheduleConfirm && sc.State != store.SchedulePending {
			break // a second click on a card that has not refreshed yet
		}
		if err := g.activate(sc); err != nil {
			g.sched.mu.Unlock()
			reply("Could not start it: " + err.Error())
			return
		}
	case actSchedulePause:
		if sc.State == store.ScheduleActive {
			sc.State = store.SchedulePaused
			sc.NextRunAt = time.Time{}
		}
	case actScheduleCancel, actScheduleDelete:
		if err := g.store.DeleteSchedule(sc.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			g.sched.mu.Unlock()
			reply("Could not delete it: " + err.Error())
			return
		}
		g.sched.mu.Unlock()
		verb := "deleted"
		if c.Action == actScheduleCancel {
			verb = "cancelled"
		}
		g.auditSchedule("schedule."+verb, sc, c.User.ID, nil)
		g.retireCard(ctx, sc, fmt.Sprintf(":wastebasket: Schedule **%s** was %s by %s.", sc.Name, verb, mention(c.User.ID)))
		return
	case actScheduleRun:
		g.sched.mu.Unlock()
		g.auditSchedule("schedule.run_now", sc, c.User.ID, nil)
		if msg := g.fireSchedule(ctx, sc, c.User); msg != "" {
			reply(msg)
		}
		return
	default:
		g.sched.mu.Unlock()
		return
	}

	err = g.store.UpdateSchedule(sc)
	g.sched.mu.Unlock()
	if err != nil {
		reply("Could not save that: " + err.Error())
		return
	}
	g.auditSchedule("schedule."+strings.TrimPrefix(c.Action, "sched_"), sc, c.User.ID, nil)
	g.refreshCard(ctx, sc, "")
}

// ---------------------------------------------------------------------------
// Running
// ---------------------------------------------------------------------------

// runSchedules fires due schedules for the life of the gateway.
func (g *Gateway) runSchedules(ctx context.Context) {
	t := time.NewTicker(scheduleTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.runDueSchedules(ctx)
		}
	}
}

// runDueSchedules fires every active schedule whose time has come, and returns
// how many it fired.
//
// The next run is computed from now, not from the missed one, so a gateway
// that was down across several runs fires each schedule once on its return
// rather than replaying the backlog.
func (g *Gateway) runDueSchedules(ctx context.Context) int {
	now := g.scheduleNow()
	due, err := g.store.DueSchedules(now)
	if err != nil {
		g.log.Error("loading due schedules failed", "err", err)
		return 0
	}
	fired := 0
	for _, d := range due {
		g.sched.mu.Lock()
		sc, err := g.store.Schedule(d.ID)
		if err != nil || sc.State != store.ScheduleActive || sc.NextRunAt.IsZero() || sc.NextRunAt.After(now) {
			g.sched.mu.Unlock()
			continue // changed since the query
		}
		timing, err := g.timingOf(sc)
		if err != nil {
			sc.State = store.SchedulePaused
			sc.NextRunAt = time.Time{}
			sc.LastResult = "paused: " + err.Error()
		} else {
			// Advanced before firing, so a crash mid-fire skips a run rather
			// than repeating it forever.
			sc.NextRunAt = timing.next(now)
		}
		if err := g.store.UpdateSchedule(sc); err != nil {
			g.sched.mu.Unlock()
			g.log.Error("advancing schedule failed", "schedule", sc.ID, "err", err)
			continue
		}
		g.sched.mu.Unlock()
		if sc.State != store.ScheduleActive {
			g.refreshCard(ctx, sc, "")
			continue
		}
		if msg := g.fireSchedule(ctx, sc, surface.User{}); msg != "" {
			g.log.Info("scheduled run skipped", "schedule", sc.ID, "reason", msg)
			continue
		}
		fired++
	}
	return fired
}

// fireSchedule starts one run. by is who pressed Run now, or zero for the
// clock. It returns why nothing ran, or "" when a run was dispatched.
func (g *Gateway) fireSchedule(ctx context.Context, sc *store.Schedule, by surface.User) string {
	cfg := g.cfg.Load()
	srf, ok := g.surfaceFor(sc.Surface)
	if !ok {
		return "its surface is not connected"
	}
	rc, ok := cfg.Runners[sc.Runner]
	if !ok {
		g.pauseSchedule(ctx, sc, fmt.Sprintf("runner `%s` is no longer configured", sc.Runner))
		return "its runner is no longer configured, so it was paused"
	}
	persona := personaFor(rc)

	in := surface.Inbound{
		Surface:    sc.Surface,
		Channel:    sc.Channel,
		User:       surface.User{ID: sc.Creator, Display: sc.CreatorDisplay},
		IsDM:       sc.IsDM,
		Addressed:  true,
		ScheduleID: sc.ID,
	}
	if st, ok := g.ChannelState(sc.Channel); ok {
		in.ChannelName = st.Info.Name
	}
	// The creator must still be someone this channel's route would answer: a
	// person removed from the workspace does not keep a bot working for them.
	if !g.audienceAllowed(ctx, cfg, in) {
		g.pauseSchedule(ctx, sc, mention(sc.Creator)+" is no longer allowed to use this channel")
		return "its creator is no longer allowed here, so it was paused"
	}

	g.queuesMu.Lock()
	if prev := g.sched.lastRun[sc.ID]; prev != "" && (g.threadActive[prev] || len(g.threadWaiting[prev]) > 0) {
		g.queuesMu.Unlock()
		g.auditSchedule("schedule.skipped", sc, by.ID, map[string]any{"reason": "previous run still going"})
		return "the previous run is still going"
	}
	if sc.Mode == store.ScheduleModeThread {
		if key := threadKey(sc.Surface, sc.Channel, sc.Thread); g.threadActive[key] {
			g.queuesMu.Unlock()
			g.auditSchedule("schedule.skipped", sc, by.ID, map[string]any{"reason": "thread busy"})
			return "its thread has a turn running"
		}
	}
	g.queuesMu.Unlock()

	if sc.Mode == store.ScheduleModeThread {
		in.Thread = sc.Thread
	} else {
		header := fmt.Sprintf(":alarm_clock: **%s** · scheduled run", sc.Name)
		if by.ID != "" {
			header = fmt.Sprintf(":alarm_clock: **%s** · run now by %s", sc.Name, mention(by.ID))
		}
		ref, err := srf.Post(ctx, surface.Post{Channel: sc.Channel, Text: header, Persona: persona})
		if err != nil {
			g.log.Error("scheduled run header post failed", "schedule", sc.ID, "err", err)
			g.recordScheduleResult(ctx, sc.ID, store.TurnError, "could not post in the channel: "+err.Error())
			return "could not post in its channel"
		}
		in.Thread = ref.ID
	}
	in.Text = schedulePreamble(sc, by) + sc.Prompt

	key := threadKey(in.Surface, in.Channel, in.Thread)
	bound, _, err := g.store.BindThread(key, in.Surface, in.Channel, sc.Runner)
	if err != nil {
		g.log.Error("bind thread for scheduled run failed", "schedule", sc.ID, "err", err)
		return "could not open its thread"
	}
	runner := bound.Runner
	if runner != sc.Runner {
		// A thread-mode schedule whose thread someone !rebind-ed follows it.
		if rc, ok = cfg.Runners[runner]; !ok {
			return "its thread's runner is no longer configured"
		}
		persona = personaFor(rc)
	}
	_ = g.store.TouchThread(key)

	g.sched.mu.Lock()
	g.sched.lastRun[sc.ID] = key
	if cur, err := g.store.Schedule(sc.ID); err == nil {
		cur.LastRunAt = g.scheduleNow()
		cur.LastResult = "running"
		if err := g.store.UpdateSchedule(cur); err != nil {
			g.log.Error("recording scheduled run failed", "schedule", sc.ID, "err", err)
		}
	}
	g.sched.mu.Unlock()
	g.auditSchedule("schedule.fired", sc, by.ID, map[string]any{"thread": key})

	if g.beginThreadTurn(ctx, in, key, runner, rc, persona) {
		g.dispatchAdmitted(ctx, in, runner, rc, persona)
	}
	return ""
}

func schedulePreamble(sc *store.Schedule, by surface.User) string {
	who := "on its schedule"
	if by.ID != "" {
		who = "because " + by.ID + " pressed Run now"
	}
	return fmt.Sprintf("[Scheduled run of %q (%s), set up by %s (%s), sent %s. Schedule: `%s` %s. Nobody typed this message and nobody may be watching: do what the instructions below say and post the result here. You cannot create or change schedules from a scheduled run.]\n\n",
		sc.Name, sc.ID, clean(sc.CreatorDisplay), sc.Creator, who, sc.Cron, sc.Timezone)
}

// pauseSchedule stops a schedule that cannot run, and says why on its card and
// in its thread.
func (g *Gateway) pauseSchedule(ctx context.Context, sc *store.Schedule, reason string) {
	g.sched.mu.Lock()
	cur, err := g.store.Schedule(sc.ID)
	if err != nil {
		g.sched.mu.Unlock()
		return
	}
	cur.State = store.SchedulePaused
	cur.NextRunAt = time.Time{}
	cur.LastResult = "paused: " + reason
	err = g.store.UpdateSchedule(cur)
	g.sched.mu.Unlock()
	if err != nil {
		g.log.Error("pausing schedule failed", "schedule", sc.ID, "err", err)
		return
	}
	g.auditSchedule("schedule.auto_paused", cur, "", map[string]any{"reason": reason})
	g.refreshCard(ctx, cur, ":warning: Paused automatically: "+reason+".")
	if srf, ok := g.surfaceFor(cur.Surface); ok {
		_, _ = srf.Post(ctx, surface.Post{
			Channel: cur.Channel, Thread: cur.Thread, Persona: g.schedulePersona(cur),
			Text: fmt.Sprintf("%s I paused schedule **%s**: %s. Press Resume on its card once that is fixed.",
				mention(cur.Creator), cur.Name, reason),
		})
	}
}

// scheduleTurnEnded records how a scheduled run's turn ended. Every place a
// turn is finalized calls it; it ignores turns that were not scheduled.
func (g *Gateway) scheduleTurnEnded(ctx context.Context, turn *turnContext, status, errMsg string) {
	if turn == nil || turn.ScheduleID == "" {
		return
	}
	g.recordScheduleResult(ctx, turn.ScheduleID, status, errMsg)
}

func (g *Gateway) recordScheduleResult(ctx context.Context, id, status, errMsg string) {
	g.sched.mu.Lock()
	sc, err := g.store.Schedule(id)
	if err != nil {
		g.sched.mu.Unlock()
		return // deleted while it ran
	}
	switch status {
	case store.TurnDone:
		sc.Failures = 0
		sc.LastResult = "ok"
	case store.TurnStopped:
		sc.LastResult = "stopped"
	default:
		sc.Failures++
		sc.LastResult = "failed: " + truncateLine(errMsg, 200)
	}
	tooMany := sc.Failures >= maxScheduleFailures && sc.State == store.ScheduleActive
	err = g.store.UpdateSchedule(sc)
	g.sched.mu.Unlock()
	if err != nil {
		g.log.Error("recording schedule result failed", "schedule", id, "err", err)
		return
	}
	if tooMany {
		g.pauseSchedule(ctx, sc, fmt.Sprintf("%d runs in a row failed (last: %s)", sc.Failures, truncateLine(errMsg, 200)))
		return
	}
	g.refreshCard(ctx, sc, "")
}

// SchedulesText answers !schedules: the schedules in this channel and the ones
// the asker created anywhere.
func (g *Gateway) SchedulesText(in surface.Inbound) string {
	list, err := g.store.SchedulesFor(in.Surface, in.User.ID, in.Channel)
	if err != nil {
		return "Could not read schedules: " + err.Error()
	}
	if len(list) == 0 {
		return "No schedules here, and you have none elsewhere. Ask me to set one up, e.g. \"every weekday at 9am, summarize open PRs\"."
	}
	var b strings.Builder
	b.WriteString("**Schedules** (manage them with the buttons on each card, or ask me)\n")
	for _, sc := range list {
		fmt.Fprintf(&b, "• **%s** `%s` — %s, `%s` %s", sc.Name, sc.ID, sc.State, sc.Cron, sc.Timezone)
		if sc.Channel != in.Channel {
			fmt.Fprintf(&b, ", in %s", g.channelLabel(sc.Channel))
		}
		if !sc.NextRunAt.IsZero() {
			fmt.Fprintf(&b, ", next %s", formatWhen(sc.NextRunAt, sc.Timezone))
		}
		if sc.Creator != in.User.ID {
			fmt.Fprintf(&b, ", by %s", mention(sc.Creator))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
