package config

import (
	"strings"
	"testing"
	"time"
)

const valid = `
runners:
  staging:
    display:  { name: "Ada", icon: ":robot_face:" }
    host:     i-0a1b2c3d4e5f67890
    cwd:      /srv/app
    harness:  claude-code
    bundle:   staging
    idle:     30m
    policy:
      approvers: [U01ABC, U02DEF]
      deny: ["Bash(git push --force*)"]
      forge:
        repos: ["acme/widgets"]
  review:
    display:  { name: "Ada Review", icon: ":atom_symbol:" }
    cwd:      /srv/app-review
    harness:  claude-code
    bundle:   review
    idle:     10m

bundles:
  base:
    memory: [org/conventions.md]
    skills: [org/deploy-check]
  staging:
    extends: base
    memory: [runners/staging.md]
  review:
    extends: base
    memory: [runners/review.md]
    mcp: [github, jira]

mcp:
  github:
    kind: local
    command: /usr/bin/mcp-github
  jira:
    kind: proxied
    url: https://mcp.example.com/v1
    auth: basic
    user: bot@example.com
    secret: jira-token
    deny: ["deleteIssue"]

routes:
  - channel: C0123456789
    runner:  review
  - channel: C0987654321
    runner:  staging
  - dm: true
    runner: staging
`

func TestParseValid(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := c.Runners["review"].Idle.Duration(); got != 10*time.Minute {
		t.Errorf("idle = %v, want 10m", got)
	}
	if r, ok := c.RunnerFor("C0123456789", false); !ok || r != "review" {
		t.Errorf("RunnerFor(react channel) = %q,%v", r, ok)
	}
	if r, ok := c.RunnerFor("", true); !ok || r != "staging" {
		t.Errorf("RunnerFor(dm) = %q,%v", r, ok)
	}
	// Unrouted channels are ignored — this replaces per-runner allowlists.
	if _, ok := c.RunnerFor("C0UNKNOWN", false); ok {
		t.Error("an unrouted channel resolved to a runner")
	}
}

func TestBundleResolutionIsBaseFirst(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.ResolveBundle("review")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"org/conventions.md", "runners/review.md"}
	if len(got.Memory) != len(want) {
		t.Fatalf("memory = %v, want %v", got.Memory, want)
	}
	for i := range want {
		if got.Memory[i] != want[i] {
			t.Fatalf("memory = %v, want %v", got.Memory, want)
		}
	}
	if len(got.Skills) != 1 || got.Skills[0] != "org/deploy-check" {
		t.Errorf("skills = %v, want the base's", got.Skills)
	}
}

func TestDefaultIdleApplied(t *testing.T) {
	src := `
runners:
  solo:
    display: { name: "Ada" }
    cwd: /srv/app
    harness: claude-code
routes:
  - dm: true
    runner: solo
`
	c, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Runners["solo"].Idle.Duration(); got != DefaultIdle {
		t.Errorf("idle = %v, want default %v", got, DefaultIdle)
	}
}

