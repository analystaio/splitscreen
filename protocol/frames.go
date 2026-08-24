package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

// FrameType is the value of the "t" discriminator on every control frame.
type FrameType string

const (
	// Handshake.
	TypeHello    FrameType = "hello"
	TypeHelloAck FrameType = "hello_ack"

	// Gateway -> runner.
	TypeMessage            FrameType = "message"
	TypeBundlePush         FrameType = "bundle.push"
	TypePermissionResponse FrameType = "permission.response"
	TypeMCPResponse        FrameType = "mcp.response"
	TypeCredentialGrant    FrameType = "credential.grant"

	// Runner -> gateway.
	TypeTextDelta         FrameType = "text.delta"
	TypeThought           FrameType = "thought"
	TypeToolStart         FrameType = "tool.start"
	TypeToolEnd           FrameType = "tool.end"
	TypePermissionRequest FrameType = "permission.request"
	TypeMCPCall           FrameType = "mcp.call"
	TypeCredentialRequest FrameType = "credential.request"
	TypeUsage             FrameType = "usage"
	TypeDone              FrameType = "done"
	TypeError             FrameType = "error"

	// Bulk transfer control (payload rides on binary frames, see blob.go).
	TypeBlobBegin FrameType = "blob.begin"
	TypeBlobEnd   FrameType = "blob.end"

	// Liveness.
	TypePing FrameType = "ping"
	TypePong FrameType = "pong"
)

// Direction records which peer is permitted to originate a frame. Enforcing it
// at decode means a compromised or buggy runner cannot, say, inject a
// permission.response and approve its own tool call.
type Direction int

const (
	DirUp   Direction = iota + 1 // runner -> gateway
	DirDown                      // gateway -> runner
	DirBoth
)

func (d Direction) String() string {
	switch d {
	case DirUp:
		return "up"
	case DirDown:
		return "down"
	case DirBoth:
		return "both"
	default:
		return "unknown"
	}
}

func (d Direction) permits(from Direction) bool {
	return d == DirBoth || d == from
}

// Frame is implemented by every control frame.
type Frame interface {
	Type() FrameType
	Direction() Direction
	Validate() error
}

var (
	// ErrUnknownFrame is returned for a "t" value this build does not know.
	// Callers should treat it as ignorable when the peer is a newer minor.
	ErrUnknownFrame = errors.New("protocol: unknown frame type")
	// ErrWrongDirection is returned when a peer sends a frame it may not originate.
	ErrWrongDirection = errors.New("protocol: frame not permitted from this peer")
)

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ValidSlug reports whether s is usable as a runner or bundle name. Slugs end
// up in filesystem paths (/run/splitscreen/<runner>/) and unit names, so they are
// deliberately narrow.
func ValidSlug(s string) bool { return slugRe.MatchString(s) }

// ---------------------------------------------------------------------------
// Handshake
// ---------------------------------------------------------------------------

// AuthMode selects how a runner proves its identity at enrollment.
type AuthMode string

const (
	// AuthToken is an enrollment token delivered out of band (parameter store,
	// operator paste). The default and the only cloud-independent mode.
	AuthToken AuthMode = "token"
	// AuthInstanceIdentity is a signed cloud instance identity document,
	// verified by the gateway against the provider's public certificate. No
	// shared secret exists in this mode.
	AuthInstanceIdentity AuthMode = "instance-identity"
)

type Auth struct {
	Mode  AuthMode `json:"mode"`
	Value string   `json:"value,omitempty"`
}

