package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/analystaio/splitscreen/internal/forge"
	"github.com/analystaio/splitscreen/internal/pricing"
	"github.com/analystaio/splitscreen/internal/store"
	"github.com/analystaio/splitscreen/internal/surface"
	"github.com/analystaio/splitscreen/protocol"
)

// dispatch routes one decoded frame from a runner.
func (g *Gateway) dispatch(ctx context.Context, c *Conn, f protocol.Frame) {
	switch fr := f.(type) {
	case *protocol.Pong:
		// lastSeen already updated by the read loop.
	case *protocol.Ping:
		_ = c.Send(&protocol.Pong{Nonce: fr.Nonce})
	case *protocol.TextDelta:
		g.onTextDelta(fr)
	case *protocol.Thought:
		g.onThought(fr)
	case *protocol.ToolStart:
		g.onToolStart(fr)
	case *protocol.ToolEnd:
		g.onToolEnd(fr)
	case *protocol.PermissionRequest:
		g.onPermissionRequest(ctx, c, fr)
	case *protocol.MCPCall:
		go g.onMCPCall(ctx, c, fr)
	case *protocol.CredentialRequest:
		go g.onCredentialRequest(ctx, c, fr)
	case *protocol.Usage:
		g.onUsage(c, fr)
	case *protocol.Done:
		g.onDone(ctx, c, fr)
	case *protocol.Error:
		g.onRunnerError(ctx, c, fr)
	case *protocol.BlobBegin:
		g.onBlobBegin(c, fr)
	case *protocol.BlobEnd:
		g.onBlobEnd(ctx, c, fr)
	default:
		g.log.Warn("unhandled frame", "runner", c.runner, "type", f.Type())
	}
}

func (g *Gateway) turnFor(turnID string) (*turnContext, bool) {
	v, ok := g.turns.Load(turnID)
	if !ok {
		return nil, false
	}
	turn := v.(*turnContext)
	turn.touch() // any frame referencing the turn is activity; feeds the stranded-turn sweep
	g.touchWorking(turn.ThreadID)
	return turn, true
}

func (g *Gateway) onTextDelta(fr *protocol.TextDelta) {
	turn, ok := g.turnFor(fr.TurnID)
	if !ok {
		return
	}
	g.streamFor(turn).AppendText(fr.Text)
}

func (g *Gateway) onThought(fr *protocol.Thought) {
	turn, ok := g.turnFor(fr.TurnID)
	if !ok {
		return
	}
	g.streamFor(turn).Thought(fr.Text)
	_ = g.store.Log(store.Event{
		Kind: "thought", Runner: turn.Runner, ThreadID: turn.ThreadID,
		TurnID: fr.TurnID,
		Detail: map[string]any{"text": fr.Text},
	})
}

func (g *Gateway) onToolStart(fr *protocol.ToolStart) {
	turn, ok := g.turnFor(fr.TurnID)
	if !ok {
		return
	}
	if err := g.store.IncrementToolCalls(fr.TurnID); err != nil {
		g.log.Warn("tool count update failed", "turn", fr.TurnID, "err", err)
	}
	// Title and detail stay separate: a surface that renders steps natively
	// shows the tool as the card's title and the arguments underneath, and one
	// that cannot joins them back into a line.
	g.streamFor(turn).StartStep(fr.CallID, fr.Tool, truncateLine(fr.Summary, 200))
	_ = g.store.Log(store.Event{
		Kind: "tool.start", Runner: turn.Runner, ThreadID: turn.ThreadID,
		TurnID: fr.TurnID, SurfaceUser: turn.User.ID,
		Detail: map[string]any{"tool": fr.Tool, "call": fr.CallID, "summary": fr.Summary},
	})
}

func (g *Gateway) onToolEnd(fr *protocol.ToolEnd) {
	turn, ok := g.turnFor(fr.TurnID)
	if !ok {
		return
	}
	g.streamFor(turn).EndStep(fr.CallID, fr.OK, truncateLine(fr.Error, 160), fr.DurationMS)
	_ = g.store.Log(store.Event{
		Kind: "tool.end", Runner: turn.Runner, ThreadID: turn.ThreadID,
		TurnID: fr.TurnID,
		Detail: map[string]any{"call": fr.CallID, "ok": fr.OK, "error": fr.Error, "duration_ms": fr.DurationMS},
	})
}

// ---------------------------------------------------------------------------
// Permissions
// ---------------------------------------------------------------------------

