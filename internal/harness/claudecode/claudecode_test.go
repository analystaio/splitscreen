package claudecode

import (
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/analystaio/splitscreen/internal/harness"
)

// newParser wires a session's stdout parser to a scripted stream, with a
// trivially-exiting process standing in for the CLI.
func newParser(t *testing.T, lines string) *session {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &session{cmd: cmd, events: make(chan harness.Event, 64)}
	s.running.Store(true)
	go s.readStdout(io.NopCloser(strings.NewReader(lines)))
	return s
}

func collect(t *testing.T, s *session) []harness.Event {
	t.Helper()
	var out []harness.Event
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-s.events:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-timeout:
			t.Fatal("timed out collecting events")
		}
	}
}

func TestParsesTextAndToolUse(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"sess-abc"}`,
		`{"type":"assistant","session_id":"sess-abc","message":{"model":"claude-opus-5","content":[{"type":"text","text":"Looking now."}]}}`,
		`{"type":"assistant","session_id":"sess-abc","message":{"content":[{"type":"tool_use","id":"call1","name":"Bash","input":{"command":"ls -la"}}]}}`,
		`{"type":"result","session_id":"sess-abc","usage":{"input_tokens":120,"output_tokens":45,"cache_creation_input_tokens":30,"cache_read_input_tokens":9000},"total_cost_usd":0.0123}`,
	}, "\n")

	s := newParser(t, stream)
	events := collect(t, s)

	var sawSession, sawText, sawTool, sawUsage, sawDone bool
	for _, ev := range events {
		switch ev.Kind {
		case harness.EventSession:
			sawSession = ev.SessionID == "sess-abc"
		case harness.EventText:
			sawText = ev.Text == "Looking now."
		case harness.EventToolUse:
			sawTool = ev.Tool == "Bash" && ev.CallID == "call1" && ev.Summary == "ls -la"
		case harness.EventUsage:
			sawUsage = true
			u := ev.Usage
			if !u.Known {
				t.Error("usage should be marked known when counters are present")
			}
			// The four counters must survive as four counters.
			if u.InputTokens != 120 || u.OutputTokens != 45 ||
				u.CacheWriteTokens != 30 || u.CacheReadTokens != 9000 {
				t.Errorf("counters = %+v", u)
			}
			if u.Model != "claude-opus-5" {
				t.Errorf("model = %q; it should carry over from the assistant message", u.Model)
			}
			if u.ProviderCostUSD == nil || *u.ProviderCostUSD != 0.0123 {
				t.Errorf("provider cost = %v; kept only as a cross-check", u.ProviderCostUSD)
			}
		case harness.EventDone:
			sawDone = ev.SessionID == "sess-abc" && ev.ToolCalls == 1
		}
	}
	if !sawSession || !sawText || !sawTool || !sawUsage || !sawDone {
		t.Fatalf("missing events (session=%v text=%v tool=%v usage=%v done=%v): %+v",
			sawSession, sawText, sawTool, sawUsage, sawDone, events)
	}
}

// Thinking blocks are reported as their own event kind, bounded, with
// redacted thinking (which has no readable text) skipped entirely.
func TestParsesThinking(t *testing.T) {
	long := strings.Repeat("x", 1500)
	stream := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"The disk is the suspect."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"` + long + `"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"redacted_thinking","data":"opaque"}]}}`,
		`{"type":"result","session_id":"s1"}`,
	}, "\n")

	var thoughts []string
	for _, ev := range collect(t, newParser(t, stream)) {
		if ev.Kind == harness.EventThinking {
			thoughts = append(thoughts, ev.Text)
		}
	}
	if len(thoughts) != 2 {
		t.Fatalf("expected 2 thinking events, got %d: %v", len(thoughts), thoughts)
	}
	if thoughts[0] != "The disk is the suspect." {
		t.Errorf("thinking text = %q", thoughts[0])
	}
	if len(thoughts[1]) > 1010 || !strings.HasSuffix(thoughts[1], "…") {
		t.Errorf("long thinking was not bounded: %d bytes", len(thoughts[1]))
	}
}

// A result with no usage block must report usage as unknown, not as zeros.
func TestMissingUsageIsUnknown(t *testing.T) {
	s := newParser(t, `{"type":"result","session_id":"s1"}`)
	for _, ev := range collect(t, s) {
		if ev.Kind == harness.EventUsage {
			if ev.Usage.Known {
				t.Fatal("usage was marked known without any counters")
			}
			if ev.Usage.InputTokens != 0 {
				t.Fatalf("counters = %+v", ev.Usage)
			}
			return
		}
	}
	t.Fatal("no usage event was emitted")
}

func TestErrorResult(t *testing.T) {
	s := newParser(t,
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"tool failed"}`)
	for _, ev := range collect(t, s) {
		if ev.Kind == harness.EventError {
			if !strings.Contains(ev.Error, "tool failed") {
				t.Fatalf("error = %q", ev.Error)
			}
			return
		}
	}
	t.Fatal("no error event was emitted")
}

func TestNonJSONNoiseIsIgnored(t *testing.T) {
	s := newParser(t, strings.Join([]string{
		`some warning printed to stdout`,
		``,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"still here"}]}}`,
	}, "\n"))
	for _, ev := range collect(t, s) {
		if ev.Kind == harness.EventText && ev.Text == "still here" {
			return
		}
	}
	t.Fatal("noise on stdout stopped parsing")
}