type Host struct {
	ID   string `json:"id,omitempty"`
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

type HarnessInfo struct {
	Adapter string `json:"adapter"`
	Version string `json:"version,omitempty"`
}

// Hello is the first frame on every runner connection. The runner *requests*
// an identity; the gateway *grants* routes in HelloAck. Routing is never
// configured runner-side.
type Hello struct {
	Protocol     string      `json:"protocol"`
	Runner       string      `json:"runner"`
	Auth         Auth        `json:"auth"`
	Host         Host        `json:"host"`
	Harness      HarnessInfo `json:"harness"`
	Capabilities []string    `json:"capabilities,omitempty"`
	// Bundle is the configuration the runner currently has materialized, if
	// any. The gateway cannot infer this: a runner's config lives on tmpfs and
	// a reboot wipes it without the gateway ever seeing a disconnect it could
	// attribute. Reporting it here is what lets the gateway push when the two
	// disagree rather than trusting its own cache.
	Bundle *BundleRef `json:"bundle,omitempty"`
}

func (*Hello) Type() FrameType      { return TypeHello }
func (*Hello) Direction() Direction { return DirUp }

func (h *Hello) Validate() error {
	var errs []error
	if _, err := ParseVersion(h.Protocol); err != nil {
		errs = append(errs, err)
	}
	if !ValidSlug(h.Runner) {
		errs = append(errs, fmt.Errorf("hello: runner %q is not a valid slug", h.Runner))
	}
	switch h.Auth.Mode {
	case AuthToken:
		if h.Auth.Value == "" {
			errs = append(errs, errors.New("hello: auth mode token requires a value"))
		}
	case AuthInstanceIdentity:
		if h.Auth.Value == "" {
			errs = append(errs, errors.New("hello: auth mode instance-identity requires a document"))
		}
	default:
		errs = append(errs, fmt.Errorf("hello: unknown auth mode %q", h.Auth.Mode))
	}
	if h.Harness.Adapter == "" {
		errs = append(errs, errors.New("hello: harness.adapter is required"))
	}
	if h.Bundle != nil && h.Bundle.Version < 0 {
		errs = append(errs, errors.New("hello: bundle version must be non-negative"))
	}
	return errors.Join(errs...)
}

// Redacted returns a copy safe to log. Hello carries the enrollment secret and
// will otherwise end up in journald verbatim.
func (h *Hello) Redacted() *Hello {
	c := *h
	if c.Auth.Value != "" {
		c.Auth.Value = "[redacted]"
	}
	return &c
}

// BundleRef identifies a materialized config bundle. The digest is what makes
// runner-side drift detectable.
type BundleRef struct {
	Version int    `json:"version"`
	Digest  string `json:"digest"`
}

// Policy is the subset of runner policy the runner needs locally. Authoritative
// evaluation happens gateway-side; this copy exists so the runner can fail fast
// and render useful messages, never as the enforcement point.
type Policy struct {
	Deny       []string `json:"deny,omitempty"`
	ForgeRepos []string `json:"forge_repos,omitempty"`
}

type HelloAck struct {
	Protocol string    `json:"protocol"`
	Runner   string    `json:"runner"`
	Bundle   BundleRef `json:"bundle"`
	Routes   []string  `json:"routes"`
	Policy   *Policy   `json:"policy,omitempty"`
}

func (*HelloAck) Type() FrameType      { return TypeHelloAck }
func (*HelloAck) Direction() Direction { return DirDown }

func (h *HelloAck) Validate() error {
	var errs []error
	if _, err := ParseVersion(h.Protocol); err != nil {
		errs = append(errs, err)
	}
	if !ValidSlug(h.Runner) {
		errs = append(errs, fmt.Errorf("hello_ack: runner %q is not a valid slug", h.Runner))
	}
	if h.Bundle.Version < 0 {
		errs = append(errs, errors.New("hello_ack: bundle version must be non-negative"))
	}
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// Conversation
// ---------------------------------------------------------------------------

type UserRef struct {
	ID      string `json:"id"`
	Display string `json:"display,omitempty"`
}

// Attachment references a file the gateway is about to stream down. Bytes never
// travel inside a control frame.
type Attachment struct {
	BlobID string `json:"blob"`
	Name   string `json:"name"`
	Mime   string `json:"mime"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// Inline requests the adapter present the file to the harness as inline
	// content (an image block) rather than as a filesystem path.
	Inline bool `json:"inline,omitempty"`
}

// Message is one inbound user message. It opens a turn; TurnID is assigned by
// the gateway so accounting and audit share a key with routing.
type Message struct {
	ThreadID    string       `json:"thread"`
	TurnID      string       `json:"turn"`
	Channel     string       `json:"channel"`
	User        UserRef      `json:"user"`
	Text        string       `json:"text"`
	Attachments []Attachment `json:"attachments,omitempty"`
	// Command carries a recognized control word (new, rebind, runner) already
	// parsed by the gateway, so runners do not each reimplement the syntax.
	Command string `json:"command,omitempty"`
}

func (*Message) Type() FrameType      { return TypeMessage }
func (*Message) Direction() Direction { return DirDown }

func (m *Message) Validate() error {
	var errs []error
	if m.ThreadID == "" {
		errs = append(errs, errors.New("message: thread is required"))
	}
	if m.TurnID == "" {
		errs = append(errs, errors.New("message: turn is required"))
	}
	if m.User.ID == "" {
		errs = append(errs, errors.New("message: user.id is required"))
	}
	for i, a := range m.Attachments {
		if a.BlobID == "" {
			errs = append(errs, fmt.Errorf("message: attachment %d missing blob id", i))
		}
		if a.Size < 0 {
			errs = append(errs, fmt.Errorf("message: attachment %d has negative size", i))
		}
	}
	return errors.Join(errs...)
}

type TextDelta struct {
	ThreadID string `json:"thread"`
	TurnID   string `json:"turn"`
	Text     string `json:"text"`
}

func (*TextDelta) Type() FrameType      { return TypeTextDelta }
func (*TextDelta) Direction() Direction { return DirUp }

func (t *TextDelta) Validate() error {
	if t.TurnID == "" {
		return errors.New("text.delta: turn is required")
	}
	return nil
}

// Thought is a piece of the model's reasoning, reported separately from
// text.delta because the two have different fates: prose becomes the answer,
// a thought is an intermediate step a surface may fold away or drop.
type Thought struct {
	ThreadID string `json:"thread"`
	TurnID   string `json:"turn"`
	Text     string `json:"text"`
}

func (*Thought) Type() FrameType      { return TypeThought }
func (*Thought) Direction() Direction { return DirUp }

func (t *Thought) Validate() error {
	if t.TurnID == "" {
		return errors.New("thought: turn is required")
	}
	return nil
}

type ToolStart struct {
	ThreadID string `json:"thread"`
	TurnID   string `json:"turn"`
	CallID   string `json:"call"`
	Tool     string `json:"tool"`
	Summary  string `json:"summary,omitempty"`
}

func (*ToolStart) Type() FrameType      { return TypeToolStart }
func (*ToolStart) Direction() Direction { return DirUp }

func (t *ToolStart) Validate() error {
	var errs []error
	if t.TurnID == "" {
		errs = append(errs, errors.New("tool.start: turn is required"))
	}
	if t.CallID == "" {
		errs = append(errs, errors.New("tool.start: call is required"))
	}
	if t.Tool == "" {
		errs = append(errs, errors.New("tool.start: tool is required"))
	}
	return errors.Join(errs...)
}

type ToolEnd struct {
	ThreadID   string `json:"thread"`
	TurnID     string `json:"turn"`
	CallID     string `json:"call"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

func (*ToolEnd) Type() FrameType      { return TypeToolEnd }
func (*ToolEnd) Direction() Direction { return DirUp }

func (t *ToolEnd) Validate() error {
	var errs []error
	if t.CallID == "" {
		errs = append(errs, errors.New("tool.end: call is required"))
	}
	if t.DurationMS < 0 {
		errs = append(errs, errors.New("tool.end: duration_ms must be non-negative"))
	}
	return errors.Join(errs...)
}

type Done struct {
	ThreadID     string `json:"thread"`
	TurnID       string `json:"turn"`
	SessionID    string `json:"session,omitempty"`
	DurationMS   int64  `json:"duration_ms"`
	NumToolCalls int    `json:"num_tool_calls"`
}

func (*Done) Type() FrameType      { return TypeDone }
func (*Done) Direction() Direction { return DirUp }

func (d *Done) Validate() error {
	if d.TurnID == "" {
		return errors.New("done: turn is required")
	}
	return nil
}

type Error struct {
	ThreadID string `json:"thread,omitempty"`
	TurnID   string `json:"turn,omitempty"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	// Fatal marks the connection unusable; the gateway drops it and the runner
	// reconnects rather than continuing in an undefined state.
	Fatal bool `json:"fatal,omitempty"`
}

func (*Error) Type() FrameType      { return TypeError }
func (*Error) Direction() Direction { return DirBoth }

func (e *Error) Validate() error {
	if e.Code == "" {
		return errors.New("error: code is required")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Permissions
// ---------------------------------------------------------------------------

type PermissionRequest struct {
	ThreadID  string          `json:"thread"`
	TurnID    string          `json:"turn"`
	RequestID string          `json:"request"`
	Tool      string          `json:"tool"`
	Input     json.RawMessage `json:"input,omitempty"`
	Cwd       string          `json:"cwd,omitempty"`
	Summary   string          `json:"summary,omitempty"`
}

func (*PermissionRequest) Type() FrameType      { return TypePermissionRequest }
func (*PermissionRequest) Direction() Direction { return DirUp }

func (p *PermissionRequest) Validate() error {
	var errs []error
	if p.RequestID == "" {
		errs = append(errs, errors.New("permission.request: request is required"))
	}
	if p.Tool == "" {
		errs = append(errs, errors.New("permission.request: tool is required"))
	}
	// TurnID is deliberately optional. A runner that cannot identify the turn
	// must still be able to ask: the gateway denies a request it cannot place,
	// which is a far better outcome than a request that cannot be sent at all
	// and leaves the harness waiting on a decision nobody will ever make.
	return errors.Join(errs...)
}

// Decision is the outcome of a permission prompt.
type Decision string

const (
	DecisionAllow Decision = "allow"
	// DecisionAllowSession grants the tool for the rest of the thread's session.
	DecisionAllowSession Decision = "allow_session"
	// DecisionAllowAlways is accepted but no longer offered. Persisting a grant
	// meant a chat click editing a reviewed config file; the CLI does that now.
	// The value stays valid so a prompt posted before an upgrade still resolves.
	DecisionAllowAlways Decision = "allow_always"
	DecisionDeny        Decision = "deny"
)

func (d Decision) valid() bool {
	switch d {
	case DecisionAllow, DecisionAllowSession, DecisionAllowAlways, DecisionDeny:
		return true
	}
	return false
}

type PermissionResponse struct {
	RequestID string   `json:"request"`
	Decision  Decision `json:"decision"`
	// DecidedBy is empty when PolicyDenied is set: no human was asked.
	DecidedBy *UserRef `json:"decided_by,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	// PolicyDenied marks a denial that came from gateway policy rather than
	// from a prompt. These are never overridable by clicking Allow, because no
	// prompt was ever posted.
	PolicyDenied bool `json:"policy_denied,omitempty"`
	// AutoApproved marks an approval granted by policy without asking a human —
	// an allow-rule match, or a runner configured to run unattended. Deny rules
	// are still evaluated first and still win, so this widens what proceeds
	// without a click, never what proceeds at all.
	AutoApproved bool `json:"auto_approved,omitempty"`
}

func (*PermissionResponse) Type() FrameType      { return TypePermissionResponse }
func (*PermissionResponse) Direction() Direction { return DirDown }

func (p *PermissionResponse) Validate() error {
	var errs []error
	if p.RequestID == "" {
		errs = append(errs, errors.New("permission.response: request is required"))
	}
	if !p.Decision.valid() {
		errs = append(errs, fmt.Errorf("permission.response: unknown decision %q", p.Decision))
	}
	if p.PolicyDenied && p.Decision != DecisionDeny {
		errs = append(errs, errors.New("permission.response: policy_denied requires decision=deny"))
	}
	if p.AutoApproved && p.Decision == DecisionDeny {
		errs = append(errs, errors.New("permission.response: auto_approved cannot carry decision=deny"))
	}
	if p.PolicyDenied && p.AutoApproved {
		errs = append(errs, errors.New("permission.response: a decision cannot be both policy-denied and auto-approved"))
	}
	// Every decision is attributable: to a human, or explicitly to policy.
	if !p.PolicyDenied && !p.AutoApproved && p.DecidedBy == nil {
		errs = append(errs, errors.New("permission.response: decided_by is required unless the decision came from policy"))
	}
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// Proxied MCP
// ---------------------------------------------------------------------------

type MCPCall struct {
	ThreadID string          `json:"thread"`
	TurnID   string          `json:"turn"`
	CallID   string          `json:"call"`
	Server   string          `json:"server"`
	Tool     string          `json:"tool"`
	Args     json.RawMessage `json:"args,omitempty"`
}

func (*MCPCall) Type() FrameType      { return TypeMCPCall }
func (*MCPCall) Direction() Direction { return DirUp }

func (m *MCPCall) Validate() error {
	var errs []error
	if m.CallID == "" {
		errs = append(errs, errors.New("mcp.call: call is required"))
	}
	if m.Server == "" {
		errs = append(errs, errors.New("mcp.call: server is required"))
	}
	if m.Tool == "" {
		errs = append(errs, errors.New("mcp.call: tool is required"))
	}
	return errors.Join(errs...)
}

type RemoteError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type MCPResponse struct {
	CallID string          `json:"call"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RemoteError    `json:"error,omitempty"`
}

func (*MCPResponse) Type() FrameType      { return TypeMCPResponse }
func (*MCPResponse) Direction() Direction { return DirDown }

func (m *MCPResponse) Validate() error {
	if m.CallID == "" {
		return errors.New("mcp.response: call is required")
	}
	if m.Result == nil && m.Error == nil {
		return errors.New("mcp.response: exactly one of result or error is required")
	}
	if m.Result != nil && m.Error != nil {
		return errors.New("mcp.response: result and error are mutually exclusive")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Credentials
// ---------------------------------------------------------------------------

// CredentialKind distinguishes what a grant is for. Forge credentials are
// minted per operation and scoped to a repository; harness credentials are
// session-scoped and materialized to tmpfs.
type CredentialKind string

const (
	CredentialForge   CredentialKind = "forge"
	CredentialHarness CredentialKind = "harness"
)

// CredentialRequest is raised by the runner on behalf of a local git credential
// helper. The gateway decides whether the runner may touch the named resource
// before minting anything.
type CredentialRequest struct {
	RequestID string         `json:"request"`
	Kind      CredentialKind `json:"kind"`
	// Resource is the repository for forge credentials ("owner/name"), empty
	// for harness credentials.
	Resource string `json:"resource,omitempty"`
	ThreadID string `json:"thread,omitempty"`
	TurnID   string `json:"turn,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	Host     string `json:"host,omitempty"`
}

func (*CredentialRequest) Type() FrameType      { return TypeCredentialRequest }
func (*CredentialRequest) Direction() Direction { return DirUp }

func (c *CredentialRequest) Validate() error {
	var errs []error
	if c.RequestID == "" {
		errs = append(errs, errors.New("credential.request: request is required"))
	}
	switch c.Kind {
	case CredentialForge:
		if c.Resource == "" {
			errs = append(errs, errors.New("credential.request: forge credentials require a resource"))
		}
	case CredentialHarness:
	default:
		errs = append(errs, fmt.Errorf("credential.request: unknown kind %q", c.Kind))
	}
	return errors.Join(errs...)
}

type CredentialGrant struct {
	RequestID string         `json:"request"`
	Kind      CredentialKind `json:"kind"`
	Username  string         `json:"username,omitempty"`
	Value     string         `json:"value,omitempty"`
	ExpiresAt *time.Time     `json:"expires_at,omitempty"`
	// Denied carries a policy refusal. The runner surfaces the reason to git
	// rather than returning an empty credential, which would present as an
	// opaque auth failure.
	Denied bool   `json:"denied,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func (*CredentialGrant) Type() FrameType      { return TypeCredentialGrant }
func (*CredentialGrant) Direction() Direction { return DirDown }

func (c *CredentialGrant) Validate() error {
	var errs []error
	if c.RequestID == "" {
		errs = append(errs, errors.New("credential.grant: request is required"))
	}
	if c.Denied {
		if c.Value != "" {
			errs = append(errs, errors.New("credential.grant: denied grant must not carry a value"))
		}
	} else if c.Value == "" {
		errs = append(errs, errors.New("credential.grant: value is required unless denied"))
	}
	return errors.Join(errs...)
}

// Redacted returns a copy safe to log.
func (c *CredentialGrant) Redacted() *CredentialGrant {
	cp := *c
	if cp.Value != "" {
		cp.Value = "[redacted]"
	}
	return &cp
}

// ---------------------------------------------------------------------------
// Bundles
// ---------------------------------------------------------------------------

// MCPKind splits servers by the rule in the design doc: does it need the
// runner's filesystem, or does it need a credential?
type MCPKind string

const (
	MCPLocal   MCPKind = "local"
	MCPProxied MCPKind = "proxied"
)

type MCPServer struct {
	Name    string            `json:"name"`
	Kind    MCPKind           `json:"kind"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

type BundleFile struct {
	// Path is relative to the materialized config directory.
	Path    string `json:"path"`
	Content []byte `json:"content"`
	Mode    uint32 `json:"mode,omitempty"`
}

type BundlePush struct {
	Version int          `json:"version"`
	Digest  string       `json:"digest"`
	Files   []BundleFile `json:"files,omitempty"`
	MCP     []MCPServer  `json:"mcp,omitempty"`
	// Model is the harness model id, empty meaning the harness picks its own
	// default. It rides the bundle rather than the welcome frame because it
	// belongs to the same class as memory and skills — things that define how a
	// session behaves — so changing it marks live sessions stale and the next
	// one starts fresh. That is also the only correct way to change models:
	// prompt caches are per-model, so switching mid-session would silently
	// re-bill the whole conversation at cold-write rates.
	Model string `json:"model,omitempty"`
	// Secrets are resolved values for names referenced by files and MCP env.
	// They exist only in the runner's memory and on tmpfs.
	Secrets map[string]string `json:"secrets,omitempty"`
}

func (*BundlePush) Type() FrameType      { return TypeBundlePush }
func (*BundlePush) Direction() Direction { return DirDown }

// safeBundlePath rejects anything that would escape the materialization root.
// This is a real control, not hygiene: bundle contents are gateway-authored but
// the gateway's own config is operator-authored and may be wrong.
func safeBundlePath(p string) error {
	if p == "" {
		return errors.New("empty path")
	}
	if path.IsAbs(p) || strings.HasPrefix(p, "/") {
		return errors.New("must be relative")
	}
	if strings.Contains(p, `\`) {
		return errors.New("backslashes are not permitted")
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return errors.New("escapes the bundle root")
	}
	if clean != p {
		return fmt.Errorf("must be in clean form (got %q, want %q)", p, clean)
	}
	return nil
}

func (b *BundlePush) Validate() error {
	var errs []error
	if b.Version < 0 {
		errs = append(errs, errors.New("bundle.push: version must be non-negative"))
	}
	if b.Digest == "" {
		errs = append(errs, errors.New("bundle.push: digest is required"))
	}
	seen := make(map[string]bool, len(b.Files))
	for i, f := range b.Files {
		if err := safeBundlePath(f.Path); err != nil {
			errs = append(errs, fmt.Errorf("bundle.push: file %d path %q: %w", i, f.Path, err))
			continue
		}
		if seen[f.Path] {
			errs = append(errs, fmt.Errorf("bundle.push: duplicate file path %q", f.Path))
		}
		seen[f.Path] = true
		if f.Mode&^0o777 != 0 {
			errs = append(errs, fmt.Errorf("bundle.push: file %q has non-permission mode bits", f.Path))
		}
	}
	names := make(map[string]bool, len(b.MCP))
	for i, s := range b.MCP {
		if s.Name == "" {
			errs = append(errs, fmt.Errorf("bundle.push: mcp %d missing name", i))
			continue
		}
		if names[s.Name] {
			errs = append(errs, fmt.Errorf("bundle.push: duplicate mcp server %q", s.Name))
		}
		names[s.Name] = true
		switch s.Kind {
		case MCPLocal:
			if s.Command == "" {
				errs = append(errs, fmt.Errorf("bundle.push: local mcp %q requires a command", s.Name))
			}
		case MCPProxied:
			if s.Command != "" {
				errs = append(errs, fmt.Errorf("bundle.push: proxied mcp %q must not declare a command", s.Name))
			}
		default:
			errs = append(errs, fmt.Errorf("bundle.push: mcp %q has unknown kind %q", s.Name, s.Kind))
		}
	}
	return errors.Join(errs...)
}

// Redacted returns a copy safe to log.
func (b *BundlePush) Redacted() *BundlePush {
	cp := *b
	cp.Files = nil
	if len(b.Secrets) > 0 {
		cp.Secrets = make(map[string]string, len(b.Secrets))
		for k := range b.Secrets {
			cp.Secrets[k] = "[redacted]"
		}
	}
	return &cp
}

// ---------------------------------------------------------------------------
// Usage
// ---------------------------------------------------------------------------

// Usage is the accounting frame. The four token counters are kept separate on
// purpose: cache reads bill at a fraction of input and cache writes at a
// premium, so any collapsed "input + output" figure is wrong by an order of
// magnitude on long threads.
//
// Runners never compute cost. They do not know the price table, the billing
// mode, or which key paid.
type Usage struct {
	ThreadID string `json:"thread"`
	TurnID   string `json:"turn"`
	Model    string `json:"model,omitempty"`

	InputTokens      int64 `json:"input"`
	CacheWriteTokens int64 `json:"cache_write"`
	CacheReadTokens  int64 `json:"cache_read"`
	OutputTokens     int64 `json:"output"`

	// ProviderCostUSD is the harness's own figure, retained only as a
	// cross-check against the gateway's versioned price table.
	ProviderCostUSD *float64 `json:"provider_cost_usd,omitempty"`
	// TTLHint records which cache tier applied ("5m", "1h"), since the write
	// multiplier differs.
	TTLHint string `json:"ttl_hint,omitempty"`

	// Known is false when the adapter cannot report usage. Rollups must count
	// these separately: a dashboard silently reporting $0 for an
	// un-instrumented harness is worse than one reporting nothing.
	Known bool `json:"known"`
}

func (*Usage) Type() FrameType      { return TypeUsage }
func (*Usage) Direction() Direction { return DirUp }

func (u *Usage) Validate() error {
	var errs []error
	if u.TurnID == "" {
		errs = append(errs, errors.New("usage: turn is required"))
	}
	if u.InputTokens < 0 || u.CacheWriteTokens < 0 || u.CacheReadTokens < 0 || u.OutputTokens < 0 {
		errs = append(errs, errors.New("usage: token counters must be non-negative"))
	}
	if u.Known && u.Model == "" {
		errs = append(errs, errors.New("usage: model is required when known is set"))
	}
	if !u.Known && (u.InputTokens|u.CacheWriteTokens|u.CacheReadTokens|u.OutputTokens) != 0 {
		errs = append(errs, errors.New("usage: counters must be zero when known is false"))
	}
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// Bulk transfer control
// ---------------------------------------------------------------------------

type BlobBegin struct {
	BlobID   string `json:"blob"`
	ThreadID string `json:"thread"`
	TurnID   string `json:"turn,omitempty"`
	Name     string `json:"name"`
	Mime     string `json:"mime,omitempty"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256,omitempty"`
}

func (*BlobBegin) Type() FrameType      { return TypeBlobBegin }
func (*BlobBegin) Direction() Direction { return DirBoth }

func (b *BlobBegin) Validate() error {
	var errs []error
	if b.BlobID == "" {
		errs = append(errs, errors.New("blob.begin: blob is required"))
	}
	if b.Size < 0 {
		errs = append(errs, errors.New("blob.begin: size must be non-negative"))
	}
	if b.Size > MaxBlobSize {
		errs = append(errs, fmt.Errorf("blob.begin: size %d exceeds cap %d", b.Size, MaxBlobSize))
	}
	if err := safeFileName(b.Name); err != nil {
		errs = append(errs, fmt.Errorf("blob.begin: name %q: %w", b.Name, err))
	}
	return errors.Join(errs...)
}

// safeFileName enforces that an attachment name is a bare filename. Both peers
// check it, so the guarantee does not depend on either implementation.
func safeFileName(n string) error {
	if n == "" {
		return errors.New("empty")
	}
	if strings.ContainsAny(n, `/\`) {
		return errors.New("must not contain path separators")
	}
	if n == "." || n == ".." {
		return errors.New("reserved name")
	}
	if strings.ContainsRune(n, 0) {
		return errors.New("contains a NUL byte")
	}
	return nil
}

type BlobEnd struct {
	BlobID string `json:"blob"`
	SHA256 string `json:"sha256,omitempty"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

func (*BlobEnd) Type() FrameType      { return TypeBlobEnd }
func (*BlobEnd) Direction() Direction { return DirBoth }

func (b *BlobEnd) Validate() error {
	var errs []error
	if b.BlobID == "" {
		errs = append(errs, errors.New("blob.end: blob is required"))
	}
	if !b.OK && b.Error == "" {
		errs = append(errs, errors.New("blob.end: a failed transfer must carry an error"))
	}
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// Liveness
// ---------------------------------------------------------------------------

type Ping struct {
	Nonce int64 `json:"nonce"`
}

func (*Ping) Type() FrameType      { return TypePing }
func (*Ping) Direction() Direction { return DirBoth }
func (*Ping) Validate() error      { return nil }

type Pong struct {
	Nonce int64 `json:"nonce"`
}

func (*Pong) Type() FrameType      { return TypePong }
func (*Pong) Direction() Direction { return DirBoth }
func (*Pong) Validate() error      { return nil }
