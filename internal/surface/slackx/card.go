package slackx

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"

	"github.com/analystaio/splitscreen/internal/surface"
)

// A card is a whole turn in one message. It is posted the moment the turn
// starts and rewritten with chat.update as the turn runs: the steps in one
// plan block, the prose under it, and the Stop button at the bottom until the
// turn ends.
//
// The streaming methods cannot do this. Slack ends a stream ten minutes after
// it starts, so a long turn had to be cut into a run of messages, and a
// streaming message cannot carry blocks until it is closed, so the Stop button
// had to be a message of its own. A rewritten message has neither limit. What
// it gives up is prose typing in letter by letter: text arrives a second or so
// at a time instead.

const (
	// cardMinInterval spaces a card's updates. chat.update is rate limited per
	// app across the workspace, and every running turn shares that budget.
	cardMinInterval = 1200 * time.Millisecond
	// cardTextLimit is how much prose one message carries. Slack caps the
	// markdown blocks of a message at 12,000 characters together; an answer
	// longer than this continues in further messages.
	cardTextLimit = 10000
	// cardDraftLimit bounds the prose shown while the turn is still running,
	// which is a glimpse of what the model is saying, not the answer.
	cardDraftLimit = 3000
	// cardMaxFailures is how many updates in a row may fail before the card
	// gives up and the gateway takes the fallback path. One failed rewrite
	// loses nothing: the next carries the same state.
	cardMaxFailures = 3
	cardCallTimeout = 10 * time.Second

	// stopInlineBlock is the block id of a Stop button that is part of a
	// card, which tells a press of it apart from the standalone control.
	stopInlineBlock = "splitscreen_stop_inline"

	cardWorking = "Working…"

	// cardRecentSteps is how many of the latest steps a running card lists,
	// and cardStepLine how long one of those lines may be.
	cardRecentSteps = 5
	cardStepLine    = 140
)

// cardAPI is the part of the Slack client a card uses.
type cardAPI interface {
	PostMessageContext(ctx context.Context, channelID string, options ...slack.MsgOption) (string, string, error)
	UpdateMessageContext(ctx context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, string, error)
	DeleteMessageContext(ctx context.Context, channelID, timestamp string) (string, string, error)
}

// OpenLive posts a turn's card. It needs no content: an empty card says the
// turn is working, which is the point of posting it early.
func (s *Surface) OpenLive(ctx context.Context, p surface.Post) (surface.Stream, error) {
	if p.Thread == "" {
		return nil, errors.New("slack: a turn's message requires a thread")
	}
	return openCard(ctx, s.api, p)
}

func openCard(ctx context.Context, api cardAPI, p surface.Post) (*card, error) {
	c := &card{
		api:     api,
		persona: p.Persona,
		turnID:  p.StopTurnID,
		opened:  time.Now(),
		steps:   map[string]surface.Step{},
	}
	opts := append(c.messageOptions(c.blocks(false, "", "")), slack.MsgOptionTS(p.Thread))
	channel, ts, err := api.PostMessageContext(ctx, p.Channel, opts...)
	if err != nil {
		return nil, fmt.Errorf("slack: post turn message: %w", err)
	}
	c.ref = surface.Ref{Channel: channel, Thread: p.Thread, ID: ts}
	c.lastPush = time.Now()
	return c, nil
}

type card struct {
	api     cardAPI
	ref     surface.Ref
	persona surface.Persona
	// turnID is the turn the Stop button names; empty when none is offered.
	turnID string
	opened time.Time

	mu     sync.Mutex
	closed bool

	text     strings.Builder
	draft    string
	stopping string
	order    []string
	steps    map[string]surface.Step

	// lastPush and notBefore pace the rewrites; timer carries a state that
	// arrived too soon after the last one.
	lastPush  time.Time
	notBefore time.Time
	timer     *time.Timer
	failures  int
}

func (c *card) Ref() surface.Ref { return c.ref }

// absorb folds one update into the card's state. Called under the lock.
func (c *card) absorb(u surface.StreamUpdate) {
	c.text.WriteString(u.Text)
	c.draft = u.Draft
	if u.Stopping != "" {
		c.stopping = u.Stopping
	}
	for _, step := range u.Steps {
		if _, ok := c.steps[step.ID]; !ok {
			c.order = append(c.order, step.ID)
		}
		c.steps[step.ID] = step
	}
}

