package runner

import (
	"context"
	"errors"
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
