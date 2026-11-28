package gateway

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/analystaio/splitscreen/config"
	"github.com/analystaio/splitscreen/internal/surface"
	"github.com/analystaio/splitscreen/internal/vault"
	"github.com/analystaio/splitscreen/protocol"
)

// vaultHarness is channelHarness with the broker configured and a fake
// upstream API the egress client is pointed at.
type vaultHarness struct {
	*harness
	srf      *channelSurface
	upstream *httptest.Server
	seen     chan *http.Request
	conn     *Conn
}

func newVaultHarness(t *testing.T) *vaultHarness {
	t.Helper()
	h, c := channelHarness(t)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	t.Setenv("SPLITSCREEN_SECRET_VAULT_KEY", base64.StdEncoding.EncodeToString(key))

	cfg := h.gw.cfg.Load()
	cfg.Gateway.Vault.KeySecret = "vault-key"
	cfg.Gateway.Intake = config.Intake{Listen: "127.0.0.1:0", PublicURL: "https://gw.test", Mode: config.IntakeLinkOnly, LinkTTL: config.Duration(15 * time.Minute)}
	cfg.Gateway.Egress.Timeout = config.Duration(5 * time.Second)
	cfg.Runners["alpha"].Egress = true

	vh := &vaultHarness{harness: h, srf: c, seen: make(chan *http.Request, 10)}
	vh.upstream = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		vh.seen <- r
		w.Header().Set("Set-Cookie", "session=1")
		w.Header().Set("X-Echo", r.Header.Get("Authorization"))
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"key":"` + r.Header.Get("Authorization") + `","q":"` + r.URL.Query().Get("api_key") + `","body":` + string(body) + `}`))
	}))
	t.Cleanup(vh.upstream.Close)
	client := vh.upstream.Client()
	addr := strings.TrimPrefix(vh.upstream.URL, "https://")
	orig := egressDo
	egressDo = func(req *http.Request) (*http.Response, error) {
		r2 := req.Clone(req.Context())
		r2.URL.Host = addr
		r2.Host = req.URL.Host
		return client.Do(r2)
	}
	t.Cleanup(func() { egressDo = orig })

	vh.conn = &Conn{runner: "alpha", gw: h.gw, hello: &protocol.Hello{}}
	h.gw.hub.Register(vh.conn)
	return vh
}

func (vh *vaultHarness) tool(t *testing.T, turn *turnContext, tool, args string) (string, error) {
	t.Helper()
	out, err := vh.gw.callSurfaceTool(context.Background(), "alpha", turn, &protocol.MCPCall{
		Server: "chat", Tool: tool, Args: json.RawMessage(args),
	})
	if err != nil {
		return "", err
	}
	return resultText(t, out), nil
}

var linkRe = regexp.MustCompile(`https://gw\.test/i/([A-Za-z0-9_-]+)`)

// requestLink runs request_secret and returns the token from the private link.
func (vh *vaultHarness) requestLink(t *testing.T, turn *turnContext, args string) string {
	t.Helper()
	out, err := vh.tool(t, turn, requestSecretTool, args)
	if err != nil {
		t.Fatal(err)
	}
	if linkRe.MatchString(out) {
		t.Fatalf("the agent saw the link: %s", out)
	}
	posts := vh.srf.postsCopy()
	last := posts[len(posts)-1]
	if !last.Ephemeral || last.User != turn.User.ID {
		t.Fatalf("link was not sent privately to the requester: %+v", last)
	}
	m := linkRe.FindStringSubmatch(last.Text)
	if m == nil {
		t.Fatalf("no link in %q", last.Text)
	}
	return m[1]
}

func (vh *vaultHarness) submit(t *testing.T, token string, form url.Values, hdr map[string]string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/i/"+token, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	vh.gw.intakeHandler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func liveTurn(vh *vaultHarness, user string) *turnContext {
	turn := &turnContext{TurnID: "turn-" + user, ThreadID: "test:C1:T1", Surface: "test", Channel: "C1", Thread: "T1",
		Runner: "alpha", User: surface.User{ID: user, Display: user}}
	vh.gw.turns.Store(turn.TurnID, turn)
	return turn
}

func (vh *vaultHarness) call(t *testing.T, fr *protocol.EgressRequest, turn *turnContext) (*protocol.EgressResponse, error) {
	t.Helper()
	if fr.RequestID == "" {
		fr.RequestID = "e1"
	}
	if fr.Method == "" {
		fr.Method = "GET"
	}
	if fr.Path == "" {
		fr.Path = "/v1/things"
	}
	_ = turn // the gateway finds the turn from fr.TurnID, as it does for a runner
	resp := vh.gw.handleEgress(context.Background(), vh.conn, fr)
	if resp.Error != nil {
		return nil, errors.New(resp.Error.Message)
	}
	return resp, nil
}

