package gateway

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/analystaio/splitscreen/config"
	"github.com/analystaio/splitscreen/internal/surface"
)

// verifyingSurface is a fake surface that can answer the audience questions.
type verifyingSurface struct {
	*fakeSurface
	members map[string]bool
	shared  map[string]bool
	calls   atomic.Int32
}

func (v *verifyingSurface) SenderIsMember(_ context.Context, id string) bool {
	v.calls.Add(1)
	return v.members[id]
}

func (v *verifyingSurface) ChannelIsInternal(_ context.Context, id string) bool {
	v.calls.Add(1)
	return !v.shared[id]
}

// wildcardHarness routes "*" (and optionally more) to alpha on a verifying surface.
func wildcardHarness(t *testing.T, routes ...config.Route) (*harness, *verifyingSurface) {
	t.Helper()
	h := newHarness(t)
	v := &verifyingSurface{
		fakeSurface: h.srf,
		members:     map[string]bool{"UMEMBER": true},
		shared:      map[string]bool{"CSHARED": true},
	}
	h.gw.surfaces["test"] = v
	h.gw.cfg.Load().Routes = routes
	return h, v
}

func msg(channel, thread, user, text string, addressed bool) surface.Inbound {
	return surface.Inbound{
		Surface: "test", Channel: channel, Thread: thread,
		User: surface.User{ID: user}, Text: text, Addressed: addressed,
	}
}

