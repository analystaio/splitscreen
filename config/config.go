// Package config loads and validates the gateway's routing configuration.
//
// One YAML file is the source of truth for which runners exist, what they are
// allowed to do, and which channels route to them. Runners never carry routing
// or allowlist config: they request an identity and the gateway grants routes.
package config

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so the YAML can say "30m" rather than a
// nanosecond count.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("expected a duration string like \"30m\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Duration() time.Duration { return time.Duration(d) }
func (d Duration) String() string          { return time.Duration(d).String() }

// Config is the whole file.
type Config struct {
	Gateway Gateway               `yaml:"gateway"`
	Runners map[string]*Runner    `yaml:"runners"`
	Routes  []Route               `yaml:"routes"`
	Bundles map[string]*Bundle    `yaml:"bundles"`
	MCP     map[string]*MCPServer `yaml:"mcp"`
	// Marketplaces are Claude Code plugin marketplaces in git repositories,
	// each pinned to a ref. A bundle enables plugins as "name@marketplace".
	Marketplaces map[string]*Marketplace `yaml:"marketplaces"`

	// Warnings are findings that do not block loading: the config runs, but
	// probably is not what someone meant. Populated by Validate.
	Warnings []string `yaml:"-"`
}

// Display is the per-runner persona. Distinct identities come from
// chat.postMessage overrides, not from separate chat apps — so personas are
// cosmetic and a runner cannot be @-mentioned individually.
type Display struct {
	Name string `yaml:"name"`
	Icon string `yaml:"icon"`
	// ShowActivity controls whether tool invocations appear in the thread.
	//
	// On a surface that renders progress natively — Slack's streaming task
	// cards — steps show as they run and collapse behind a disclosure when the
	// turn ends, so only "hidden" changes anything: it suppresses them.
	//
	// The transient/full distinction still governs the fallback path, where the
	// whole message is rewritten on every edit. There, a turn can run for
	// minutes and a thread showing nothing reads as broken, but once the answer
	// lands a list of shell commands is noise to anyone who does not know what a
	// tool call is. "transient" resolves that: visible while the turn runs,
	// removed when it finishes.
	ShowActivity string `yaml:"show_activity"`
}

const (
	// ActivityTransient shows tool lines while a turn runs, then strips them
	// from the finished message. The default, and the fallback path only:
	// a native stream keeps its task cards, collapsed.
	ActivityTransient = "transient"
	// ActivityHidden never shows them, on either path.
	ActivityHidden = "hidden"
	// ActivityFull leaves them in the finished message. Fallback path only.
	ActivityFull = "full"
)

// EffectiveActivity resolves the display mode, defaulting to transient.
func (d Display) EffectiveActivity() string {
	switch d.ShowActivity {
	case ActivityHidden, ActivityFull:
		return d.ShowActivity
	default:
		return ActivityTransient
	}
}

type Runner struct {
	Display Display `yaml:"display"`
	Host    string  `yaml:"host"`
	Cwd     string  `yaml:"cwd"`
	Harness string  `yaml:"harness"`
	Bundle  string  `yaml:"bundle"`
	// Model overrides the harness's own default model. Left empty, the harness
	// chooses — which means the model silently follows whatever the agent CLI
	// on the runner defaults to, and changes under you when that default moves.
	// Set it to pin the model explicitly.
	Model  string   `yaml:"model"`
	Idle   Duration `yaml:"idle"`
	Policy Policy   `yaml:"policy"`

	// MaxConcurrent caps how many turns this runner may run at once. Beyond it,
	// new turns queue with an in-thread position notice and dispatch as slots
	// free. 0 means unlimited. This is the coding-task concurrency limit — the
	// runner's own MaxSessions is the memory backstop, a different axis.
	MaxConcurrent int `yaml:"max_concurrent"`

	// TokenSecret names the enrollment secret this runner authenticates with.
	// Defaults to "runner-<name>".
	TokenSecret string `yaml:"token_secret"`
	// HarnessSecret names the credential shipped to the runner and materialized
	// to tmpfs. Empty means the runner authenticates by some other means —
	// cloud IAM, for instance — and the gateway ships nothing.
	HarnessSecret string `yaml:"harness_secret"`
	// HarnessEnv is the environment variable the harness credential is injected
	// as. Adapters supply a sensible default.
	HarnessEnv string `yaml:"harness_env"`
	// Billing is "api-key" or "subscription". Subscription runners have no
	// marginal dollar cost; the scarce resource is the rate-limit window, so
	// cost reports render them differently rather than as $0.
	Billing string `yaml:"billing"`

	// ContextHeader controls whether each message reaches the agent prefixed
	// with a one-line header naming the surface, channel, and sender. Default
	// on: several people and channels can share one runner, and the agent
	// cannot credit or answer the right person without it.
	ContextHeader *bool `yaml:"context_header"`

	// WorkingStatus is the text of the surface's native "working" indicator
	// while this runner has a turn in flight — rendered by Slack as
	// "<app> is working…". Unset uses DefaultWorkingStatus; an explicit empty
	// string turns the indicator off for this runner.
	WorkingStatus *string `yaml:"working_status"`

	// Wake, when set, lets the gateway start the runner's host when a message
	// queues for it while it is offline. Hosts that stop themselves when idle
	// (per-task boxes) are then as reachable as ones that never sleep: the
	// message is held, the host boots, the runner connects, the queue drains.
	Wake *Wake `yaml:"wake"`
}

