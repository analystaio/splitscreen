package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/analystaio/splitscreen/config"
	"github.com/analystaio/splitscreen/internal/secrets"
	"github.com/analystaio/splitscreen/internal/store"
	"github.com/analystaio/splitscreen/internal/surface"
	"github.com/analystaio/splitscreen/protocol"
)

// fakeSurface records what the gateway would have posted.
type fakeSurface struct {
	mu        sync.Mutex
	posts     []surface.Post
	updates   []surface.Post
	prompts   []surface.Prompt
	resolved  []string
	uploads   []string
	handler   surface.Handler
	channels  map[string]surface.ChannelInfo
	seq       int
	statuses  []surface.Status
	statusErr func(surface.Status) error
}

func (f *fakeSurface) SetStatus(_ context.Context, st surface.Status) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, st)
	if f.statusErr != nil {
		return f.statusErr(st)
	}
	return nil
}

func (f *fakeSurface) statusTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.statuses))
	for i, st := range f.statuses {
		out[i] = st.Channel + ":" + st.Text
	}
	return out
}

func (f *fakeSurface) Name() string { return "test" }

func (f *fakeSurface) Start(ctx context.Context, h surface.Handler) error {
	f.mu.Lock()
	f.handler = h
	f.mu.Unlock()
	<-ctx.Done()
	return nil
}

func (f *fakeSurface) Post(_ context.Context, p surface.Post) (surface.Ref, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, p)
	f.seq++
	return surface.Ref{Channel: p.Channel, Thread: p.Thread, ID: "m" + itoa(f.seq)}, nil
}

func (f *fakeSurface) Update(_ context.Context, _ surface.Ref, p surface.Post) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, p)
	return nil
}

func (f *fakeSurface) Prompt(_ context.Context, p surface.Prompt) (surface.Ref, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, p)
	f.seq++
	return surface.Ref{Channel: p.Channel, Thread: p.Thread, ID: "p" + itoa(f.seq)}, nil
}

func (f *fakeSurface) Resolve(_ context.Context, _ surface.Ref, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved = append(f.resolved, text)
	return nil
}

func (f *fakeSurface) Upload(_ context.Context, u surface.Upload) error {
	body, _ := io.ReadAll(u.Content)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = append(f.uploads, u.Name+":"+string(body))
	return nil
}

func (f *fakeSurface) Channel(_ context.Context, id string) (surface.ChannelInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if info, ok := f.channels[id]; ok {
		return info, nil
	}
	return surface.ChannelInfo{ID: id, Membership: surface.MembershipJoined, Name: "test-" + id}, nil
}

func (f *fakeSurface) setChannel(info surface.ChannelInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.channels == nil {
		f.channels = map[string]surface.ChannelInfo{}
	}
	f.channels[info.ID] = info
}

func (f *fakeSurface) Close() error { return nil }

func (f *fakeSurface) allText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, p := range f.posts {
		b.WriteString(p.Text)
		b.WriteString("\n")
	}
	for _, p := range f.updates {
		b.WriteString(p.Text)
		b.WriteString("\n")
	}
	return b.String()
}

