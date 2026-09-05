package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/avarant/splitscreen/config"
	"github.com/avarant/splitscreen/internal/store"
	"github.com/avarant/splitscreen/internal/surface"
	"github.com/avarant/splitscreen/protocol"
)

// turnContext is what the gateway needs to route a runner's replies back to the
// right place. Every upward frame carries a turn id; this is the join.
type turnContext struct {
	TurnID    string
	ThreadID  string
	Surface   string
	Channel   string
	Thread    string
	Runner    string
	User      surface.User
	Persona   surface.Persona
	Activity  string
	StartedAt time.Time
}

func newID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not a condition worth continuing through.
		panic("gateway: crypto/rand unavailable: " + err.Error())
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

func threadKey(surfaceName, channel, thread string) string {
	return surfaceName + ":" + channel + ":" + thread
}

// OnMessage handles one inbound user message. It resolves routing, opens a
// turn, relays any attachments, and dispatches to the runner.
func (g *Gateway) OnMessage(ctx context.Context, in surface.Inbound) {
	cfg := g.cfg.Load()
	key := threadKey(in.Surface, in.Channel, in.Thread)

	text, cmd, cmdArg := parseCommand(in.Text)

	// An existing binding wins over routing config: the session lives on that
	// runner's disk, so a thread cannot follow a re-pointed channel.
	bound, err := g.store.Thread(key)
	isNew := errors.Is(err, store.ErrNotFound)
	if err != nil && !isNew {
		g.log.Error("thread lookup failed", "thread", key, "err", err)
		return
	}

	// Starting a conversation requires being addressed; continuing one does not.
	// Channel routing says WHICH runner may answer here, not that everything
	// said here is for it — a channel people also talk to each other in would
	// otherwise get an answer to every message, including messages about the
	// bot rather than to it.
	//
	// A bare "!status" is addressing too: nobody types it at a colleague.
	if isNew && !in.Addressed && cmd == "" {
		return
	}

	var runnerName string
	switch {
	case !isNew:
		runnerName = bound.Runner
	default:
		var ok bool
		runnerName, ok = cfg.RunnerFor(in.Channel, in.IsDM)
		if !ok {
			// Unrouted channels are ignored entirely. This replaces the
			// per-runner allowlists the old bridges each carried.
			return
		}
	}

	// !runner only makes sense before a session exists.
	if cmd == "runner" {
		if !isNew {
			g.notice(ctx, in, "This thread is already bound to `"+runnerName+"`. Start a new thread, or use `!rebind`.")
			return
		}
		if _, ok := cfg.Runners[cmdArg]; !ok {
			g.notice(ctx, in, "No runner named `"+cmdArg+"`.")
			return
		}
		runnerName = cmdArg
	}

	rc, ok := cfg.Runners[runnerName]
	if !ok {
		g.notice(ctx, in, "Runner `"+runnerName+"` is no longer configured.")
		return
	}
	persona := personaFor(rc)

	thread, created, err := g.store.BindThread(key, in.Surface, in.Channel, runnerName)
	if err != nil {
		g.log.Error("bind thread failed", "thread", key, "err", err)
		return
	}
	_ = created
	_ = g.store.TouchThread(key)

	switch cmd {
	case "new":
		if err := g.store.ClearSession(key); err != nil {
			g.log.Error("clear session failed", "thread", key, "err", err)
		}
		// Session grants were scoped to the session that just ended.
		if n := g.grants.Clear(key); n > 0 {
			g.log.Info("cleared session grants", "thread", key, "count", n)
		}
		g.notice(ctx, in, "Started a fresh session on `"+runnerName+"`.")
		if text == "" {
			return
		}
	case "rebind":
		target, ok := cfg.RunnerFor(in.Channel, in.IsDM)
		if !ok {
			g.notice(ctx, in, "This channel has no route to rebind to.")
			return
		}
		if target == thread.Runner {
			g.notice(ctx, in, "Already on `"+target+"`.")
			return
		}
		g.grants.Clear(key)
		if err := g.store.RebindThread(key, target); err != nil {
			g.log.Error("rebind failed", "thread", key, "err", err)
			return
		}
		_ = g.store.Log(store.Event{
			Kind: "thread.rebound", Runner: target, ThreadID: key,
			SurfaceUser: in.User.ID,
			Detail:      map[string]any{"from": thread.Runner, "to": target},
		})
		g.notice(ctx, in, "Rebound to `"+target+"`; a fresh session starts on the next message.")
		return
	case "status":
		g.notice(ctx, in, g.StatusText())
		return
	case "cost":
		g.notice(ctx, in, g.CostText(7*24*time.Hour))
		return
	case "routes":
		g.notice(ctx, in, g.RoutesText())
		return
	}

	if strings.TrimSpace(text) == "" && len(in.Files) == 0 {
		return
	}

	// Concurrency gate. Only when the runner is online: an offline runner has no
	// active turns, so its messages belong in the persisted offline queue
	// (dispatchTurn handles that), not this in-memory concurrency queue. admit
	// returns true when it has parked the message with a position notice.
	if _, online := g.hub.Get(runnerName); online && rc.MaxConcurrent > 0 {
		if g.admit(ctx, in, runnerName, rc, persona) {
			return
		}
	}

	g.dispatchTurn(ctx, in, runnerName, rc, persona)
}