// Wake names the machine to start for an offline runner. It is explicit rather
// than inferred from Host: Host is informational and free-form, and turning a
// display field into an API target would make a typo a call against the wrong
// instance.
type Wake struct {
	// EC2Instance is the instance id to StartInstances.
	EC2Instance string `yaml:"ec2_instance"`
	// Region is the instance's region. Empty uses the gateway's ambient region.
	Region string `yaml:"region"`
}

// DefaultWorkingStatus is the working-indicator text when a runner sets none.
const DefaultWorkingStatus = "is working…"

// WorkingText resolves WorkingStatus: the default when unset, "" when off.
func (r *Runner) WorkingText() string {
	if r == nil || r.WorkingStatus == nil {
		return DefaultWorkingStatus
	}
	return *r.WorkingStatus
}

// WantsContextHeader resolves ContextHeader, defaulting to on.
func (r *Runner) WantsContextHeader() bool {
	return r == nil || r.ContextHeader == nil || *r.ContextHeader
}

// Wakeable reports whether the gateway can start this runner's host.
func (r *Runner) Wakeable() bool { return r != nil && r.Wake != nil && r.Wake.EC2Instance != "" }

// EffectiveTokenSecret is the enrollment secret name for a runner.
func (r *Runner) EffectiveTokenSecret(name string) string {
	if r.TokenSecret != "" {
		return r.TokenSecret
	}
	return "runner-" + name
}

const (
	BillingAPIKey       = "api-key"
	BillingSubscription = "subscription"
)

type Policy struct {
	// Approvers may resolve permission prompts. Distinct from who may talk to
	// the runner.
	Approvers []string `yaml:"approvers"`
	// Deny is evaluated gateway-side before any prompt is posted, so it cannot
	// be overridden by clicking Allow.
	Deny []string `yaml:"deny"`
	// Allow auto-approves matching tools without posting a prompt. Deny is
	// evaluated first and wins, so this only ever removes a click.
	Allow []string `yaml:"allow"`
	// AutoApprove runs the runner unattended: anything not denied proceeds
	// without asking. Every decision is still recorded and attributed to policy,
	// and deny rules are still the hard boundary — which makes them the only
	// remaining control, so they are worth writing carefully.
	AutoApprove bool        `yaml:"auto_approve"`
	Forge       ForgePolicy `yaml:"forge"`
}

type ForgePolicy struct {
	// Repos bounds which repositories the gateway will mint credentials for,
	// in "owner/name" form.
	Repos []string `yaml:"repos"`
}

// Route binds a channel (or the DM surface) to exactly one runner.
type Route struct {
	Channel string `yaml:"channel"`
	DM      bool   `yaml:"dm"`
	Runner  string `yaml:"runner"`
}

// Marketplace is a plugin marketplace in a git repository on the forge.
//
// Runners fetch it themselves, at exactly Ref, with a read-only credential the
// gateway mints for Repo (any runner whose bundle uses the marketplace may read
// it; nothing else about its forge policy changes). Plugins load from that
// checkout for each session, so what a runner runs is what the config pins:
// moving Ref is a reviewable edit, and a new commit on the repo changes nothing
// until it does.
type Marketplace struct {
	Repo string `yaml:"repo"` // "owner/name"
	Ref  string `yaml:"ref"`  // tag, branch or commit; pin a tag or commit
}