func testLogger() *slog.Logger {
	if os.Getenv("SPLITSCREEN_TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

const testConfig = `
gateway:
  heartbeat: 1s
  stream_interval: 20ms
runners:
  alpha:
    display: { name: "Alpha" }
    cwd: /tmp
    harness: claude-code
    policy:
      deny: ["Bash(rm -rf /*)"]
      forge:
        repos: ["acme/widgets"]
routes:
  - { channel: C1, runner: alpha }
`

type harness struct {
	t   *testing.T
	gw  *Gateway
	srf *fakeSurface
	srv *httptest.Server
	st  *store.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	cfg, err := config.Parse([]byte(testConfig))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	t.Setenv("SPLITSCREEN_SECRET_RUNNER_ALPHA", "s3cret")

	srf := &fakeSurface{}
	gw, err := New(Options{
		Config:   cfg,
		Store:    st,
		Secrets:  secrets.NewEnvBackend(),
		Surfaces: map[string]surface.Surface{"test": srf},
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(gw.handleRunner))
	t.Cleanup(srv.Close)

	return &harness{t: t, gw: gw, srf: srf, srv: srv, st: st}
}

// connect performs the runner handshake and returns the socket.
func (h *harness) connect(t *testing.T, token string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { ws.Close(websocket.StatusNormalClosure, "") })

	hello, err := protocol.Encode(&protocol.Hello{
		Protocol: protocol.Version,
		Runner:   "alpha",
		Auth:     protocol.Auth{Mode: protocol.AuthToken, Value: token},
		Host:     protocol.Host{OS: "linux", Arch: "amd64"},
		Harness:  protocol.HarnessInfo{Adapter: "claude-code"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	return ws
}

// connectWithBundle performs the handshake claiming to already hold a bundle.
func (h *harness) connectWithBundle(t *testing.T, token string, b *protocol.BundleRef) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { ws.Close(websocket.StatusNormalClosure, "") })

	hello, err := protocol.Encode(&protocol.Hello{
		Protocol: protocol.Version,
		Runner:   "alpha",
		Auth:     protocol.Auth{Mode: protocol.AuthToken, Value: token},
		Host:     protocol.Host{OS: "linux", Arch: "amd64"},
		Harness:  protocol.HarnessInfo{Adapter: "claude-code"},
		Bundle:   b,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	return ws
}

// readFrame reads one frame of the expected type, skipping heartbeats.
func readFrame[T protocol.Frame](t *testing.T, ws *websocket.Conn) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var zero T
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		f, err := protocol.Decode(data, protocol.DirDown)
		if err != nil {
			continue
		}
		if got, ok := f.(T); ok {
			return got
		}
		_ = zero
	}
}

func send(t *testing.T, ws *websocket.Conn, f protocol.Frame) {
	t.Helper()
	data, err := protocol.Encode(f)
	if err != nil {
		t.Fatalf("encode %s: %v", f.Type(), err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write %s: %v", f.Type(), err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A wrong enrollment token must not get a connection.
func TestHandshakeRejectsBadToken(t *testing.T) {
	h := newHarness(t)
	ws := h.connect(t, "wrong")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := ws.Read(ctx); err == nil {
		t.Fatal("expected the connection to be closed after a bad token")
	}
}

func TestHandshakeRejectsUnknownRunner(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "")

	hello, _ := protocol.Encode(&protocol.Hello{
		Protocol: protocol.Version, Runner: "ghost",
		Auth:    protocol.Auth{Mode: protocol.AuthToken, Value: "s3cret"},
		Harness: protocol.HarnessInfo{Adapter: "claude-code"},
	})
	_ = ws.Write(ctx, websocket.MessageText, hello)
	if _, _, err := ws.Read(ctx); err == nil {
		t.Fatal("expected an unconfigured runner to be refused")
	}
}

// The full happy path: message in, streamed output back, turn accounted for.
func TestTurnRoundTrip(t *testing.T) {
	h := newHarness(t)
	ws := h.connect(t, "s3cret")

	ack := readFrame[*protocol.HelloAck](t, ws)
	if ack.Runner != "alpha" {
		t.Fatalf("ack for %q", ack.Runner)
	}
	if len(ack.Routes) != 1 || ack.Routes[0] != "C1" {
		t.Fatalf("routes = %v, want [C1]", ack.Routes)
	}

	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U1"}, Text: "hello", Addressed: true,
	})

	msg := readFrame[*protocol.Message](t, ws)
	if msg.Text != "hello" || msg.User.ID != "U1" {
		t.Fatalf("message = %+v", msg)
	}

	send(t, ws, &protocol.TextDelta{ThreadID: msg.ThreadID, TurnID: msg.TurnID, Text: "hi there"})
	send(t, ws, &protocol.Usage{
		ThreadID: msg.ThreadID, TurnID: msg.TurnID, Model: "claude-sonnet-5",
		InputTokens: 100, CacheReadTokens: 900, OutputTokens: 50, Known: true,
	})
	send(t, ws, &protocol.Done{ThreadID: msg.ThreadID, TurnID: msg.TurnID, SessionID: "sess-1"})

	eventually(t, "streamed output", func() bool {
		return strings.Contains(h.srf.allText(), "hi there")
	})

	// The session id must be persisted, or an idle-killed session could not
	// resume and the conversation would silently restart.
	eventually(t, "session persisted", func() bool {
		th, err := h.st.Thread(msg.ThreadID)
		return err == nil && th.SessionID == "sess-1"
	})

	rows, err := h.st.CostByRunner(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Key != "alpha" {
		t.Fatalf("cost rows = %+v", rows)
	}
	if rows[0].CacheRead != 900 {
		t.Errorf("cache reads = %d, want 900 kept separate from input", rows[0].CacheRead)
	}
	if rows[0].CostUSD <= 0 {
		t.Errorf("cost = %v, want a priced turn", rows[0].CostUSD)
	}
}

// A policy-denied tool must never reach a human: no prompt is posted, so no
// click can approve it.
func TestPolicyDenialSkipsThePrompt(t *testing.T) {
	h := newHarness(t)
	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)

	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U1"}, Text: "go", Addressed: true,
	})
	msg := readFrame[*protocol.Message](t, ws)

	send(t, ws, &protocol.PermissionRequest{
		ThreadID: msg.ThreadID, TurnID: msg.TurnID, RequestID: "req1",
		Tool: "Bash", Input: json.RawMessage(`{"command":"rm -rf /var"}`),
	})

	resp := readFrame[*protocol.PermissionResponse](t, ws)
	if resp.Decision != protocol.DecisionDeny {
		t.Fatalf("decision = %q, want deny", resp.Decision)
	}
	if !resp.PolicyDenied {
		t.Error("expected the denial to be marked as policy-driven")
	}

	h.srf.mu.Lock()
	prompts := len(h.srf.prompts)
	h.srf.mu.Unlock()
	if prompts != 0 {
		t.Fatalf("posted %d prompts for a policy-denied tool; it must never be offered", prompts)
	}
}

// An allowed tool is prompted, and the decision carries the human who made it.
func TestPermissionPromptAndDecision(t *testing.T) {
	h := newHarness(t)
	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)

	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U1"}, Text: "go", Addressed: true,
	})
	msg := readFrame[*protocol.Message](t, ws)

	send(t, ws, &protocol.PermissionRequest{
		ThreadID: msg.ThreadID, TurnID: msg.TurnID, RequestID: "req2",
		Tool: "Bash", Input: json.RawMessage(`{"command":"ls"}`),
	})

	eventually(t, "prompt posted", func() bool {
		h.srf.mu.Lock()
		defer h.srf.mu.Unlock()
		return len(h.srf.prompts) == 1
	})

	h.gw.OnDecision(context.Background(), surface.Decision{
		RequestID: "req2", Decision: protocol.DecisionAllow,
		User: surface.User{ID: "U9", Display: "alice"},
	})

	resp := readFrame[*protocol.PermissionResponse](t, ws)
	if resp.Decision != protocol.DecisionAllow {
		t.Fatalf("decision = %q", resp.Decision)
	}
	if resp.DecidedBy == nil || resp.DecidedBy.ID != "U9" {
		t.Fatalf("decided_by = %+v, want the approving user", resp.DecidedBy)
	}

	// A second click on a resolved prompt must not send a second response.
	h.gw.OnDecision(context.Background(), surface.Decision{
		RequestID: "req2", Decision: protocol.DecisionDeny,
		User: surface.User{ID: "U9"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			break // expected: nothing more arrives
		}
		if typ != websocket.MessageText {
			continue
		}
		if f, derr := protocol.Decode(data, protocol.DirDown); derr == nil {
			if _, dup := f.(*protocol.PermissionResponse); dup {
				t.Fatal("a resolved prompt was answered twice")
			}
		}
	}
}

