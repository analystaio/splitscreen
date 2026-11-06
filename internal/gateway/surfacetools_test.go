package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/analystaio/splitscreen/config"
	"github.com/analystaio/splitscreen/internal/mcpproxy"
	"github.com/analystaio/splitscreen/internal/surface"
	"github.com/analystaio/splitscreen/protocol"
)

// channelSurface is a surface that can verify people and create channels.
type channelSurface struct {
	*verifyingSurface
	mu       sync.Mutex
	requests []surface.ChannelRequest
	err      error
	failures map[string]string
}

func (c *channelSurface) CreateChannel(_ context.Context, req surface.ChannelRequest) (surface.CreatedChannel, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, req)
	if c.err != nil {
		return surface.CreatedChannel{}, c.err
	}
	return surface.CreatedChannel{ID: "CNEW", Name: req.Name, InviteFailures: c.failures}, nil
}

func (c *channelSurface) created() []surface.ChannelRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]surface.ChannelRequest(nil), c.requests...)
}

func channelHarness(t *testing.T) (*harness, *channelSurface) {
	t.Helper()
	h, v := wildcardHarness(t, wildcard)
	c := &channelSurface{verifyingSurface: v}
	h.gw.surfaces["test"] = c
	cfg := h.gw.cfg.Load()
	cfg.MCP = map[string]*config.MCPServer{"chat": {Kind: config.MCPSurface}}
	cfg.Bundles = map[string]*config.Bundle{"b": {MCP: []string{"chat"}}}
	cfg.Runners["alpha"].Bundle = "b"
	return h, c
}

func memberTurn() *turnContext {
	return &turnContext{
		TurnID: "turn1", Surface: "test", Channel: "C1", Thread: "T1",
		Runner: "alpha", User: surface.User{ID: "UMEMBER"},
	}
}

func create(h *harness, turn *turnContext, args string) (json.RawMessage, error) {
	return h.gw.callSurfaceTool(context.Background(), "alpha", turn, &protocol.MCPCall{
		Server: "chat", Tool: createChannelTool, Args: json.RawMessage(args),
	})
}

// resultText unwraps an MCP tool result the way a client would.
func resultText(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Content) != 1 || r.Content[0].Type != "text" {
		t.Fatalf("result = %s", raw)
	}
	return r.Content[0].Text
}

func TestCreateChannelInvitesTheRequester(t *testing.T) {
	h, c := channelHarness(t)
	out, err := create(h, memberTurn(), `{"name":"launch-plan"}`)
	if err != nil {
		t.Fatal(err)
	}
	got := c.created()
	if len(got) != 1 || got[0].Name != "launch-plan" || got[0].Private {
		t.Fatalf("requests = %+v", got)
	}
	if len(got[0].Invite) != 1 || got[0].Invite[0] != "UMEMBER" {
		t.Fatalf("invite = %v, want the requester", got[0].Invite)
	}
	if txt := resultText(t, out); !strings.Contains(txt, "<#CNEW>") {
		t.Fatalf("result = %s", txt)
	}
}

func TestCreateChannelPrivateWithExtraInvites(t *testing.T) {
	h, c := channelHarness(t)
	if _, err := create(h, memberTurn(), `{"name":"x","private":true,"invite":["U222","UMEMBER"]}`); err != nil {
		t.Fatal(err)
	}
	got := c.created()[0]
	if !got.Private {
		t.Fatal("not private")
	}
	if strings.Join(got.Invite, ",") != "UMEMBER,U222" {
		t.Fatalf("invite = %v, want the requester first and no duplicates", got.Invite)
	}
}

func TestCreateChannelRefusesAGuest(t *testing.T) {
	h, c := channelHarness(t)
	turn := memberTurn()
	turn.User.ID = "UGUEST"
	if _, err := create(h, turn, `{"name":"x"}`); err == nil {
		t.Fatal("a guest created a channel")
	}
	if n := len(c.created()); n != 0 {
		t.Fatalf("%d channels created", n)
	}
}

// A surface that can create but cannot verify must not create.
type unverifiedCreator struct {
	*fakeSurface
	called bool
}

func (u *unverifiedCreator) CreateChannel(context.Context, surface.ChannelRequest) (surface.CreatedChannel, error) {
	u.called = true
	return surface.CreatedChannel{}, nil
}

func TestCreateChannelFailsClosedWithoutAVerifier(t *testing.T) {
	h, _ := channelHarness(t)
	u := &unverifiedCreator{fakeSurface: h.srf}
	h.gw.surfaces["test"] = u
	if _, err := create(h, memberTurn(), `{"name":"x"}`); err == nil || u.called {
		t.Fatalf("err = %v, called = %v", err, u.called)
	}
}

func TestCreateChannelNeedsATurn(t *testing.T) {
	h, c := channelHarness(t)
	if _, err := create(h, nil, `{"name":"x"}`); err == nil || len(c.created()) != 0 {
		t.Fatalf("err = %v", err)
	}
}