func addSecret(t *testing.T, vh *vaultHarness, turn *turnContext, name string, form url.Values) {
	t.Helper()
	tok := vh.requestLink(t, turn, `{"name":"`+name+`","hosts":["api.vendor.test"],"purpose":"orders"}`)
	if form.Get("value") == "" {
		form.Set("value", "sk_test_SECRET")
	}
	if form.Get("hosts") == "" {
		form.Set("hosts", "api.vendor.test")
	}
	if form.Get("header") == "" && form.Get("query") == "" {
		form.Set("header", "Authorization")
		form.Set("format", "Bearer {value}")
	}
	if code, body := vh.submit(t, tok, form, nil); code != 200 {
		t.Fatalf("submit: %d %s", code, body)
	}
}

func TestIntakeStoresEncryptedAndLinkWorksOnce(t *testing.T) {
	vh := newVaultHarness(t)
	turn := memberTurn()
	tok := vh.requestLink(t, turn, `{"name":"vendor","hosts":["api.vendor.test"],"purpose":"orders"}`)

	// The form renders, says what the key is for, and is never cached.
	rec := httptest.NewRecorder()
	vh.gw.intakeHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/i/"+tok, nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "api.vendor.test") || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("form: %d %q", rec.Code, rec.Header())
	}

	// A bad form does not burn the link.
	if code, _ := vh.submit(t, tok, url.Values{"value": {""}, "hosts": {"api.vendor.test"}}, nil); code != 400 {
		t.Fatalf("empty value: %d", code)
	}
	if code, _ := vh.submit(t, tok, url.Values{"value": {"k"}, "hosts": {"https://evil.test/x"}}, nil); code != 400 {
		t.Fatalf("bad host: %d", code)
	}

	form := url.Values{"value": {"sk_test_SECRET"}, "hosts": {"api.vendor.test"}, "users": {"any"},
		"header": {"Authorization"}, "format": {"Bearer {value}"}}
	if code, body := vh.submit(t, tok, form, nil); code != 200 {
		t.Fatalf("submit: %d %s", code, body)
	}
	if code, _ := vh.submit(t, tok, form, nil); code != http.StatusGone {
		t.Fatalf("second use: %d", code)
	}

	s, err := vh.st.VaultSecret("vendor")
	if err != nil {
		t.Fatal(err)
	}
	if s.Owner != "UMEMBER" || strings.Contains(string(s.Ciphertext), "SECRET") {
		t.Fatalf("stored %+v", s)
	}
	pol, _ := vault.DecodePolicy(s.Policy)
	if len(pol.Runners) != 1 || pol.Runners[0] != "alpha" || pol.Users != vault.UsersAny {
		t.Fatalf("policy %+v", pol)
	}
	// The thread hears about it, without the value.
	all := vh.srf.allText()
	if !strings.Contains(all, "added secret `vendor`") || strings.Contains(all, "SECRET") {
		t.Fatalf("thread: %s", all)
	}
}

func TestIntakeTailnetModeChecksTheBrowser(t *testing.T) {
	vh := newVaultHarness(t)
	cfg := vh.gw.cfg.Load()
	cfg.Gateway.Intake.Mode = config.IntakeTailnet
	cfg.People = map[string]config.Person{"UMEMBER": {Tailnet: "member@example.com"}}
	tok := vh.requestLink(t, memberTurn(), `{"name":"vendor","hosts":["api.vendor.test"]}`)
	form := url.Values{"value": {"k"}, "hosts": {"api.vendor.test"}, "users": {"any"}, "header": {"Authorization"}, "format": {"Bearer {value}"}}

	if code, _ := vh.submit(t, tok, form, nil); code != 403 {
		t.Fatalf("no tailnet header: %d", code)
	}
	if code, _ := vh.submit(t, tok, form, map[string]string{"Tailscale-User-Login": "someone@example.com"}); code != 403 {
		t.Fatalf("wrong person: %d", code)
	}
	if code, body := vh.submit(t, tok, form, map[string]string{"Tailscale-User-Login": "Member@example.com"}); code != 200 {
		t.Fatalf("right person: %d %s", code, body)
	}
}

func TestRequestSecretChecks(t *testing.T) {
	vh := newVaultHarness(t)
	if _, err := vh.tool(t, nil, requestSecretTool, `{"name":"x","hosts":["a.test"]}`); err == nil {
		t.Fatal("worked without a turn")
	}
	guest := memberTurn()
	guest.User.ID = "UGUEST"
	if _, err := vh.tool(t, guest, requestSecretTool, `{"name":"x","hosts":["a.test"]}`); err == nil {
		t.Fatal("a guest got a link")
	}
	for _, args := range []string{`{"name":"Bad Name","hosts":["a.test"]}`, `{"name":"x","hosts":["127.0.0.1"]}`, `{"name":"x","hosts":[]}`} {
		if _, err := vh.tool(t, memberTurn(), requestSecretTool, args); err == nil {
			t.Errorf("%s accepted", args)
		}
	}
	vh.gw.cfg.Load().Gateway.Intake.Listen = ""
	if _, err := vh.tool(t, memberTurn(), requestSecretTool, `{"name":"x","hosts":["a.test"]}`); err == nil {
		t.Fatal("worked with intake off")
	}
}

