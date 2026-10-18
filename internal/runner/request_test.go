package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A reply that arrives before send even returns — an auto-approved permission
// answered at once — must still reach the caller. With the waiter registered
// after sending, it was dropped and the caller waited out its deadline.
func TestReplyDuringSendIsNotLost(t *testing.T) {
	r := &Runner{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	v, err := r.requestVia(ctx, "req-1", func() error {
		r.resolvePending("req-1", "answer")
		return nil
	})
	if err != nil {
		t.Fatalf("reply was lost: %v", err)
	}
	if v != "answer" {
		t.Fatalf("reply = %v", v)
	}
}

// A failed send returns at once and leaves no waiter behind.
func TestFailedSendLeavesNoWaiter(t *testing.T) {
	r := &Runner{}
	boom := errors.New("connection closed")
	if _, err := r.requestVia(context.Background(), "req-2", func() error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, ok := r.pending.Load("req-2"); ok {
		t.Fatal("a waiter outlived its failed send")
	}
}

// send-file used to report success when it had no thread or turn: the gateway
// then dropped the file with nowhere to deliver it. It must fail visibly.
func TestSendFileNeedsThreadAndTurn(t *testing.T) {
	r := testRunner(t)
	path := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(path, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := r.sendFile(context.Background(), "", path, "")
	if err == nil || !strings.Contains(err.Error(), "no thread") {
		t.Errorf("want a no-thread error, got %v", err)
	}
	err = r.sendFile(context.Background(), "slack:C1:1.2", path, "")
	if err == nil || !strings.Contains(err.Error(), "no running turn") {
		t.Errorf("want a no-running-turn error, got %v", err)
	}
}
