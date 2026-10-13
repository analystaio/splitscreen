package gateway

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/analystaio/splitscreen/internal/store"
	"github.com/analystaio/splitscreen/internal/surface"
	"github.com/analystaio/splitscreen/protocol"
)

// Stopping a turn is the chat-side Esc. A surface that can (surface.Stopper)
// shows a Stop control under a running turn; `!stop` in the thread does the
// same on any surface. Either way the gateway sends turn.cancel and the runner
// interrupts the harness, keeping the session: the next message carries on
// the same conversation. The turn still ends through the runner's done frame,
// so the thread and concurrency slots are released by the normal path.

// stopControlDelay is how long a turn runs before it is offered a Stop
// control. A quick answer never needs one, and a control posted and deleted
// within a second is flicker, not help.
var stopControlDelay = 5 * time.Second

// stopControlMaxWait bounds how long the control waits for the turn's own
// message to appear, so it lands under the answer rather than above it. A turn
// that is still silent after this gets the control anyway: being able to stop a
// long think matters more than where the button sits.
var stopControlMaxWait = 30 * time.Second

// stopState is one turn's stop control and what became of it.
type stopState struct {
	mu    sync.Mutex
	timer *time.Timer
	ref   surface.Ref
	// ended is set once the turn has finished; nothing is posted after it.
	ended bool
	// by is who asked to stop, once someone has.
	by *surface.User
	// armedAt is when the control was first due, for stopControlMaxWait.
	armedAt time.Time
}

// canCancel reports whether a runner is connected and new enough to act on
// turn.cancel. An older runner drops the frame as unknown, so offering it a
// Stop control would promise something that silently does nothing.
func (g *Gateway) canCancel(runner string) (*Conn, bool) {
	conn, online := g.hub.Get(runner)
	if !online {
		return nil, false
	}
	return conn, conn.peerVer.Major == protocol.Ours().Major &&
		conn.peerVer.Minor >= protocol.MinorTurnCancel
}

// armStop schedules a dispatched turn's Stop control.
func (g *Gateway) armStop(turn *turnContext) {
	srf, ok := g.surfaceFor(turn.Surface)
	if !ok {
		return
	}
	stopper, ok := srf.(surface.Stopper)
	if !ok {
		return
	}
	// The surface draws its own stop button on a working thread; a second one
	// would only be clutter.
	if turn.Surface == "slack" && g.cfg.Load().Gateway.Slack.NativeStop {
		return
	}
	if _, ok := g.canCancel(turn.Runner); !ok {
		return
	}
	st := &turn.stop
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.ended || st.timer != nil {
		return
	}
	st.timer = time.AfterFunc(stopControlDelay, func() { g.postStop(turn, srf, stopper) })
}

// stopControlRetry is how often a due control checks again for the turn's
// message.
const stopControlRetry = time.Second

func (g *Gateway) postStop(turn *turnContext, srf surface.Surface, stopper surface.Stopper) {
	st := &turn.stop
	st.mu.Lock()
	if st.ended || st.by != nil {
		st.mu.Unlock()
		return
	}
	if st.armedAt.IsZero() {
		st.armedAt = time.Now()
	}
	if !g.turnMessagePosted(turn) && time.Since(st.armedAt) < stopControlMaxWait {
		st.timer = time.AfterFunc(stopControlRetry, func() { g.postStop(turn, srf, stopper) })
		st.mu.Unlock()
		return
	}
	st.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ref, err := stopper.PostStop(ctx, surface.StopControl{
		Channel: turn.Channel, Thread: turn.Thread,
		Persona: turn.Persona, TurnID: turn.TurnID,
	})
	if err != nil {
		g.log.Warn("stop control post failed", "turn", turn.TurnID, "err", err)
		return
	}
	// The turn may have ended, or been stopped by command, while the post was
	// in flight; the control must not outlive either.
	st.mu.Lock()
	ended, by := st.ended, st.by
	if !ended {
		st.ref = ref
	}
	st.mu.Unlock()
	switch {
	case ended:
		if err := stopper.Delete(ctx, ref); err != nil {
			g.log.Warn("stop control delete failed", "turn", turn.TurnID, "err", err)
		}
	case by != nil:
		_ = srf.Resolve(ctx, ref, stoppingText(by))
	}
}

// turnMessagePosted reports whether the turn's streamed message exists yet.
func (g *Gateway) turnMessagePosted(turn *turnContext) bool {
	v, ok := g.streams.Load(turn.TurnID)
	return ok && v.(*stream).Posted()
}

func stoppingText(by *surface.User) string {
	return fmt.Sprintf("Stopping… — requested by <@%s>", by.ID)
}