// dispatchTurn opens a turn for an admitted message and hands it to the runner,
// falling back to the persisted offline queue if the runner is unreachable. It
// is the single dispatch path: OnMessage calls it when a slot is free, and the
// queue drains through it when a slot later frees.
func (g *Gateway) dispatchTurn(ctx context.Context, in surface.Inbound, runnerName string, rc *config.Runner, persona surface.Persona) {
	cfg := g.cfg.Load()
	key := threadKey(in.Surface, in.Channel, in.Thread)
	text, _, _ := parseCommand(in.Text)

	turn := &turnContext{
		TurnID:    newID("turn"),
		ThreadID:  key,
		Surface:   in.Surface,
		Channel:   in.Channel,
		Thread:    in.Thread,
		Runner:    runnerName,
		User:      in.User,
		Persona:   persona,
		Activity:  rc.Display.EffectiveActivity(),
		StartedAt: time.Now(),
	}
	if err := g.store.StartTurn(store.Turn{
		ID: turn.TurnID, ThreadID: key, Channel: in.Channel,
		Runner: runnerName, SurfaceUser: in.User.ID,
	}); err != nil {
		g.log.Error("start turn failed", "turn", turn.TurnID, "err", err)
		return
	}
	g.turns.Store(turn.TurnID, turn)

	msg := &protocol.Message{
		ThreadID: key,
		TurnID:   turn.TurnID,
		Channel:  in.Channel,
		User:     protocol.UserRef{ID: in.User.ID, Display: in.User.Display},
		Text:     text,
	}

	conn, online := g.hub.Get(runnerName)
	// Hub membership is not liveness: a hung host leaves its connection half-
	// open and the reaper takes up to five heartbeats to notice. Consulting the
	// heartbeat state here means a message in that window queues instead of
	// being written into a socket nothing is reading.
	if online && conn.State(cfg.Gateway.Heartbeat.Duration()) == StateDisconnected {
		conn.CloseWith("declared dead by dispatch: heartbeat silence")
		online = false
	}

	// Attachments are streamed before the message so the runner has them on disk
	// by the time it is asked to act.
	if online && len(in.Files) > 0 {
		atts, err := g.relayFilesToRunner(ctx, conn, turn, in.Files)
		if err != nil {
			g.log.Error("attachment relay failed", "turn", turn.TurnID, "err", err)
			g.notice(ctx, in, "Could not transfer an attachment: "+err.Error())
		}
		msg.Attachments = atts
	} else if len(in.Files) > 0 {
		g.notice(ctx, in, "Attachments were dropped: `"+runnerName+"` is offline.")
	}

	if !online {
		g.queueMessage(ctx, in, runnerName, key, msg)
		return
	}
	if err := conn.Send(msg); err != nil {
		g.log.Error("dispatch failed", "runner", runnerName, "err", err)
		g.queueMessage(ctx, in, runnerName, key, msg)
	}
}

