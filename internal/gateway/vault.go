package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/analystaio/splitscreen/config"
	"github.com/analystaio/splitscreen/internal/store"
	"github.com/analystaio/splitscreen/internal/surface"
	"github.com/analystaio/splitscreen/internal/vault"
	"github.com/analystaio/splitscreen/protocol"
)

// The secrets broker (docs/secrets-broker.md). People hand the gateway a key
// through a one-time link; the agent uses it through its runner's egress
// proxy; the gateway attaches it to the outbound request. The value never
// reaches a runner, the chat history, or the agent's context.

const (
	requestSecretTool = "request_secret"
	listSecretsTool   = "list_secrets"
	revokeSecretTool  = "revoke_secret"

	kindStatic = "static"
	// maxSecretValue bounds what intake accepts. API keys are short; anything
	// near this is a mistake (a pasted file) rather than a key.
	maxSecretValue = 8 << 10
)

const secretToolDefs = `{
  "name":"request_secret",
  "description":"Ask the person you are working for to hand you an API key or token, without pasting it in chat. They get a private one-time link to a page where they paste it; you never see the value or the link. Once added, use it with list_secrets. Never ask anyone to paste a key in chat.",
  "inputSchema":{"type":"object","required":["name","hosts"],"additionalProperties":false,"properties":{
    "name":{"type":"string","description":"Short lowercase name for the secret, e.g. stripe-test or vendor-api (letters, digits, - and _)."},
    "hosts":{"type":"array","minItems":1,"maxItems":5,"items":{"type":"string"},"description":"API host names the key is for, e.g. [\"api.stripe.com\"]. No scheme or path. The key will only ever be sent to these."},
    "purpose":{"type":"string","description":"One line on what it is for, shown to the person on the page."},
    "header":{"type":"string","description":"Header the key goes in. Default Authorization."},
    "format":{"type":"string","description":"Header value with {value} where the key goes. Default \"Bearer {value}\"."},
    "query":{"type":"string","description":"Send the key as this query parameter instead of a header."}
  }}
},{
  "name":"list_secrets",
  "description":"List the secrets you can use from this runner, with how to call each one. Values are never shown.",
  "inputSchema":{"type":"object","additionalProperties":false,"properties":{}}
},{
  "name":"revoke_secret",
  "description":"Revoke a secret. Only its owner can, and the value is destroyed.",
  "inputSchema":{"type":"object","required":["name"],"additionalProperties":false,"properties":{
    "name":{"type":"string"}
  }}
}`

// sealerCache keeps the vault key parsed between calls, re-reading it when
// the backend's value changes (a rotation, or a config reload naming another).
type sealerCache struct {
	mu     sync.Mutex
	raw    string
	sealer *vault.Sealer
}

func (g *Gateway) vaultSealer() (*vault.Sealer, error) {
	cfg := g.cfg.Load()
	if !cfg.Gateway.Vault.Enabled() {
		return nil, errors.New("the secrets vault is not configured on this gateway (gateway.vault.key_secret)")
	}
	sec, err := g.secrets.Get(cfg.Gateway.Vault.KeySecret)
	if err != nil {
		return nil, fmt.Errorf("vault key: %w", err)
	}
	g.sealers.mu.Lock()
	defer g.sealers.mu.Unlock()
	if g.sealers.sealer != nil && g.sealers.raw == sec.Value {
		return g.sealers.sealer, nil
	}
	s, err := vault.NewSealer(sec.Value)
	if err != nil {
		return nil, err
	}
	g.sealers.raw, g.sealers.sealer = sec.Value, s
	return s, nil
}

// runnerHardened reports whether per-person secrets can work on a runner: the
// operator says it is hardened and the connected process found it so.
func (g *Gateway) runnerHardened(runner string) bool {
	rc, ok := g.runnerConfig(runner)
	if !ok || !rc.Hardened {
		return false
	}
	c, ok := g.hub.Get(runner)
	return ok && c.hello != nil && c.hello.Hardened
}

// runnerHasLiveTurn reports whether any turn is running on a runner. A secret
// open to anyone still only works while someone is using the runner.
func (g *Gateway) runnerHasLiveTurn(runner string) bool {
	found := false
	g.turns.Range(func(_, v any) bool {
		if t := v.(*turnContext); t.Runner == runner {
			found = true
			return false
		}
		return true
	})
	return found
}

// ---------------------------------------------------------------------------
// Surface tools
// ---------------------------------------------------------------------------