// A credential request outside the runner's forge policy is refused with a
// reason, not answered with an empty credential.
func TestForgePolicyRefusal(t *testing.T) {
	h := newHarness(t)
	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)

	send(t, ws, &protocol.CredentialRequest{
		RequestID: "c1", Kind: protocol.CredentialForge, Resource: "evil/repo",
	})

	grant := readFrame[*protocol.CredentialGrant](t, ws)
	if !grant.Denied {
		t.Fatal("expected a repository outside policy to be refused")
	}
	if grant.Value != "" {
		t.Fatal("a denied grant must not carry a credential")
	}
	if !strings.Contains(grant.Reason, "evil/repo") {
		t.Errorf("reason %q should name the repository", grant.Reason)
	}
}

// Messages accepted while a runner is offline are persisted and replayed, rather
// than silently dropped.
func TestOfflineQueueingAndDrain(t *testing.T) {
	h := newHarness(t)

	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U1"}, Text: "while you were out", Addressed: true,
	})

	depth, err := h.st.QueueDepth("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if depth != 1 {
		t.Fatalf("queue depth = %d, want 1", depth)
	}
	if !strings.Contains(h.srf.allText(), "offline") {
		t.Errorf("the thread should say the runner is offline, got: %q", h.srf.allText())
	}

	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)

	msg := readFrame[*protocol.Message](t, ws)
	if msg.Text != "while you were out" {
		t.Fatalf("drained message = %q", msg.Text)
	}
	eventually(t, "queue drained", func() bool {
		d, _ := h.st.QueueDepth("alpha")
		return d == 0
	})
}

