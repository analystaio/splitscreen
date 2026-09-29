package config

import (
	"path/filepath"
	"strings"
)

// Gateway holds settings for the control plane itself.
type Gateway struct {
	// Listen is the runner-facing WebSocket address. Bind to a private address:
	// nothing on the public internet should be able to open this socket.
	Listen string `yaml:"listen"`
	TLS    TLS    `yaml:"tls"`

	// Store is the SQLite path. Losing it costs the audit log and routing
	// state, not any live session — those live on the runners.
	Store string `yaml:"store"`

	// SecretsDir is a directory of one-file-per-secret, layered over the
	// environment. Must not be group- or world-readable.
	SecretsDir string `yaml:"secrets_dir"`

	// SecretsSSM reads secrets from AWS Parameter Store using the host's own
	// IAM identity, so there is no bootstrap secret on the gateway and every
	// read is attributable.
	SecretsSSM SecretsSSM `yaml:"secrets_ssm"`

	Slack Slack `yaml:"slack"`
	Forge Forge `yaml:"forge"`

	// StreamInterval is how often a turn's output is pushed to the surface —
	// appended to a native stream, or written over the message on the fallback
	// path. Consolidating every runner onto one chat app means one rate-limit
	// bucket, so this is a real control and not a cosmetic one.
	StreamInterval Duration `yaml:"stream_interval"`

	// Heartbeat is the ping period; a runner is degraded after two missed and
	// disconnected after five.
	Heartbeat Duration `yaml:"heartbeat"`

	// QueueLimit bounds per-runner offline queueing.
	QueueLimit int `yaml:"queue_limit"`

	// SecretExpiryWarning is how far ahead to warn about expiring credentials,
	// so a mandatory rotation is a calendar item rather than an outage.
	SecretExpiryWarning Duration `yaml:"secret_expiry_warning"`
}

type TLS struct {
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
}

type SecretsSSM struct {
	// Prefix is the parameter path secrets live under, e.g. "/splitscreen".
	// Empty disables the backend.
	Prefix string `yaml:"prefix"`
	Region string `yaml:"region"`
	// CacheTTL bounds staleness. Authentication resolves a secret on every
	// runner connection, so an uncached backend would put an API call in the
	// reconnect path of a flapping runner.
	CacheTTL Duration `yaml:"cache_ttl"`
}

type Slack struct {
	// Secret names for the tokens. Values never appear in this file.
	BotTokenSecret string `yaml:"bot_token_secret"`
	AppTokenSecret string `yaml:"app_token_secret"`
}

const (
	ForgeGitHubApp = "github-app"
	ForgeStatic    = "static-token"
)

type Forge struct {
	Kind           string `yaml:"kind"`
	AppID          string `yaml:"app_id"`
	InstallationID string `yaml:"installation_id"`
	KeyFile        string `yaml:"key_file"`
	// TokenSecret names a personal access token for the static-token kind.
	TokenSecret string `yaml:"token_secret"`
	Username    string `yaml:"username"`
}

