package gateway

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/analystaio/splitscreen/config"
	"github.com/analystaio/splitscreen/internal/store"
	"github.com/analystaio/splitscreen/internal/surface"
	"github.com/analystaio/splitscreen/internal/vault"
)

// The intake page is where a person pastes a secret. It is reached only
// through a one-time link the gateway sent that person privately; the agent
// that asked never sees the link. It listens on its own address, apart from
// the runner endpoint, so it can be exposed (behind tailscale serve or an
// access proxy) without exposing anything else.

// ServeIntake runs the intake page until ctx is cancelled. A config reload
// does not move it: changing gateway.intake.listen needs a restart.
func (g *Gateway) ServeIntake(ctx context.Context) error {
	addr := g.cfg.Load().Gateway.Intake.Listen
	srv := &http.Server{
		Addr:              addr,
		Handler:           g.intakeHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("intake: listen on %s: %w", addr, err)
	}
	g.log.Info("intake page listening", "addr", ln.Addr().String())
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (g *Gateway) intakeHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /i/{token}", g.intakeForm)
	mux.HandleFunc("POST /i/{token}", g.intakeSubmit)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		intakeHeaders(w)
		http.Error(w, "not found", http.StatusNotFound)
	})
	return mux
}

func intakeHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
}

// intakeLookup finds a live link. The reason is safe to show: it never says
// whether a token ever existed.
func (g *Gateway) intakeLookup(r *http.Request) (store.VaultIntake, string) {
	token := r.PathValue("token")
	if len(token) < 16 || len(token) > 64 {
		return store.VaultIntake{}, "This link is not valid."
	}
	in, err := g.store.VaultIntake(hashToken(token))
	if err != nil || in.UsedAt != nil || !time.Now().Before(in.ExpiresAt) {
		return store.VaultIntake{}, "This link has expired or was already used. Ask the agent for a new one."
	}
	return in, ""
}

// intakeIdentity applies the tailnet mode's second check: the browser's
// Tailscale login must be the person the link was made for.
func (g *Gateway) intakeIdentity(cfg *config.Config, r *http.Request, in store.VaultIntake) string {
	if cfg.Gateway.Intake.Mode != config.IntakeTailnet {
		return ""
	}
	person, ok := cfg.People[in.SurfaceUser]
	if !ok {
		return "This gateway does not know your tailnet login, so it cannot accept the key. Ask its operator to add you to people."
	}
	login := r.Header.Get("Tailscale-User-Login")
	if login == "" {
		return "Open this link over the tailnet."
	}
	if !strings.EqualFold(login, person.Tailnet) {
		return "This link was made for someone else."
	}
	return ""
}

type intakeView struct {
	Name, Purpose, Hosts, Header, Format, Query string
	Users                                       string
	Expires                                     string
	Hardened                                    bool
	Runner                                      string
	Error                                       string
	Done                                        bool
	Message                                     string
}

