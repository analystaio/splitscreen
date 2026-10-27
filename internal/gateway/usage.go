package gateway

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/analystaio/splitscreen/config"
	"github.com/analystaio/splitscreen/internal/store"
	"github.com/analystaio/splitscreen/protocol"
)

// accountFor names the subscription account a runner draws on. The limit
// belongs to the credential, not the runner, so runners sharing a harness
// secret share one reading. A runner using its own on-disk login is its own
// account.
func accountFor(name string, rc *config.Runner) string {
	if rc != nil && rc.HarnessSecret != "" {
		return rc.HarnessSecret
	}
	return name
}

// recordRateLimits stores the subscription-window reading a turn reported.
func (g *Gateway) recordRateLimits(runner string, rc *config.Runner, limits []protocol.RateLimit) {
	if len(limits) == 0 {
		return
	}
	rows := make([]store.RateLimit, len(limits))
	for i, rl := range limits {
		rows[i] = store.RateLimit{Window: rl.Window, Utilization: rl.Utilization, Status: rl.Status}
		if rl.ResetsAt > 0 {
			rows[i].ResetsAt = time.Unix(rl.ResetsAt, 0)
		}
	}
	if err := g.store.RecordRateLimits(accountFor(runner, rc), runner, rows); err != nil {
		g.log.Error("record rate limits failed", "runner", runner, "err", err)
	}
}

// UsageText renders the latest subscription-window reading of every account,
// the asking runner's first.
//
// The readings are what the harness reported at the end of each account's
// most recent turn; nothing is fetched on demand, because the only way to ask
// is to spend a turn. Hence the "as of" on every line.
func (g *Gateway) UsageText(runner string, rc *config.Runner) string {
	rows, err := g.store.RateLimits()
	if err != nil {
		return "Could not read usage limits: " + err.Error()
	}
	byAccount := map[string][]store.RateLimit{}
	for _, rl := range rows {
		byAccount[rl.Account] = append(byAccount[rl.Account], rl)
	}

	// Every subscription account is listed, read or not: an account with no
	// reading yet is worth seeing as such rather than silently missing.
	own := accountFor(runner, rc)
	accounts := map[string]bool{own: true}
	for name, r := range g.cfg.Load().Runners {
		if r.Billing == config.BillingSubscription {
			accounts[accountFor(name, r)] = true
		}
	}
	for a := range byAccount {
		accounts[a] = true
	}
	names := make([]string, 0, len(accounts))
	for a := range accounts {
		if a != own {
			names = append(names, a)
		}
	}
	sort.Strings(names)
	names = append([]string{own}, names...)

	now := time.Now()
	var b strings.Builder
	b.WriteString("*Subscription limits*\n")
	for _, a := range names {
		fmt.Fprintf(&b, "• `%s`", a)
		if a == own {
			b.WriteString(" _(this runner)_")
		}
		limits := byAccount[a]
		if len(limits) == 0 {
			b.WriteString(" — _no reading yet; one arrives with its next turn_\n")
			continue
		}
		sortWindows(limits)
		var parts []string
		latest := limits[0].At
		for _, rl := range limits {
			parts = append(parts, windowText(rl, now))
			if rl.At.After(latest) {
				latest = rl.At
			}
		}
		fmt.Fprintf(&b, " — %s · _as of %s ago_\n", strings.Join(parts, " · "), humanDuration(now.Sub(latest)))
	}
	b.WriteString("\n_Each reading is from that account's most recent turn on any runner._\n")
	return b.String()
}

// windowOrder puts the windows people ask about first; anything else follows
// alphabetically.
var windowOrder = map[string]int{"five_hour": 0, "seven_day": 1}

var windowLabels = map[string]string{
	"five_hour":        "5h",
	"seven_day":        "7d",
	"seven_day_opus":   "7d opus",
	"seven_day_sonnet": "7d sonnet",
}

func sortWindows(limits []store.RateLimit) {
	rank := func(w string) int {
		if r, ok := windowOrder[w]; ok {
			return r
		}
		return len(windowOrder)
	}
	sort.SliceStable(limits, func(i, j int) bool {
		ri, rj := rank(limits[i].Window), rank(limits[j].Window)
		if ri != rj {
			return ri < rj
		}
		return limits[i].Window < limits[j].Window
	})
}

func windowText(rl store.RateLimit, now time.Time) string {
	label := windowLabels[rl.Window]
	if label == "" {
		label = rl.Window
	}
	pct := int(rl.Utilization*100 + 0.5)
	// A window past its reset has restarted since the reading; quoting the old
	// figure as current would overstate it.
	if !rl.ResetsAt.IsZero() && !rl.ResetsAt.After(now) {
		return fmt.Sprintf("%s _reset %s ago (was %d%%)_", label, humanDuration(now.Sub(rl.ResetsAt)), pct)
	}
	text := fmt.Sprintf("%s *%d%%*", label, pct)
	if rl.Status == "rejected" {
		text += " :no_entry: limit reached"
	}
	if !rl.ResetsAt.IsZero() {
		text += fmt.Sprintf(", resets in %s", humanDuration(rl.ResetsAt.Sub(now)))
	}
	return text
}