func TestEgressAttachesKeyAndCleansUp(t *testing.T) {
	vh := newVaultHarness(t)
	addSecret(t, vh, memberTurn(), "vendor", url.Values{"users": {"any"}})
	turn := liveTurn(vh, "UMEMBER")

	resp, err := vh.call(t, &protocol.EgressRequest{
		Secret: "vendor", Method: "POST", Path: "/v1/orders?limit=5", Body: []byte(`{"a":1}`),
		Headers:  map[string][]string{"Authorization": {"Bearer agent-supplied"}, "Content-Type": {"application/json"}, "Cookie": {"x=1"}, "Accept-Encoding": {"gzip"}},
		ThreadID: turn.ThreadID, TurnID: turn.TurnID,
	}, turn)
	if err != nil {
		t.Fatal(err)
	}
	r := <-vh.seen
	if got := r.Header.Get("Authorization"); got != "Bearer sk_test_SECRET" {
		t.Fatalf("upstream saw Authorization %q", got)
	}
	if r.Header.Get("Cookie") != "" || r.Header.Get("Accept-Encoding") == "identity" || r.URL.Path != "/v1/orders" || r.URL.Query().Get("limit") != "5" || r.Host != "api.vendor.test" {
		t.Fatalf("upstream request %s %s host=%s cookie=%q", r.Method, r.URL, r.Host, r.Header.Get("Cookie"))
	}
	if resp.Status != 201 || strings.Contains(string(resp.Body), "sk_test_SECRET") ||
		!strings.Contains(string(resp.Body), "[secret:vendor]") || !strings.Contains(string(resp.Body), `"a":1`) {
		t.Fatalf("response %d %s", resp.Status, resp.Body)
	}
	if resp.Headers["Set-Cookie"] != nil || strings.Contains(resp.Headers["X-Echo"][0], "SECRET") {
		t.Fatalf("response headers %v", resp.Headers)
	}
	if s, _ := vh.st.VaultSecret("vendor"); s.LastUsedAt == nil {
		t.Fatal("use not recorded")
	}
}

func TestEgressQueryParameter(t *testing.T) {
	vh := newVaultHarness(t)
	addSecret(t, vh, memberTurn(), "maps", url.Values{"users": {"any"}, "query": {"api_key"}, "header": {""}})
	turn := liveTurn(vh, "UMEMBER")
	resp, err := vh.call(t, &protocol.EgressRequest{Secret: "maps", Path: "/geo?q=x", ThreadID: turn.ThreadID, TurnID: turn.TurnID}, turn)
	if err != nil {
		t.Fatal(err)
	}
	r := <-vh.seen
	if r.URL.Query().Get("api_key") != "sk_test_SECRET" || r.Header.Get("Authorization") != "" {
		t.Fatalf("upstream %s auth=%q", r.URL, r.Header.Get("Authorization"))
	}
	if strings.Contains(string(resp.Body), "sk_test_SECRET") {
		t.Fatalf("value leaked back: %s", resp.Body)
	}
}