type requestSecretArgs struct {
	Name    string   `json:"name"`
	Hosts   []string `json:"hosts"`
	Purpose string   `json:"purpose"`
	Header  string   `json:"header"`
	Format  string   `json:"format"`
	Query   string   `json:"query"`
}

func decodeStrict(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (g *Gateway) requestSecret(ctx context.Context, runner string, turn *turnContext, raw json.RawMessage) (json.RawMessage, error) {
	if turn == nil || turn.Runner != runner {
		return nil, errors.New("request_secret needs a running turn: ask from a chat thread")
	}
	if turn.ScheduleID != "" {
		return nil, errors.New("request_secret: a scheduled run has nobody to hand a key over")
	}
	cfg := g.cfg.Load()
	if !cfg.Gateway.Intake.Enabled() {
		return nil, errors.New("request_secret: this gateway has no intake page configured (gateway.intake)")
	}
	if _, err := g.vaultSealer(); err != nil {
		return nil, fmt.Errorf("request_secret: %w", err)
	}
	var args requestSecretArgs
	if err := decodeStrict(raw, &args); err != nil {
		return nil, fmt.Errorf("request_secret: bad arguments: %w", err)
	}
	if !vault.ValidName(args.Name) {
		return nil, fmt.Errorf("request_secret: %q is not a valid name: lowercase letters, digits, - and _, up to 63", args.Name)
	}
	pol := vault.Policy{Hosts: args.Hosts, Header: args.Header, Format: args.Format, Query: args.Query, Purpose: truncateLine(args.Purpose, 200)}
	pol.Normalize()
	if err := pol.Validate(); err != nil {
		return nil, fmt.Errorf("request_secret: %w", err)
	}
	if existing, err := g.store.VaultSecret(args.Name); err == nil && existing.RevokedAt == nil && existing.Owner != turn.User.ID {
		return nil, fmt.Errorf("request_secret: %q already belongs to someone else; pick another name", args.Name)
	}

	srf := g.surfaces[turn.Surface]
	if v, ok := srf.(surface.AudienceVerifier); !ok || !v.SenderIsMember(ctx, turn.User.ID) {
		return nil, errors.New("request_secret: only a verified full member can hand over secrets")
	}

	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	ttl := cfg.Gateway.Intake.LinkTTL.Duration()
	if err := g.store.CreateVaultIntake(store.VaultIntake{
		TokenHash: hashToken(token), Name: args.Name, Request: pol.Encode(),
		Surface: turn.Surface, SurfaceUser: turn.User.ID, Runner: runner, ThreadID: turn.ThreadID,
		Channel: turn.Channel, ThreadTS: turn.Thread, ExpiresAt: time.Now().Add(ttl),
	}); err != nil {
		return nil, err
	}
	link := strings.TrimRight(cfg.Gateway.Intake.PublicURL, "/") + "/i/" + token
	text := fmt.Sprintf("Add `%s` (for %s) here: %s\nOnly you can see this. The link works once, for %s.",
		args.Name, strings.Join(pol.Hosts, ", "), link, linkDuration(ttl))
	rc, _ := g.runnerConfig(runner)
	if _, err := srf.Post(ctx, surface.Post{
		Channel: turn.Channel, Thread: turn.Thread, Text: text,
		Ephemeral: true, User: turn.User.ID, Persona: personaFor(rc),
	}); err != nil {
		return nil, fmt.Errorf("request_secret: could not send the link: %w", err)
	}
	_ = g.store.Log(store.Event{Kind: "secret.intake.created", Runner: runner, ThreadID: turn.ThreadID,
		TurnID: turn.TurnID, SurfaceUser: turn.User.ID, Detail: map[string]any{"secret": args.Name, "hosts": pol.Hosts}})
	return toolText(fmt.Sprintf("Sent %s a private one-time link to add %q (expires in %s). "+
		"You will not see the link or the value. The thread will say when it has been added; then call list_secrets.",
		turn.User.Display, args.Name, linkDuration(ttl))), nil
}

func (g *Gateway) listSecrets(runner string, turn *turnContext) (json.RawMessage, error) {
	rc, ok := g.runnerConfig(runner)
	if !ok || !rc.Egress {
		return toolText("This runner cannot use secrets (egress is off in the gateway config)."), nil
	}
	list, err := g.store.VaultSecrets()
	if err != nil {
		return nil, err
	}
	hardened := g.runnerHardened(runner)
	var b strings.Builder
	now := time.Now()
	for _, s := range list {
		pol, err := vault.DecodePolicy(s.Policy)
		if err != nil || !pol.AllowsRunner(runner) || (s.ExpiresAt != nil && now.After(*s.ExpiresAt)) {
			continue
		}
		who := "anyone using this runner"
		if pol.Users == vault.UsersOwner {
			who = "only in turns started by its owner"
			if !hardened {
				who += " (this runner is not hardened, so it cannot be used here)"
			} else if turn != nil && turn.User.ID != s.Owner {
				who += " (not the person asking now)"
			}
		}
		fmt.Fprintf(&b, "- %s: owner <@%s>, usable %s\n", s.Name, s.Owner, who)
		fmt.Fprintf(&b, "  call: \"$SPLITSCREEN_EGRESS/s/%s/<path>\" goes to https://%s/<path>\n", s.Name, pol.Hosts[0])
		for _, h := range pol.Hosts[1:] {
			fmt.Fprintf(&b, "        \"$SPLITSCREEN_EGRESS/s/%s@%s/<path>\" goes to https://%s/<path>\n", s.Name, h, h)
		}
		if len(pol.Methods) > 0 {
			fmt.Fprintf(&b, "  methods: %s\n", strings.Join(pol.Methods, ", "))
		}
		if pol.Purpose != "" {
			fmt.Fprintf(&b, "  purpose: %s\n", pol.Purpose)
		}
		if s.ExpiresAt != nil {
			fmt.Fprintf(&b, "  expires: %s\n", s.ExpiresAt.Format("2006-01-02"))
		}
	}
	if b.Len() == 0 {
		return toolText("No secrets are available to this runner. Ask for one with request_secret."), nil
	}
	return toolText("The gateway attaches the key; do not add an Authorization header. " +
		"Point SDKs at the URL as their base URL.\n" + b.String()), nil
}

func (g *Gateway) revokeSecret(runner string, turn *turnContext, raw json.RawMessage) (json.RawMessage, error) {
	if turn == nil || turn.Runner != runner {
		return nil, errors.New("revoke_secret needs a running turn: ask from a chat thread")
	}
	var args struct {
		Name string `json:"name"`
	}
	if err := decodeStrict(raw, &args); err != nil {
		return nil, fmt.Errorf("revoke_secret: bad arguments: %w", err)
	}
	s, err := g.store.VaultSecret(args.Name)
	if err != nil || s.RevokedAt != nil {
		return nil, fmt.Errorf("revoke_secret: no secret called %q", args.Name)
	}
	if s.Owner != turn.User.ID {
		return nil, fmt.Errorf("revoke_secret: only its owner, <@%s>, can revoke %q", s.Owner, args.Name)
	}
	if err := g.store.RevokeVaultSecret(args.Name); err != nil {
		return nil, err
	}
	_ = g.store.Log(store.Event{Kind: "secret.revoked", Runner: runner, ThreadID: turn.ThreadID,
		TurnID: turn.TurnID, SurfaceUser: turn.User.ID, Detail: map[string]any{"secret": args.Name}})
	return toolText(fmt.Sprintf("Revoked %q and destroyed its value. Revoke the key with its provider too.", args.Name)), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// linkDuration reads "15 minutes" or "2 hours" for a link's lifetime.
func linkDuration(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
	return fmt.Sprintf("%d minutes", int(d.Minutes()))
}

// ---------------------------------------------------------------------------
// Egress
// ---------------------------------------------------------------------------

// hopHeaders and credential headers never pass from the agent upstream: the
// agent must not be able to smuggle a credential of its own to a trusted host,
// or steer the connection.
var droppedRequestHeaders = []string{
	"Authorization", "Proxy-Authorization", "Cookie", "Host", "Connection", "Keep-Alive",
	"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "Content-Length",
	"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Real-Ip",
	// Left to the client, which then decompresses before redaction sees the
	// body. A compressed body passed through would hide an echoed key.
	"Accept-Encoding",
}

var droppedResponseHeaders = []string{
	"Set-Cookie", "Set-Cookie2", "Connection", "Keep-Alive", "Transfer-Encoding", "Content-Length",
}

type egressRefusal struct {
	code string
	msg  string
}

func (e *egressRefusal) Error() string { return e.msg }

func refuse(code, format string, args ...any) error {
	return &egressRefusal{code: code, msg: fmt.Sprintf(format, args...)}
}

func (g *Gateway) onEgressRequest(ctx context.Context, c *Conn, fr *protocol.EgressRequest) {
	if serr := c.Send(g.handleEgress(ctx, c, fr)); serr != nil {
		g.log.Error("egress response send failed", "err", serr)
	}
}

// handleEgress decides, makes the call, and audits it, whatever the outcome.
func (g *Gateway) handleEgress(ctx context.Context, c *Conn, fr *protocol.EgressRequest) *protocol.EgressResponse {
	started := time.Now()
	turn, _ := g.turnFor(fr.TurnID)
	resp, host, err := g.egress(ctx, c, fr, turn)

	detail := map[string]any{
		"secret": fr.Secret, "host": host, "method": fr.Method, "path": pathOnly(fr.Path),
		"attributed": fr.Attributed, "ms": time.Since(started).Milliseconds(),
	}
	ev := store.Event{Kind: "secret.used", Runner: c.runner, ThreadID: fr.ThreadID, TurnID: fr.TurnID}
	if turn != nil {
		ev.SurfaceUser = turn.User.ID
	}
	if err != nil {
		code := "egress_failed"
		var r *egressRefusal
		if errors.As(err, &r) {
			code = r.code
			ev.Kind = "secret.refused"
		}
		detail["error"] = err.Error()
		ev.Detail = detail
		_ = g.store.Log(ev)
		g.log.Warn("egress refused", "runner", c.runner, "secret", fr.Secret, "reason", err)
		resp = &protocol.EgressResponse{RequestID: fr.RequestID, Error: &protocol.RemoteError{Code: code, Message: err.Error()}}
	} else {
		detail["status"] = resp.Status
		detail["bytes"] = len(resp.Body)
		ev.Detail = detail
		_ = g.store.Log(ev)
		_ = g.store.TouchVaultSecret(fr.Secret)
	}
	return resp
}

func pathOnly(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}

// egress decides and, if allowed, makes one call. It returns the host it chose
// for the audit record.
func (g *Gateway) egress(ctx context.Context, c *Conn, fr *protocol.EgressRequest, turn *turnContext) (*protocol.EgressResponse, string, error) {
	cfg := g.cfg.Load()
	rc, ok := cfg.Runners[c.runner]
	if !ok || !rc.Egress {
		return nil, "", refuse("egress_off", "egress is off for runner %q", c.runner)
	}
	sealer, err := g.vaultSealer()
	if err != nil {
		return nil, "", err
	}
	sec, err := g.store.VaultSecret(fr.Secret)
	if err != nil || sec.RevokedAt != nil {
		return nil, "", refuse("not_found", "no secret called %q (list_secrets shows what exists)", fr.Secret)
	}
	if sec.ExpiresAt != nil && time.Now().After(*sec.ExpiresAt) {
		return nil, "", refuse("expired", "secret %q expired on %s", fr.Secret, sec.ExpiresAt.Format("2006-01-02"))
	}
	pol, err := vault.DecodePolicy(sec.Policy)
	if err != nil {
		return nil, "", fmt.Errorf("secret %q has an unreadable policy: %w", fr.Secret, err)
	}

	host := fr.Host
	if host == "" {
		host = pol.Hosts[0]
	}
	host = strings.ToLower(host)
	switch {
	case !pol.AllowsHost(host):
		return nil, host, refuse("host", "%q may only be sent to %s", fr.Secret, strings.Join(pol.Hosts, ", "))
	case egressHostDenied(cfg, host):
		return nil, host, refuse("host", "%s is never reachable through egress", host)
	case !pol.AllowsRunner(c.runner):
		return nil, host, refuse("runner", "%q is not available to runner %q", fr.Secret, c.runner)
	case !pol.AllowsMethod(fr.Method):
		return nil, host, refuse("method", "%q only allows %s", fr.Secret, strings.Join(pol.Methods, ", "))
	}

	// Who is asking. Attribution is believed only from a runner the operator
	// says is hardened and that found itself so, and only for a turn that is
	// running on this runner in the thread it names.
	attributed := fr.Attributed && rc.Hardened && c.hello != nil && c.hello.Hardened &&
		turn != nil && turn.Runner == c.runner && turn.ThreadID == fr.ThreadID
	switch pol.Users {
	case vault.UsersOwner:
		if !attributed {
			if !g.runnerHardened(c.runner) {
				return nil, host, refuse("owner_only", "%q is limited to its owner, and runner %q is not hardened, so it cannot tell who asked", fr.Secret, c.runner)
			}
			return nil, host, refuse("owner_only", "%q is limited to its owner, and this request could not be traced to a turn (run it from the session, not a detached process)", fr.Secret)
		}
		if turn.User.ID != sec.Owner {
			return nil, host, refuse("owner_only", "%q may only be used in turns started by its owner <@%s>", fr.Secret, sec.Owner)
		}
	default:
		if !attributed && !g.runnerHasLiveTurn(c.runner) {
			return nil, host, refuse("no_turn", "secrets can only be used while a turn is running")
		}
	}

	value, err := sealer.Open(sec.Name, sec.Ciphertext, sec.Nonce, sec.KeyID)
	if err != nil {
		return nil, host, err
	}

	target, err := url.Parse("https://" + host + fr.Path)
	if err != nil || target.Host != host || target.User != nil {
		return nil, host, refuse("path", "path %q does not form a URL on %s", fr.Path, host)
	}
	if pol.Query != "" {
		q := target.Query()
		q.Set(pol.Query, value)
		target.RawQuery = q.Encode()
	}

	callCtx, cancel := context.WithTimeout(ctx, cfg.Gateway.Egress.Timeout.Duration())
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, fr.Method, target.String(), bytes.NewReader(fr.Body))
	if err != nil {
		return nil, host, refuse("request", "bad request: %v", err)
	}
	for k, vs := range fr.Headers {
		ck := http.CanonicalHeaderKey(k)
		if slices.Contains(droppedRequestHeaders, ck) || strings.HasPrefix(ck, "Proxy-") ||
			(pol.Header != "" && ck == http.CanonicalHeaderKey(pol.Header)) {
			continue
		}
		for _, v := range vs {
			req.Header.Add(ck, v)
		}
	}
	if pol.Query == "" {
		req.Header.Set(pol.Header, pol.HeaderValue(value))
	}
	if len(fr.Body) > 0 {
		req.ContentLength = int64(len(fr.Body))
	}

	res, err := egressDo(req)
	if err != nil {
		return nil, host, fmt.Errorf("calling %s: %v", host, redact(err.Error(), value, sec.Name))
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, protocol.MaxEgressBody+1))
	if err != nil {
		return nil, host, fmt.Errorf("reading %s's response: %v", host, err)
	}
	if len(body) > protocol.MaxEgressBody {
		return nil, host, refuse("too_large", "%s answered with more than %d KiB, the egress limit", host, protocol.MaxEgressBody>>10)
	}

	out := &protocol.EgressResponse{RequestID: fr.RequestID, Status: res.StatusCode, Headers: map[string][]string{}}
	for k, vs := range res.Header {
		if slices.Contains(droppedResponseHeaders, http.CanonicalHeaderKey(k)) {
			continue
		}
		for _, v := range vs {
			out.Headers[k] = append(out.Headers[k], redact(v, value, sec.Name))
		}
	}
	out.Body = []byte(redact(string(body), value, sec.Name))
	return out, host, nil
}

