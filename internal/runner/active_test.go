package runner

import (
	"os"
	"testing"
	"time"
)

func TestActiveMarkerFollowsTurns(t *testing.T) {
	r := testRunner(t)
	ts := &threadSession{threadID: "t1", lastActivity: time.Now()}
	r.sessions.Store("t1", ts)

	ts.setTurn("turn-1")
	r.touchActive()
	fi, err := os.Stat(r.activePath())
	if err != nil {
		t.Fatalf("no marker while a turn is in flight: %v", err)
	}
	if time.Since(fi.ModTime()) > time.Second {
		t.Errorf("marker mtime is stale: %s", fi.ModTime())
	}

	// Events inside the throttle window do not rewrite it; a later one does.
	r.activeTouched.Store(time.Now().Add(-activeThrottle - time.Second).UnixNano())
	old := time.Now().Add(-time.Minute)
	_ = os.Chtimes(r.activePath(), old, old)
	r.markActive()
	fi, _ = os.Stat(r.activePath())
	if time.Since(fi.ModTime()) > time.Second {
		t.Error("a harness event past the throttle did not refresh the marker")
	}

	ts.endTurn()
	r.settleActive()
	if _, err := os.Stat(r.activePath()); !os.IsNotExist(err) {
		t.Error("marker survived the last turn ending")
	}
}