// queueMessage persists a message for an offline runner and reports the depth
// in-thread, so a runner restart is visible-and-recovered rather than silent
// data loss.
func (g *Gateway) queueMessage(ctx context.Context, in surface.Inbound, runner, threadID string, msg *protocol.Message) {
	cfg := g.cfg.Load()
	depth, _ := g.store.QueueDepth(runner)
	if cfg.Gateway.QueueLimit > 0 && depth >= cfg.Gateway.QueueLimit {
		g.notice(ctx, in, fmt.Sprintf("`%s` is offline and its queue is full (%d). This message was dropped.", runner, depth))
		return
	}
	raw, err := protocol.Encode(msg)
	if err != nil {
		g.log.Error("encode for queue failed", "err", err)
		return
	}
	if err := g.store.Enqueue(runner, threadID, raw); err != nil {
		g.log.Error("enqueue failed", "runner", runner, "err", err)
		return
	}
	g.notice(ctx, in, fmt.Sprintf("`%s` is offline — queued (%d waiting).", runner, depth+1))
}

// drainQueue replays anything accepted while a runner was away.
func (g *Gateway) drainQueue(ctx context.Context, conn *Conn) {
	msgs, err := g.store.Dequeue(conn.runner, 1000)
	if err != nil {
		g.log.Error("dequeue failed", "runner", conn.runner, "err", err)
		return
	}
	if len(msgs) == 0 {
		return
	}
	g.log.Info("draining queue", "runner", conn.runner, "count", len(msgs))
	for _, m := range msgs {
		if err := conn.SendRawJSON(m.Frame); err != nil {
			g.log.Error("queue drain send failed", "runner", conn.runner, "err", err)
			return
		}
		if err := g.store.DeleteQueued(m.ID); err != nil {
			g.log.Error("queue delete failed", "id", m.ID, "err", err)
		}
	}
}

// admit applies the runner's concurrency cap. It returns false when a slot is
// free (the caller dispatches immediately), or true when it has parked the
// message: either at the back of the queue with a position notice, or rejected
// because the queue is full.
//
// A message queues even when active < cap if others are already waiting, so the
// queue stays FIFO — otherwise a latecomer would jump ahead of everyone parked.
func (g *Gateway) admit(ctx context.Context, in surface.Inbound, runnerName string, rc *config.Runner, persona surface.Persona) bool {
	g.queuesMu.Lock()
	q := g.queues[runnerName]
	if q == nil {
		q = &runnerQueue{}
		g.queues[runnerName] = q
	}
	if q.active < rc.MaxConcurrent && len(q.waiting) == 0 {
		q.active++
		g.queuesMu.Unlock()
		return false
	}
	if len(q.waiting) >= maxQueueDepth {
		g.queuesMu.Unlock()
		g.notice(ctx, in, fmt.Sprintf("`%s` is busy and its queue is full (%d waiting). Please try again in a bit.", runnerName, maxQueueDepth))
		return true
	}
	// Append under the lock so a concurrent turnSlotFreed always sees this item;
	// posting the notice (I/O) happens after, and its ref is attached back on.
	qt := &queuedTurn{in: in, runner: runnerName, rc: rc, persona: persona}
	q.waiting = append(q.waiting, qt)
	pos, active := len(q.waiting), q.active
	g.queuesMu.Unlock()

	ref, err := g.postQueueNotice(ctx, in, persona, runnerName, pos, active)
	if err != nil {
		g.log.Warn("queue notice post failed", "runner", runnerName, "err", err)
		return true
	}
	// The item may already have been dispatched while we posted; setting the ref
	// is still safe, and the advance edit tolerates a ref that arrived too late.
	g.queuesMu.Lock()
	qt.noticeRef = ref
	g.queuesMu.Unlock()
	return true
}