// redact replaces the secret's value, raw and URL-encoded, wherever an API
// echoes it back.
func redact(s, value, name string) string {
	if value == "" {
		return s
	}
	mark := "[secret:" + name + "]"
	s = strings.ReplaceAll(s, value, mark)
	if esc := url.QueryEscape(value); esc != value {
		s = strings.ReplaceAll(s, esc, mark)
	}
	return s
}

func egressHostDenied(cfg *config.Config, host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	for _, d := range append(slices.Clone(config.DefaultEgressDenyHosts), cfg.Gateway.Egress.DenyHosts...) {
		if strings.EqualFold(h, d) {
			return true
		}
	}
	return false
}

// egressDo makes the upstream call. A variable so tests can point it at a
// local server, which the real client refuses to reach.
var egressDo = func(req *http.Request) (*http.Response, error) { return egressClient().Do(req) }

var (
	egressClientOnce sync.Once
	egressHTTP       *http.Client
)

// egressClient never follows redirects (the 3xx goes back to the agent, and
// following it means a new request that passes policy again), ignores proxy
// settings in the gateway's environment, and refuses to connect to loopback,
// link-local or unspecified addresses whatever DNS says.
func egressClient() *http.Client {
	egressClientOnce.Do(func() {
		dialer := &net.Dialer{Timeout: 10 * time.Second, Control: refuseLocalAddrs}
		tr := &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          50,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		}
		egressHTTP = &http.Client{
			Transport:     tr,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	})
	return egressHTTP
}

func refuseLocalAddrs(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() {
		return fmt.Errorf("egress refuses to connect to %s", host)
	}
	return nil
}