// stopNote is how a stopped turn's message ends, or "" when nobody asked to
// stop it.
func (turn *turnContext) stopNote(interrupted bool) string {
	turn.stop.mu.Lock()
	by := turn.stop.by
	turn.stop.mu.Unlock()
	if by == nil {
		return ""
	}
	if !interrupted {
		return fmt.Sprintf("Finished before the stop from <@%s> landed.", by.ID)
	}
	return fmt.Sprintf("Stopped by <@%s> after %s.", by.ID, time.Since(turn.StartedAt).Round(time.Second))
}

// OnStop handles a press of a turn's Stop control, or of the surface's own
// stop button on a working thread.
func (g *Gateway) OnStop(ctx context.Context, r surface.StopRequest) {
	if r.TurnID == "" {
		g.onNativeStop(ctx, r)
		return
	}
	v, ok := g.turns.Load(r.TurnID)
	if !ok {
		// Finished, or from a previous gateway process: either way the control
		// is spent and should stop looking live.
		if srf, ok := g.surfaceFor(r.Surface); ok {
			_ = srf.Resolve(ctx, r.Ref, "This turn already finished.")
		}
		return
	}
	turn := v.(*turnContext)
	if msg := g.stopTurn(ctx, turn, r.User); msg != "" {
		if srf, ok := g.surfaceFor(turn.Surface); ok {
			_, _ = srf.Post(ctx, surface.Post{
				Channel: turn.Channel, Thread: turn.Thread,
				Text: msg, Ephemeral: true, User: r.User.ID,
			})
		}
	}
}

// onNativeStop handles the surface's own stop button, which names a thread
// rather than a turn.
func (g *Gateway) onNativeStop(ctx context.Context, r surface.StopRequest) {
	key := threadKey(r.Surface, r.Channel, r.Thread)
	turn := g.runningTurnIn(key)
	if turn == nil {
		// Nothing to stop, but the surface is still showing the thread as
		// working until told otherwise.
		g.clearWorking(key)
		return
	}
	if r.StreamStopped {
		if v, ok := g.streams.Load(turn.TurnID); ok {
			v.(*stream).SurfaceStopped()
		}
	}
	if msg := g.stopTurn(ctx, turn, r.User); msg != "" {
		if srf, ok := g.surfaceFor(turn.Surface); ok {
			_, _ = srf.Post(ctx, surface.Post{
				Channel: turn.Channel, Thread: turn.Thread,
				Text: msg, Ephemeral: true, User: r.User.ID,
			})
		}
	}
}

// runningTurnIn finds the thread's turn in flight, if any.
func (g *Gateway) runningTurnIn(key string) *turnContext {
	var found *turnContext
	g.turns.Range(func(_, value any) bool {
		turn := value.(*turnContext)
		if turn.ThreadID == key {
			found = turn
			return false
		}
		return true
	})
	return found
}

// stopTurn asks the runner to stop a turn. It returns a message for the person
// who asked when the stop could not be sent, and "" when it was.
func (g *Gateway) stopTurn(ctx context.Context, turn *turnContext, user surface.User) string {
	st := &turn.stop
	st.mu.Lock()
	switch {
	case st.ended:
		st.mu.Unlock()
		return "This turn already finished."
	case st.by != nil:
		st.mu.Unlock()
		return "Already stopping."
	}
	if turn.queued.Load() {
		st.mu.Unlock()
		return fmt.Sprintf("`%s` is offline and this message is still queued for it, so there is nothing running to stop yet.", turn.Runner)
	}
	conn, ok := g.canCancel(turn.Runner)
	if !ok {
		st.mu.Unlock()
		if conn == nil {
			return fmt.Sprintf("`%s` is not connected, so it cannot be told to stop.", turn.Runner)
		}
		return fmt.Sprintf("`%s` runs an older splitscreen that cannot stop a turn; update it, or use `!new` to start over.", turn.Runner)
	}
	st.by = &user
	ref := st.ref
	st.mu.Unlock()

	if err := conn.Send(&protocol.TurnCancel{
		ThreadID: turn.ThreadID, TurnID: turn.TurnID,
		By: &protocol.UserRef{ID: user.ID, Display: user.Display},
	}); err != nil {
		st.mu.Lock()
		st.by = nil
		st.mu.Unlock()
		g.log.Error("turn cancel send failed", "runner", turn.Runner, "err", err)
		return "Could not reach `" + turn.Runner + "` to stop it: " + err.Error()
	}
	_ = g.store.Log(store.Event{
		Kind: "turn.stop_requested", Runner: turn.Runner, ThreadID: turn.ThreadID,
		TurnID: turn.TurnID, SurfaceUser: user.ID,
	})

	if srf, ok := g.surfaceFor(turn.Surface); ok && ref != (surface.Ref{}) {
		if err := srf.Resolve(ctx, ref, stoppingText(&user)); err != nil {
			g.log.Warn("stop control update failed", "turn", turn.TurnID, "err", err)
		}
	}
	g.denyPromptsFor(ctx, turn, user)
	return ""
}