// Append takes the turn's latest state and rewrites the message, or arranges
// to once the pacing allows. The state is kept here, so an update that is
// delayed or fails is not lost: the next rewrite carries it.
func (c *card) Append(ctx context.Context, u surface.StreamUpdate) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("slack: turn message already closed")
	}
	c.absorb(u)
	return c.pushLocked(ctx)
}

// pushLocked rewrites the message now if the pacing allows, and otherwise
// leaves a timer to do it. Called under the lock.
func (c *card) pushLocked(ctx context.Context) error {
	now := time.Now()
	at := c.lastPush.Add(cardMinInterval)
	if c.notBefore.After(at) {
		at = c.notBefore
	}
	if now.Before(at) {
		c.deferLocked(at.Sub(now))
		return nil
	}
	c.lastPush = now
	err := c.update(ctx, c.blocks(false, c.shownText(), ""))
	if err == nil {
		c.failures = 0
		return nil
	}
	var limited *slack.RateLimitedError
	if errors.As(err, &limited) {
		c.notBefore = now.Add(limited.RetryAfter)
		c.deferLocked(limited.RetryAfter)
		return nil
	}
	c.failures++
	if c.failures >= cardMaxFailures {
		return fmt.Errorf("slack: update turn message: %w", err)
	}
	c.deferLocked(cardMinInterval)
	return nil
}

func (c *card) deferLocked(d time.Duration) {
	if c.timer != nil {
		return
	}
	c.timer = time.AfterFunc(d, func() {
		ctx, cancel := context.WithTimeout(context.Background(), cardCallTimeout)
		defer cancel()
		c.mu.Lock()
		defer c.mu.Unlock()
		c.timer = nil
		if c.closed {
			return
		}
		_ = c.pushLocked(ctx)
	})
}

// shownText is the prose a running card shows: what has been placed, then the
// draft. Only the tail is kept, since it is what is being said now.
func (c *card) shownText() string {
	text := strings.TrimSpace(c.text.String())
	if draft := strings.TrimSpace(c.draft); draft != "" {
		if text != "" {
			text += "\n\n"
		}
		text += draft
	}
	if len(text) > cardDraftLimit {
		text = "…" + strings.ToValidUTF8(text[len(text)-cardDraftLimit:], "")
	}
	return text
}

// Close writes the finished message. An answer too long for one message
// carries on in further ones; a turn that showed nothing leaves no message.
func (c *card) Close(ctx context.Context, u surface.StreamUpdate) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.absorb(u)
	c.draft = ""

	text := strings.TrimSpace(c.text.String())
	if text == "" && len(c.order) == 0 && u.Footer == "" {
		if _, _, err := c.api.DeleteMessageContext(ctx, c.ref.Channel, c.ref.ID); err != nil {
			return fmt.Errorf("slack: delete empty turn message: %w", err)
		}
		return nil
	}

	parts := splitText(text, cardTextLimit)
	first, rest := "", []string(nil)
	if len(parts) > 0 {
		first, rest = parts[0], parts[1:]
	}
	footer := u.Footer
	if len(rest) > 0 {
		footer = "" // it belongs under the last message
	}
	if err := c.updateWithRetry(ctx, c.blocks(true, first, footer)); err != nil {
		// The answer did not land. Leave the card as the record of the work,
		// without a live button, and report the failure so the caller posts
		// the answer itself.
		_ = c.update(ctx, c.blocks(true, "", "The answer is below."))
		return fmt.Errorf("slack: close turn message: %w", err)
	}
	for i, part := range rest {
		blocks := []slack.Block{slack.NewMarkdownBlock("", part)}
		if i == len(rest)-1 && u.Footer != "" {
			blocks = append(blocks, contextBlock(u.Footer))
		}
		opts := append(c.messageOptionsFor(blocks, part), slack.MsgOptionTS(c.ref.Thread))
		if _, _, err := c.api.PostMessageContext(ctx, c.ref.Channel, opts...); err != nil {
			return fmt.Errorf("slack: post rest of answer: %w", err)
		}
	}
	return nil
}

// updateWithRetry is the closing rewrite, which has no later one to carry what
// it failed to deliver: a rate limit is waited out rather than skipped.
func (c *card) updateWithRetry(ctx context.Context, blocks []slack.Block) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = c.update(ctx, blocks)
		var limited *slack.RateLimitedError
		if !errors.As(err, &limited) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(limited.RetryAfter, 15*time.Second)):
		}
	}
	return err
}