func TestCreateChannelRefusesAnotherRunnersTurn(t *testing.T) {
	h, c := channelHarness(t)
	turn := memberTurn()
	turn.Runner = "other"
	if _, err := create(h, turn, `{"name":"x"}`); err == nil || len(c.created()) != 0 {
		t.Fatalf("err = %v", err)
	}
}

func TestCreateChannelValidatesInput(t *testing.T) {
	h, c := channelHarness(t)
	many := `["U1A"` + strings.Repeat(`,"U1A"`, maxChannelInvites) + `]`
	for _, args := range []string{
		`{"name":""}`, `{"name":"#general"}`, `{"name":"Has Caps"}`, `{"name":"a b"}`,
		`{"name":"-dash"}`, `{"name":"` + strings.Repeat("a", 81) + `"}`,
		`{"name":"ok","invite":["not-an-id"]}`, `{"name":"ok","invite":` + many + `}`,
		`{"name":"ok","topic":"unknown field"}`, `not json`, ``,
	} {
		if _, err := create(h, memberTurn(), args); err == nil {
			t.Errorf("accepted %s", args)
		}
	}
	if n := len(c.created()); n != 0 {
		t.Fatalf("%d channels created from bad input", n)
	}
}

// The channel exists, so this must read as success with a warning: an error
// would send the agent back to retry and hit name_taken.
func TestInviteFailureIsNotACreateFailure(t *testing.T) {
	h, c := channelHarness(t)
	c.failures = map[string]string{"UMEMBER": "cant_invite"}
	out, err := create(h, memberTurn(), `{"name":"x"}`)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	txt := resultText(t, out)
	for _, want := range []string{"cant_invite", "do not create it again", "the person who asked"} {
		if !strings.Contains(txt, want) {
			t.Errorf("result %q is missing %q", txt, want)
		}
	}
}

func TestCreateErrorPropagates(t *testing.T) {
	h, c := channelHarness(t)
	c.err = errors.New("name_taken")
	if _, err := create(h, memberTurn(), `{"name":"x"}`); err == nil || !strings.Contains(err.Error(), "name_taken") {
		t.Fatalf("err = %v", err)
	}
}

func TestServerNotInBundleIsRefused(t *testing.T) {
	h, c := channelHarness(t)
	cfg := h.gw.cfg.Load()
	cfg.Bundles[cfg.Runners["alpha"].Bundle].MCP = nil
	if _, err := create(h, memberTurn(), `{"name":"x"}`); err == nil || len(c.created()) != 0 {
		t.Fatalf("err = %v, created %d", err, len(c.created()))
	}
}

func TestToolListParses(t *testing.T) {
	h, _ := channelHarness(t)
	out, err := h.gw.callSurfaceTool(context.Background(), "alpha", memberTurn(), &protocol.MCPCall{
		Server: "chat", Tool: mcpproxy.ListTool,
	})
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 1 || listed.Tools[0].Name != createChannelTool || len(listed.Tools[0].InputSchema) == 0 {
		t.Fatalf("tools = %s", out)
	}
}

func TestUnknownToolIsRefused(t *testing.T) {
	h, _ := channelHarness(t)
	if _, err := h.gw.callSurfaceTool(context.Background(), "alpha", memberTurn(), &protocol.MCPCall{
		Server: "chat", Tool: "delete_channel",
	}); err == nil {
		t.Fatal("an unknown tool ran")
	}
}

// The path Clank really uses: a runner's mcp.call over the websocket, ending in
// a response and an audit row naming the person who asked.
func TestCreateChannelOverTheRunnerConnection(t *testing.T) {
	h, c := channelHarness(t)
	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)
	h.gw.turns.Store("turn1", memberTurn())

	send(t, ws, &protocol.MCPCall{
		TurnID: "turn1", CallID: "call1", Server: "chat", Tool: createChannelTool,
		Args: json.RawMessage(`{"name":"over-the-wire"}`),
	})
	resp := readFrame[*protocol.MCPResponse](t, ws)
	if resp.Error != nil {
		t.Fatalf("error = %+v", resp.Error)
	}
	if got := c.created(); len(got) != 1 || got[0].Name != "over-the-wire" {
		t.Fatalf("requests = %+v", got)
	}
	rec, err := h.st.MCPCall("call1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.SurfaceUser != "UMEMBER" || rec.Tool != createChannelTool || !rec.OK {
		t.Fatalf("audit row: %+v", rec)
	}
}

// A runner must get this as a proxied server: that is what makes it reach the
// gateway through the shim, and what lets runners use it with no upgrade.
func TestSurfaceServerReachesTheRunnerAsProxied(t *testing.T) {
	h, _ := channelHarness(t)
	push, err := h.gw.buildBundle("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(push.MCP) != 1 || push.MCP[0].Name != "chat" || push.MCP[0].Kind != protocol.MCPProxied {
		t.Fatalf("mcp = %+v", push.MCP)
	}
}
