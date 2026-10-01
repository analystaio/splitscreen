package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/analystaio/splitscreen/internal/harness"
	_ "github.com/analystaio/splitscreen/internal/harness/claudecode"
	"github.com/analystaio/splitscreen/protocol"
)

// fakeGateway is the other end of a runner connection: it reads frames and lets
// a test answer them.
type fakeGateway struct {
	t      *testing.T
	srv    *httptest.Server
	frames chan protocol.Frame
	conn   chan *websocket.Conn
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	g := &fakeGateway{
		t:      t,
		frames: make(chan protocol.Frame, 64),
		conn:   make(chan *websocket.Conn, 1),
	}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		ws.SetReadLimit(protocol.MaxControlFrame + protocol.MaxChunk + 4096)
		g.conn <- ws
		for {
			typ, data, err := ws.Read(r.Context())
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			if f, derr := protocol.Decode(data, protocol.DirUp); derr == nil {
				select {
				case g.frames <- f:
				default:
				}
			}
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGateway) url() string { return "ws" + strings.TrimPrefix(g.srv.URL, "http") }

func (g *fakeGateway) reply(ws *websocket.Conn, f protocol.Frame) {
	g.t.Helper()
	data, err := protocol.Encode(f)
	if err != nil {
		g.t.Fatalf("encode %s: %v", f.Type(), err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
		g.t.Fatalf("reply: %v", err)
	}
}

// await returns the next frame of the requested type.
func await[T protocol.Frame](g *fakeGateway) (T, bool) {
	var zero T
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f := <-g.frames:
			if got, ok := f.(T); ok {
				return got, true
			}
		case <-deadline:
			return zero, false
		}
	}
}

