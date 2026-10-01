package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/analystaio/splitscreen/config"
	"github.com/analystaio/splitscreen/internal/store"
	"github.com/analystaio/splitscreen/internal/surface"
	"github.com/analystaio/splitscreen/internal/wake"
)

// Wake-on-message: when a message queues for an offline runner whose config
// names a machine to start, the gateway starts it and says so in the thread.
// The queue is what makes this work — the message was always going to wait for
// the runner to connect; waking just means something is now bringing it back.

const (
	// wakeInterval bounds start requests per runner. A burst of messages to a
	// sleeping host is one boot, not one API call each.
	wakeInterval = 2 * time.Minute
	// wakeCallTimeout bounds a single start request, which runs on the path
	// that answers the message.
	wakeCallTimeout = 15 * time.Second
	// Defaults for retrying a host caught mid-shutdown. EC2 refuses to start
	// an instance while it is stopping, and a host that has just idled out is
	// exactly when someone sends it a message.
	defaultWakeRetryEvery = 15 * time.Second
	defaultWakeRetryFor   = 5 * time.Minute
)

// wakeState is per runner.
type wakeState struct {
	last     time.Time // last start request
	retrying bool      // a stop-then-start retry loop is running
	// notices are the in-thread messages announcing the wake, edited when the
	// outcome changes and again when the runner connects.
	notices []wakeNotice
}

type wakeNotice struct {
	surface string
	channel string
	thread  string
	persona surface.Persona
	ref     surface.Ref
}

type wakeKind int

const (
	wakeStarted          wakeKind = iota // was stopped; this request started it
	wakeAlreadyUp                        // pending or running: on its way already
	wakeAlreadyRequested                 // started within wakeInterval
	wakeWaitingForStop                   // still stopping; a retry will start it
	wakeFailed
)

type wakeStatus struct {
	kind wakeKind
	err  error
}

// requestWake starts the runner's machine, at most once per wakeInterval.
func (g *Gateway) requestWake(ctx context.Context, runner string, rc *config.Runner) wakeStatus {
	if g.waker == nil {
		return wakeStatus{kind: wakeFailed, err: errors.New("this gateway has no way to start machines")}
	}
	g.wakeMu.Lock()
	st := g.wakeStateFor(runner)
	if st.retrying {
		g.wakeMu.Unlock()
		return wakeStatus{kind: wakeWaitingForStop}
	}
	if !st.last.IsZero() && time.Since(st.last) < wakeInterval {
		g.wakeMu.Unlock()
		return wakeStatus{kind: wakeAlreadyRequested}
	}
	st.last = time.Now()
	g.wakeMu.Unlock()

	status := g.startMachine(ctx, runner, rc)
	if status.kind == wakeWaitingForStop {
		g.wakeMu.Lock()
		st.retrying = true
		g.wakeMu.Unlock()
		go g.retryWakeAfterStop(runner, rc)
	}
	return status
}

// startMachine issues one start request and records it.
func (g *Gateway) startMachine(ctx context.Context, runner string, rc *config.Runner) wakeStatus {
	cctx, cancel := context.WithTimeout(ctx, wakeCallTimeout)
	defer cancel()
	res, err := g.waker.Start(cctx, rc.Wake.Region, rc.Wake.EC2Instance)

	var status wakeStatus
	switch {
	case errors.Is(err, wake.ErrStopping):
		status = wakeStatus{kind: wakeWaitingForStop}
	case err != nil:
		status = wakeStatus{kind: wakeFailed, err: err}
	case res.Started():
		status = wakeStatus{kind: wakeStarted}
	default:
		status = wakeStatus{kind: wakeAlreadyUp}
	}

	detail := map[string]any{"instance": rc.Wake.EC2Instance, "previous_state": res.PreviousState}
	if err != nil {
		detail["error"] = err.Error()
		g.log.Warn("wake failed", "runner", runner, "instance", rc.Wake.EC2Instance, "err", err)
	} else {
		g.log.Info("wake requested", "runner", runner, "instance", rc.Wake.EC2Instance,
			"previous_state", res.PreviousState)
	}
	_ = g.store.Log(store.Event{Kind: "runner.wake", Runner: runner, Detail: detail})
	return status
}

