package protocol

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		from Direction
		in   Frame
	}{
		{"hello", DirUp, &Hello{
			Protocol: Version, Runner: "review",
			Auth:    Auth{Mode: AuthToken, Value: "enrol-secret"},
			Host:    Host{ID: "i-0abc", OS: "linux", Arch: "amd64"},
			Harness: HarnessInfo{Adapter: "claude-code", Version: "2.1.4"},
		}},
		{"hello_ack", DirDown, &HelloAck{
			Protocol: Version, Runner: "review",
			Bundle: BundleRef{Version: 14, Digest: "sha256:abc"},
			Routes: []string{"C0123456789"},
		}},
		{"message", DirDown, &Message{
			ThreadID: "t1", TurnID: "turn1", Channel: "C1",
			User: UserRef{ID: "U1", Display: "alice"}, Text: "hi",
		}},
		{"text.delta", DirUp, &TextDelta{ThreadID: "t1", TurnID: "turn1", Text: "wor"}},
		{"thought", DirUp, &Thought{ThreadID: "t1", TurnID: "turn1", Text: "the disk is the suspect"}},
		{"tool.start", DirUp, &ToolStart{TurnID: "turn1", CallID: "c1", Tool: "Bash"}},
		{"tool.end", DirUp, &ToolEnd{TurnID: "turn1", CallID: "c1", OK: true, DurationMS: 12}},
		{"permission.request", DirUp, &PermissionRequest{
			TurnID: "turn1", RequestID: "r1", Tool: "Bash",
			Input: json.RawMessage(`{"command":"ls"}`),
		}},
		{"permission.response", DirDown, &PermissionResponse{
			RequestID: "r1", Decision: DecisionAllow, DecidedBy: &UserRef{ID: "U1"},
		}},
		{"mcp.call", DirUp, &MCPCall{CallID: "m1", Server: "jira", Tool: "get_issue"}},
		{"mcp.response", DirDown, &MCPResponse{CallID: "m1", Result: json.RawMessage(`{"ok":true}`)}},
		{"credential.request", DirUp, &CredentialRequest{
			RequestID: "cr1", Kind: CredentialForge, Resource: "acme/widgets",
		}},
		{"credential.grant", DirDown, &CredentialGrant{
			RequestID: "cr1", Kind: CredentialForge, Username: "x-access-token", Value: "ghs_xxx",
		}},
		{"usage", DirUp, &Usage{
			ThreadID: "t1", TurnID: "turn1", Model: "claude-opus-5",
			InputTokens: 100, CacheWriteTokens: 20, CacheReadTokens: 9000, OutputTokens: 400,
			Known: true,
		}},
		{"done", DirUp, &Done{TurnID: "turn1", SessionID: "s1"}},
		{"error", DirUp, &Error{Code: "harness_auth", Message: "token expired"}},
		{"blob.begin", DirDown, &BlobBegin{BlobID: "b1", Name: "report.csv", Size: 1024}},
		{"blob.end", DirDown, &BlobEnd{BlobID: "b1", OK: true}},
		{"ping", DirUp, &Ping{Nonce: 7}},
		{"pong", DirDown, &Pong{Nonce: 7}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := Encode(tc.in)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			var probe struct {
				T FrameType `json:"t"`
			}
			if err := json.Unmarshal(raw, &probe); err != nil {
				t.Fatalf("discriminator: %v", err)
			}
			if probe.T != tc.in.Type() {
				t.Fatalf("discriminator = %q, want %q", probe.T, tc.in.Type())
			}
			got, err := Decode(raw, tc.from)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got.Type() != tc.in.Type() {
				t.Fatalf("round-tripped to %q, want %q", got.Type(), tc.in.Type())
			}
			again, err := Encode(got)
			if err != nil {
				t.Fatalf("re-encode: %v", err)
			}
			if string(again) != string(raw) {
				t.Fatalf("re-encode differs:\n got %s\nwant %s", again, raw)
			}
		})
	}
}

func TestEveryKnownTypeIsRoundTripped(t *testing.T) {
	// Guards against adding a frame type and forgetting to cover it.
	covered := map[FrameType]bool{}
	for _, ft := range []FrameType{
		TypeHello, TypeHelloAck, TypeMessage, TypeTextDelta, TypeThought, TypeToolStart, TypeToolEnd,
		TypePermissionRequest, TypePermissionResponse, TypeMCPCall, TypeMCPResponse,
		TypeCredentialRequest, TypeCredentialGrant, TypeUsage, TypeDone, TypeError,
		TypeBlobBegin, TypeBlobEnd, TypePing, TypePong, TypeBundlePush,
	} {
		covered[ft] = true
	}
	for _, ft := range KnownTypes() {
		if !covered[ft] {
			t.Errorf("frame type %q is registered but not covered by tests", ft)
		}
	}
	if len(covered) != len(registry) {
		t.Errorf("test list has %d types, registry has %d", len(covered), len(registry))
	}
}