type pendingPrompt struct {
	RequestID string
	Runner    string
	Turn      *turnContext
	Ref       surface.Ref
	Tool      string
	CreatedAt time.Time
}

func (g *Gateway) onPermissionRequest(ctx context.Context, c *Conn, fr *protocol.PermissionRequest) {
	turn, ok := g.turnFor(fr.TurnID)
	if !ok {
		// Without a turn there is nowhere to ask, and defaulting to allow would
		// be exactly the wrong failure direction.
		_ = c.Send(&protocol.PermissionResponse{
			RequestID: fr.RequestID, Decision: protocol.DecisionDeny,
			PolicyDenied: true, Reason: "no live turn for this request",
		})
		return
	}

	if err := g.store.RecordPermissionRequest(fr.RequestID, turn.ThreadID, fr.TurnID,
		c.runner, fr.Tool, string(fr.Input)); err != nil {
		g.log.Error("record permission request failed", "err", err)
	}

	rc, _ := g.runnerConfig(c.runner)

	// Policy is evaluated before the prompt is posted. A denied tool is never
	// offered to a human, so no click can approve it.
	if rc != nil {
		if rule, denied := MatchDeny(rc.Policy.Deny, fr.Tool, fr.Input); denied {
			reason := "denied by gateway policy: " + rule
			_ = c.Send(&protocol.PermissionResponse{
				RequestID: fr.RequestID, Decision: protocol.DecisionDeny,
				PolicyDenied: true, Reason: reason,
			})
			_ = g.store.RecordPermissionDecision(fr.RequestID, string(protocol.DecisionDeny), "", reason, true)
			_ = g.store.Log(store.Event{
				Kind: "permission.policy_denied", Runner: c.runner, ThreadID: turn.ThreadID,
				TurnID: fr.TurnID, Detail: map[string]any{"tool": fr.Tool, "rule": rule},
			})
			g.streamFor(turn).NoteStep("policy_"+fr.RequestID,
				"blocked by policy: "+fr.Tool+" ("+rule+")", true)
			return
		}
	}

	// Everything below removes a click, never a boundary: a denied tool has
	// already returned above and cannot reach here.
	autoAllow := func(reason, kind string) {
		_ = c.Send(&protocol.PermissionResponse{
			RequestID: fr.RequestID, Decision: protocol.DecisionAllow,
			AutoApproved: true, Reason: reason,
		})
		_ = g.store.RecordPermissionDecision(fr.RequestID, string(protocol.DecisionAllow), "", reason, false)
		_ = g.store.Log(store.Event{
			Kind: kind, Runner: c.runner, ThreadID: turn.ThreadID,
			TurnID: fr.TurnID, SurfaceUser: turn.User.ID,
			Detail: map[string]any{"tool": fr.Tool, "reason": reason},
		})
	}

	// A grant someone issued earlier in this thread.
	if gr, ok := g.grants.Held(turn.ThreadID, fr.Tool); ok {
		autoAllow("allowed for this session by <@"+gr.By+">", "permission.session_grant")
		return
	}

	// Static policy: an allow rule, or a runner that runs unattended.
	if rc != nil {
		if reason, auto := autoApprove(rc, fr.Tool, fr.Input); auto {
			autoAllow(reason, "permission.auto_approved")
			return
		}
	}

	srf, ok := g.surfaceFor(turn.Surface)
	if !ok {
		_ = c.Send(&protocol.PermissionResponse{
			RequestID: fr.RequestID, Decision: protocol.DecisionDeny,
			PolicyDenied: true, Reason: "surface unavailable",
		})
		return
	}

	// Flush pending output first so the prompt lands after the text explaining
	// why it is being asked for.
	g.streamFor(turn).flush(ctx)

	ref, err := srf.Prompt(ctx, surface.Prompt{
		Channel:   turn.Channel,
		Thread:    turn.Thread,
		Persona:   turn.Persona,
		RequestID: fr.RequestID,
		Tool:      fr.Tool,
		Summary:   fr.Summary,
		Detail:    SummarizeInput(fr.Input),
	})
	if err != nil {
		g.log.Error("permission prompt failed", "err", err)
		_ = c.Send(&protocol.PermissionResponse{
			RequestID: fr.RequestID, Decision: protocol.DecisionDeny,
			PolicyDenied: true, Reason: "could not post prompt: " + err.Error(),
		})
		return
	}

	g.prompts.Store(fr.RequestID, &pendingPrompt{
		RequestID: fr.RequestID, Runner: c.runner, Turn: turn,
		Ref: ref, Tool: fr.Tool, CreatedAt: time.Now(),
	})
}

