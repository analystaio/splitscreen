package runner

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/analystaio/splitscreen/internal/harness"
	"github.com/analystaio/splitscreen/protocol"
)

func TestParseEgressPath(t *testing.T) {
	cases := []struct {
		path, query       string
		secret, host, out string
		ok                bool
	}{
		{"/s/vendor/v1/orders", "limit=5", "vendor", "", "/v1/orders?limit=5", true},
		{"/s/vendor@api.b.test/x", "", "vendor", "api.b.test", "/x", true},
		{"/s/vendor", "", "vendor", "", "/", true},
		{"/s/vendor/", "", "vendor", "", "/", true},
		{"/s/", "", "", "", "", false},
		{"/v1/x", "", "", "", "", false},
	}
	for _, c := range cases {
		s, h, p, ok := parseEgressPath(c.path, c.query)
		if ok != c.ok || s != c.secret || h != c.host || (ok && p != c.out) {
			t.Errorf("%s?%s: got %q %q %q %v", c.path, c.query, s, h, p, ok)
		}
	}
}

func egressRunner(t *testing.T) (*Runner, *fakeGateway, func(f protocol.Frame)) {
	t.Helper()
	g := newFakeGateway(t)
	r, ws := connectedRunner(t, g)
	r.gatewayMinor.Store(protocol.MinorEgress)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r.startEgress(ctx)
	if r.egressEnv() == "" {
		t.Fatal("egress proxy did not start")
	}
	return r, g, func(f protocol.Frame) { g.reply(ws, f) }
}

func TestEgressRoundTrip(t *testing.T) {
	r, g, reply := egressRunner(t)

	type result struct {
		status int
		hdr    http.Header
		body   string
	}
	out := make(chan result, 1)
	go func() {
		res, err := http.Post(r.egressEnv()+"/s/vendor@api.b.test/v1/orders?limit=5", "application/json", strings.NewReader(`{"a":1}`))
		if err != nil {
			t.Errorf("post: %v", err)
			out <- result{}
			return
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		out <- result{res.StatusCode, res.Header, string(b)}
	}()

	req, ok := await[*protocol.EgressRequest](g)
	if !ok {
		t.Fatal("no egress request")
	}
	if req.Secret != "vendor" || req.Host != "api.b.test" || req.Method != "POST" ||
		req.Path != "/v1/orders?limit=5" || string(req.Body) != `{"a":1}` {
		t.Fatalf("request %+v", req)
	}
	// Nobody's harness made this call, so it claims no thread.
	if req.Attributed || req.ThreadID != "" {
		t.Fatalf("unattributed call claimed %q", req.ThreadID)
	}
	reply(&protocol.EgressResponse{RequestID: req.RequestID, Status: 201,
		Headers: map[string][]string{"X-From": {"upstream"}}, Body: []byte(`{"ok":true}`)})
	res := <-out
	if res.status != 201 || res.hdr.Get("X-From") != "upstream" || res.body != `{"ok":true}` {
		t.Fatalf("got %+v", res)
	}

	// A refusal comes back as an error the agent can read.
	go func() {
		res, err := http.Get(r.egressEnv() + "/s/mine/x")
		if err != nil {
			out <- result{}
			return
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		out <- result{res.StatusCode, res.Header, string(b)}
	}()
	req, _ = await[*protocol.EgressRequest](g)
	reply(&protocol.EgressResponse{RequestID: req.RequestID, Error: &protocol.RemoteError{Code: "owner_only", Message: "limited to its owner"}})
	res = <-out
	if res.status != 403 || res.hdr.Get("X-Splitscreen-Egress-Error") != "owner_only" || !strings.Contains(res.body, "limited to its owner") {
		t.Fatalf("refusal: %+v", res)
	}

	// Misuse is explained without a gateway round trip.
	resp, err := http.Get(r.egressEnv() + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("bad path: %d", resp.StatusCode)
	}
}

func TestEgressNotOfferedToOldGateway(t *testing.T) {
	r, _, _ := egressRunner(t)
	r.gatewayMinor.Store(protocol.MinorEgress - 1)
	if r.egressEnv() != "" {
		t.Fatal("offered egress to a gateway that cannot answer it")
	}
	for _, kv := range r.buildEnv(t.TempDir(), "t1") {
		if strings.HasPrefix(kv, "SPLITSCREEN_EGRESS=") {
			t.Fatal("SPLITSCREEN_EGRESS set")
		}
	}
}

// procSession is a harness session backed by a real process, for attribution.
type procSession struct{ cmd *exec.Cmd }

func (p *procSession) Send(context.Context, harness.Input) error { return nil }
func (p *procSession) Events() <-chan harness.Event              { return nil }
func (p *procSession) SessionID() string                         { return "" }
func (p *procSession) Close() error                              { return p.cmd.Process.Kill() }
func (p *procSession) Running() bool                             { return p.cmd.ProcessState == nil }
func (p *procSession) Process() (int, uint64) {
	return p.cmd.Process.Pid, harness.ProcessStart(p.cmd.Process.Pid)
}

// connectFrom starts a shell that opens a TCP connection to addr from a
// grandchild process, and returns the shell and the server's end.
func connectFrom(t *testing.T, ln net.Listener) (*exec.Cmd, net.Conn) {
	t.Helper()
	port := ln.Addr().(*net.TCPAddr).Port
	// sh -> bash: the socket is held by a descendant, not the "harness" itself.
	cmd := exec.Command("sh", "-c", fmt.Sprintf(`bash -c 'exec 3<>/dev/tcp/127.0.0.1/%d; read -t 10 x <&3' ; true`, port))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
	c, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return cmd, c
}

func TestAttributionFollowsTheHarnessTree(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("attribution reads /proc")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("needs bash")
	}
	r, err := New(Options{Name: "alpha", Gateway: "ws://x", RuntimeRoot: t.TempDir(), Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	inside, insideConn := connectFrom(t, ln)
	outside, outsideConn := connectFrom(t, ln)
	_ = outside

	r.sessions.Store("thread-a", &threadSession{threadID: "thread-a", sess: &procSession{cmd: inside}})

	if thread, ok := r.attributeConn(insideConn); !ok || thread != "thread-a" {
		t.Fatalf("inside: %q %v", thread, ok)
	}
	if thread, ok := r.attributeConn(outsideConn); ok {
		t.Fatalf("a process outside every harness was attributed to %q", thread)
	}

	// A harness whose PID was recycled is not trusted.
	r.sessions.Store("thread-a", &threadSession{threadID: "thread-a", sess: &fakeStartSession{procSession{inside}}})
	if _, ok := r.attributeConn(insideConn); ok {
		t.Fatal("attributed through a harness with the wrong start time")
	}
}

type fakeStartSession struct{ procSession }

func (f *fakeStartSession) Process() (int, uint64) {
	pid, start := f.procSession.Process()
	return pid, start + 1
}

func TestWritableBy(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip()
	}
	dir := t.TempDir()
	if why := writableBy(dir, getuid()); why == "" {
		t.Fatal("a temp dir we own was reported safe")
	}
	if why := writableBy("/usr/bin/env", getuid()); why != "" && getuid() != 0 {
		t.Fatalf("/usr/bin/env: %s", why)
	}
}

func getuid() int { return os.Getuid() }
