package gateway

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/analystaio/splitscreen/config"
	"github.com/analystaio/splitscreen/internal/surface"
)

// The working indicator: while a thread has a turn in flight (or a message
// waiting on a machine or a free slot), the surface shows "<app> is working…"
// in that thread. It is cosmetic and must behave like it — never blocking a
// turn, never failing one, never retried into a rate limit:
//
//   - Every call goes through one worker goroutine, in order, so a clear can
//     never overtake the set it cancels, and nothing on the turn path waits
//     for the surface.
//   - A permanent failure (surface.ErrStatusUnavailable: a missing scope, a
//     channel the app may not set status in) is logged once and latches the
//     indicator off for that channel. Transient failures are dropped.
//   - Slack expires the indicator after two minutes without a message, and
//     clears it whenever the app posts in the thread — which the streamed
//     answer does. So a running turn's indicator is re-set on its activity
//     (at most every workingRefreshMin) and by a ticker for quiet stretches.

const (
	// workingRefreshMin rate-limits re-sets driven by turn activity.
	workingRefreshMin = 30 * time.Second
	// workingTick re-sets indicators that saw no activity, inside Slack's
	// two-minute expiry.
	workingTick = 60 * time.Second
	// workingWaitFor bounds how long a waiting indicator (machine starting,
	// queued for a slot) is kept alive. A box that never comes back must not
	// leave a thread saying it is starting forever.
	workingWaitFor     = 10 * time.Minute
	workingCallTimeout = 5 * time.Second
	workingQueueDepth  = 256

	// Texts for the waiting states. Rendered after the app's name.
	workingStartingText = "is starting up…"
	workingQueuedText   = "is waiting for a free slot…"
)

type workingEntry struct {
	surface string
	status  surface.Status
	// running marks a dispatched turn: refreshed for as long as it lasts.
	// Waiting states are refreshed only within workingWaitFor of being set.
	running  bool
	setAt    time.Time
	lastSent time.Time
}

type workingOp struct {
	surface string
	status  surface.Status
}

type workingTracker struct {
	mu      sync.Mutex
	entries map[string]*workingEntry // thread key -> indicator
	latched map[string]bool          // "surface:channel" -> permanently off
	ops     chan workingOp
}

func newWorkingTracker() *workingTracker {
	return &workingTracker{
		entries: map[string]*workingEntry{},
		latched: map[string]bool{},
		ops:     make(chan workingOp, workingQueueDepth),
	}
}

// setWorking shows text in a thread. An empty text (the runner turned the
// indicator off) does nothing.
func (g *Gateway) setWorking(key, surfaceName, channel, thread string, persona surface.Persona, text string, running bool) {
	if text == "" {
		return
	}
	w := g.working
	now := time.Now()
	st := surface.Status{Channel: channel, Thread: thread, Text: text, Persona: persona}
	w.mu.Lock()
	if w.latched[surfaceName+":"+channel] {
		w.mu.Unlock()
		return
	}
	w.entries[key] = &workingEntry{surface: surfaceName, status: st, running: running, setAt: now, lastSent: now}
	w.mu.Unlock()
	g.sendWorking(workingOp{surface: surfaceName, status: st})
}

// setWorkingForTurn is setWorking for a dispatched turn, with the runner's text.
func (g *Gateway) setWorkingForTurn(turn *turnContext) {
	rc, ok := g.runnerConfig(turn.Runner)
	if !ok {
		return
	}
	g.setWorking(turn.ThreadID, turn.Surface, turn.Channel, turn.Thread, turn.Persona, rc.WorkingText(), true)
}

// setWaiting shows a waiting state (machine starting, queued for a slot) for a
// message that has not reached its runner yet, unless the runner turned the
// indicator off.
func (g *Gateway) setWaiting(in surface.Inbound, rc *config.Runner, text string) {
	if rc.WorkingText() == "" {
		return
	}
	g.setWorking(threadKey(in.Surface, in.Channel, in.Thread), in.Surface, in.Channel, in.Thread,
		personaFor(rc), text, false)
}

// touchWorking re-sets a thread's indicator on turn activity, rate-limited.
func (g *Gateway) touchWorking(key string) {
	w := g.working
	w.mu.Lock()
	e, ok := w.entries[key]
	if !ok || time.Since(e.lastSent) < workingRefreshMin {
		w.mu.Unlock()
		return
	}
	e.lastSent = time.Now()
	op := workingOp{surface: e.surface, status: e.status}
	w.mu.Unlock()
	g.sendWorking(op)
}

// clearWorking removes a thread's indicator. Call it before anything that may
// start the thread's next turn, or the clear lands after that turn's set.
func (g *Gateway) clearWorking(key string) {
	w := g.working
	w.mu.Lock()
	e, ok := w.entries[key]
	delete(w.entries, key)
	w.mu.Unlock()
	if !ok {
		return
	}
	st := e.status
	st.Text = ""
	g.sendWorking(workingOp{surface: e.surface, status: st})
}

// refreshWorking re-sets every indicator Slack would otherwise expire, and
// forgets waiting ones past their window (they then expire on their own).
func (g *Gateway) refreshWorking() {
	w := g.working
	now := time.Now()
	var due []workingOp
	w.mu.Lock()
	for key, e := range w.entries {
		if !e.running && now.Sub(e.setAt) > workingWaitFor {
			delete(w.entries, key)
			continue
		}
		if now.Sub(e.lastSent) >= workingTick-5*time.Second {
			e.lastSent = now
			due = append(due, workingOp{surface: e.surface, status: e.status})
		}
	}
	w.mu.Unlock()
	for _, op := range due {
		g.sendWorking(op)
	}
}

// sendWorking hands a call to the worker. A full queue drops it: the indicator
// is cosmetic and the next refresh repairs it.
func (g *Gateway) sendWorking(op workingOp) {
	select {
	case g.working.ops <- op:
	default:
		g.log.Debug("working indicator queue full; dropping an update")
	}
}

// runWorking is the worker. It lives for the life of the process.
func (g *Gateway) runWorking() {
	for op := range g.working.ops {
		srf, ok := g.surfaceFor(op.surface)
		if !ok {
			continue
		}
		st, ok := srf.(surface.Statuser)
		if !ok {
			continue
		}
		latchKey := op.surface + ":" + op.status.Channel
		g.working.mu.Lock()
		latched := g.working.latched[latchKey]
		g.working.mu.Unlock()
		if latched {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), workingCallTimeout)
		err := st.SetStatus(ctx, op.status)
		cancel()
		if err == nil {
			continue
		}
		if errors.Is(err, surface.ErrStatusUnavailable) {
			g.working.mu.Lock()
			g.working.latched[latchKey] = true
			for key, e := range g.working.entries {
				if e.surface == op.surface && e.status.Channel == op.status.Channel {
					delete(g.working.entries, key)
				}
			}
			g.working.mu.Unlock()
			g.log.Warn("working indicator unavailable in this channel; not trying again until restart",
				"surface", op.surface, "channel", op.status.Channel, "err", err)
			continue
		}
		g.log.Debug("working indicator update failed", "channel", op.status.Channel, "err", err)
	}
}

// tickWorking drives refreshWorking until ctx ends.
func (g *Gateway) tickWorking(ctx context.Context) {
	t := time.NewTicker(workingTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.refreshWorking()
		}
	}
}