// Threads are sticky: an existing binding outlives a routing change, because the
// session lives on that runner's disk.
func TestThreadBindingIsSticky(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.gw.OnMessage(ctx, surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U1"}, Text: "first", Addressed: true,
	})

	key := threadKey("test", "C1", "T1")
	th, err := h.st.Thread(key)
	if err != nil {
		t.Fatal(err)
	}
	if th.Runner != "alpha" {
		t.Fatalf("bound to %q", th.Runner)
	}

	// Repoint the channel; the existing thread must not follow.
	cfg := h.gw.Config()
	cfg.Routes[0].Runner = "alpha"
	h.gw.applyConfig(cfg)

	h.gw.OnMessage(ctx, surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U1"}, Text: "second",
	})
	th2, _ := h.st.Thread(key)
	if th2.Runner != th.Runner {
		t.Fatalf("thread moved from %q to %q", th.Runner, th2.Runner)
	}
}

// An unrouted channel is ignored entirely — this is what replaces the
// per-runner channel allowlists of the predecessor.
func TestUnroutedChannelIsIgnored(t *testing.T) {
	h := newHarness(t)
	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C-UNKNOWN", Thread: "T9",
		User: surface.User{ID: "U1"}, Text: "anyone there?", Addressed: true,
	})
	if got := h.srf.allText(); got != "" {
		t.Fatalf("an unrouted channel produced output: %q", got)
	}
	if _, err := h.st.Thread(threadKey("test", "C-UNKNOWN", "T9")); err == nil {
		t.Fatal("an unrouted channel created a thread binding")
	}
}

// Routing a channel says which runner may answer there, not that everything
// said there is for it. Without this, a runner cannot share a channel with the
// humans who use it — it answers people talking to each other, including people
// talking ABOUT it.
func TestUnaddressedMessageDoesNotStartAThread(t *testing.T) {
	h := newHarness(t)
	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T-CHAT",
		User: surface.User{ID: "U1"}, Text: "did anyone look at the migration yet",
	})
	if got := h.srf.allText(); got != "" {
		t.Fatalf("an unaddressed message produced output: %q", got)
	}
	if _, err := h.st.Thread(threadKey("test", "C1", "T-CHAT")); err == nil {
		t.Fatal("an unaddressed message created a thread binding")
	}
}