func queued(t *testing.T, h *harness) int {
	t.Helper()
	d, err := h.st.QueueDepth("alpha")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

var wildcard = config.Route{Channel: config.WildcardChannel, Runner: "alpha"}

func TestWildcardAcceptsAMemberInAnyChannel(t *testing.T) {
	h, _ := wildcardHarness(t, wildcard)
	h.gw.OnMessage(context.Background(), msg("CANY", "T1", "UMEMBER", "<@UBOT> hi", true))
	if got := queued(t, h); got != 1 {
		t.Fatalf("queue depth = %d, want 1", got)
	}
}

func TestWildcardStillNeedsAddressing(t *testing.T) {
	h, v := wildcardHarness(t, wildcard)
	h.gw.OnMessage(context.Background(), msg("CANY", "T1", "UMEMBER", "lunch?", false))
	if got := queued(t, h); got != 0 {
		t.Fatalf("queue depth = %d, want 0", got)
	}
	if n := v.calls.Load(); n != 0 {
		t.Fatalf("chatter cost %d verification calls, want 0", n)
	}
}

func TestWildcardRefusesGuestsSilently(t *testing.T) {
	h, _ := wildcardHarness(t, wildcard)
	h.gw.OnMessage(context.Background(), msg("CANY", "T1", "UGUEST", "<@UBOT> hi", true))
	if got := queued(t, h); got != 0 {
		t.Fatalf("queue depth = %d, want 0", got)
	}
	if txt := h.srf.allText(); txt != "" {
		t.Fatalf("answered a guest: %q", txt)
	}
}

func TestWildcardRefusesSharedChannels(t *testing.T) {
	h, _ := wildcardHarness(t, wildcard)
	h.gw.OnMessage(context.Background(), msg("CSHARED", "T1", "UMEMBER", "<@UBOT> hi", true))
	if got := queued(t, h); got != 0 {
		t.Fatalf("queue depth = %d, want 0", got)
	}
}

// bindThread makes T1 an existing conversation, the state in which a reply needs
// no addressing. Without it an unaddressed reply is dropped as chatter before the
// gate is reached, and these tests would pass with the gate missing.
func bindThread(t *testing.T, h *harness, channel string) {
	t.Helper()
	if _, _, err := h.st.BindThread(threadKey("test", channel, "T1"), "test", channel, "alpha"); err != nil {
		t.Fatal(err)
	}
}

// The bypass worth a test of its own: once a thread exists, replies need no
// addressing, so an unchecked reply path would let a guest drive it.
func TestGuestCannotContinueAMembersThread(t *testing.T) {
	h, _ := wildcardHarness(t, wildcard)
	bindThread(t, h, "CANY")
	ctx := context.Background()

	h.gw.OnMessage(ctx, msg("CANY", "T1", "UGUEST", "now do this", false))
	if got := queued(t, h); got != 0 {
		t.Fatalf("a guest's reply queued %d, want 0", got)
	}

	// The control: the same reply from a member does get through.
	h.gw.OnMessage(ctx, msg("CANY", "T1", "UMEMBER", "now do this", false))
	if got := queued(t, h); got != 1 {
		t.Fatalf("a member's reply queued %d, want 1", got)
	}
}

func TestGuestCannotRunCommands(t *testing.T) {
	h, _ := wildcardHarness(t, wildcard)
	h.gw.OnMessage(context.Background(), msg("CANY", "T1", "UGUEST", "!status", false))
	if txt := h.srf.allText(); txt != "" {
		t.Fatalf("answered a guest's command: %q", txt)
	}
}

// A channel that turns shared after a thread started takes the thread with it.
func TestThreadStopsWhenItsChannelBecomesShared(t *testing.T) {
	h, v := wildcardHarness(t, wildcard)
	bindThread(t, h, "CANY")
	ctx := context.Background()

	h.gw.OnMessage(ctx, msg("CANY", "T1", "UMEMBER", "first", false))
	if got := queued(t, h); got != 1 {
		t.Fatalf("queued %d before the share, want 1", got)
	}
	v.shared["CANY"] = true
	h.gw.OnMessage(ctx, msg("CANY", "T1", "UMEMBER", "second", false))
	if got := queued(t, h); got != 1 {
		t.Fatalf("queued %d after the share, want still 1", got)
	}
}

// A surface that cannot answer must fail closed, not open.
func TestWildcardFailsClosedWithoutAVerifier(t *testing.T) {
	h := newHarness(t)
	h.gw.cfg.Load().Routes = []config.Route{wildcard}
	h.gw.OnMessage(context.Background(), msg("CANY", "T1", "UMEMBER", "<@UBOT> hi", true))
	if got := queued(t, h); got != 0 {
		t.Fatalf("queue depth = %d, want 0", got)
	}
}

// Routes that name a channel, and everything deployed today, are untouched.
func TestNamedRouteIsNotGated(t *testing.T) {
	h, v := wildcardHarness(t, wildcard, config.Route{Channel: "CSHARED", Runner: "alpha"})
	h.gw.OnMessage(context.Background(), msg("CSHARED", "T1", "UGUEST", "<@UBOT> hi", true))
	if got := queued(t, h); got != 1 {
		t.Fatalf("queue depth = %d, want 1", got)
	}
	if n := v.calls.Load(); n != 0 {
		t.Fatalf("a named route made %d verification calls, want 0", n)
	}
}

func TestMembersOnlyDMRefusesGuests(t *testing.T) {
	h, v := wildcardHarness(t, config.Route{DM: true, Runner: "alpha", MembersOnly: true})
	ctx := context.Background()
	guest := msg("DGUEST", "T1", "UGUEST", "hi", true)
	guest.IsDM = true
	h.gw.OnMessage(ctx, guest)
	if got := queued(t, h); got != 0 {
		t.Fatalf("guest DM queued %d, want 0", got)
	}

	// A DM is never "internal" in the channel sense, so only the sender counts.
	v.shared["DMEMBER"] = true
	member := msg("DMEMBER", "T2", "UMEMBER", "hi", true)
	member.IsDM = true
	h.gw.OnMessage(ctx, member)
	if got := queued(t, h); got != 1 {
		t.Fatalf("member DM queued %d, want 1", got)
	}
}

func TestPlainDMIsNotGated(t *testing.T) {
	h, _ := wildcardHarness(t, config.Route{DM: true, Runner: "alpha"})
	in := msg("D1", "T1", "UGUEST", "hi", true)
	in.IsDM = true
	h.gw.OnMessage(context.Background(), in)
	if got := queued(t, h); got != 1 {
		t.Fatalf("queue depth = %d, want 1", got)
	}
}

func TestRoutesTextDescribesTheWildcard(t *testing.T) {
	h, _ := wildcardHarness(t, wildcard)
	txt := h.gw.RoutesText()
	if !strings.Contains(txt, "any channel") || strings.Contains(txt, "Unlisted channels are ignored") {
		t.Fatalf("routes text: %q", txt)
	}
}
