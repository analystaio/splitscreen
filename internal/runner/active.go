package runner

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

// The activity marker lets something outside the runner — a host that stops
// itself when idle, say — tell "no turn in flight" from "a long turn with the
// harness thinking quietly", without inferring it from CPU. While any turn is
// in flight, <runtime-root>/<name>/active exists and its mtime is at most
// activeRefresh old; with no turn in flight it does not exist.

const (
	activeFile = "active"
	// activeRefresh is the heartbeat while a turn runs. Observers should treat
	// an mtime older than about twice this as stale (a runner killed mid-turn
	// cannot remove the file).
	activeRefresh = 30 * time.Second
	// activeThrottle bounds how often harness events touch the file.
	activeThrottle = 5 * time.Second
)

func (r *Runner) activePath() string {
	return filepath.Join(r.opts.RuntimeRoot, r.opts.Name, activeFile)
}

// markActive records that a turn is in flight, at most once per activeThrottle.
func (r *Runner) markActive() {
	now := time.Now().UnixNano()
	last := r.activeTouched.Load()
	if now-last < int64(activeThrottle) || !r.activeTouched.CompareAndSwap(last, now) {
		return
	}
	r.touchActive()
}

func (r *Runner) touchActive() {
	p := r.activePath()
	t := time.Now()
	if err := os.Chtimes(p, t, t); err == nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.Close()
	}
}

// anyTurnInFlight reports whether any session is mid-turn.
func (r *Runner) anyTurnInFlight() bool {
	busy := false
	r.sessions.Range(func(_, v any) bool {
		busy = v.(*threadSession).isBusy()
		return !busy
	})
	return busy
}

// maintainActive keeps the marker in step with reality: refreshed while any
// turn runs, removed when none does, and removed on shutdown.
func (r *Runner) maintainActive(ctx context.Context) {
	sync := func() {
		if r.anyTurnInFlight() {
			r.activeTouched.Store(time.Now().UnixNano())
			r.touchActive()
		} else {
			_ = os.Remove(r.activePath())
		}
	}
	sync()
	t := time.NewTicker(activeRefresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = os.Remove(r.activePath())
			return
		case <-t.C:
			sync()
		}
	}
}

// settleActive removes the marker as soon as the last turn ends, rather than
// leaving it for up to activeRefresh.
func (r *Runner) settleActive() {
	if !r.anyTurnInFlight() {
		_ = os.Remove(r.activePath())
	}
}