// The other half: once a thread is underway, replies are the reply. Requiring a
// mention per turn would make every conversation read like dictation.
func TestBoundThreadContinuesWithoutBeingAddressed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.gw.OnMessage(ctx, surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T-CONV",
		User: surface.User{ID: "U1"}, Text: "hi", Addressed: true,
	})
	key := threadKey("test", "C1", "T-CONV")
	if _, err := h.st.Thread(key); err != nil {
		t.Fatalf("addressed message did not bind a thread: %v", err)
	}
	h.gw.OnMessage(ctx, surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T-CONV",
		User: surface.User{ID: "U1"}, Text: "and the other thing",
	})
	th, err := h.st.Thread(key)
	if err != nil {
		t.Fatalf("follow-up dropped the binding: %v", err)
	}
	if th.Runner != "alpha" {
		t.Fatalf("runner = %q, want alpha", th.Runner)
	}
}

// "!status" is addressing: nobody types it at a colleague. Commands have to work
// without a mention or the gate makes the bot harder to operate than to use.
func TestBareCommandIsTreatedAsAddressing(t *testing.T) {
	h := newHarness(t)
	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T-CMD",
		User: surface.User{ID: "U1"}, Text: "!status",
	})
	if got := h.srf.allText(); got == "" {
		t.Fatal("a bare !status in a routed channel produced no output")
	}
}

// A runner must not be able to originate a gateway frame. Direction enforcement
// lives in the decoder, so this is defence at the boundary rather than in a
// handler somebody might forget.
func TestRunnerCannotApproveItsOwnToolCall(t *testing.T) {
	h := newHarness(t)
	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)

	forged, err := protocol.Encode(&protocol.PermissionResponse{
		RequestID: "req3", Decision: protocol.DecisionAllow,
		DecidedBy: &protocol.UserRef{ID: "U1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageText, forged); err != nil {
		t.Fatal(err)
	}

	errFrame := readFrame[*protocol.Error](t, ws)
	if errFrame.Code != "bad_frame" {
		t.Fatalf("code = %q, want bad_frame", errFrame.Code)
	}
	if !strings.Contains(errFrame.Message, "not permitted") {
		t.Errorf("message %q should explain the direction violation", errFrame.Message)
	}
}

// A second connection for the same runner displaces the first: after an
// ungraceful restart the old one is a zombie, and refusing would lock the runner
// out permanently.
func TestSecondConnectionDisplacesTheFirst(t *testing.T) {
	h := newHarness(t)
	first := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, first)

	second := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, second)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		if _, _, err := first.Read(ctx); err != nil {
			break // the first connection was closed, as intended
		}
	}
	if _, ok := h.gw.hub.Get("alpha"); !ok {
		t.Fatal("the runner should still be registered via the newer connection")
	}
}

// Unknown usage must be recorded as unknown, never as zero cost.
func TestUnknownUsageIsNotZero(t *testing.T) {
	h := newHarness(t)
	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)

	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U1"}, Text: "go", Addressed: true,
	})
	msg := readFrame[*protocol.Message](t, ws)

	send(t, ws, &protocol.Usage{ThreadID: msg.ThreadID, TurnID: msg.TurnID, Known: false})
	send(t, ws, &protocol.Done{ThreadID: msg.ThreadID, TurnID: msg.TurnID})

	eventually(t, "usage recorded", func() bool {
		rows, err := h.st.CostByRunner(time.Now().Add(-time.Hour))
		return err == nil && len(rows) == 1 && rows[0].UnknownTurns == 1
	})

	text := h.gw.CostText(time.Hour)
	if !strings.Contains(text, "reported no usage") {
		t.Errorf("cost report should call out unmetered turns, got:\n%s", text)
	}
}

