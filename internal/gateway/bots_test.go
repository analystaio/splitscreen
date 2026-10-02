package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/analystaio/splitscreen/config"
	"github.com/analystaio/splitscreen/internal/surface"
	"github.com/analystaio/splitscreen/protocol"
)

func botMsg(thread, botID, userID, text string, addressed bool) surface.Inbound {
	return surface.Inbound{
		Surface: "test", Channel: "C1", Thread: thread,
		User: surface.User{ID: userID, Display: "Alert Relay"}, BotID: botID,
		Text: text, Addressed: addressed,
	}
}

func allowBots(h *harness, ids ...string) {
	h.gw.cfg.Load().Routes[0].AllowBots = ids
}

// By default another bot is ignored outright: no turn, no queue, no reply.
func TestBotIgnoredByDefault(t *testing.T) {
	h := newHarness(t)
	h.gw.OnMessage(context.Background(), botMsg("T1", "BRELAY", "URELAY", "<@UBOT> alarm", true))

	if d, _ := h.st.QueueDepth("alpha"); d != 0 {
		t.Fatalf("queue depth = %d, want 0", d)
	}
	if txt := h.srf.allText(); txt != "" {
		t.Fatalf("gateway answered a bot: %q", txt)
	}
}

// An unlisted bot cannot continue a thread a person started either, or two bots
// in one thread could keep each other going.
func TestUnlistedBotCannotContinueAThread(t *testing.T) {
	h := newHarness(t)
	allowBots(h, "BRELAY")
	ctx := context.Background()

	h.gw.OnMessage(ctx, offlineMsg("T1", "hello"))
	posts := len(h.srf.postsCopy())
	h.gw.OnMessage(ctx, botMsg("T1", "BOTHER", "UOTHER", "me too", false))

	// The person's turn holds the thread, so an admitted message would park
	// behind it and say so.
	h.gw.queuesMu.Lock()
	parked := len(h.gw.threadWaiting[threadKey("test", "C1", "T1")])
	h.gw.queuesMu.Unlock()
	if parked != 0 || len(h.srf.postsCopy()) != posts {
		t.Fatalf("an unlisted bot was admitted: parked=%d, posts %d -> %d", parked, posts, len(h.srf.postsCopy()))
	}
}

// An allowed bot is held to the human rules: it must address the bot to start.
func TestAllowedBotMustStillAddress(t *testing.T) {
	h := newHarness(t)
	allowBots(h, "BRELAY")
	h.gw.OnMessage(context.Background(), botMsg("T1", "BRELAY", "URELAY", "just chatting", false))
	if d, _ := h.st.QueueDepth("alpha"); d != 0 {
		t.Fatalf("queue depth = %d, want 0", d)
	}
}

// The alert-relay case end to end: an allowed bot's message to a sleeping
// runner wakes its machine, queues, and is delivered on connect with a context
// header that says it came from a bot. Matching by the bot's user id works too.
func TestAllowedBotWakesQueuesAndDelivers(t *testing.T) {
	for _, allowed := range []string{"BRELAY", "URELAY"} {
		t.Run(allowed, func(t *testing.T) {
			h, f := wakeHarness(t, fakeStart{prev: "stopped"})
			allowBots(h, allowed)

			h.gw.OnMessage(context.Background(), botMsg("T1", "BRELAY", "URELAY", "<@UBOT> alarm X went into ALARM", true))

			if f.count() != 1 {
				t.Fatalf("StartInstances calls = %d, want 1", f.count())
			}
			if !strings.Contains(h.srf.allText(), "asleep") {
				t.Errorf("wake notice = %q", h.srf.allText())
			}
			if d, _ := h.st.QueueDepth("alpha"); d != 1 {
				t.Fatalf("queue depth = %d, want 1", d)
			}

			ws := h.connect(t, "s3cret")
			readFrame[*protocol.HelloAck](t, ws)
			msg := readFrame[*protocol.Message](t, ws)
			if !strings.Contains(msg.Text, "alarm X went into ALARM") {
				t.Errorf("delivered text = %q", msg.Text)
			}
			if !strings.Contains(msg.Context, "from bot Alert Relay (URELAY)") {
				t.Errorf("context header = %q", msg.Context)
			}
		})
	}
}

// Bots are never admitted to DMs; only channel routes carry an allowlist.
func TestBotDMIgnored(t *testing.T) {
	h := newHarness(t)
	allowBots(h, "BRELAY")
	cfg := h.gw.cfg.Load()
	cfg.Routes = append(cfg.Routes, config.Route{DM: true, Runner: "alpha"})
	in := botMsg("T1", "BRELAY", "URELAY", "hi", true)
	in.IsDM, in.Channel = true, "D1"
	h.gw.OnMessage(context.Background(), in)
	if txt := h.srf.allText(); txt != "" {
		t.Fatalf("gateway answered a bot DM: %q", txt)
	}
}