// MCPServer declares one MCP server available to runners.
//
// "local" servers run on the runner: they need its filesystem and hold no
// shared credential. "proxied" servers run through the gateway: they need a
// credential, so the runner never sees them directly.
type MCPServer struct {
	Kind    string            `yaml:"kind"`
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`

	URL  string `yaml:"url"`
	Auth string `yaml:"auth"`
	User string `yaml:"user"`
	// Secret names the credential in the secret backend. The value is resolved
	// at call time and stays on the gateway.
	Secret string   `yaml:"secret"`
	Deny   []string `yaml:"deny"`
}

const (
	MCPLocal   = "local"
	MCPProxied = "proxied"
)

// Defaults for gateway settings that have a sensible one.
const (
	DefaultListen              = "127.0.0.1:8443"
	DefaultStore               = "splitscreen.db"
	DefaultStreamInterval      = 700 // milliseconds
	DefaultHeartbeatSeconds    = 20  // seconds
	DefaultQueueLimit          = 100 // messages per runner
	DefaultExpiryWarningHours  = 14 * 24
	DefaultPermissionTimeoutMS = 15 * 60 * 1000
)

func (c *Config) applyGatewayDefaults() {
	g := &c.Gateway
	if g.Listen == "" {
		g.Listen = DefaultListen
	}
	if g.Store == "" {
		g.Store = DefaultStore
	}
	if g.StreamInterval == 0 {
		g.StreamInterval = Duration(DefaultStreamInterval) * Duration(1_000_000)
	}
	if g.Heartbeat == 0 {
		g.Heartbeat = Duration(DefaultHeartbeatSeconds) * Duration(1_000_000_000)
	}
	if g.QueueLimit == 0 {
		g.QueueLimit = DefaultQueueLimit
	}
	if g.SecretExpiryWarning == 0 {
		g.SecretExpiryWarning = Duration(DefaultExpiryWarningHours) * Duration(3_600_000_000_000)
	}
	if g.Forge.Kind == "" && (g.Forge.AppID != "" || g.Forge.KeyFile != "") {
		g.Forge.Kind = ForgeGitHubApp
	}
}

func (c *Config) validateGateway(p *problems) {
	g := c.Gateway

	if g.TLS.Cert != "" || g.TLS.Key != "" {
		if g.TLS.Cert == "" || g.TLS.Key == "" {
			p.addf("gateway.tls: cert and key must be set together")
		}
	}
	if g.SecretsDir != "" && !filepath.IsAbs(g.SecretsDir) {
		p.addf("gateway.secrets_dir %q must be an absolute path", g.SecretsDir)
	}
	if pfx := g.SecretsSSM.Prefix; pfx != "" && !strings.HasPrefix(pfx, "/") {
		p.addf("gateway.secrets_ssm.prefix %q must be an absolute parameter path", pfx)
	}
	if g.Heartbeat.Duration() <= 0 {
		p.addf("gateway.heartbeat must be positive")
	}
	if g.StreamInterval.Duration() <= 0 {
		p.addf("gateway.stream_interval must be positive")
	}
	if g.QueueLimit < 0 {
		p.addf("gateway.queue_limit must not be negative")
	}

	switch g.Forge.Kind {
	case "":
	case ForgeGitHubApp:
		if g.Forge.AppID == "" || g.Forge.InstallationID == "" || g.Forge.KeyFile == "" {
			p.addf("gateway.forge: kind %s requires app_id, installation_id, and key_file", ForgeGitHubApp)
		}
	case ForgeStatic:
		if g.Forge.TokenSecret == "" {
			p.addf("gateway.forge: kind %s requires token_secret", ForgeStatic)
		}
	default:
		p.addf("gateway.forge: unknown kind %q", g.Forge.Kind)
	}
}

func (c *Config) validateMCP(p *problems) {
	for name, s := range c.MCP {
		if s == nil {
			p.addf("mcp %q: empty definition", name)
			continue
		}
		if strings.TrimSpace(name) == "" {
			p.addf("mcp: server with an empty name")
		}
		switch s.Kind {
		case MCPLocal:
			if s.Command == "" {
				p.addf("mcp %q: local servers require a command", name)
			}
			if s.URL != "" || s.Secret != "" {
				p.addf("mcp %q: local servers must not declare url or secret — a credentialed server belongs on the gateway", name)
			}
		case MCPProxied:
			if s.URL == "" {
				p.addf("mcp %q: proxied servers require a url", name)
			}
			if s.Command != "" {
				p.addf("mcp %q: proxied servers must not declare a command — the runner never executes them", name)
			}
			switch s.Auth {
			case "", "none":
			case "bearer":
				if s.Secret == "" {
					p.addf("mcp %q: bearer auth requires a secret", name)
				}
			case "basic":
				if s.Secret == "" || s.User == "" {
					p.addf("mcp %q: basic auth requires user and secret", name)
				}
			default:
				p.addf("mcp %q: unknown auth %q", name, s.Auth)
			}
		default:
			p.addf("mcp %q: kind must be %q or %q, got %q", name, MCPLocal, MCPProxied, s.Kind)
		}
	}

	// Bundles may only reference servers that exist.
	for bname, b := range c.Bundles {
		if b == nil {
			continue
		}
		for _, ref := range b.MCP {
			if _, ok := c.MCP[ref]; !ok {
				p.addf("bundle %q: references unknown mcp server %q", bname, ref)
			}
		}
	}
}

// ProxiedServers returns the subset of MCP servers the gateway executes.
func (c *Config) ProxiedServers() []string {
	var out []string
	for name, s := range c.MCP {
		if s != nil && s.Kind == MCPProxied {
			out = append(out, name)
		}
	}
	return out
}

// SecretRefs lists every secret name the config expects to resolve, so startup
// can fail loudly on a missing one rather than at first use.
// RunnerTokenSecrets maps each runner enrollment secret to the runner that
// authenticates with it. These are per-runner by nature: a missing one means
// that runner cannot connect, not that the gateway cannot run.
func (c *Config) RunnerTokenSecrets() map[string]string {
	out := map[string]string{}
	for name, r := range c.Runners {
		if r != nil {
			out[r.EffectiveTokenSecret(name)] = name
		}
	}
	return out
}

// SecretRefs lists every secret the config references.
func (c *Config) SecretRefs() []string {
	seen := map[string]bool{}
	add := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
		}
	}
	add(c.Gateway.Slack.BotTokenSecret)
	add(c.Gateway.Slack.AppTokenSecret)
	add(c.Gateway.Forge.TokenSecret)
	for name, r := range c.Runners {
		if r == nil {
			continue
		}
		add(r.EffectiveTokenSecret(name))
		add(r.HarnessSecret)
	}
	for _, s := range c.MCP {
		if s != nil {
			add(s.Secret)
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	return out
}
