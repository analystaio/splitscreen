package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/analystaio/splitscreen/internal/store"
	"github.com/analystaio/splitscreen/internal/surface"
)

// A routed channel the bot has not joined behaves exactly like an unrouted one:
// the platform simply never delivers the message. That makes it the worst class
// of misconfiguration — silence, with nothing to read. This turns it into a
// stated problem in status output, and a log line at reload.

// channelState is a cached membership check.
type channelState struct {
	Info    surface.ChannelInfo
	Runner  string
	Surface string
	Checked time.Time
}

type channelCache struct {
	mu    sync.RWMutex
	byID  map[string]channelState
	fresh time.Time
}

// ChannelRefreshInterval bounds how stale membership can be. Someone removing
// the bot from a channel is rare and not urgent; discovering it within the hour
// is enough.
const ChannelRefreshInterval = time.Hour

// RefreshChannels re-checks membership for every routed channel.
//
// Called at startup, after each reload, and on a slow timer. Failures are
// recorded as Unknown rather than as problems: a surface that lacks the scope to
// answer must not paint a healthy deployment red.
func (g *Gateway) RefreshChannels(ctx context.Context) {
	cfg := g.cfg.Load()

	next := map[string]channelState{}
	for _, route := range cfg.Routes {
		if route.DM || route.Channel == "" {
			continue // DMs have no membership to check
		}
		for name, srf := range g.surfaces {
			checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			info, err := srf.Channel(checkCtx, route.Channel)
			cancel()
			if err != nil {
				g.log.Warn("channel check failed", "surface", name, "channel", route.Channel, "err", err)
				continue
			}
			next[route.Channel] = channelState{
				Info: info, Runner: route.Runner, Surface: name, Checked: time.Now(),
			}

			switch info.Membership {
			case surface.MembershipNotJoined:
				g.log.Error("routed channel is not joined; messages will never arrive",
					"channel", route.Channel, "name", info.Name,
					"runner", route.Runner, "detail", info.Detail)
				_ = g.store.Log(store.Event{
					Kind: "channel.not_joined", Runner: route.Runner,
					Detail: map[string]any{
						"channel": route.Channel, "name": info.Name, "detail": info.Detail,
					},
				})
			case surface.MembershipUnknown:
				g.log.Warn("cannot verify channel membership",
					"channel", route.Channel, "runner", route.Runner, "detail", info.Detail)
			}
			break // one surface per channel; the first that answers wins
		}
	}

	g.channels.mu.Lock()
	g.channels.byID = next
	g.channels.fresh = time.Now()
	g.channels.mu.Unlock()
}

// ChannelState returns the cached check for a channel.
func (g *Gateway) ChannelState(id string) (channelState, bool) {
	g.channels.mu.RLock()
	defer g.channels.mu.RUnlock()
	s, ok := g.channels.byID[id]
	return s, ok
}

// UnreachableChannels lists routed channels the bot definitively cannot receive
// from. Unknown results are excluded on purpose — reporting "we could not check"
// as "broken" is how a check gets ignored.
func (g *Gateway) UnreachableChannels() []channelState {
	g.channels.mu.RLock()
	defer g.channels.mu.RUnlock()
	var out []channelState
	for _, s := range g.channels.byID {
		if s.Info.Membership == surface.MembershipNotJoined {
			out = append(out, s)
		}
	}
	return out
}

// watchChannels keeps the cache warm.
func (g *Gateway) watchChannels(ctx context.Context) {
	g.RefreshChannels(ctx)
	t := time.NewTicker(ChannelRefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.RefreshChannels(ctx)
		}
	}
}

// channelLabel renders a channel for humans: its name when known, its id
// otherwise, with a marker when the bot cannot receive from it.
func (g *Gateway) channelLabel(id string) string {
	st, ok := g.ChannelState(id)
	if !ok {
		return "`" + id + "`"
	}
	label := "`" + id + "`"
	if st.Info.Name != "" {
		label = "#" + st.Info.Name
	}
	switch st.Info.Membership {
	case surface.MembershipNotJoined:
		return label + " :warning: *not joined*"
	case surface.MembershipUnknown:
		return label + " _(unverified)_"
	default:
		return label
	}
}
