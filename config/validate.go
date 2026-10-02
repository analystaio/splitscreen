package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/analystaio/splitscreen/protocol"
)

var (
	ec2InstanceID = regexp.MustCompile(`^i-[0-9a-f]{8}([0-9a-f]{9})?$`)
	awsRegion     = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d$`)
)

// IsEC2InstanceID reports whether s has the shape of an EC2 instance id.
func IsEC2InstanceID(s string) bool { return ec2InstanceID.MatchString(s) }

// ValidationError collects every problem in a config rather than stopping at
// the first. An operator editing routing should see all of it in one pass, not
// discover the next mistake after each redeploy.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return "config: " + e.Problems[0]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "config: %d problems:", len(e.Problems))
	for _, p := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(p)
	}
	return b.String()
}

// problems separates the two kinds of finding a config can have.
//
// An error means the config cannot work and must not be applied. A warning
// means it probably is not what someone meant, but it runs — and blocking on
// those makes legitimate intermediate states impossible to reach, such as
// removing a runner's last route before removing the runner.
type problems struct {
	list  []string
	warns []string
}

func (p *problems) addf(format string, args ...any) {
	p.list = append(p.list, fmt.Sprintf(format, args...))
}

func (p *problems) warnf(format string, args ...any) {
	p.warns = append(p.warns, fmt.Sprintf(format, args...))
}

// Validate enforces every invariant the gateway relies on at runtime. It is
// called on load, so a reload either applies wholly or not at all.
func (c *Config) Validate() error {
	var p problems

	if len(c.Runners) == 0 {
		p.addf("no runners are defined")
	}

	c.validateGateway(&p)
	c.validateRunners(&p)
	c.validateMarketplaces(&p)
	c.validateBundles(&p)
	c.validateRoutes(&p)
	c.validateMCP(&p)

	sort.Strings(p.warns)
	c.Warnings = p.warns

	if len(p.list) == 0 {
		return nil
	}
	sort.Strings(p.list)
	return &ValidationError{Problems: p.list}
}

func (c *Config) validateRunners(p *problems) {
	for name, r := range c.Runners {
		if !protocol.ValidSlug(name) {
			p.addf("runner %q: name must be a lowercase slug (a-z, 0-9, dashes) — it becomes a filesystem path and a unit name", name)
		}
		if r == nil {
			p.addf("runner %q: empty definition", name)
			continue
		}
		if r.Harness == "" {
			p.addf("runner %q: harness is required", name)
		}
		if r.Cwd == "" {
			p.addf("runner %q: cwd is required", name)
		} else if !filepath.IsAbs(r.Cwd) {
			p.addf("runner %q: cwd %q must be an absolute path", name, r.Cwd)
		}
		if r.Idle.Duration() <= 0 {
			p.addf("runner %q: idle must be positive", name)
		}
		switch r.Display.ShowActivity {
		case "", ActivityTransient, ActivityHidden, ActivityFull:
		default:
			p.addf("runner %q: display.show_activity must be %q, %q, or %q, got %q",
				name, ActivityTransient, ActivityHidden, ActivityFull, r.Display.ShowActivity)
		}
		if r.Display.Name == "" {
			p.addf("runner %q: display.name is required — it is what users see on every message", name)
		}
		if r.Bundle != "" {
			if _, ok := c.Bundles[r.Bundle]; !ok {
				p.addf("runner %q: references unknown bundle %q", name, r.Bundle)
			}
		}
		seenApprover := map[string]bool{}
		for _, a := range r.Policy.Approvers {
			if a == "" {
				p.addf("runner %q: empty approver entry", name)
				continue
			}
			if seenApprover[a] {
				p.addf("runner %q: duplicate approver %q", name, a)
			}
			seenApprover[a] = true
		}
		for i, d := range r.Policy.Deny {
			if strings.TrimSpace(d) == "" {
				p.addf("runner %q: deny rule %d is empty", name, i)
			}
		}
		for i, a := range r.Policy.Allow {
			if strings.TrimSpace(a) == "" {
				p.addf("runner %q: allow rule %d is empty", name, i)
			}
		}
		if r.Policy.AutoApprove && len(r.Policy.Deny) == 0 {
			// Not fatal — an operator may genuinely want this — but in auto mode
			// the deny list is the only thing left between the agent and the
			// machine, so an empty one should never be silent.
			p.warnf("runner %q runs unattended with no deny rules: every tool call proceeds unchecked", name)
		}
		if r.Policy.AutoApprove && len(r.Policy.Approvers) > 0 {
			p.warnf("runner %q sets approvers but also auto_approve, so no prompt is ever posted and the approver list has no effect", name)
		}
		if r.Wake != nil {
			switch {
			case r.Wake.EC2Instance == "":
				// Legitimate on a template: `runner add` fills it from the new
				// runner's host. Anywhere else it means "wake" was meant and
				// will not happen.
				p.warnf("runner %q declares wake with no ec2_instance; it will not be woken (fine for a template)", name)
			case !IsEC2InstanceID(r.Wake.EC2Instance):
				p.addf("runner %q: wake.ec2_instance %q is not an EC2 instance id (i-…)", name, r.Wake.EC2Instance)
			}
			if r.Wake.Region != "" && !awsRegion.MatchString(r.Wake.Region) {
				p.addf("runner %q: wake.region %q is not an AWS region", name, r.Wake.Region)
			}
		}
		for _, repo := range r.Policy.Forge.Repos {
			if strings.Count(repo, "/") != 1 || strings.HasPrefix(repo, "/") || strings.HasSuffix(repo, "/") {
				p.addf("runner %q: forge repo %q must be in owner/name form", name, repo)
			}
		}
	}
}

func (c *Config) validateBundles(p *problems) {
	for name, b := range c.Bundles {
		if !protocol.ValidSlug(name) {
			p.addf("bundle %q: name must be a lowercase slug", name)
		}
		if b == nil {
			p.addf("bundle %q: empty definition", name)
			continue
		}
		if b.Extends != "" {
			if _, ok := c.Bundles[b.Extends]; !ok {
				p.addf("bundle %q: extends unknown bundle %q", name, b.Extends)
				continue
			}
		}
		if _, err := c.ResolveBundle(name); err != nil {
			p.addf("bundle %q: %v", name, strings.TrimPrefix(err.Error(), "config: "))
		}
		for _, id := range b.Plugins {
			plugin, market, ok := SplitPlugin(id)
			switch {
			case !ok:
				p.addf("bundle %q: plugin %q must be name@marketplace", name, id)
			case !protocol.ValidSlug(plugin):
				p.addf("bundle %q: plugin name %q must be a lowercase slug", name, plugin)
			case c.Marketplaces[market] == nil:
				p.addf("bundle %q: plugin %q uses unknown marketplace %q", name, id, market)
			}
		}
	}
}

// refRe is deliberately narrow: the ref is passed to git, so nothing that could
// read as an option or a refspec with a colon gets through.
var refRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)

func (c *Config) validateMarketplaces(p *problems) {
	for name, m := range c.Marketplaces {
		if !protocol.ValidSlug(name) {
			p.addf("marketplace %q: name must be a lowercase slug", name)
		}
		if m == nil {
			p.addf("marketplace %q: empty definition", name)
			continue
		}
		if strings.Count(m.Repo, "/") != 1 || strings.HasPrefix(m.Repo, "/") || strings.HasSuffix(m.Repo, "/") {
			p.addf("marketplace %q: repo %q must be in owner/name form", name, m.Repo)
		}
		if m.Ref == "" {
			p.addf("marketplace %q: ref is required (pin a tag or commit)", name)
		} else if !refRe.MatchString(m.Ref) || strings.Contains(m.Ref, "..") {
			p.addf("marketplace %q: ref %q is not a plain tag, branch or commit", name, m.Ref)
		}
	}
}

func (c *Config) validateRoutes(p *problems) {
	claimed := map[string]int{}
	dmRoutes := 0

	for i, r := range c.Routes {
		switch {
		case r.Channel != "" && r.DM:
			p.addf("route %d: set exactly one of channel or dm, not both", i)
		case r.Channel == "" && !r.DM:
			p.addf("route %d: set exactly one of channel or dm", i)
		}

		if r.Runner == "" {
			p.addf("route %d: runner is required", i)
		} else if _, ok := c.Runners[r.Runner]; !ok {
			p.addf("route %d: references unknown runner %q", i, r.Runner)
		}

		if r.Channel != "" {
			// One channel maps to exactly one runner. Without this, Socket Mode
			// style delivery would make "who answered me" nondeterministic —
			// the original bug this architecture exists to fix.
			if prev, dup := claimed[r.Channel]; dup {
				p.addf("channel %s is claimed by route %d and route %d — a channel maps to exactly one runner", r.Channel, prev, i)
			} else {
				claimed[r.Channel] = i
			}
		}

		if r.DM {
			dmRoutes++
			if dmRoutes > 1 {
				// There is one bot user, hence one DM conversation per person,
				// hence at most one DM route.
				p.addf("route %d: a second dm route is defined — there is only one DM surface to route", i)
			}
		}
	}

	// A runner nothing routes to is usually a typo, but it is also the state you
	// pass through while removing one. Warn; do not block.
	routed := map[string]bool{}
	for _, r := range c.Routes {
		routed[r.Runner] = true
	}
	for name := range c.Runners {
		if !routed[name] {
			p.warnf("runner %q has no routes — it will connect and receive nothing", name)
		}
	}
}