var intakeTmpl = template.Must(template.New("intake").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Add a secret</title>
<style>
body{font:15px/1.5 system-ui,sans-serif;max-width:34rem;margin:2.5rem auto;padding:0 1rem;color:#1d1d1f}
@media(prefers-color-scheme:dark){body{background:#161618;color:#ececec}input,select{background:#222;color:#ececec;border-color:#444}}
h1{font-size:1.3rem;margin:0 0 .25rem}p.sub{margin:0 0 1.5rem;opacity:.7}
label{display:block;font-weight:600;margin:1rem 0 .3rem}small{display:block;font-weight:400;opacity:.7}
input,select{width:100%;box-sizing:border-box;padding:.55rem .6rem;font:inherit;border:1px solid #c7c7cc;border-radius:6px}
details{margin-top:1rem}summary{cursor:pointer;opacity:.8}
.err{background:#fde8e8;color:#8a1c1c;padding:.7rem .9rem;border-radius:6px}
.ok{background:#e6f4ea;color:#1e5b2e;padding:.7rem .9rem;border-radius:6px}
.warn{opacity:.85;font-size:.9rem;margin-top:.3rem}
button{margin-top:1.5rem;padding:.6rem 1.2rem;font:inherit;font-weight:600;border:0;border-radius:6px;background:#2f6fde;color:#fff;cursor:pointer}
code{font-size:.92em}
</style></head><body>
{{if .Done}}<h1>Added</h1><p class="ok">{{.Message}}</p><p>You can close this page.</p>
{{else if .Error}}{{if .Name}}<h1>Add <code>{{.Name}}</code></h1>{{else}}<h1>Add a secret</h1>{{end}}<p class="err">{{.Error}}</p>
{{else}}
<h1>Add <code>{{.Name}}</code></h1>
<p class="sub">{{if .Purpose}}{{.Purpose}}. {{end}}The agent will be able to use it, never to read it.</p>
<form method="post" autocomplete="off">
<label for="value">Key or token</label>
<input id="value" name="value" type="password" required autofocus spellcheck="false" maxlength="8192">
<label for="hosts">Only ever sent to<small>Host names, comma separated</small></label>
<input id="hosts" name="hosts" value="{{.Hosts}}" required spellcheck="false">
<label for="users">Who can use it</label>
<select id="users" name="users">
<option value="owner"{{if eq .Users "owner"}} selected{{end}}>Only when I ask</option>
<option value="any"{{if eq .Users "any"}} selected{{end}}>Anyone who talks to {{.Runner}}</option>
</select>
{{if not .Hardened}}<p class="warn">{{.Runner}} is not hardened, so it cannot tell who asked: a key limited to you will not work there until it is.</p>{{end}}
<label for="expires">Expires<small>Optional</small></label>
<input id="expires" name="expires" type="date">
<details><summary>How it is sent</summary>
<label for="header">Header</label><input id="header" name="header" value="{{.Header}}">
<label for="format">Header value<small>{value} is replaced by the key</small></label><input id="format" name="format" value="{{.Format}}">
<label for="query">Or as query parameter<small>Leave the header empty to use this</small></label><input id="query" name="query" value="{{.Query}}">
</details>
<button type="submit">Add secret</button>
</form>
{{end}}
</body></html>`))

func (g *Gateway) renderIntake(w http.ResponseWriter, status int, v intakeView) {
	intakeHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = intakeTmpl.Execute(w, v)
}

func (g *Gateway) intakeForm(w http.ResponseWriter, r *http.Request) {
	in, why := g.intakeLookup(r)
	if why != "" {
		g.renderIntake(w, http.StatusGone, intakeView{Error: why})
		return
	}
	if !g.runnerEgress(in.Runner) {
		g.renderIntake(w, http.StatusGone, intakeView{Name: in.Name, Error: "The agent that asked can no longer use secrets, so nothing would be stored. Ask its operator."})
		return
	}
	cfg := g.cfg.Load()
	if why := g.intakeIdentity(cfg, r, in); why != "" {
		g.renderIntake(w, http.StatusForbidden, intakeView{Name: in.Name, Error: why})
		return
	}
	pol, _ := vault.DecodePolicy(in.Request)
	hardened := g.runnerHardened(in.Runner)
	users := vault.UsersOwner
	if !hardened {
		users = vault.UsersAny
	}
	g.renderIntake(w, http.StatusOK, intakeView{
		Name: in.Name, Purpose: pol.Purpose, Hosts: strings.Join(pol.Hosts, ", "),
		Header: pol.Header, Format: pol.Format, Query: pol.Query, Users: users,
		Hardened: hardened, Runner: runnerDisplay(cfg, in.Runner),
	})
}

func runnerDisplay(cfg *config.Config, runner string) string {
	if rc, ok := cfg.Runners[runner]; ok && rc.Display.Name != "" {
		return rc.Display.Name
	}
	return runner
}

func (g *Gateway) intakeSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	in, why := g.intakeLookup(r)
	if why != "" {
		g.renderIntake(w, http.StatusGone, intakeView{Error: why})
		return
	}
	if !g.runnerEgress(in.Runner) {
		g.renderIntake(w, http.StatusGone, intakeView{Name: in.Name, Error: "The agent that asked can no longer use secrets, so nothing would be stored. Ask its operator."})
		return
	}
	cfg := g.cfg.Load()
	if why := g.intakeIdentity(cfg, r, in); why != "" {
		g.renderIntake(w, http.StatusForbidden, intakeView{Name: in.Name, Error: why})
		return
	}
	if err := r.ParseForm(); err != nil {
		g.renderIntake(w, http.StatusBadRequest, intakeView{Name: in.Name, Error: "The form could not be read."})
		return
	}
	value := strings.TrimSpace(r.PostForm.Get("value"))
	fail := func(msg string) {
		g.renderIntake(w, http.StatusBadRequest, intakeView{Name: in.Name, Error: msg + " Go back and try again."})
	}
	switch {
	case value == "":
		fail("The key is empty.")
		return
	case len(value) > maxSecretValue:
		fail("That is too long to be a key.")
		return
	case strings.ContainsAny(value, "\r\n"):
		fail("A key is one line.")
		return
	}

	requested, _ := vault.DecodePolicy(in.Request)
	pol := vault.Policy{
		Header: strings.TrimSpace(r.PostForm.Get("header")),
		Format: strings.TrimSpace(r.PostForm.Get("format")),
		Query:  strings.TrimSpace(r.PostForm.Get("query")),
		Users:  r.PostForm.Get("users"),
		// The runner that asked is the one that gets it. Widening that is an
		// operator's job, not something a link should allow.
		Runners: []string{in.Runner},
		Purpose: requested.Purpose,
	}
	if pol.Query != "" && pol.Header == "" {
		pol.Format = ""
	}
	for _, h := range strings.Split(r.PostForm.Get("hosts"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			pol.Hosts = append(pol.Hosts, h)
		}
	}
	pol.Normalize()
	if err := pol.Validate(); err != nil {
		fail(err.Error() + ".")
		return
	}
	var expires *time.Time
	if e := strings.TrimSpace(r.PostForm.Get("expires")); e != "" {
		t, err := time.ParseInLocation("2006-01-02", e, time.UTC)
		if err != nil || !t.After(time.Now()) {
			fail("The expiry must be a date in the future.")
			return
		}
		t = t.Add(24*time.Hour - time.Second)
		expires = &t
	}

	sealer, err := g.vaultSealer()
	if err != nil {
		g.log.Error("intake: vault unavailable", "err", err)
		g.renderIntake(w, http.StatusServiceUnavailable, intakeView{Name: in.Name, Error: "The vault is not available. Nothing was stored."})
		return
	}
	ct, nonce, err := sealer.Seal(in.Name, value)
	if err != nil {
		g.renderIntake(w, http.StatusInternalServerError, intakeView{Name: in.Name, Error: "Encryption failed. Nothing was stored."})
		return
	}
	// Claim last, so a mistake in the form does not burn the link; then store.
	if err := g.store.ClaimVaultIntake(in.TokenHash); err != nil {
		g.renderIntake(w, http.StatusGone, intakeView{Error: "This link has expired or was already used. Ask the agent for a new one."})
		return
	}
	err = g.store.PutVaultSecret(store.VaultSecret{
		Name: in.Name, Owner: in.SurfaceUser, Kind: kindStatic, Ciphertext: ct, Nonce: nonce,
		KeyID: sealer.KeyID(), Policy: pol.Encode(), ExpiresAt: expires,
	})
	if errors.Is(err, store.ErrSecretOwned) {
		g.renderIntake(w, http.StatusConflict, intakeView{Name: in.Name, Error: "Someone else already has a secret with this name. Ask the agent for a link with another name."})
		return
	}
	if err != nil {
		g.log.Error("intake: store failed", "err", err)
		g.renderIntake(w, http.StatusInternalServerError, intakeView{Name: in.Name, Error: "Storing it failed. Ask the agent for a new link."})
		return
	}

	who := "only when you ask"
	if pol.Users == vault.UsersAny {
		who = "by anyone who talks to " + runnerDisplay(cfg, in.Runner)
	}
	_ = g.store.Log(store.Event{Kind: "secret.created", Runner: in.Runner, ThreadID: in.ThreadID,
		SurfaceUser: in.SurfaceUser, Detail: map[string]any{
			"secret": in.Name, "hosts": pol.Hosts, "users": pol.Users, "via": cfg.Gateway.Intake.Mode,
		}})
	if srf, ok := g.surfaces[in.Surface]; ok {
		rc, _ := g.runnerConfig(in.Runner)
		text := fmt.Sprintf("<@%s> added secret `%s` (sent only to %s, usable %s).",
			in.SurfaceUser, in.Name, strings.Join(pol.Hosts, ", "), who)
		if _, err := srf.Post(r.Context(), surface.Post{
			Channel: in.Channel, Thread: in.ThreadTS, Text: text, Persona: personaFor(rc),
		}); err != nil {
			g.log.Warn("intake: thread notice failed", "err", err)
		}
	}
	g.renderIntake(w, http.StatusOK, intakeView{Done: true, Message: fmt.Sprintf(
		"%s is stored. It will be sent only to %s, and used %s.", in.Name, strings.Join(pol.Hosts, ", "), who)})
}