// OnDecision handles a human resolving a permission prompt.
func (g *Gateway) OnDecision(ctx context.Context, d surface.Decision) {
	v, ok := g.prompts.Load(d.RequestID)
	if !ok {
		return // already resolved, or from a previous gateway process
	}
	p := v.(*pendingPrompt)

	rc, _ := g.runnerConfig(p.Runner)
	if !approverAllowed(rc, d.User.ID) {
		srf, ok := g.surfaceFor(p.Turn.Surface)
		if ok {
			_, _ = srf.Post(ctx, surface.Post{
				Channel: p.Turn.Channel, Thread: p.Turn.Thread,
				Text:      "You are not an approver for `" + p.Runner + "`.",
				Ephemeral: true, User: d.User.ID,
			})
		}
		_ = g.store.Log(store.Event{
			Kind: "permission.unauthorized", Runner: p.Runner, ThreadID: p.Turn.ThreadID,
			SurfaceUser: d.User.ID, Detail: map[string]any{"request": d.RequestID},
		})
		return
	}

	// Claim the prompt before acting so two fast clicks cannot both resolve it.
	if _, loaded := g.prompts.LoadAndDelete(d.RequestID); !loaded {
		return
	}

	who := d.User.Display
	if who == "" {
		who = d.User.ID
	}
	if err := g.store.RecordPermissionDecision(d.RequestID, string(d.Decision), d.User.ID, "", false); err != nil {
		g.log.Error("record decision failed", "err", err)
	}
	_ = g.store.Log(store.Event{
		Kind: "permission.decided", Runner: p.Runner, ThreadID: p.Turn.ThreadID,
		TurnID: p.Turn.TurnID, SurfaceUser: d.User.ID,
		Detail: map[string]any{"request": d.RequestID, "tool": p.Tool, "decision": string(d.Decision)},
	})

	if d.Decision == protocol.DecisionAllowSession || d.Decision == protocol.DecisionAllowAlways {
		// allow_always is no longer offered; treat a stale one as session-scoped
		// rather than silently granting more than the button now promises.
		g.grants.Grant(p.Turn.ThreadID, p.Tool, d.User.ID)
	}

	conn, online := g.hub.Get(p.Runner)
	if online {
		if err := conn.Send(&protocol.PermissionResponse{
			RequestID: d.RequestID,
			Decision:  d.Decision,
			DecidedBy: &protocol.UserRef{ID: d.User.ID, Display: d.User.Display},
		}); err != nil {
			g.log.Error("permission response send failed", "err", err)
		}
	}

	if srf, ok := g.surfaceFor(p.Turn.Surface); ok {
		text := fmt.Sprintf("`%s` — *%s* by <@%s>", p.Tool, d.Decision, d.User.ID)
		if d.Decision == protocol.DecisionAllowSession || d.Decision == protocol.DecisionAllowAlways {
			text = fmt.Sprintf("`%s` — allowed for the rest of this session by <@%s>", p.Tool, d.User.ID)
		}
		if !online {
			text += " _(runner disconnected before the decision reached it)_"
		}
		if err := srf.Resolve(ctx, p.Ref, text); err != nil {
			g.log.Warn("resolve prompt failed", "err", err)
		}
	}
}

// failPendingFor resolves outstanding prompts when a runner drops. Leaving live
// buttons for a connection that no longer exists invites a click that silently
// does nothing.
func (g *Gateway) failPendingFor(runner string) {
	ctx := context.Background()
	g.prompts.Range(func(key, value any) bool {
		p := value.(*pendingPrompt)
		if p.Runner != runner {
			return true
		}
		g.prompts.Delete(key)
		_ = g.store.RecordPermissionDecision(p.RequestID, string(protocol.DecisionDeny), "",
			"runner disconnected before a decision", true)
		if srf, ok := g.surfaceFor(p.Turn.Surface); ok {
			_ = srf.Resolve(ctx, p.Ref, fmt.Sprintf("`%s` — cancelled: `%s` disconnected", p.Tool, runner))
		}
		return true
	})
}