func (c *card) update(ctx context.Context, blocks []slack.Block) error {
	_, _, _, err := c.api.UpdateMessageContext(ctx, c.ref.Channel, c.ref.ID, c.messageOptions(blocks)...)
	return err
}

func (c *card) messageOptions(blocks []slack.Block) []slack.MsgOption {
	return c.messageOptionsFor(blocks, strings.TrimSpace(c.text.String()))
}

// messageOptionsFor adds the plain text Slack uses for notifications and for
// clients that cannot draw the blocks.
func (c *card) messageOptionsFor(blocks []slack.Block, text string) []slack.MsgOption {
	if text == "" {
		text = cardWorking
	}
	opts := []slack.MsgOption{
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionText(ToMrkdwn(truncate(text, 3000)), false),
	}
	return append(opts, personaOptions(c.persona)...)
}

// blocks lays the card out. What the model said before its first step opens
// the message as text. Under it come the steps: while the turn runs, the
// latest few as plain lines, and once it has finished, all of them folded into
// one plan. Then the prose, and last either the Stop button (running), the
// line saying a stop was asked for, or the footer (finished).
//
// The plan waits for the end because a client folds it shut again on every
// rewrite of the message, so a running turn could not be read while open.
// Called under the lock.
func (c *card) blocks(final bool, text, footer string) []slack.Block {
	var out []slack.Block
	intro, steps := c.intro()
	if intro != "" {
		out = append(out, slack.NewMarkdownBlock("", intro))
	}
	if final {
		if plan := c.plan(steps); plan != nil {
			out = append(out, plan)
		}
	} else if recent := c.recent(steps); recent != nil {
		out = append(out, recent)
	}
	if text != "" {
		out = append(out, slack.NewMarkdownBlock("", text))
	}
	if final {
		if footer != "" {
			out = append(out, contextBlock(footer))
		}
		if len(out) == 0 {
			out = append(out, contextBlock("_Finished._"))
		}
		return out
	}
	if len(out) == 0 {
		out = append(out, contextBlock("_"+cardWorking+"_"))
	}
	switch {
	case c.stopping != "":
		out = append(out, contextBlock(c.stopping))
	case c.turnID != "":
		btn := slack.NewButtonBlockElement(stopAction, c.turnID,
			slack.NewTextBlockObject(slack.PlainTextType, "Stop", false, false))
		btn.Style = slack.StyleDanger
		out = append(out, slack.NewActionBlock(stopInlineBlock, btn))
	}
	return out
}

func contextBlock(text string) *slack.ContextBlock {
	return slack.NewContextBlock("",
		slack.NewTextBlockObject(slack.MarkdownType, ToMrkdwn(text), false, false))
}

// intro separates what the model said before doing anything from the steps
// that followed: the text, and the ids of the remaining steps in order.
func (c *card) intro() (string, []string) {
	var said []string
	n := 0
	for ; n < len(c.order); n++ {
		step := c.steps[c.order[n]]
		if !step.Prose {
			break
		}
		line := step.Title
		if step.Detail != "" {
			line += "\n" + step.Detail
		}
		said = append(said, line)
	}
	return strings.Join(said, "\n\n"), c.order[n:]
}

// recent shows a running turn's latest steps as plain lines, nil when there
// are none: the newest cardRecentSteps, under a count of the ones before.
func (c *card) recent(ids []string) *slack.ContextBlock {
	if len(ids) == 0 {
		return nil
	}
	var lines []string
	if earlier := len(ids) - cardRecentSteps; earlier > 0 {
		ids = ids[earlier:]
		noun := "steps"
		if earlier == 1 {
			noun = "step"
		}
		lines = append(lines, fmt.Sprintf("%d earlier %s", earlier, noun))
	}
	for _, id := range ids {
		step := c.steps[id]
		mark := "✓"
		switch step.Status {
		case surface.StepRunning:
			mark = "⏳"
		case surface.StepFailed:
			mark = "✗"
		}
		line := truncateTitle(step.Title)
		if step.Detail != "" && !step.Prose {
			line += " · " + strings.Join(strings.Fields(step.Detail), " ")
		}
		lines = append(lines, mark+" "+truncate(line, cardStepLine))
	}
	// Plain text, so nothing in a command line is read as formatting.
	return slack.NewContextBlock("",
		slack.NewTextBlockObject(slack.PlainTextType, strings.Join(lines, "\n"), true, false))
}