func TestValidationRejects(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "duplicate channel claim",
			src: `
runners:
  a: { display: {name: A}, cwd: /a, harness: h }
  b: { display: {name: B}, cwd: /b, harness: h }
routes:
  - { channel: C1, runner: a }
  - { channel: C1, runner: b }
`,
			want: "claimed by route 0 and route 1",
		},
		{
			name: "plugin without a marketplace",
			src: `
runners:
  a: { display: {name: A}, cwd: /a, harness: h, bundle: x }
bundles:
  x: { plugins: [tools] }
routes:
  - { channel: C1, runner: a }
`,
			want: `plugin "tools" must be name@marketplace`,
		},
		{
			name: "plugin from an unknown marketplace",
			src: `
runners:
  a: { display: {name: A}, cwd: /a, harness: h, bundle: x }
bundles:
  x: { plugins: [tools@nope] }
routes:
  - { channel: C1, runner: a }
`,
			want: `unknown marketplace "nope"`,
		},
		{
			name: "marketplace without a ref",
			src: `
runners:
  a: { display: {name: A}, cwd: /a, harness: h }
marketplaces:
  m: { repo: acme/plugins }
routes:
  - { channel: C1, runner: a }
`,
			want: "ref is required",
		},
		{
			name: "marketplace ref that looks like an option",
			src: `
runners:
  a: { display: {name: A}, cwd: /a, harness: h }
marketplaces:
  m: { repo: acme/plugins, ref: "--upload-pack=x" }
routes:
  - { channel: C1, runner: a }
`,
			want: "is not a plain tag, branch or commit",
		},
		{
			name: "marketplace repo not owner/name",
			src: `
runners:
  a: { display: {name: A}, cwd: /a, harness: h }
marketplaces:
  m: { repo: plugins, ref: v1 }
routes:
  - { channel: C1, runner: a }
`,
			want: "must be in owner/name form",
		},
		{
			name: "unknown runner in route",
			src: `
runners:
  a: { display: {name: A}, cwd: /a, harness: h }
routes:
  - { channel: C1, runner: a }
  - { channel: C2, runner: ghost }
`,
			want: `unknown runner "ghost"`,
		},
		{
			name: "unknown bundle reference",
			src: `
runners:
  a: { display: {name: A}, cwd: /a, harness: h, bundle: nope }
routes:
  - { channel: C1, runner: a }
`,
			want: `unknown bundle "nope"`,
		},
		{
			name: "bundle extends cycle",
			src: `
runners:
  a: { display: {name: A}, cwd: /a, harness: h, bundle: x }
bundles:
  x: { extends: y }
  y: { extends: x }
routes:
  - { channel: C1, runner: a }
`,
			want: "circular extends",
		},
		{
			name: "two dm routes",
			src: `
runners:
  a: { display: {name: A}, cwd: /a, harness: h }
  b: { display: {name: B}, cwd: /b, harness: h }
routes:
  - { dm: true, runner: a }
  - { dm: true, runner: b }
`,
			want: "second dm route",
		},
		{
			name: "route with both channel and dm",
			src: `
runners:
  a: { display: {name: A}, cwd: /a, harness: h }
routes:
  - { channel: C1, dm: true, runner: a }
`,
			want: "exactly one of channel or dm",
		},
		{
			name: "relative cwd",
			src: `
runners:
  a: { display: {name: A}, cwd: relative/path, harness: h }
routes:
  - { channel: C1, runner: a }
`,
			want: "must be an absolute path",
		},
		{
			name: "missing harness",
			src: `
runners:
  a: { display: {name: A}, cwd: /a }
routes:
  - { channel: C1, runner: a }
`,
			want: "harness is required",
		},
		{
			name: "missing display name",
			src: `
runners:
  a: { cwd: /a, harness: h }
routes:
  - { channel: C1, runner: a }
`,
			want: "display.name is required",
		},
		{
			name: "runner name is not a slug",
			src: `
runners:
  "Dev3 React": { display: {name: A}, cwd: /a, harness: h }
routes:
  - { channel: C1, runner: "Dev3 React" }
`,
			want: "must be a lowercase slug",
		},
		{
			name: "malformed forge repo",
			src: `
runners:
  a:
    display: {name: A}
    cwd: /a
    harness: h
    policy: { forge: { repos: ["justaname"] } }
routes:
  - { channel: C1, runner: a }
`,
			want: "owner/name form",
		},
		{
			name: "no runners at all",
			src:  `routes: []`,
			want: "no runners are defined",
		},
		{
			name: "typo'd key is not silently ignored",
			src: `
runners:
  a: { display: {name: A}, cwd: /a, harness: h, idel: 5m }
routes:
  - { channel: C1, runner: a }
`,
			want: "idel",
		},
		{
			name: "bad duration",
			src: `
runners:
  a: { display: {name: A}, cwd: /a, harness: h, idle: "half an hour" }
routes:
  - { channel: C1, runner: a }
`,
			want: "invalid duration",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			if err == nil {
				t.Fatal("expected rejection, got a valid config")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// A bad edit must not partially apply: Parse either returns a usable config or
// an error listing everything wrong with it.
func TestValidationReportsAllProblems(t *testing.T) {
	src := `
runners:
  a: { display: {name: A}, cwd: relative, harness: "" }
routes:
  - { channel: C1, runner: ghost }
  - { channel: C1, runner: ghost }
`
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatal("expected rejection")
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("error type = %T, want *ValidationError", err)
	}
	if len(ve.Problems) < 4 {
		t.Errorf("reported %d problems, expected all of them:\n%s", len(ve.Problems), err)
	}
	for _, want := range []string{"absolute path", "harness is required", "unknown runner", "claimed by route"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing problem %q in:\n%s", want, err)
		}
	}
}