// reconcileTurns closes out a dropped runner's in-flight turns once it is
// clear the runner is not coming right back.
//
// The grace period exists because a network blip is not a crash: sessions live
// on the runner, and after a reconnect their events keep flowing under the
// same turn ids, so failing turns at the instant of disconnect would bounce
// work that is still happening. But past the grace window the turn can never
// finish — either the host died with the work, or it rebooted and the harness
// process is gone — and the alternative is what the dev3 hang produced: rows
// stuck "running" forever and threads that just go silent. Better to say so
// in-thread and let the sender decide when to retry.
func (g *Gateway) reconcileTurns(runner string, grace time.Duration) {
	time.Sleep(grace)
	if c, ok := g.hub.Get(runner); ok && !c.closed.Load() {
		return
	}
	ctx := context.Background()
	g.turns.Range(func(key, value any) bool {
		turn := value.(*turnContext)
		if turn.Runner != runner || turn.queued.Load() {
			return true
		}
		g.turns.Delete(key)
		g.log.Warn("failing turn: runner gone past grace",
			"runner", runner, "turn", turn.TurnID, "thread", turn.ThreadID)
		if v, loaded := g.streams.Load(turn.TurnID); loaded {
			v.(*stream).Close(ctx)
		}
		_ = g.store.FinishTurn(turn.TurnID, store.TurnError, "runner_offline: connection lost mid-turn",
			time.Since(turn.StartedAt).Milliseconds(), 0)
		if srf, ok := g.surfaceFor(turn.Surface); ok {
			_, _ = srf.Post(ctx, surface.Post{
				Channel: turn.Channel, Thread: turn.Thread, Persona: turn.Persona,
				Text: fmt.Sprintf(":warning: `%s` went offline mid-turn. Completed file changes are saved on the runner; message again to pick the session back up — if it is still down, the message will queue and deliver when it returns.", runner),
			})
		}
		g.clearWorking(turn.ThreadID)
		g.turnSlotFreed(ctx, runner)
		g.endThreadTurn(ctx, turn.ThreadID)
		return true
	})
}

// strandedTurnTimeout bounds how long a turn may go with zero upward frames
// before the sweep declares it orphaned. It sits well beyond any real tool call
// (which emit start/end frames) — a turn silent this long has lost or
// misattributed its Done. Belt-and-suspenders behind per-thread serialization.
const strandedTurnTimeout = 60 * time.Minute

// sweepStrandedTurns finalizes turns that have gone silent past the timeout,
// so a lost/misattributed Done cannot strand a turn "running" forever or leak
// its concurrency slot. Runs for the life of the gateway.
func (g *Gateway) sweepStrandedTurns(ctx context.Context) {
	t := time.NewTicker(2 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.sweepStrandedOnce(ctx, strandedTurnTimeout)
		}
	}
}

// sweepStrandedOnce finalizes every turn silent longer than timeout. Split out
// from the ticker loop so it can be tested directly.
func (g *Gateway) sweepStrandedOnce(ctx context.Context, timeout time.Duration) int {
	n := 0
	g.turns.Range(func(key, value any) bool {
		turn := value.(*turnContext)
		// A queued turn is waiting in a persisted queue for its runner to come
		// back, not stuck: finalizing it would orphan the message it delivers.
		if turn.idle() < timeout || turn.queued.Load() {
			return true
		}
		g.turns.Delete(key)
		n++
		g.log.Warn("finalizing stranded turn",
			"turn", turn.TurnID, "thread", turn.ThreadID, "runner", turn.Runner,
			"idle", turn.idle().Round(time.Second))
		if v, loaded := g.streams.Load(turn.TurnID); loaded {
			v.(*stream).Close(ctx)
		}
		_ = g.store.FinishTurn(turn.TurnID, store.TurnError,
			"stranded: no activity past timeout (Done lost or misattributed)",
			time.Since(turn.StartedAt).Milliseconds(), 0)
		g.clearWorking(turn.ThreadID)
		g.turnSlotFreed(ctx, turn.Runner)
		g.endThreadTurn(ctx, turn.ThreadID)
		return true
	})
	return n
}

// ---------------------------------------------------------------------------
// Proxied MCP
// ---------------------------------------------------------------------------