// turnSlotFreed is called once per ended turn. It releases the runner's slot
// and, if anyone is waiting, admits the head of the queue and advances the
// positions shown to everyone still parked.
func (g *Gateway) turnSlotFreed(ctx context.Context, runnerName string) {
	g.queuesMu.Lock()
	q := g.queues[runnerName]
	if q == nil {
		g.queuesMu.Unlock()
		return
	}
	if q.active > 0 {
		q.active--
	}
	if len(q.waiting) == 0 {
		g.queuesMu.Unlock()
		return
	}
	next := q.waiting[0]
	q.waiting = q.waiting[1:]
	q.active++
	rest := append([]*queuedTurn(nil), q.waiting...)
	active := q.active
	g.queuesMu.Unlock()

	g.editQueueNotice(ctx, next, 0, active) // 0 => "starting now"
	g.dispatchTurn(ctx, next.in, next.runner, next.rc, next.persona)
	for i, qt := range rest {
		g.editQueueNotice(ctx, qt, i+1, active)
	}
}

// queueNoticeText renders the position message. pos 0 means the turn is now
// starting; the notice is left as a breadcrumb above the turn's own output.
func queueNoticeText(runner string, pos, active int) string {
	if pos <= 0 {
		return "▶️ A slot freed — starting now…"
	}
	return fmt.Sprintf("⏳ `%s` is at capacity (%d running) — you're #%d in the queue. I'll start this automatically as soon as a slot frees.", runner, active, pos)
}

func (g *Gateway) postQueueNotice(ctx context.Context, in surface.Inbound, persona surface.Persona, runner string, pos, active int) (surface.Ref, error) {
	srf, ok := g.surfaceFor(in.Surface)
	if !ok {
		return surface.Ref{}, fmt.Errorf("gateway: no surface %q", in.Surface)
	}
	return srf.Post(ctx, surface.Post{
		Channel: in.Channel,
		Thread:  in.Thread,
		Text:    queueNoticeText(runner, pos, active),
		Persona: persona,
	})
}

func (g *Gateway) editQueueNotice(ctx context.Context, qt *queuedTurn, pos, active int) {
	if qt.noticeRef == (surface.Ref{}) {
		return
	}
	srf, ok := g.surfaceFor(qt.in.Surface)
	if !ok {
		return
	}
	if err := srf.Update(ctx, qt.noticeRef, surface.Post{
		Channel: qt.in.Channel,
		Thread:  qt.in.Thread,
		Text:    queueNoticeText(qt.runner, pos, active),
		Persona: qt.persona,
	}); err != nil {
		g.log.Warn("queue notice update failed", "runner", qt.runner, "err", err)
	}
}

// parseCommand splits a leading !command off a message.
//
// Commands are recognized on the gateway so runners do not each reimplement the
// syntax, and so a command is auditable even when no runner is connected.
func parseCommand(text string) (rest, cmd, arg string) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "!") {
		return text, "", ""
	}
	line, remainder, _ := strings.Cut(trimmed, "\n")
	fields := strings.Fields(strings.TrimPrefix(line, "!"))
	if len(fields) == 0 {
		return text, "", ""
	}
	cmd = strings.ToLower(fields[0])
	switch cmd {
	case "new", "rebind", "status", "cost", "routes":
	case "runner":
		if len(fields) > 1 {
			arg = fields[1]
		}
	default:
		return text, "", ""
	}
	return strings.TrimSpace(remainder), cmd, arg
}

// notice posts an ephemeral message to the requesting user. Operational chatter
// does not belong in a shared thread.
func (g *Gateway) notice(ctx context.Context, in surface.Inbound, text string) {
	s, ok := g.surfaceFor(in.Surface)
	if !ok {
		return
	}
	_, err := s.Post(ctx, surface.Post{
		Channel:   in.Channel,
		Thread:    in.Thread,
		Text:      text,
		Ephemeral: true,
		User:      in.User.ID,
	})
	if err != nil {
		g.log.Warn("notice post failed", "err", err)
	}
}

// approverAllowed reports whether a user may resolve a permission prompt.
// An empty approver list means anyone in the channel may decide.
func approverAllowed(rc *config.Runner, userID string) bool {
	if rc == nil || len(rc.Policy.Approvers) == 0 {
		return true
	}
	for _, a := range rc.Policy.Approvers {
		if a == userID {
			return true
		}
	}
	return false
}