func TestAdapterIsRegistered(t *testing.T) {
	a, err := harness.Get("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if a.PermissionToolName() == "" {
		t.Error("the adapter must name a permission tool; without it the harness would decide for itself")
	}
	if a.DefaultCredentialEnv() == "" {
		t.Error("the adapter must name a credential variable")
	}
}

// The model has to reach the CLI as a flag: nothing else sets it. The env is a
// strict allowlist that drops ANTHROPIC_MODEL, and no settings.json is
// materialized, so an unset model silently follows the CLI's own default.
func TestModelIsPassedAsAFlag(t *testing.T) {
	args := buildArgs(harness.SessionConfig{Cwd: "/w", Model: "claude-opus-5"})

	idx := -1
	for i, a := range args {
		if a == "--model" {
			idx = i
			break
		}
	}
	if idx == -1 {
		t.Fatalf("--model absent from args: %v", args)
	}
	if idx+1 >= len(args) || args[idx+1] != "claude-opus-5" {
		t.Fatalf("--model not followed by the model id: %v", args)
	}
}

// Empty means "keep the harness default" — passing an empty --model would be a
// CLI error rather than a no-op.
func TestNoModelMeansNoFlag(t *testing.T) {
	for _, a := range buildArgs(harness.SessionConfig{Cwd: "/w"}) {
		if a == "--model" {
			t.Fatal("--model passed when no model was configured")
		}
	}
}

// A resumed session must still carry the model; an idle kill mid-conversation
// is the most likely moment for it to get dropped.
func TestResumeStillCarriesTheModel(t *testing.T) {
	args := buildArgs(harness.SessionConfig{Cwd: "/w", Model: "claude-opus-5", ResumeID: "sess-1"})
	var sawModel, sawResume bool
	for _, a := range args {
		switch a {
		case "--model":
			sawModel = true
		case "--resume":
			sawResume = true
		}
	}
	if !sawModel || !sawResume {
		t.Fatalf("model=%v resume=%v, want both: %v", sawModel, sawResume, args)
	}
}

// A tool call that starts and never ends renders as a hung tool on any surface
// that gives a tool call a lifecycle — a spinner that never resolves, or the
// warning Slack draws on an unfinished task card. The CLI reports the result as
// a user message, which the adapter must read.
func TestToolResultsCloseTheCall(t *testing.T) {
	s := newParser(t, strings.Join([]string{
		`{"type":"assistant","message":{"model":"claude-opus-5","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"df -h /"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"Filesystem  Size","is_error":false}]}}`,
	}, "\n"))

	var start, end *harness.Event
	for ev := range s.Events() {
		switch ev.Kind {
		case harness.EventToolUse:
			e := ev
			start = &e
		case harness.EventToolEnd:
			e := ev
			end = &e
		}
	}
	if start == nil {
		t.Fatal("no tool_use event")
	}
	if end == nil {
		t.Fatal("the tool call never ended: no tool_end event")
	}
	if end.CallID != start.CallID {
		t.Fatalf("result landed on the wrong call: %q vs %q", end.CallID, start.CallID)
	}
	if !end.OK {
		t.Fatal("a successful tool reported as failed")
	}
}

// A failing tool has to carry its reason, and the reason has to survive both
// result shapes: a bare string, and the block array image-capable tools return.
func TestFailedToolResultCarriesItsReason(t *testing.T) {
	s := newParser(t, strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_2","name":"Read","input":{"file_path":"/etc/shadow"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":[{"type":"text","text":"EACCES: permission denied"}],"is_error":true}]}}`,
	}, "\n"))

	for ev := range s.Events() {
		if ev.Kind != harness.EventToolEnd {
			continue
		}
		if ev.OK {
			t.Fatal("a failed tool reported as OK")
		}
		if !strings.Contains(ev.Error, "permission denied") {
			t.Fatalf("the reason was lost: %q", ev.Error)
		}
		return
	}
	t.Fatal("no tool_end event for the failed call")
}

func TestBuildArgsLoadsPluginDirs(t *testing.T) {
	args := buildArgs(harness.SessionConfig{Cwd: "/w", PluginDirs: []string{"/m/plugins/a", "/m/plugins/b"}})
	got := strings.Join(args, " ")
	if !strings.Contains(got, "--plugin-dir /m/plugins/a --plugin-dir /m/plugins/b") {
		t.Fatalf("args = %v", args)
	}
	for _, a := range buildArgs(harness.SessionConfig{Cwd: "/w"}) {
		if a == "--plugin-dir" {
			t.Fatal("--plugin-dir without plugins")
		}
	}
}

func TestBuildArgsStrictUnlessLoose(t *testing.T) {
	has := func(args []string, flag string) bool {
		for _, a := range args {
			if a == flag {
				return true
			}
		}
		return false
	}
	strict := buildArgs(harness.SessionConfig{Cwd: "/w", MCPConfigPath: "/m.json"})
	if !has(strict, "--strict-mcp-config") || !has(strict, "--mcp-config") {
		t.Fatalf("strict args = %v", strict)
	}
	loose := buildArgs(harness.SessionConfig{Cwd: "/w", MCPConfigPath: "/m.json", LooseMCP: true})
	if has(loose, "--strict-mcp-config") || !has(loose, "--mcp-config") {
		t.Fatalf("loose args = %v: the runner's own servers must still be passed", loose)
	}
}