func (g *Gateway) onMCPCall(ctx context.Context, c *Conn, fr *protocol.MCPCall) {
	started := time.Now()
	turn, _ := g.turnFor(fr.TurnID)

	rec := store.MCPRecord{
		CallID: fr.CallID, Runner: c.runner, TurnID: fr.TurnID,
		Server: fr.Server, Tool: fr.Tool, Args: string(fr.Args),
	}
	if turn != nil {
		rec.ThreadID = turn.ThreadID
		rec.SurfaceUser = turn.User.ID
	}

	respond := func(result json.RawMessage, err error) {
		rec.DurationMS = time.Since(started).Milliseconds()
		rec.OK = err == nil
		if err != nil {
			rec.Error = err.Error()
		}
		if rerr := g.store.RecordMCPCall(rec); rerr != nil {
			g.log.Error("record mcp call failed", "err", rerr)
		}
		resp := &protocol.MCPResponse{CallID: fr.CallID}
		if err != nil {
			resp.Error = &protocol.RemoteError{Code: "mcp_error", Message: err.Error()}
		} else {
			if len(result) == 0 {
				result = json.RawMessage(`{}`)
			}
			resp.Result = result
		}
		if serr := c.Send(resp); serr != nil {
			g.log.Error("mcp response send failed", "err", serr)
		}
	}

	if !g.proxy.Has(fr.Server) {
		respond(nil, fmt.Errorf("server %q is not proxied by this gateway", fr.Server))
		return
	}

	callCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	result, err := g.proxy.Call(callCtx, fr.Server, fr.Tool, fr.Args)
	respond(result, err)
}

// ---------------------------------------------------------------------------
// Forge credentials
// ---------------------------------------------------------------------------

func (g *Gateway) onCredentialRequest(ctx context.Context, c *Conn, fr *protocol.CredentialRequest) {
	turn, _ := g.turnFor(fr.TurnID)
	rec := store.CredentialRecord{
		RequestID: fr.RequestID, Runner: c.runner,
		Kind: string(fr.Kind), Resource: fr.Resource, TurnID: fr.TurnID,
	}
	if turn != nil {
		rec.ThreadID = turn.ThreadID
		rec.SurfaceUser = turn.User.ID
	}

	deny := func(reason string) {
		rec.Granted = false
		rec.Reason = reason
		if err := g.store.RecordCredential(rec); err != nil {
			g.log.Error("record credential failed", "err", err)
		}
		g.log.Warn("credential denied", "runner", c.runner, "resource", fr.Resource, "reason", reason)
		_ = c.Send(&protocol.CredentialGrant{
			RequestID: fr.RequestID, Kind: fr.Kind, Denied: true, Reason: reason,
		})
	}

	if fr.Kind != protocol.CredentialForge {
		deny("only forge credentials are minted on request")
		return
	}

	rc, ok := g.runnerConfig(c.runner)
	if !ok {
		deny("runner is not configured")
		return
	}
	// Policy is checked before capability: a repository outside the allowlist is
	// refused for that reason whether or not a provider happens to be
	// configured, and the answer does not depend on gateway internals.
	// The runner's forge policy grants the installation's access. A repository
	// outside it is still readable if it hosts a plugin marketplace this
	// runner's bundle uses — read-only, so a runner can fetch its plugins but
	// never change them (or anyone else's).
	access := forge.ReadWrite
	if !RepoAllowed(rc.Policy.Forge.Repos, fr.Resource) {
		if !g.cfg.Load().MarketplaceReadable(c.runner, fr.Resource) {
			// An empty allowlist denies everything: a runner with no declared
			// repositories has no business minting git credentials.
			deny(fmt.Sprintf("repository %q is outside this runner's forge policy", fr.Resource))
			return
		}
		access = forge.ReadOnly
	}
	if g.forge == nil {
		deny("no forge provider is configured on the gateway")
		return
	}

	mintCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cred, err := g.forge.Mint(mintCtx, fr.Resource, access)
	if err != nil {
		deny("mint failed: " + err.Error())
		return
	}

	rec.Granted = true
	if !cred.ExpiresAt.IsZero() {
		exp := cred.ExpiresAt
		rec.ExpiresAt = &exp
	}
	if err := g.store.RecordCredential(rec); err != nil {
		g.log.Error("record credential failed", "err", err)
	}

	grant := &protocol.CredentialGrant{
		RequestID: fr.RequestID,
		Kind:      fr.Kind,
		Username:  cred.Username,
		Value:     cred.Token,
	}
	if !cred.ExpiresAt.IsZero() {
		exp := cred.ExpiresAt
		grant.ExpiresAt = &exp
	}
	if err := c.Send(grant); err != nil {
		g.log.Error("credential grant send failed", "err", err)
	}
}

// ---------------------------------------------------------------------------
// Accounting and completion
// ---------------------------------------------------------------------------