func TestMCPValidation(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "credentialed server declared as local",
			src: `
runners:
  a: { display: {name: A}, cwd: /a, harness: h }
mcp:
  jira: { kind: local, command: /bin/x, secret: jira-token }
routes:
  - { channel: C1, runner: a }
`,
			want: "must not declare url or secret",
		},
		{
			name: "proxied server declaring a command",
			src: `
runners:
  a: { display: {name: A}, cwd: /a, harness: h }
mcp:
  jira: { kind: proxied, url: https://x, command: /bin/x }
routes:
  - { channel: C1, runner: a }
`,
			want: "the runner never executes them",
		},
		{
			name: "basic auth without a user",
			src: `
runners:
  a: { display: {name: A}, cwd: /a, harness: h }
mcp:
  jira: { kind: proxied, url: https://x, auth: basic, secret: s }
routes:
  - { channel: C1, runner: a }
`,
			want: "basic auth requires user and secret",
		},
		{
			name: "bundle referencing an undeclared server",
			src: `
runners:
  a: { display: {name: A}, cwd: /a, harness: h, bundle: b }
bundles:
  b: { mcp: [ghost] }
routes:
  - { channel: C1, runner: a }
`,
			want: `unknown mcp server "ghost"`,
		},
		{
			name: "incomplete github app forge",
			src: `
gateway:
  forge: { kind: github-app, app_id: "123" }
runners:
  a: { display: {name: A}, cwd: /a, harness: h }
routes:
  - { channel: C1, runner: a }
`,
			want: "requires app_id, installation_id, and key_file",
		},
		{
			name: "half-configured tls",
			src: `
gateway:
  tls: { cert: /etc/cert.pem }
runners:
  a: { display: {name: A}, cwd: /a, harness: h }
routes:
  - { channel: C1, runner: a }
`,
			want: "cert and key must be set together",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			if err == nil {
				t.Fatal("expected rejection")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestSecretRefsAndDefaults(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.Gateway.Listen != DefaultListen {
		t.Errorf("listen = %q, want default", c.Gateway.Listen)
	}
	// A runner with no explicit token_secret still has a resolvable name, so
	// startup can verify every secret exists before serving traffic.
	refs := map[string]bool{}
	for _, r := range c.SecretRefs() {
		refs[r] = true
	}
	for _, want := range []string{"runner-staging", "runner-review", "jira-token"} {
		if !refs[want] {
			t.Errorf("SecretRefs missing %q (got %v)", want, c.SecretRefs())
		}
	}
	if got := c.ProxiedServers(); len(got) != 1 || got[0] != "jira" {
		t.Errorf("ProxiedServers = %v, want [jira]", got)
	}
}

// A runner with no routes is a warning, not an error. It is also the state a
// config passes through while a route is being removed, so blocking on it would
// make that edit impossible without a simultaneous runner deletion.
func TestUnroutedRunnerIsAWarning(t *testing.T) {
	src := `
runners:
  a: { display: {name: A}, cwd: /a, harness: h }
  orphan: { display: {name: O}, cwd: /o, harness: h }
routes:
  - { channel: C1, runner: a }
`
	c, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("an unrouted runner blocked loading: %v", err)
	}
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "orphan") {
		t.Fatalf("warnings = %v, want one naming orphan", c.Warnings)
	}
}

func TestValidConfigHasNoWarnings(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings) != 0 {
		t.Errorf("warnings = %v, want none", c.Warnings)
	}
}

func TestMarketplaceReadable(t *testing.T) {
	cfg, err := Parse([]byte(`
runners:
  a: { display: {name: A}, cwd: /a, harness: h, bundle: child }
  b: { display: {name: B}, cwd: /b, harness: h }
marketplaces:
  m: { repo: acme/plugins, ref: v1 }
bundles:
  base: { plugins: [tools@m] }
  child: { extends: base }
routes:
  - { channel: C1, runner: a }
  - { channel: C2, runner: b }
`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MarketplaceReadable("a", "acme/plugins") {
		t.Error("a uses the marketplace through its base bundle")
	}
	if !cfg.MarketplaceReadable("a", "ACME/Plugins") {
		t.Error("GitHub repo names are case-insensitive")
	}
	if cfg.MarketplaceReadable("b", "acme/plugins") || cfg.MarketplaceReadable("a", "acme/other") {
		t.Error("readable beyond the marketplaces a runner's bundle uses")
	}
}

func TestStrictMCPDefaultsOnAndInherits(t *testing.T) {
	cfg, err := Parse([]byte(`
runners:
  a: { display: {name: A}, cwd: /a, harness: h, bundle: child }
bundles:
  base: {}
  loose: { strict_mcp: false }
  child: { extends: loose }
  back: { extends: loose, strict_mcp: true }
routes:
  - { channel: C1, runner: a }
`))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"base": true, "loose": false, "child": false, "back": true} {
		rb, err := cfg.ResolveBundle(name)
		if err != nil {
			t.Fatal(err)
		}
		if rb.StrictMCP != want {
			t.Errorf("bundle %s: StrictMCP = %v, want %v", name, rb.StrictMCP, want)
		}
	}
}