func TestEgressPolicy(t *testing.T) {
	vh := newVaultHarness(t)
	addSecret(t, vh, memberTurn(), "shared", url.Values{"users": {"any"}})
	addSecret(t, vh, memberTurn(), "mine", url.Values{"users": {"owner"}})

	refused := func(fr *protocol.EgressRequest, turn *turnContext, want string) {
		t.Helper()
		_, err := vh.call(t, fr, turn)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: got %v, want %q", fr.Secret, err, want)
		}
	}

	// Nothing works without someone using the runner.
	refused(&protocol.EgressRequest{Secret: "shared"}, nil, "while a turn is running")

	member := liveTurn(vh, "UMEMBER")
	other := liveTurn(vh, "UOTHER")
	ok := func(fr *protocol.EgressRequest, turn *turnContext) {
		t.Helper()
		if _, err := vh.call(t, fr, turn); err != nil {
			t.Fatalf("%s: %v", fr.Secret, err)
		}
		<-vh.seen
	}
	ok(&protocol.EgressRequest{Secret: "shared"}, nil)

	refused(&protocol.EgressRequest{Secret: "nope"}, member, "no secret called")
	refused(&protocol.EgressRequest{Secret: "shared", Host: "evil.test"}, member, "may only be sent to")
	vh.gw.cfg.Load().Runners["alpha"].Egress = false
	refused(&protocol.EgressRequest{Secret: "shared"}, member, "egress is off")
	vh.gw.cfg.Load().Runners["alpha"].Egress = true

	// Owner-only needs a hardened runner, an attributed request, and the owner.
	attributed := func(turn *turnContext) *protocol.EgressRequest {
		return &protocol.EgressRequest{Secret: "mine", ThreadID: turn.ThreadID, TurnID: turn.TurnID, Attributed: true}
	}
	refused(attributed(member), member, "is not hardened")
	vh.gw.cfg.Load().Runners["alpha"].Hardened = true
	refused(attributed(member), member, "is not hardened") // the runner itself has not said so
	vh.conn.hello.Hardened = true
	refused(&protocol.EgressRequest{Secret: "mine", ThreadID: member.ThreadID, TurnID: member.TurnID}, member, "could not be traced")
	refused(attributed(other), other, "only be used in turns started by its owner")
	wrongThread := attributed(member)
	wrongThread.ThreadID = "test:C9:T9"
	refused(wrongThread, member, "could not be traced")
	ok(attributed(member), member)

	// A revoked secret is gone, and only its owner can revoke it.
	if _, err := vh.tool(t, other, revokeSecretTool, `{"name":"mine"}`); err == nil {
		t.Fatal("someone else revoked it")
	}
	if _, err := vh.tool(t, member, revokeSecretTool, `{"name":"mine"}`); err != nil {
		t.Fatal(err)
	}
	refused(attributed(member), member, "no secret called")

	// The audit trail has uses and refusals, never the value.
	var n int
	rows, _ := vh.st.EventsByKind("secret.", 1000)
	for _, e := range rows {
		if strings.Contains(e.Detail, "sk_test_SECRET") {
			t.Fatalf("value in audit: %+v", e)
		}
		if strings.HasPrefix(e.Kind, "secret.") {
			n++
		}
	}
	if n < 10 {
		t.Fatalf("only %d secret events", n)
	}
}

func TestEgressRefusesDeniedHostsEvenIfAllowed(t *testing.T) {
	cfg := &config.Config{}
	for _, h := range []string{"169.254.169.254", "metadata.google.internal", "metadata.google.internal:80"} {
		if !egressHostDenied(cfg, h) {
			t.Errorf("%s not denied", h)
		}
	}
	cfg.Gateway.Egress.DenyHosts = []string{"internal.corp.test"}
	if !egressHostDenied(cfg, "internal.corp.test") || egressHostDenied(cfg, "api.vendor.test") {
		t.Fatal("configured deny list")
	}
	if err := refuseLocalAddrs("tcp", "127.0.0.1:443", nil); err == nil {
		t.Fatal("loopback dial allowed")
	}
	if err := refuseLocalAddrs("tcp", "[fe80::1]:443", nil); err == nil {
		t.Fatal("link-local dial allowed")
	}
	if err := refuseLocalAddrs("tcp", "93.184.216.34:443", nil); err != nil {
		t.Fatal(err)
	}
}

func TestListSecretsExplainsUse(t *testing.T) {
	vh := newVaultHarness(t)
	addSecret(t, vh, memberTurn(), "vendor", url.Values{"users": {"owner"}})
	out, err := vh.tool(t, memberTurn(), listSecretsTool, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"$SPLITSCREEN_EGRESS/s/vendor/", "https://api.vendor.test/", "not hardened"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "sk_test") {
		t.Fatal("value listed")
	}
}

// An API that gzips its answer must still have an echoed key redacted: the
// agent's Accept-Encoding is dropped, so the client decompresses first.
func TestEgressRedactsCompressedResponses(t *testing.T) {
	vh := newVaultHarness(t)
	gz := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			_, _ = w.Write([]byte("plain:" + r.Header.Get("Authorization")))
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		_, _ = zw.Write([]byte("gz:" + r.Header.Get("Authorization")))
		_ = zw.Close()
	}))
	defer gz.Close()
	client, addr := gz.Client(), strings.TrimPrefix(gz.URL, "https://")
	egressDo = func(req *http.Request) (*http.Response, error) {
		r2 := req.Clone(req.Context())
		r2.URL.Host = addr
		return client.Do(r2)
	}
	addSecret(t, vh, memberTurn(), "vendor", url.Values{"users": {"any"}})
	turn := liveTurn(vh, "UMEMBER")
	resp, err := vh.call(t, &protocol.EgressRequest{Secret: "vendor", ThreadID: turn.ThreadID, TurnID: turn.TurnID,
		Headers: map[string][]string{"Accept-Encoding": {"gzip"}}}, turn)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != "gz:Bearer [secret:vendor]" {
		t.Fatalf("body %q", resp.Body)
	}
}
