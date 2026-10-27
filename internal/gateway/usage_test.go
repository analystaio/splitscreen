package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/analystaio/splitscreen/internal/store"
	"github.com/analystaio/splitscreen/internal/surface"
	"github.com/analystaio/splitscreen/protocol"
)

func TestUsageCommandShowsTheLatestReading(t *testing.T) {
	h := newHarness(t)
	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)

	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U1"}, Text: "go", Addressed: true,
	})
	msg := readFrame[*protocol.Message](t, ws)

	resets := time.Now().Add(3*time.Hour + 30*time.Minute).Unix()
	send(t, ws, &protocol.Usage{
		ThreadID: msg.ThreadID, TurnID: msg.TurnID, Known: false,
		RateLimits: []protocol.RateLimit{
			{Window: "seven_day", Utilization: 1, ResetsAt: resets, Status: "rejected"},
			{Window: "five_hour", Utilization: 0.01, ResetsAt: resets, Status: "allowed"},
		},
	})
	send(t, ws, &protocol.Done{ThreadID: msg.ThreadID, TurnID: msg.TurnID})

	eventually(t, "rate limits recorded", func() bool {
		rows, err := h.st.RateLimits()
		return err == nil && len(rows) == 2
	})

	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T2",
		User: surface.User{ID: "U1"}, Text: "!usage", Addressed: true,
	})

	h.srf.mu.Lock()
	defer h.srf.mu.Unlock()
	var text string
	for _, p := range h.srf.posts {
		if p.Ephemeral && strings.Contains(p.Text, "Subscription limits") {
			text = p.Text
		}
	}
	if text == "" {
		t.Fatalf("no ephemeral usage notice among %+v", h.srf.posts)
	}
	// alpha has no harness secret, so it is its own account. The five-hour
	// window is listed first whatever order the harness sent.
	want := "`alpha` _(this runner)_ — 5h *1%*, resets in 3h · 7d *100%* :no_entry: limit reached, resets in 3h · _as of "
	if !strings.Contains(text, want) {
		t.Errorf("usage notice:\n%s\nwant it to contain:\n%s", text, want)
	}
}

func TestUsageCommandWithoutAReading(t *testing.T) {
	h := newHarness(t)
	rc, _ := h.gw.runnerConfig("alpha")
	if text := h.gw.UsageText("alpha", rc); !strings.Contains(text, "`alpha` _(this runner)_ — _no reading yet") {
		t.Errorf("usage text:\n%s", text)
	}
}

func TestWindowPastItsResetIsNotQuotedAsCurrent(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	got := windowText(store.RateLimit{
		Window: "five_hour", Utilization: 1.01, ResetsAt: now.Add(-20 * time.Minute), Status: "rejected",
	}, now)
	if got != "5h _reset 20m ago (was 101%)_" {
		t.Errorf("got %q", got)
	}
}