func TestWebStatusPage(t *testing.T) {
	h := newHarness(t)
	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)

	// A turn with unmetered usage, so the page has to render the case where a
	// number is genuinely unknown.
	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U1"}, Text: "go", Addressed: true,
	})
	msg := readFrame[*protocol.Message](t, ws)
	send(t, ws, &protocol.Usage{ThreadID: msg.ThreadID, TurnID: msg.TurnID, Known: false})
	send(t, ws, &protocol.Done{ThreadID: msg.ThreadID, TurnID: msg.TurnID})
	eventually(t, "turn recorded", func() bool {
		rows, err := h.st.CostByRunner(time.Now().Add(-time.Hour))
		return err == nil && len(rows) == 1
	})

	rec := httptest.NewRecorder()
	h.gw.handleIndex(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"alpha", "connected", "Cache read", "Unmetered"} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	// The page must say that unmetered work is unmeasured, not free.
	if !strings.Contains(body, "not free, only unmeasured") {
		t.Error("the page should explain what an unmetered turn means")
	}

	rec = httptest.NewRecorder()
	h.gw.handleIndex(rec, httptest.NewRequest("GET", "/nope", nil))
	if rec.Code != 404 {
		t.Errorf("unknown path returned %d", rec.Code)
	}
}

// A routed channel the bot never joined is the worst failure mode: the platform
// simply never delivers, so it is indistinguishable from having no route. The
// check exists to turn that silence into a statement.
func TestUnjoinedChannelIsReported(t *testing.T) {
	h := newHarness(t)
	h.srf.setChannel(surface.ChannelInfo{
		ID: "C1", Name: "react-migration",
		Membership: surface.MembershipNotJoined,
		Detail:     "invite the bot into the channel",
	})

	h.gw.RefreshChannels(context.Background())

	bad := h.gw.UnreachableChannels()
	if len(bad) != 1 {
		t.Fatalf("unreachable = %d, want 1", len(bad))
	}
	if bad[0].Runner != "alpha" {
		t.Errorf("runner = %q", bad[0].Runner)
	}

	status := h.gw.StatusText()
	if !strings.Contains(status, "Routed but not joined") {
		t.Errorf("status does not flag the channel:\n%s", status)
	}
	if !strings.Contains(status, "react-migration") {
		t.Errorf("status should name the channel:\n%s", status)
	}
	if !strings.Contains(status, "never reach the gateway") {
		t.Error("status should say what the consequence is, not just that something is off")
	}

	if !strings.Contains(h.gw.RoutesText(), "not joined") {
		t.Errorf("routes should mark the channel:\n%s", h.gw.RoutesText())
	}
}

// A surface that cannot answer must not be reported as broken. Painting a
// healthy deployment red is how a check gets ignored.
func TestUnverifiableChannelIsNotAProblem(t *testing.T) {
	h := newHarness(t)
	h.srf.setChannel(surface.ChannelInfo{
		ID: "C1", Membership: surface.MembershipUnknown,
		Detail: "needs the channels:read scope",
	})

	h.gw.RefreshChannels(context.Background())

	if bad := h.gw.UnreachableChannels(); len(bad) != 0 {
		t.Fatalf("an unverifiable channel was reported as unreachable: %+v", bad)
	}
	status := h.gw.StatusText()
	if strings.Contains(status, "Routed but not joined") {
		t.Errorf("status flagged an unverifiable channel:\n%s", status)
	}
	if !strings.Contains(h.gw.RoutesText(), "unverified") {
		t.Errorf("routes should mark it unverified rather than silently fine:\n%s", h.gw.RoutesText())
	}
}

func TestJoinedChannelRendersItsName(t *testing.T) {
	h := newHarness(t)
	h.srf.setChannel(surface.ChannelInfo{
		ID: "C1", Name: "agents", Membership: surface.MembershipJoined,
	})
	h.gw.RefreshChannels(context.Background())

	routes := h.gw.RoutesText()
	if !strings.Contains(routes, "#agents") {
		t.Errorf("routes should show the channel name:\n%s", routes)
	}
	if strings.Contains(routes, "not joined") || strings.Contains(routes, "unverified") {
		t.Errorf("a joined channel was flagged:\n%s", routes)
	}
}