// A runner must not be able to approve its own tool call.
func TestDirectionIsEnforced(t *testing.T) {
	raw, err := Encode(&PermissionResponse{
		RequestID: "r1", Decision: DecisionAllow, DecidedBy: &UserRef{ID: "U1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(raw, DirUp); !errors.Is(err, ErrWrongDirection) {
		t.Fatalf("decoding a gateway frame from a runner: got %v, want ErrWrongDirection", err)
	}
	if _, err := Decode(raw, DirDown); err != nil {
		t.Fatalf("decoding from the gateway should succeed: %v", err)
	}
}

func TestDecodeRejects(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"missing discriminator", `{"turn":"t1"}`, "discriminator"},
		{"unknown type", `{"t":"nope"}`, "unknown frame type"},
		{"malformed json", `{`, "malformed"},
		{"bad decision", `{"t":"permission.response","request":"r1","decision":"maybe","decided_by":{"id":"U1"}}`, "unknown decision"},
		{"policy denial must deny", `{"t":"permission.response","request":"r1","decision":"allow","policy_denied":true}`, "policy_denied requires"},
		{"anonymous approval", `{"t":"permission.response","request":"r1","decision":"allow"}`, "decided_by is required"},
		{"negative tokens", `{"t":"usage","turn":"t1","model":"m","input":-1,"known":true}`, "non-negative"},
		{"unknown usage with counters", `{"t":"usage","turn":"t1","input":10,"known":false}`, "must be zero when known is false"},
		{"oversized blob", `{"t":"blob.begin","blob":"b1","name":"x","size":999999999}`, "exceeds cap"},
		{"blob path traversal", `{"t":"blob.begin","blob":"b1","name":"../../etc/passwd","size":1}`, "path separators"},
		{"mcp response with neither", `{"t":"mcp.response","call":"m1"}`, "exactly one of result or error"},
		{"mcp response with both", `{"t":"mcp.response","call":"m1","result":{},"error":{"code":"x","message":"y"}}`, "mutually exclusive"},
		{"bad runner slug", `{"t":"hello","protocol":"1.0","runner":"Dev3 React","auth":{"mode":"token","value":"v"},"harness":{"adapter":"claude-code"}}`, "not a valid slug"},
		{"unknown auth mode", `{"t":"hello","protocol":"1.0","runner":"dev3","auth":{"mode":"sudo"},"harness":{"adapter":"claude-code"}}`, "unknown auth mode"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := DirDown
			if strings.Contains(tc.raw, `"hello"`) || strings.Contains(tc.raw, `"usage"`) {
				dir = DirUp
			}
			_, err := Decode([]byte(tc.raw), dir)
			if err == nil {
				t.Fatalf("expected rejection, got none")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestBundlePathTraversalRejected(t *testing.T) {
	bad := []string{"../escape", "/etc/passwd", "a/../../b", "", `windows\path`, "./relative"}
	for _, p := range bad {
		b := &BundlePush{Version: 1, Digest: "sha256:x", Files: []BundleFile{{Path: p}}}
		if err := b.Validate(); err == nil {
			t.Errorf("path %q was accepted, want rejection", p)
		}
	}
	ok := &BundlePush{Version: 1, Digest: "sha256:x", Files: []BundleFile{
		{Path: "CLAUDE.md", Mode: 0o600},
		{Path: "skills/deploy-check/SKILL.md"},
	}}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid bundle rejected: %v", err)
	}
}

func TestBundleMCPKindRules(t *testing.T) {
	// A proxied server must not declare a command: its whole point is that the
	// runner never executes it and never holds its credential.
	b := &BundlePush{Version: 1, Digest: "d", MCP: []MCPServer{
		{Name: "jira", Kind: MCPProxied, Command: "/usr/bin/jira-mcp"},
	}}
	if err := b.Validate(); err == nil || !strings.Contains(err.Error(), "must not declare a command") {
		t.Fatalf("got %v, want a proxied-server command rejection", err)
	}

	b = &BundlePush{Version: 1, Digest: "d", MCP: []MCPServer{{Name: "fs", Kind: MCPLocal}}}
	if err := b.Validate(); err == nil || !strings.Contains(err.Error(), "requires a command") {
		t.Fatalf("got %v, want a local-server command requirement", err)
	}
}

func TestRedaction(t *testing.T) {
	h := &Hello{Auth: Auth{Mode: AuthToken, Value: "super-secret"}}
	if got := h.Redacted().Auth.Value; got == "super-secret" {
		t.Error("Hello.Redacted leaked the enrollment token")
	}
	if h.Auth.Value != "super-secret" {
		t.Error("Hello.Redacted mutated the original")
	}
	g := &CredentialGrant{RequestID: "r", Value: "ghs_secret"}
	if g.Redacted().Value == "ghs_secret" {
		t.Error("CredentialGrant.Redacted leaked the token")
	}
	b := &BundlePush{Secrets: map[string]string{"jira": "tok"}}
	if b.Redacted().Secrets["jira"] == "tok" {
		t.Error("BundlePush.Redacted leaked a secret")
	}
}

func TestEncodeRejectsInvalid(t *testing.T) {
	// Encoding validates too: a peer should never have to diagnose our bugs.
	if _, err := Encode(&Usage{TurnID: ""}); err == nil {
		t.Fatal("expected encode of an invalid frame to fail")
	}
}

func TestControlFrameSizeCap(t *testing.T) {
	huge := `{"t":"text.delta","turn":"t1","text":"` + strings.Repeat("a", MaxControlFrame) + `"}`
	if _, err := Decode([]byte(huge), DirUp); err == nil || !strings.Contains(err.Error(), "exceeds cap") {
		t.Fatalf("got %v, want a size-cap rejection", err)
	}
}

func TestChunkRoundTrip(t *testing.T) {
	payload := []byte("some file bytes")
	frame, err := EncodeChunk(ChunkHeader{BlobID: "b1", Seq: 3}, payload)
	if err != nil {
		t.Fatal(err)
	}
	h, got, err := DecodeChunk(frame)
	if err != nil {
		t.Fatal(err)
	}
	if h.BlobID != "b1" || h.Seq != 3 {
		t.Fatalf("header = %+v", h)
	}
	if string(got) != string(payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
}

func TestChunkRejects(t *testing.T) {
	if _, err := EncodeChunk(ChunkHeader{Seq: 1}, []byte("x")); err == nil {
		t.Error("chunk without a blob id was accepted")
	}
	if _, err := EncodeChunk(ChunkHeader{BlobID: "b"}, nil); err == nil {
		t.Error("empty chunk was accepted")
	}
	if _, err := EncodeChunk(ChunkHeader{BlobID: "b"}, make([]byte, MaxChunk+1)); err == nil {
		t.Error("oversized chunk was accepted")
	}
	for _, bad := range [][]byte{
		{},
		{0, 0},
		{0, 0, 0, 0},                // zero-length header
		{0, 0, 0, 200, 'x'},         // header longer than the frame
		{0xff, 0xff, 0xff, 0xff, 1}, // absurd header length
	} {
		if _, _, err := DecodeChunk(bad); err == nil {
			t.Errorf("malformed frame %v was accepted", bad)
		}
	}
}

func TestVersionNegotiation(t *testing.T) {
	ours := Ours()
	cases := []struct {
		peer string
		want Compatibility
	}{
		{Version, Compatible},
		{"1.7", CompatibleWithWarning},
		{"2.0", Incompatible},
		{"0.9", Incompatible},
	}
	for _, tc := range cases {
		v, got, err := NegotiateString(tc.peer)
		if err != nil {
			t.Fatalf("peer %q: %v", tc.peer, err)
		}
		if got != tc.want {
			t.Errorf("peer %q (ours %s): got %v, want %v", tc.peer, ours, got, tc.want)
		}
		if v.String() != tc.peer {
			t.Errorf("peer %q parsed to %q", tc.peer, v)
		}
	}
	if _, _, err := NegotiateString("banana"); err == nil {
		t.Error("malformed version was accepted")
	}
}

// A permission request without a known turn must still be sendable: the gateway
// denies what it cannot place, which is strictly better than a request the
// runner cannot transmit while a harness waits for an answer.
func TestPermissionRequestWithoutTurnIsValid(t *testing.T) {
	req := &PermissionRequest{RequestID: "r1", Tool: "Bash"}
	if err := req.Validate(); err != nil {
		t.Fatalf("rejected: %v", err)
	}
	raw, err := Encode(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := Decode(raw, DirUp); err != nil {
		t.Fatalf("decode: %v", err)
	}
}