// retryWakeAfterStop keeps asking until a stopping machine can be started, the
// runner turns up on its own, or the retry window closes.
func (g *Gateway) retryWakeAfterStop(runner string, rc *config.Runner) {
	ctx := context.Background()
	finish := func(text string) {
		g.wakeMu.Lock()
		st := g.wakeStateFor(runner)
		st.retrying = false
		st.last = time.Now()
		g.wakeMu.Unlock()
		if text != "" {
			g.editWakeNotices(ctx, runner, text)
		}
	}

	deadline := time.Now().Add(g.wakeRetryFor)
	for time.Now().Before(deadline) {
		time.Sleep(g.wakeRetryEvery)
		if _, online := g.hub.Get(runner); online {
			finish("")
			return
		}
		status := g.startMachine(ctx, runner, rc)
		if status.kind == wakeWaitingForStop {
			continue
		}
		finish(wakeText(runner, status, -1))
		return
	}
	finish(wakeText(runner, wakeStatus{kind: wakeFailed,
		err: fmt.Errorf("it was still shutting down after %s", g.wakeRetryFor)}, -1))
}

// postWakeNotice announces the wake in-thread. Unlike the plain offline notice
// this one is visible to the whole channel and edited as things progress: a
// machine booting is news for everyone waiting on it, not just the sender.
func (g *Gateway) postWakeNotice(ctx context.Context, in surface.Inbound, runner string, persona surface.Persona, text string) {
	srf, ok := g.surfaceFor(in.Surface)
	if !ok {
		return
	}
	ref, err := srf.Post(ctx, surface.Post{
		Channel: in.Channel, Thread: in.Thread, Text: text, Persona: persona,
	})
	if err != nil {
		g.log.Warn("wake notice post failed", "runner", runner, "err", err)
		return
	}
	g.wakeMu.Lock()
	st := g.wakeStateFor(runner)
	st.notices = append(st.notices, wakeNotice{
		surface: in.Surface, channel: in.Channel, thread: in.Thread, persona: persona, ref: ref,
	})
	g.wakeMu.Unlock()
}

func (g *Gateway) editWakeNotices(ctx context.Context, runner, text string) {
	g.wakeMu.Lock()
	notices := append([]wakeNotice(nil), g.wakeStateFor(runner).notices...)
	g.wakeMu.Unlock()
	for _, n := range notices {
		srf, ok := g.surfaceFor(n.surface)
		if !ok {
			continue
		}
		if err := srf.Update(ctx, n.ref, surface.Post{
			Channel: n.channel, Thread: n.thread, Text: text, Persona: n.persona,
		}); err != nil {
			g.log.Warn("wake notice update failed", "runner", runner, "err", err)
		}
	}
}

// wakeConnected closes out a wake when the runner arrives: every notice is
// edited to say so, and the rate limit resets so the next sleep wakes promptly.
func (g *Gateway) wakeConnected(ctx context.Context, runner string) {
	g.wakeMu.Lock()
	st, ok := g.wakes[runner]
	if !ok {
		g.wakeMu.Unlock()
		return
	}
	notices := st.notices
	delete(g.wakes, runner)
	g.wakeMu.Unlock()
	if len(notices) == 0 {
		return
	}
	text := fmt.Sprintf("✅ `%s` is awake — running your message now.", runner)
	for _, n := range notices {
		if srf, ok := g.surfaceFor(n.surface); ok {
			_ = srf.Update(ctx, n.ref, surface.Post{
				Channel: n.channel, Thread: n.thread, Text: text, Persona: n.persona,
			})
		}
	}
}

// wakeStateFor returns the runner's state, creating it. Caller holds wakeMu.
func (g *Gateway) wakeStateFor(runner string) *wakeState {
	st, ok := g.wakes[runner]
	if !ok {
		st = &wakeState{}
		g.wakes[runner] = st
	}
	return st
}

// wakeText renders a wake notice. depth < 0 omits the queue count, for edits
// made after other messages may have joined the queue.
func wakeText(runner string, s wakeStatus, depth int) string {
	queued := "Your message is queued"
	if depth > 0 {
		queued = fmt.Sprintf("Your message is queued (%d waiting)", depth)
	}
	switch s.kind {
	case wakeStarted:
		return fmt.Sprintf("💤 `%s` is asleep — starting it now. %s and runs as soon as it connects, usually within 1–2 minutes.", runner, queued)
	case wakeAlreadyUp, wakeAlreadyRequested:
		return fmt.Sprintf("⏳ `%s` is starting up. %s and runs as soon as it connects.", runner, queued)
	case wakeWaitingForStop:
		return fmt.Sprintf("💤 `%s` is still shutting down — I'll start it again as soon as it has stopped. %s.", runner, queued)
	default:
		return fmt.Sprintf("⚠️ `%s` is offline and could not be started: %v. %s and runs whenever it next connects.", runner, s.err, queued)
	}
}
