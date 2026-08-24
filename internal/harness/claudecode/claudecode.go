// Package claudecode drives the Claude Code CLI in headless streaming mode.
package claudecode

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/avarant/splitscreen/internal/harness"
)

func init() { harness.Register(&Adapter{Binary: "claude"}) }

// Adapter starts Claude Code sessions.
type Adapter struct{ Binary string }

func (a *Adapter) Name() string { return "claude-code" }

// DefaultCredentialEnv is the API-key variable. A subscription deployment
// overrides this with the OAuth token variable instead.
func (a *Adapter) DefaultCredentialEnv() string { return "ANTHROPIC_API_KEY" }

// buildArgs is separate from Start so the command line can be asserted without
// executing anything: the flags below are the whole contract with the CLI.
func buildArgs(cfg harness.SessionConfig) []string {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
	}
	// Without this the model is whatever the CLI on the runner defaults to,
	// which moves when that default moves. Empty stays unset rather than
	// guessing an id, so the harness keeps its own default deliberately.
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.MCPConfigPath != "" {
		// Strict mode means the runner fully determines the tool surface and
		// nothing leaks in from user or project scope. Deterministic beats
		// convenient for a control plane.
		args = append(args, "--mcp-config", cfg.MCPConfigPath, "--strict-mcp-config")
	}
	if cfg.PermissionTool != "" {
		args = append(args, "--permission-prompt-tool", cfg.PermissionTool)
	}
	if cfg.ResumeID != "" {
		args = append(args, "--resume", cfg.ResumeID)
	}
	return args
}

// PermissionToolName is the tool the CLI is told to call for every permission
// decision. Routing prompts through a tool rather than a shell hook is both
// supported and language-agnostic, and it is what lets the gateway be the
// enforcement point.
func (a *Adapter) PermissionToolName() string { return "mcp__splitscreen__permission_prompt" }

// Start launches a session process.
func (a *Adapter) Start(ctx context.Context, cfg harness.SessionConfig) (harness.Session, error) {
	if cfg.Cwd == "" {
		return nil, errors.New("claudecode: cwd is required")
	}
	bin := a.Binary
	if bin == "" {
		bin = "claude"
	}

	cmd := exec.Command(bin, buildArgs(cfg)...)
	cmd.Dir = cfg.Cwd
	// Env is exactly what the caller built. Constructing it from scratch, rather
	// than filtering the parent's, is what keeps a newly introduced variable
	// from leaking in unnoticed.
	cmd.Env = cfg.Env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("claudecode: start %s: %w", bin, err)
	}

	s := &session{
		cmd:    cmd,
		stdin:  stdin,
		events: make(chan harness.Event, 256),
	}
	s.running.Store(true)

	go s.readStderr(stderr)
	go s.readStdout(stdout)
	return s, nil
}

type session struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser

	mu        sync.Mutex
	sessionID string
	stderrBuf strings.Builder

	events  chan harness.Event
	running atomic.Bool
	closed  sync.Once
	toolN   atomic.Int64

	// callStart is when each tool call was announced, so its result can carry a
	// duration. Keyed by tool_use id; entries are removed when the result
	// arrives, and a turn that dies mid-call simply leaves one behind.
	callStart sync.Map
}

func (s *session) Events() <-chan harness.Event { return s.events }
func (s *session) Running() bool                { return s.running.Load() }

func (s *session) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// Send writes one user message in the CLI's stream-json input shape.
func (s *session) Send(ctx context.Context, in harness.Input) error {
	if !s.running.Load() {
		return errors.New("claudecode: session is not running")
	}

	var content any = in.Text
	if len(in.Images) > 0 {
		blocks := make([]map[string]any, 0, len(in.Images)+1)
		for _, img := range in.Images {
			blocks = append(blocks, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": img.Mime,
					"data":       base64.StdEncoding.EncodeToString(img.Data),
				},
			})
		}
		if in.Text != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": in.Text})
		}
		content = blocks
	}

	payload := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": content,
		},
		"parent_tool_use_id": nil,
	}
	if id := s.SessionID(); id != "" {
		payload["session_id"] = id
	}

	line, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.stdin.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("claudecode: write to session: %w", err)
	}
	return nil
}

func (s *session) Close() error {
	s.closed.Do(func() {
		s.running.Store(false)
		_ = s.stdin.Close()
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
	})
	return nil
}

func (s *session) emit(e harness.Event) {
	select {
	case s.events <- e:
	default:
		// The consumer is the runner's own loop; a full buffer means it has
		// stalled, and blocking here would deadlock the reader.
	}
}

func (s *session) readStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		s.mu.Lock()
		if s.stderrBuf.Len() < 8192 {
			s.stderrBuf.WriteString(sc.Text())
			s.stderrBuf.WriteString("\n")
		}
		s.mu.Unlock()
	}
}

// streamEvent is the subset of the CLI's stream-json vocabulary this adapter
// consumes. Unknown types are ignored so a CLI upgrade that adds an event does
// not break the adapter.
type streamEvent struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	Message   json.RawMessage `json:"message"`
	Usage     *rawUsage       `json:"usage"`
	TotalCost *float64        `json:"total_cost_usd"`
	NumTurns  int             `json:"num_turns"`
	IsError   bool            `json:"is_error"`
	Result    string          `json:"result"`
	Model     string          `json:"model"`
}

type rawUsage struct {
	InputTokens              int64  `json:"input_tokens"`
	OutputTokens             int64  `json:"output_tokens"`
	CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64  `json:"cache_read_input_tokens"`
	CacheCreationTTL         string `json:"cache_creation_ttl"`
}