// plan renders the finished turn's steps as one plan block, nil when there
// are none. Slack caps a plan at maxPlanTasks, so the steps past the reserve
// share one card that counts them, shows the newest and carries their
// aggregate status.
func (c *card) plan(ids []string) *slack.PlanBlock {
	if len(ids) == 0 {
		return nil
	}
	own := ids
	var extra []string
	if len(own) > maxPlanTasks-1 {
		own, extra = ids[:maxPlanTasks-1], ids[maxPlanTasks-1:]
	}
	tasks := make([]*slack.TaskCardBlock, 0, len(own)+1)
	for _, id := range own {
		step := settled(c.steps[id])
		task := slack.NewTaskCardBlock(id, truncateTitle(step.Title))
		task.Status = taskStatus(step.Status)
		task.Details = richText(detailFor(step))
		if step.Output != "" {
			task.Output = richText(truncate(step.Output, 2000))
		}
		tasks = append(tasks, task)
	}
	if len(extra) > 0 {
		var failed bool
		for _, id := range extra {
			failed = failed || c.steps[id].Status == surface.StepFailed
		}
		task := slack.NewTaskCardBlock(overflowID, fmt.Sprintf("…and %d more steps", len(extra)))
		task.Status = slack.TaskCardStatusComplete
		if failed {
			task.Status = slack.TaskCardStatusError
		}
		newest := c.steps[extra[len(extra)-1]]
		detail := newest.Title
		if newest.Detail != "" {
			detail += ": " + newest.Detail
		}
		task.Details = richText(truncate(detail, 2000))
		tasks = append(tasks, task)
	}
	return slack.NewPlanBlock(c.finalTitle(ids)).WithTasks(tasks...)
}

// settled is a step as a finished turn renders it. The turn is over, so a step
// still marked running never reported its end — a lost update, or one that
// arrived after the last flush. Slack spins the plan's header while any task
// card is in progress, and a finished message is never rewritten again, so
// leaving it as it stands spins that header forever on a message that is
// complete. Showing it as done is the truth of it: the turn finished.
func settled(step surface.Step) surface.Step {
	if step.Status == surface.StepRunning {
		step.Status = surface.StepDone
	}
	return step
}

// finalTitle summarizes the finished plan: step count, failures, and how long
// the turn ran.
func (c *card) finalTitle(ids []string) string {
	var failed int
	for _, id := range ids {
		if c.steps[id].Status == surface.StepFailed {
			failed++
		}
	}
	title := fmt.Sprintf("%d steps", len(ids))
	if len(ids) == 1 {
		title = "1 step"
	}
	if failed > 0 {
		title += fmt.Sprintf(" · %d failed", failed)
	}
	if took := time.Since(c.opened).Round(time.Second); took > 0 {
		title += " · " + took.String()
	}
	return title
}

// richText wraps plain text for a task card's details, nil when empty.
func richText(text string) *slack.RichTextBlock {
	if text == "" {
		return nil
	}
	return slack.NewRichTextBlock("",
		slack.NewRichTextSection(slack.NewRichTextSectionTextElement(text, nil)))
}

// splitText cuts text into pieces of at most limit bytes, at a paragraph break
// where there is one, else a line break, else a space. A code fence open at a
// cut is closed there and reopened in the next piece, so neither half renders
// as one long code block or loses its formatting.
func splitText(text string, limit int) []string {
	var parts []string
	for len(text) > limit {
		window := text[:limit]
		cut := strings.LastIndex(window, "\n\n")
		if cut < limit/2 {
			cut = strings.LastIndex(window, "\n")
		}
		if cut < limit/2 {
			cut = strings.LastIndex(window, " ")
		}
		if cut < limit/2 {
			cut = len(strings.ToValidUTF8(window[:limit-4], ""))
		}
		piece := strings.TrimRight(text[:cut], "\n ")
		text = strings.TrimLeft(text[cut:], "\n ")
		if strings.Count(piece, "```")%2 == 1 {
			piece += "\n```"
			text = "```\n" + text
		}
		parts = append(parts, piece)
	}
	if text != "" {
		parts = append(parts, text)
	}
	return parts
}
