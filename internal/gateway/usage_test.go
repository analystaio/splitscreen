package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/analystaio/splitscreen/config"
	"github.com/analystaio/splitscreen/internal/surface"
	"github.com/analystaio/splitscreen/protocol"
)

func TestTurnRateLimitsAreRecordedPerAccount(t *testing.T) {
	h := newHarness(t)
	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)

	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U1"}, Text: "go", Addressed: true,
	})
	msg := readFrame[*protocol.Message](t, ws)

	// A refused turn: no token counts, but the reading still counts.
	resets := time.Now().Add(3 * time.Hour).Unix()
	send(t, ws, &protocol.Usage{
		ThreadID: msg.ThreadID, TurnID: msg.TurnID, Known: false,
		RateLimits: []protocol.RateLimit{
			{Window: "five_hour", Utilization: 0.01, ResetsAt: resets, Status: "allowed"},
			{Window: "seven_day", Utilization: 1, ResetsAt: resets, Status: "rejected"},
		},
	})
	send(t, ws, &protocol.Done{ThreadID: msg.ThreadID, TurnID: msg.TurnID})

	eventually(t, "rate limits recorded", func() bool {
		rows, err := h.st.RateLimits()
		return err == nil && len(rows) == 2
	})
	rows, _ := h.st.RateLimits()
	// alpha has no harness secret, so it is its own account.
	if r := rows[1]; r.Account != "alpha" || r.Runner != "alpha" || r.Window != "seven_day" ||
		r.Status != "rejected" || r.ResetsAt.Unix() != resets {
		t.Errorf("seven_day row = %+v", r)
	}
}

func TestAccountIsTheHarnessSecret(t *testing.T) {
	if got := accountFor("box-a", &config.Runner{HarnessSecret: "token-1"}); got != "token-1" {
		t.Errorf("got %q, want the harness secret", got)
	}
	if got := accountFor("box-a", &config.Runner{}); got != "box-a" {
		t.Errorf("got %q, want the runner name", got)
	}
}
