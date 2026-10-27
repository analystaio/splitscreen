package gateway

import (
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
