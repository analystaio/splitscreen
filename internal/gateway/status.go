package gateway

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/analystaio/splitscreen/config"
)

// StatusText renders the runner roster.
//
// Slack-first is deliberate: this covers most of what a dashboard would, needs
// no new infrastructure, and the audience is already here.
func (g *Gateway) StatusText() string {
	cfg := g.cfg.Load()
	heartbeat := cfg.Gateway.Heartbeat.Duration()

	names := make([]string, 0, len(cfg.Runners))
	for n := range cfg.Runners {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("*Runners*\n")
	for _, name := range names {
		rc := cfg.Runners[name]
		conn, online := g.hub.Get(name)
		depth, _ := g.store.QueueDepth(name)

		switch {
		case !online && rc.Wakeable():
			b.WriteString(fmt.Sprintf("• `%s` — :zzz: asleep, wakes on message (`%s`)", name, rc.Wake.EC2Instance))
		case !online:
			// A configured runner that never connects is usually a typo in a
			// route or a stopped unit; it should read as a problem, not silence.
			b.WriteString(fmt.Sprintf("• `%s` — :red_circle: disconnected", name))
		default:
			state := conn.State(heartbeat)
			icon := ":large_green_circle:"
			if state != StateConnected {
				icon = ":large_yellow_circle:"
			}
			b.WriteString(fmt.Sprintf("• `%s` — %s %s, up %s, bundle v%d",
				name, icon, state, since(conn.ConnectedAt()), conn.BundleVersion()))
			if h := conn.Harness(); h.Adapter != "" {
				b.WriteString(fmt.Sprintf(", harness %s", h.Adapter))
			}
		}
		if depth > 0 {
			b.WriteString(fmt.Sprintf(", *%d queued*", depth))
		}
		if rc != nil {
			b.WriteString("\n    " + PostureText(rc))
			if rc.Display.Name != "" {
				b.WriteString(fmt.Sprintf(" · posts as _%s_", rc.Display.Name))
			}
			b.WriteString(fmt.Sprintf("\n    cwd `%s`, idle %s", rc.Cwd, rc.Idle))
		}
		b.WriteString("\n")
	}

	if bad := g.UnreachableChannels(); len(bad) > 0 {
		b.WriteString("\n:warning: *Routed but not joined*\n")
		for _, st := range bad {
			name := st.Info.Name
			if name == "" {
				name = st.Info.ID
			}
			fmt.Fprintf(&b, "• #%s → `%s` — %s\n", name, st.Runner, st.Info.Detail)
		}
		b.WriteString("_Messages in these channels never reach the gateway._\n")
	}

	if servers := g.proxy.Names(); len(servers) > 0 {
		sort.Strings(servers)
		b.WriteString("\n*Proxied MCP*: " + strings.Join(servers, ", ") + "\n")
	}
	if g.forge != nil {
		b.WriteString("*Forge*: " + g.forge.Name() + "\n")
	} else {
		b.WriteString("*Forge*: not configured — git credentials will be refused\n")
	}
	return b.String()
}

// RoutesText renders the routing table.
func (g *Gateway) RoutesText() string {
	cfg := g.cfg.Load()
	var b strings.Builder
	b.WriteString("*Routes*\n")
	if len(cfg.Routes) == 0 {
		b.WriteString("_none — every channel is ignored_\n")
		return b.String()
	}
	for _, r := range cfg.Routes {
		target := "direct messages"
		if !r.DM {
			target = g.channelLabel(r.Channel)
		}
		b.WriteString(fmt.Sprintf("• %s → `%s`\n", target, r.Runner))
	}
	b.WriteString("\n_Unlisted channels are ignored. A thread keeps its runner even after a route changes; use `!rebind`._\n")
	return b.String()
}

// CostText renders a spend rollup.
//
// Runners billing against a subscription have no marginal dollar cost — their
// scarce resource is the rate-limit window — so they are reported in tokens
// rather than as $0.00.
func (g *Gateway) CostText(window time.Duration) string {
	cfg := g.cfg.Load()
	since := time.Now().Add(-window)

	var b strings.Builder
	fmt.Fprintf(&b, "*Usage — last %s*\n", humanDuration(window))

	byRunner, err := g.store.CostByRunner(since)
	if err != nil {
		return "Could not read usage: " + err.Error()
	}
	if len(byRunner) == 0 {
		return b.String() + "_no turns recorded_\n"
	}

	var totalUSD float64
	var unknown int64
	for _, r := range byRunner {
		rc := cfg.Runners[r.Key]
		subscription := rc != nil && rc.Billing == config.BillingSubscription

		tokens := r.Input + r.CacheWrite + r.CacheRead + r.Output
		if subscription {
			fmt.Fprintf(&b, "• `%s` — %d turns, %s tokens _(subscription: no marginal cost; consumes the rate-limit window)_\n",
				r.Key, r.Turns, humanCount(tokens))
		} else {
			fmt.Fprintf(&b, "• `%s` — %d turns, $%.2f, %s tokens\n",
				r.Key, r.Turns, r.CostUSD, humanCount(tokens))
			totalUSD += r.CostUSD
		}
		// Cache reads dominate long threads and bill at a fraction of input;
		// showing the split is what makes the idle-timeout tradeoff priceable.
		fmt.Fprintf(&b, "    in %s · cache write %s · cache read %s · out %s\n",
			humanCount(r.Input), humanCount(r.CacheWrite), humanCount(r.CacheRead), humanCount(r.Output))
		unknown += r.UnknownTurns
	}

	fmt.Fprintf(&b, "\n*Total (metered)*: $%.2f\n", totalUSD)
	if unknown > 0 {
		// Never render unknown usage as zero: a report showing $0 for an
		// un-instrumented harness is worse than one showing nothing.
		fmt.Fprintf(&b, ":warning: %d turns reported no usage and are excluded from every figure above.\n", unknown)
	}
	if g.prices.Version == "unset-defaults" {
		fmt.Fprintf(&b, "_Priced with the built-in placeholder table; set a real one before trusting these numbers._\n")
	} else {
		fmt.Fprintf(&b, "_Price table %s._\n", g.prices.Version)
	}

	if top, err := g.store.TopThreads(since, 5); err == nil && len(top) > 0 {
		b.WriteString("\n*Most expensive threads*\n")
		for _, t := range top {
			fmt.Fprintf(&b, "• %s — %d turns, $%.2f\n", t.Key, t.Turns, t.CostUSD)
		}
	}
	return b.String()
}

func since(t time.Time) string { return humanDuration(time.Since(t)) }

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func humanCount(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
}