// connectedRunner returns a runner already talking to the fake gateway.
func connectedRunner(t *testing.T, g *fakeGateway) (*Runner, *websocket.Conn) {
	t.Helper()

	r, err := New(Options{
		Name:        "alpha",
		Gateway:     g.url(),
		Token:       "tok",
		Cwd:         t.TempDir(),
		RuntimeRoot: t.TempDir(),
		SelfPath:    "/usr/bin/splitscreen",
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = r.connectOnce(ctx) }()

	var ws *websocket.Conn
	select {
	case ws = <-g.conn:
	case <-time.After(5 * time.Second):
		t.Fatal("the runner never connected")
	}

	// The runner announces itself before anything else.
	hello, ok := await[*protocol.Hello](g)
	if !ok {
		t.Fatal("no hello frame")
	}
	if hello.Runner != "alpha" || hello.Auth.Value != "tok" {
		t.Fatalf("hello = %+v", hello.Redacted())
	}
	return r, ws
}

func TestPermissionRequestRoundTrip(t *testing.T) {
	g := newFakeGateway(t)
	r, ws := connectedRunner(t, g)

	result := make(chan PermissionResult, 1)
	go func() {
		res, err := r.requestPermission(context.Background(), "thread-1", "Bash",
			json.RawMessage(`{"command":"ls"}`))
		if err != nil {
			t.Errorf("requestPermission: %v", err)
		}
		result <- res
	}()

	req, ok := await[*protocol.PermissionRequest](g)
	if !ok {
		t.Fatal("no permission request reached the gateway")
	}
	if req.Tool != "Bash" || req.ThreadID != "thread-1" {
		t.Fatalf("request = %+v", req)
	}

	g.reply(ws, &protocol.PermissionResponse{
		RequestID: req.RequestID, Decision: protocol.DecisionAllow,
		DecidedBy: &protocol.UserRef{ID: "U1"},
	})

	select {
	case res := <-result:
		if res.Behavior != "allow" {
			t.Fatalf("behavior = %q", res.Behavior)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the decision never came back")
	}
}

// A denial must carry its reason: an opaque refusal is much harder to act on
// than "policy refused this".
func TestPermissionDenialCarriesReason(t *testing.T) {
	g := newFakeGateway(t)
	r, ws := connectedRunner(t, g)

	result := make(chan PermissionResult, 1)
	go func() {
		res, _ := r.requestPermission(context.Background(), "thread-1", "Bash", nil)
		result <- res
	}()

	req, ok := await[*protocol.PermissionRequest](g)
	if !ok {
		t.Fatal("no permission request reached the gateway")
	}
	g.reply(ws, &protocol.PermissionResponse{
		RequestID: req.RequestID, Decision: protocol.DecisionDeny,
		PolicyDenied: true, Reason: "denied by gateway policy: Bash(rm*)",
	})

	res := <-result
	if res.Behavior != "deny" {
		t.Fatalf("behavior = %q", res.Behavior)
	}
	if !strings.Contains(res.Message, "gateway policy") {
		t.Fatalf("message = %q", res.Message)
	}
}

// If the control plane goes away mid-decision, the answer must be deny. Failing
// open here would let a dropped connection approve a tool call.
func TestPermissionFailsClosedOnDisconnect(t *testing.T) {
	g := newFakeGateway(t)
	r, ws := connectedRunner(t, g)

	result := make(chan PermissionResult, 1)
	go func() {
		res, _ := r.requestPermission(context.Background(), "thread-1", "Bash", nil)
		result <- res
	}()
	if _, ok := await[*protocol.PermissionRequest](g); !ok {
		t.Fatal("no request")
	}

	_ = ws.Close(websocket.StatusGoingAway, "gateway restarting")
	r.failPending("gateway connection lost")

	select {
	case res := <-result:
		if res.Behavior != "deny" {
			t.Fatalf("behavior = %q, want deny — a lost connection is not an approval", res.Behavior)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter was never released")
	}
}

func TestCredentialRefusalSurfacesTheReason(t *testing.T) {
	g := newFakeGateway(t)
	r, ws := connectedRunner(t, g)

	errCh := make(chan error, 1)
	go func() {
		_, err := r.requestCredential(context.Background(), "evil/repo")
		errCh <- err
	}()

	req, ok := await[*protocol.CredentialRequest](g)
	if !ok {
		t.Fatal("no credential request")
	}
	if req.Resource != "evil/repo" {
		t.Fatalf("resource = %q", req.Resource)
	}

	g.reply(ws, &protocol.CredentialGrant{
		RequestID: req.RequestID, Kind: protocol.CredentialForge,
		Denied: true, Reason: `repository "evil/repo" is outside this runner's forge policy`,
	})

	err := <-errCh
	if err == nil {
		t.Fatal("a refusal was reported as success")
	}
	if !strings.Contains(err.Error(), "evil/repo") {
		t.Fatalf("error = %v", err)
	}
}

func TestProxiedMCPRoundTrip(t *testing.T) {
	g := newFakeGateway(t)
	r, ws := connectedRunner(t, g)

	out := make(chan json.RawMessage, 1)
	go func() {
		res, err := r.callProxiedMCP(context.Background(), "thread-1", "jira", "get_issue",
			json.RawMessage(`{"key":"PROJ-1"}`))
		if err != nil {
			t.Errorf("call: %v", err)
		}
		out <- res
	}()

	call, ok := await[*protocol.MCPCall](g)
	if !ok {
		t.Fatal("no mcp call")
	}
	if call.Server != "jira" || call.Tool != "get_issue" {
		t.Fatalf("call = %+v", call)
	}
	g.reply(ws, &protocol.MCPResponse{
		CallID: call.CallID, Result: json.RawMessage(`{"summary":"a bug"}`),
	})

	select {
	case res := <-out:
		if !strings.Contains(string(res), "a bug") {
			t.Fatalf("result = %s", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no result")
	}
}

// Attachments arrive as a chunked transfer and land in the thread's spool.
func TestInboundBlobAssembly(t *testing.T) {
	g := newFakeGateway(t)
	r, ws := connectedRunner(t, g)

	payload := []byte("id,name\n1,widget\n")
	g.reply(ws, &protocol.BlobBegin{
		BlobID: "b1", ThreadID: "thread-1", Name: "data.csv",
		Mime: "text/csv", Size: int64(len(payload)),
	})

	chunk, err := protocol.EncodeChunk(protocol.ChunkHeader{BlobID: "b1"}, payload)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageBinary, chunk); err != nil {
		t.Fatal(err)
	}

	g.reply(ws, &protocol.BlobEnd{BlobID: "b1", OK: true})

	// takeBlob only succeeds once the transfer is complete, so polling it is
	// also the assertion that an interrupted transfer is never handed over.
	var path string
	var data []byte
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if d, p, ok := r.takeBlob("b1"); ok {
			data, path = d, p
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if path == "" {
		t.Fatal("the attachment never materialized")
	}
	if string(data) != string(payload) {
		t.Fatalf("inlined content = %q", data)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(payload) {
		t.Fatalf("content = %q", body)
	}
	// Attachments land inside the thread's own spool directory.
	if !strings.Contains(path, filepath.Join("threads")) {
		t.Errorf("path = %q, expected it under the thread spool", path)
	}
}

// A checksum mismatch must discard the file rather than hand the harness
// something that is not what the sender had.
func TestInboundBlobChecksumMismatchDiscards(t *testing.T) {
	g := newFakeGateway(t)
	r, ws := connectedRunner(t, g)

	g.reply(ws, &protocol.BlobBegin{
		BlobID: "b2", ThreadID: "thread-1", Name: "x.bin", Size: 4,
	})
	chunk, _ := protocol.EncodeChunk(protocol.ChunkHeader{BlobID: "b2"}, []byte("data"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ws.Write(ctx, websocket.MessageBinary, chunk)

	g.reply(ws, &protocol.BlobEnd{BlobID: "b2", OK: true, SHA256: strings.Repeat("0", 64)})

	time.Sleep(300 * time.Millisecond)
	if _, _, ok := r.takeBlob("b2"); ok {
		t.Fatal("a corrupt attachment was accepted")
	}
}

func TestCleanupRemovesAbandonedSpools(t *testing.T) {
	r, err := New(Options{
		Name: "alpha", Gateway: "ws://x", Token: "t",
		Cwd: t.TempDir(), RuntimeRoot: t.TempDir(),
		SelfPath: "/usr/bin/splitscreen",
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	// One live thread, one abandoned.
	r.sessions.Store("live", &threadSession{threadID: "live"})
	for _, id := range []string{"live", "gone"} {
		dir := filepath.Join(r.opts.RuntimeRoot, "alpha", "threads", threadDirName(id))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	r.CleanupThreads()

	live := filepath.Join(r.opts.RuntimeRoot, "alpha", "threads", threadDirName("live"))
	gone := filepath.Join(r.opts.RuntimeRoot, "alpha", "threads", threadDirName("gone"))
	if _, err := os.Stat(live); err != nil {
		t.Error("a live thread's spool was removed")
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Error("an abandoned spool survived; these grew forever in the predecessor")
	}
}

func TestIPCRoundTrip(t *testing.T) {
	r, err := New(Options{
		Name: "alpha", Gateway: "ws://x", Token: "t",
		Cwd: t.TempDir(), RuntimeRoot: t.TempDir(),
		SelfPath: "/usr/bin/splitscreen",
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.startIPC(); err != nil {
		t.Fatal(err)
	}
	defer r.ipc.close()

	socket := SocketPath(r.opts.RuntimeRoot, "alpha")
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	// Only the runner's own user may speak to it.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %o, want 600", perm)
	}

	resp, err := CallIPC(socket, IPCRequest{Op: OpPing}, 5*time.Second)
	if err != nil || !resp.OK {
		t.Fatalf("ping: %v %+v", err, resp)
	}

	resp, _ = CallIPC(socket, IPCRequest{Op: "nonsense"}, 5*time.Second)
	if resp.OK || !strings.Contains(resp.Error, "unknown op") {
		t.Errorf("unknown op = %+v", resp)
	}

	// With no gateway connection, a credential request must fail rather than
	// return an empty credential.
	resp, _ = CallIPC(socket, IPCRequest{Op: OpCredential, Resource: "acme/app"}, 5*time.Second)
	if resp.OK {
		t.Error("a credential was issued with no gateway connection")
	}
}

func TestAdapterSelection(t *testing.T) {
	if _, err := New(Options{
		Name: "alpha", Gateway: "ws://x", Token: "t",
		Cwd: t.TempDir(), RuntimeRoot: t.TempDir(),
		SelfPath: "/x", HarnessName: "nonexistent",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}); err == nil {
		t.Fatal("an unknown harness was accepted")
	}
	if _, err := harness.Get("claude-code"); err != nil {
		t.Fatalf("the default adapter is not registered: %v", err)
	}
}

// A transfer that never finishes must not be handed to the harness as a
// truncated file. Silent truncation is worse than a missing attachment.
func TestIncompleteBlobIsNotHandedOver(t *testing.T) {
	g := newFakeGateway(t)
	r, ws := connectedRunner(t, g)

	g.reply(ws, &protocol.BlobBegin{
		BlobID: "b3", ThreadID: "thread-1", Name: "partial.bin", Size: 1000,
	})
	chunk, _ := protocol.EncodeChunk(protocol.ChunkHeader{BlobID: "b3"}, []byte("only the start"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ws.Write(ctx, websocket.MessageBinary, chunk)

	// No blob.end: the gateway went away mid-transfer.
	time.Sleep(300 * time.Millisecond)
	if _, _, ok := r.takeBlob("b3"); ok {
		t.Fatal("an unfinished transfer was handed over")
	}
}