// denyPromptsFor resolves a stopped turn's open permission prompts, so they do
// not keep live buttons for a turn that is no longer running.
func (g *Gateway) denyPromptsFor(ctx context.Context, turn *turnContext, user surface.User) {
	g.prompts.Range(func(key, value any) bool {
		p := value.(*pendingPrompt)
		if p.Turn.TurnID != turn.TurnID {
			return true
		}
		if _, loaded := g.prompts.LoadAndDelete(key); !loaded {
			return true
		}
		reason := "turn stopped by " + user.ID
		if conn, ok := g.hub.Get(p.Runner); ok {
			_ = conn.Send(&protocol.PermissionResponse{
				RequestID: p.RequestID, Decision: protocol.DecisionDeny, Reason: reason,
			})
		}
		_ = g.store.RecordPermissionDecision(p.RequestID, string(protocol.DecisionDeny), user.ID, reason, false)
		if srf, ok := g.surfaceFor(p.Turn.Surface); ok {
			_ = srf.Resolve(ctx, p.Ref, fmt.Sprintf("`%s` — cancelled: turn stopped by <@%s>", p.Tool, user.ID))
		}
		return true
	})
}

// settleStop retires a turn's Stop control when the turn ends, however it
// ends. The control is always deleted: how the turn ended belongs on the turn's
// own message (noted reports it was put there as a footer), and is posted on its
// own only when there is no message to carry it.
func (g *Gateway) settleStop(ctx context.Context, turn *turnContext, interrupted, noted bool) {
	note := turn.stopNote(interrupted)
	st := &turn.stop
	st.mu.Lock()
	if st.ended {
		st.mu.Unlock()
		return
	}
	st.ended = true
	if st.timer != nil {
		st.timer.Stop()
	}
	ref := st.ref
	st.mu.Unlock()

	srf, ok := g.surfaceFor(turn.Surface)
	if !ok {
		return
	}
	if ref != (surface.Ref{}) {
		deleted := false
		if stopper, ok := srf.(surface.Stopper); ok {
			if err := stopper.Delete(ctx, ref); err != nil {
				g.log.Warn("stop control delete failed", "turn", turn.TurnID, "err", err)
			} else {
				deleted = true
			}
		}
		if !deleted {
			// A control that cannot be deleted must at least stop looking live.
			text := note
			if text == "" || noted {
				text = "Finished."
			}
			_ = srf.Resolve(ctx, ref, text)
			return
		}
	}
	if note != "" && !noted {
		_, _ = srf.Post(ctx, surface.Post{
			Channel: turn.Channel, Thread: turn.Thread, Persona: turn.Persona, Text: "_" + note + "_",
		})
	}
}

// stopThread is `!stop`: stop the thread's running turn and drop anything
// parked behind it, the way Esc in a terminal also discards queued input.
func (g *Gateway) stopThread(ctx context.Context, in surface.Inbound, key string) {
	running := g.runningTurnIn(key)

	dropped := g.dropParked(ctx, key, in.User)

	if running != nil {
		if msg := g.stopTurn(ctx, running, in.User); msg != "" {
			g.notice(ctx, in, msg)
		}
	}
	switch {
	case running == nil && dropped == 0:
		g.notice(ctx, in, "Nothing is running in this thread.")
	case dropped > 0:
		g.notice(ctx, in, fmt.Sprintf("Dropped %d queued message(s) in this thread.", dropped))
	}
}

// dropParked removes a thread's messages that have not started: those parked
// behind its running turn, and one waiting for a runner slot. A message waiting
// for a slot holds the thread's turn slot without a turn, so dropping it also
// releases that.
func (g *Gateway) dropParked(ctx context.Context, key string, by surface.User) int {
	g.queuesMu.Lock()
	parked := g.threadWaiting[key]
	delete(g.threadWaiting, key)
	var slotted []*queuedTurn
	for _, q := range g.queues {
		kept := q.waiting[:0]
		for _, qt := range q.waiting {
			if threadKey(qt.in.Surface, qt.in.Channel, qt.in.Thread) == key {
				slotted = append(slotted, qt)
				continue
			}
			kept = append(kept, qt)
		}
		q.waiting = kept
	}
	if len(slotted) > 0 {
		delete(g.threadActive, key)
	}
	g.queuesMu.Unlock()

	text := fmt.Sprintf("⏹ Dropped — <@%s> stopped this thread.", by.ID)
	for _, qt := range append(parked, slotted...) {
		if qt.noticeRef == (surface.Ref{}) {
			continue
		}
		if srf, ok := g.surfaceFor(qt.in.Surface); ok {
			_ = srf.Update(ctx, qt.noticeRef, surface.Post{
				Channel: qt.in.Channel, Thread: qt.in.Thread, Text: text, Persona: qt.persona,
			})
		}
	}
	if len(slotted) > 0 {
		g.clearWorking(key)
	}
	return len(parked) + len(slotted)
}