type assistantMessage struct {
	Model   string `json:"model"`
	Content []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
	} `json:"content"`
	Usage *rawUsage `json:"usage"`
}

// userMessage carries tool results back from the CLI. The role is "user"
// because the harness models a tool result as something handed to the model,
// not as something the model said.
type userMessage struct {
	Content []struct {
		Type      string          `json:"type"`
		ToolUseID string          `json:"tool_use_id"`
		IsError   bool            `json:"is_error"`
		Content   json.RawMessage `json:"content"`
	} `json:"content"`
}

// resultText pulls readable text out of a tool result, which is a bare string
// for most tools and a block array for the ones that can return images.
func resultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return truncateResult(text)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return truncateResult(strings.Join(parts, " "))
	}
	return ""
}

// truncateResult bounds an error before it crosses the wire. A failing tool can
// return a great deal of output and none of it belongs in a chat message.
func truncateResult(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 400 {
		return s
	}
	return s[:400] + "…"
}

// truncateThinking bounds a thinking block before it crosses the wire. A
// thought is rendered as one step in a turn's progress, so anything past a
// couple of sentences is weight the reader never sees.
func truncateThinking(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 1000 {
		return s
	}
	return s[:1000] + "…"
}

func (s *session) readStdout(r io.Reader) {
	defer close(s.events)
	defer s.running.Store(false)

	sc := bufio.NewScanner(r)
	// Assistant messages carrying tool inputs can be large; the default 64KiB
	// scanner limit truncates them into parse errors.
	sc.Buffer(make([]byte, 0, 256*1024), 16*1024*1024)

	var lastModel string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue // non-JSON noise on stdout is not fatal
		}

		if ev.SessionID != "" {
			s.mu.Lock()
			s.sessionID = ev.SessionID
			s.mu.Unlock()
		}

		switch ev.Type {
		case "system":
			if ev.Subtype == "init" && ev.SessionID != "" {
				s.emit(harness.Event{Kind: harness.EventSession, SessionID: ev.SessionID})
			}

		case "assistant":
			var msg assistantMessage
			if err := json.Unmarshal(ev.Message, &msg); err != nil {
				continue
			}
			if msg.Model != "" {
				lastModel = msg.Model
			}
			for _, block := range msg.Content {
				switch block.Type {
				case "text":
					if block.Text != "" {
						s.emit(harness.Event{Kind: harness.EventText, Text: block.Text})
					}
				case "thinking":
					// redacted_thinking carries no readable text and is skipped.
					if t := truncateThinking(block.Thinking); t != "" {
						s.emit(harness.Event{Kind: harness.EventThinking, Text: t})
					}
				case "tool_use":
					s.toolN.Add(1)
					s.callStart.Store(block.ID, time.Now())
					s.emit(harness.Event{
						Kind:    harness.EventToolUse,
						Tool:    block.Name,
						CallID:  block.ID,
						Summary: summarize(block.Input),
					})
				}
			}

		case "user":
			// A tool's result comes back as a user message carrying
			// tool_result blocks. Without this the CLI reports every call
			// starting and none finishing, which reads as a hung tool on any
			// surface that renders a tool call as something with a lifecycle.
			var msg userMessage
			if err := json.Unmarshal(ev.Message, &msg); err != nil {
				continue
			}
			for _, block := range msg.Content {
				if block.Type != "tool_result" || block.ToolUseID == "" {
					continue
				}
				out := harness.Event{
					Kind:   harness.EventToolEnd,
					CallID: block.ToolUseID,
					OK:     !block.IsError,
				}
				if block.IsError {
					out.Error = resultText(block.Content)
				}
				if started, ok := s.callStart.LoadAndDelete(block.ToolUseID); ok {
					out.DurationMS = time.Since(started.(time.Time)).Milliseconds()
				}
				s.emit(out)
			}

		case "result":
			usage := harness.Usage{Model: lastModel}
			if ev.Model != "" {
				usage.Model = ev.Model
			}
			if ev.Usage != nil {
				usage.Known = true
				usage.InputTokens = ev.Usage.InputTokens
				usage.OutputTokens = ev.Usage.OutputTokens
				usage.CacheWriteTokens = ev.Usage.CacheCreationInputTokens
				usage.CacheReadTokens = ev.Usage.CacheReadInputTokens
				usage.TTLHint = ev.Usage.CacheCreationTTL
				usage.ProviderCostUSD = ev.TotalCost
			}
			// Emitting usage separately from done keeps "we have no numbers"
			// distinguishable from "the numbers are zero".
			s.emit(harness.Event{Kind: harness.EventUsage, Usage: usage})

			if ev.IsError {
				s.emit(harness.Event{
					Kind:  harness.EventError,
					Error: firstNonEmpty(ev.Result, ev.Subtype, "harness reported an error"),
				})
				continue
			}
			s.emit(harness.Event{
				Kind:      harness.EventDone,
				SessionID: s.SessionID(),
				ToolCalls: int(s.toolN.Load()),
			})
		}
	}

	if err := s.cmd.Wait(); err != nil {
		s.mu.Lock()
		stderr := strings.TrimSpace(s.stderrBuf.String())
		s.mu.Unlock()
		msg := err.Error()
		if stderr != "" {
			msg += ": " + lastLines(stderr, 5)
		}
		// The channel is closed by the deferred close; emit before returning.
		select {
		case s.events <- harness.Event{Kind: harness.EventError, Error: msg}:
		default:
		}
	}
}

func summarize(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil {
		return ""
	}
	for _, k := range []string{"command", "file_path", "path", "pattern", "url", "query", "description"} {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