// Bundle is the harness configuration materialized onto a runner. Contents are
// interpreted by the harness adapter, not by the gateway: memory files and
// skills mean something to a Claude Code adapter and something else, or
// nothing, to another.
type Bundle struct {
	Extends string   `yaml:"extends"`
	Memory  []string `yaml:"memory"`
	Skills  []string `yaml:"skills"`
	// Plugins are "name@marketplace" entries; the marketplace must be defined.
	Plugins []string `yaml:"plugins"`
	MCP     []string `yaml:"mcp"`
	// StrictMCP, when false, lets the harness load MCP servers from anywhere it
	// normally would — plugins included — instead of only the bundle's `mcp`
	// servers. Unset inherits; the default is strict.
	//
	// Turning it off is a trade: plugins can bring their own servers (the
	// standard marketplace shape), but so can the working tree. A headless
	// session loads a repository's .mcp.json without asking, so any branch the
	// agent checks out can start a server, outside the permission check.
	StrictMCP *bool `yaml:"strict_mcp"`
}

// DefaultIdle applies when a runner does not set one.
const DefaultIdle = 30 * time.Minute

// Load reads and fully validates a config file. It returns either a usable
// config or an error listing every problem found — never a partially applied
// one. Callers implement reload by swapping the returned pointer only on
// success, so a bad edit leaves the running config untouched.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse is Load over an in-memory document.
func Parse(raw []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // a typo'd key is an error, not a silently ignored line
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.applyDefaults()
	c.applyGatewayDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	for _, r := range c.Runners {
		if r.Idle == 0 {
			r.Idle = Duration(DefaultIdle)
		}
	}
}

// RunnerFor resolves an inbound message to a runner name. Unrouted channels
// return false and are ignored — this replaces per-runner channel allowlists.
func (c *Config) RunnerFor(channel string, isDM bool) (string, bool) {
	for _, r := range c.Routes {
		if isDM && r.DM {
			return r.Runner, true
		}
		if !isDM && r.Channel != "" && r.Channel == channel {
			return r.Runner, true
		}
	}
	return "", false
}

// SplitPlugin splits "name@marketplace". ok is false for any other shape.
func SplitPlugin(id string) (name, marketplace string, ok bool) {
	i := strings.LastIndex(id, "@")
	if i <= 0 || i == len(id)-1 {
		return "", "", false
	}
	return id[:i], id[i+1:], true
}

// MarketplacesFor returns the marketplaces a resolved bundle's plugins use.
func (c *Config) MarketplacesFor(rb *ResolvedBundle) map[string]*Marketplace {
	out := map[string]*Marketplace{}
	for _, id := range rb.Plugins {
		if _, m, ok := SplitPlugin(id); ok {
			if mp, ok := c.Marketplaces[m]; ok {
				out[m] = mp
			}
		}
	}
	return out
}

// MarketplaceReadable reports whether runner may read repo because its bundle
// uses a marketplace hosted there. That grants a read-only credential only.
func (c *Config) MarketplaceReadable(runner, repo string) bool {
	rc, ok := c.Runners[runner]
	if !ok || rc.Bundle == "" {
		return false
	}
	rb, err := c.ResolveBundle(rc.Bundle)
	if err != nil {
		return false
	}
	for _, mp := range c.MarketplacesFor(rb) {
		if strings.EqualFold(mp.Repo, repo) {
			return true
		}
	}
	return false
}

// ResolvedBundle is a bundle with its inheritance chain flattened.
type ResolvedBundle struct {
	Name      string
	Memory    []string
	Skills    []string
	Plugins   []string
	MCP       []string
	StrictMCP bool
}

// ResolveBundle flattens an extends chain, base first. Later layers append to
// earlier ones, so an org base contributes shared rules and a runner overlay
// adds its own without restating them.
//
// An explicitly empty list in a child (plugins: []) still yields an empty
// result only if the base contributed nothing; suppression is deliberately not
// supported, because "why did my base rule vanish" is a worse failure mode than
// "why is this list longer than I expected".
func (c *Config) ResolveBundle(name string) (*ResolvedBundle, error) {
	seen := map[string]bool{}
	var chain []*Bundle
	for cur := name; cur != ""; {
		if seen[cur] {
			return nil, fmt.Errorf("config: bundle %q has a circular extends chain", name)
		}
		seen[cur] = true
		b, ok := c.Bundles[cur]
		if !ok {
			return nil, fmt.Errorf("config: bundle %q not found", cur)
		}
		chain = append([]*Bundle{b}, chain...) // prepend: base ends up first
		cur = b.Extends
	}
	out := &ResolvedBundle{Name: name, StrictMCP: true}
	for _, b := range chain {
		if b.StrictMCP != nil {
			out.StrictMCP = *b.StrictMCP // the most specific layer that says wins
		}
		out.Memory = append(out.Memory, b.Memory...)
		out.Skills = append(out.Skills, b.Skills...)
		out.Plugins = append(out.Plugins, b.Plugins...)
		out.MCP = append(out.MCP, b.MCP...)
	}
	return out, nil
}