// A runner's config lives on tmpfs. After a reboot it has nothing, but the
// gateway's cache still says it pushed — so the push decision must rest on what
// the runner reports, not on what the gateway remembers. Getting this wrong
// leaves a runner connected, healthy, and completely unconfigured.
func TestBundleIsRepushedWhenTheRunnerLostIt(t *testing.T) {
	h := newHarness(t)

	first := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, first)
	push := readFrame[*protocol.BundlePush](t, first)
	if push.Version != 1 {
		t.Fatalf("first push version = %d", push.Version)
	}
	digest := push.Digest

	// Reconnect claiming to still hold it: the gateway should not resend.
	second := h.connectWithBundle(t, "s3cret", &protocol.BundleRef{Version: 1, Digest: digest})
	readFrame[*protocol.HelloAck](t, second)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	for {
		typ, data, err := second.Read(ctx)
		if err != nil {
			break // nothing more arrived, as intended
		}
		if typ != websocket.MessageText {
			continue
		}
		if f, derr := protocol.Decode(data, protocol.DirDown); derr == nil {
			if _, resent := f.(*protocol.BundlePush); resent {
				t.Fatal("the gateway resent a bundle the runner already had")
			}
		}
	}

	// Now reconnect having lost it — a reboot the gateway never saw.
	third := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, third)
	again := readFrame[*protocol.BundlePush](t, third)
	if again.Digest != digest {
		t.Fatalf("digest = %q, want the same content", again.Digest)
	}
	// The version deliberately does NOT advance: see
	// TestRepushKeepsTheVersionWhenContentIsUnchanged. What matters here is only
	// that the bundle was sent again at all.
}

// Losing a bundle is not a configuration change. Re-sending it must not bump the
// version, or every runner restart would mark its live sessions stale and tell
// people to restart conversations for a change that never happened.
func TestRepushKeepsTheVersionWhenContentIsUnchanged(t *testing.T) {
	h := newHarness(t)

	first := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, first)
	original := readFrame[*protocol.BundlePush](t, first)

	// Reconnect having lost it, as after a reboot.
	second := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, second)
	again := readFrame[*protocol.BundlePush](t, second)

	if again.Digest != original.Digest {
		t.Fatalf("content changed unexpectedly: %q -> %q", original.Digest, again.Digest)
	}
	if again.Version != original.Version {
		t.Fatalf("version advanced %d -> %d for identical content", original.Version, again.Version)
	}
}

// Tool lines exist to show a long turn is alive. Once the answer arrives they
// are noise to anyone who does not know what a tool call is, so the finished
// message must not carry them.
func TestActivityLinesAreStrippedWhenTheTurnEnds(t *testing.T) {
	h := newHarness(t)
	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)

	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U1"}, Text: "where is your config?", Addressed: true,
	})
	msg := readFrame[*protocol.Message](t, ws)

	send(t, ws, &protocol.ToolStart{
		ThreadID: msg.ThreadID, TurnID: msg.TurnID,
		CallID: "c1", Tool: "Bash", Summary: `echo "$CLAUDE_CONFIG_DIR"`,
	})
	send(t, ws, &protocol.TextDelta{
		ThreadID: msg.ThreadID, TurnID: msg.TurnID,
		Text: "It is /run/user/1000/splitscreen/review/config.",
	})

	// While the turn runs, the activity is visible.
	eventually(t, "activity shown mid-turn", func() bool {
		return strings.Contains(h.srf.allText(), "Bash")
	})

	send(t, ws, &protocol.Done{ThreadID: msg.ThreadID, TurnID: msg.TurnID})

	eventually(t, "final message rendered", func() bool {
		h.srf.mu.Lock()
		defer h.srf.mu.Unlock()
		for _, p := range h.srf.updates {
			if strings.Contains(p.Text, "/run/user/1000") && !strings.Contains(p.Text, "Bash") {
				return true
			}
		}
		return false
	})

	// The last thing written must carry the answer and not the tool line.
	h.srf.mu.Lock()
	defer h.srf.mu.Unlock()
	last := h.srf.updates[len(h.srf.updates)-1].Text
	if strings.Contains(last, "Bash") || strings.Contains(last, "echo") {
		t.Fatalf("the finished message still shows tool calls:\n%s", last)
	}
	if !strings.Contains(last, "/run/user/1000") {
		t.Fatalf("the answer was lost:\n%s", last)
	}
}