func (g *Gateway) onUsage(c *Conn, fr *protocol.Usage) {
	u := store.Usage{
		Model:            fr.Model,
		InputTokens:      fr.InputTokens,
		CacheWriteTokens: fr.CacheWriteTokens,
		CacheReadTokens:  fr.CacheReadTokens,
		OutputTokens:     fr.OutputTokens,
		TTLHint:          fr.TTLHint,
		Known:            fr.Known,
		ReportedUSD:      fr.ProviderCostUSD,
	}

	// Subscription runners have no marginal dollar cost; pricing them would
	// invent a number. Their scarce resource is the rate-limit window, which the
	// token counters already capture.
	rc, _ := g.runnerConfig(c.runner)
	billsInDollars := rc == nil || rc.Billing != "subscription"

	if fr.Known && billsInDollars {
		if cost, ok := g.prices.Cost(fr.Model, pricing.Counters{
			Input:      fr.InputTokens,
			CacheWrite: fr.CacheWriteTokens,
			CacheRead:  fr.CacheReadTokens,
			Output:     fr.OutputTokens,
			TTLHint:    fr.TTLHint,
		}); ok {
			u.ComputedUSD = &cost
			u.PriceTableVer = g.prices.Version
		} else {
			g.log.Warn("no price for model; turn left unpriced", "model", fr.Model, "turn", fr.TurnID)
		}
	}

	if err := g.store.RecordUsage(fr.TurnID, u); err != nil {
		g.log.Error("record usage failed", "turn", fr.TurnID, "err", err)
	}
}

func (g *Gateway) onDone(ctx context.Context, c *Conn, fr *protocol.Done) {
	turn, ok := g.turnFor(fr.TurnID)
	if !ok {
		return
	}
	defer g.turns.Delete(fr.TurnID)

	if v, loaded := g.streams.Load(fr.TurnID); loaded {
		s := v.(*stream)
		s.Close(ctx)
		if !s.HasOutput() {
			if srf, ok := g.surfaceFor(turn.Surface); ok {
				_, _ = srf.Post(ctx, surface.Post{
					Channel: turn.Channel, Thread: turn.Thread,
					Text: "_(no output)_", Persona: turn.Persona,
				})
			}
		}
	}

	if fr.SessionID != "" {
		if err := g.store.SetThreadSession(turn.ThreadID, fr.SessionID); err != nil {
			g.log.Error("set session failed", "thread", turn.ThreadID, "err", err)
		}
	}
	duration := fr.DurationMS
	if duration == 0 {
		duration = time.Since(turn.StartedAt).Milliseconds()
	}
	if err := g.store.FinishTurn(fr.TurnID, store.TurnDone, "", duration, fr.NumToolCalls); err != nil {
		g.log.Error("finish turn failed", "turn", fr.TurnID, "err", err)
	}
	_ = g.store.TouchThread(turn.ThreadID)
	g.clearWorking(turn.ThreadID)
	g.turnSlotFreed(ctx, turn.Runner)
	g.endThreadTurn(ctx, turn.ThreadID)
}

func (g *Gateway) onRunnerError(ctx context.Context, c *Conn, fr *protocol.Error) {
	g.log.Error("runner error", "runner", c.runner, "code", fr.Code, "message", fr.Message, "fatal", fr.Fatal)
	_ = g.store.Log(store.Event{
		Kind: "runner.error", Runner: c.runner, TurnID: fr.TurnID,
		Detail: map[string]any{"code": fr.Code, "message": fr.Message, "fatal": fr.Fatal},
	})

	if turn, ok := g.turnFor(fr.TurnID); ok {
		if v, loaded := g.streams.Load(fr.TurnID); loaded {
			v.(*stream).Close(ctx)
		}
		if srf, ok := g.surfaceFor(turn.Surface); ok {
			_, _ = srf.Post(ctx, surface.Post{
				Channel: turn.Channel, Thread: turn.Thread, Persona: turn.Persona,
				Text: fmt.Sprintf(":warning: `%s` — %s", fr.Code, truncateLine(fr.Message, 500)),
			})
		}
		_ = g.store.FinishTurn(fr.TurnID, store.TurnError, fr.Code+": "+fr.Message,
			time.Since(turn.StartedAt).Milliseconds(), 0)
		g.turns.Delete(fr.TurnID)
		g.clearWorking(turn.ThreadID)
		g.turnSlotFreed(ctx, turn.Runner)
		g.endThreadTurn(ctx, turn.ThreadID)
	}

	if fr.Fatal {
		c.CloseWith("fatal error reported by runner: " + fr.Code)
	}
}

func truncateLine(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
