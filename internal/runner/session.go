package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/avarant/splitscreen/internal/harness"
	"github.com/avarant/splitscreen/protocol"
)

// threadSession is one harness conversation, keyed on a gateway thread.
//
// Sessions are per-thread and long-lived. An idle one is killed to reclaim
// memory; the harness session id is retained so the next message resumes
// transparently.
type threadSession struct {
	threadID  string
	sessionID string
	sessionMu sync.Mutex

	sess    harness.Session
	dir     string
	mcpPath string

	lastActivity time.Time
	turnMu       sync.RWMutex
	currentTurn  string
	busy         bool

	sendMu sync.Mutex
}

func (t *threadSession) setTurn(id string) {
	t.turnMu.Lock()
	t.currentTurn = id
	t.busy = true
	t.lastActivity = time.Now()
	t.turnMu.Unlock()
}

// endTurn marks the session as between turns. The turn id is kept so late
// events (usage arriving after done) still attribute correctly.
func (t *threadSession) endTurn() {
	t.turnMu.Lock()
	t.busy = false
	t.lastActivity = time.Now()
	t.turnMu.Unlock()
}

func (t *threadSession) isBusy() bool {
	t.turnMu.RLock()
	defer t.turnMu.RUnlock()
	return t.busy
}

func (t *threadSession) touch() {
	t.turnMu.Lock()
	t.lastActivity = time.Now()
	t.turnMu.Unlock()
}

func (t *threadSession) turn() string {
	t.turnMu.RLock()
	defer t.turnMu.RUnlock()
	return t.currentTurn
}

func (t *threadSession) idle() time.Duration {
	t.turnMu.RLock()
	defer t.turnMu.RUnlock()
	return time.Since(t.lastActivity)
}

func threadDirName(threadID string) string {
	sum := sha256.Sum256([]byte(threadID))
	return hex.EncodeToString(sum[:8])
}

// sessionFor returns the session for a thread, starting one if needed.
func (r *Runner) sessionFor(ctx context.Context, threadID string) (*threadSession, error) {
	v, _ := r.sessions.LoadOrStore(threadID, &threadSession{
		threadID:     threadID,
		sessionID:    r.sessionIDs.get(threadID),
		lastActivity: time.Now(),
	})
	ts := v.(*threadSession)

	ts.sessionMu.Lock()
	defer ts.sessionMu.Unlock()

	if ts.sess != nil && ts.sess.Running() {
		return ts, nil
	}

	if err := r.makeRoom(threadID); err != nil {
		return nil, err
	}

	configDir := r.bundle.ConfigDir()
	if configDir == "" {
		return nil, fmt.Errorf("runner: no bundle has been materialized yet")
	}
	if err := r.refreshMemory(configDir); err != nil {
		// Stale notes are better than no session.
		r.log.Warn("refreshing durable notes failed", "err", err)
	}

	dir := filepath.Join(r.opts.RuntimeRoot, r.opts.Name, "threads", threadDirName(threadID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// The MCP config is per-thread so the shims can carry the thread id, which
	// is how a permission prompt or a git credential request is attributed to
	// the right conversation without the harness having to know about any of it.
	mcpPath := filepath.Join(dir, "mcp.json")
	if err := r.writeThreadMCP(mcpPath, threadID); err != nil {
		return nil, err
	}

	env := r.buildEnv(configDir)
	cfg := harness.SessionConfig{
		Cwd:            r.opts.Cwd,
		Model:          r.bundle.Model(),
		ConfigDir:      configDir,
		Env:            env,
		ResumeID:       ts.sessionID,
		MCPConfigPath:  mcpPath,
		PermissionTool: r.adapter.PermissionToolName(),
	}

	sess, err := r.adapter.Start(ctx, cfg)
	if err != nil {
		return nil, err
	}
	ts.sess = sess
	ts.dir = dir
	ts.mcpPath = mcpPath
	ts.lastActivity = time.Now()

	go r.pumpEvents(ts, sess)
	r.log.Info("session started", "thread", threadID, "resume", cfg.ResumeID != "")
	return ts, nil
}

// buildEnv constructs the harness environment from scratch.
//
// An allowlist built here cannot fail open the way filtering the parent's
// environment does: a denylist misses every variable introduced after it was
// written, which is exactly how a credential stopped reaching the harness in
// the predecessor to this system.
func (r *Runner) buildEnv(configDir string) []string {
	allow := []string{
		"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "TZ",
		"TERM", "TMPDIR", "SSL_CERT_FILE", "SSL_CERT_DIR",
		// Cloud IAM credentials, for deployments where the harness authenticates
		// against a provider endpoint and no long-lived secret exists at all.
		"AWS_REGION", "AWS_DEFAULT_REGION", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_WEB_IDENTITY_TOKEN_FILE",
		"AWS_ROLE_ARN", "GOOGLE_APPLICATION_CREDENTIALS", "CLOUD_ML_REGION",
	}
	env := make([]string, 0, len(allow)+8)
	for _, k := range allow {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	env = append(env, "CLAUDE_CONFIG_DIR="+configDir)
	env = append(env, "SPLITSCREEN_RUNNER="+r.opts.Name)
	env = append(env, "SPLITSCREEN_SOCKET="+SocketPath(r.opts.RuntimeRoot, r.opts.Name))

	// The harness credential is the one secret that must exist on the runner,
	// because the agent process authenticates from here.
	for k, v := range r.bundle.Secrets() {
		env = append(env, k+"="+v)
	}
	return env
}

func (r *Runner) writeThreadMCP(path, threadID string) error {
	base := r.bundle.MCPPath()
	body, err := os.ReadFile(base)
	if err != nil {
		return fmt.Errorf("runner: read base mcp config: %w", err)
	}
	var doc struct {
		Servers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("runner: parse base mcp config: %w", err)
	}
	for _, entry := range doc.Servers {
		args, ok := entry["args"].([]any)
		if !ok {
			continue
		}
		isShim := false
		for _, a := range args {
			if s, ok := a.(string); ok && (s == "mcp-shim" || s == "permission-shim") {
				isShim = true
				break
			}
		}
		if isShim {
			entry["args"] = append(args, "--thread", threadID)
		}
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

// handleMessage runs one turn.
func (r *Runner) handleMessage(ctx context.Context, msg *protocol.Message) {
	fail := func(code string, err error) {
		r.log.Error("turn failed", "thread", msg.ThreadID, "turn", msg.TurnID, "err", err)
		_ = r.send(ctx, &protocol.Error{
			ThreadID: msg.ThreadID, TurnID: msg.TurnID,
			Code: code, Message: err.Error(),
		})
	}

	if msg.Command == "new" {
		r.endSession(msg.ThreadID)
	}

	ts, err := r.sessionFor(ctx, msg.ThreadID)
	if err != nil {
		fail("session_start_failed", err)
		return
	}
	ts.setTurn(msg.TurnID)
	r.touchActive()

	in := harness.Input{Text: withContext(msg)}
	for _, att := range msg.Attachments {
		data, path, ok := r.takeBlob(att.BlobID)
		if !ok {
			r.log.Warn("attachment was never delivered", "blob", att.BlobID)
			continue
		}
		if att.Inline && len(data) > 0 {
			in.Images = append(in.Images, harness.Image{Mime: att.Mime, Data: data})
			continue
		}
		// Non-image files are handed over as paths; the harness reads them with
		// its own tools, which keeps large files out of the prompt.
		in.Text += fmt.Sprintf("\n\n[attachment saved to %s]", path)
	}

	if err := ts.send(ctx, in); err != nil {
		fail("send_failed", err)
		return
	}
}

func (t *threadSession) send(ctx context.Context, in harness.Input) error {
	t.sendMu.Lock()
	defer t.sendMu.Unlock()
	t.sessionMu.Lock()
	sess := t.sess
	t.sessionMu.Unlock()
	if sess == nil {
		return fmt.Errorf("runner: session is not running")
	}
	return sess.Send(ctx, in)
}

// pumpEvents forwards harness output to the gateway.
func (r *Runner) pumpEvents(ts *threadSession, sess harness.Session) {
	ctx := context.Background()
	for ev := range sess.Events() {
		// Every event counts as activity. Idle is measured from the last thing
		// the harness did, not from when the turn started — otherwise a turn
		// that outlives the idle timeout is reaped mid-flight while it is
		// still streaming tool calls.
		ts.touch()
		r.markActive()
		turn := ts.turn()
		switch ev.Kind {
		case harness.EventText:
			_ = r.send(ctx, &protocol.TextDelta{ThreadID: ts.threadID, TurnID: turn, Text: ev.Text})

		case harness.EventThinking:
			_ = r.send(ctx, &protocol.Thought{ThreadID: ts.threadID, TurnID: turn, Text: ev.Text})

		case harness.EventSession:
			ts.sessionMu.Lock()
			ts.sessionID = ev.SessionID
			ts.sessionMu.Unlock()
			r.rememberSession(ts.threadID, ev.SessionID)

		case harness.EventToolUse:
			_ = r.send(ctx, &protocol.ToolStart{
				ThreadID: ts.threadID, TurnID: turn,
				CallID: ev.CallID, Tool: ev.Tool, Summary: ev.Summary,
			})

		case harness.EventToolEnd:
			_ = r.send(ctx, &protocol.ToolEnd{
				ThreadID: ts.threadID, TurnID: turn,
				CallID: ev.CallID, OK: ev.OK, Error: ev.Error,
				DurationMS: ev.DurationMS,
			})

		case harness.EventUsage:
			_ = r.send(ctx, &protocol.Usage{
				ThreadID: ts.threadID, TurnID: turn,
				Model:            ev.Usage.Model,
				InputTokens:      ev.Usage.InputTokens,
				CacheWriteTokens: ev.Usage.CacheWriteTokens,
				CacheReadTokens:  ev.Usage.CacheReadTokens,
				OutputTokens:     ev.Usage.OutputTokens,
				TTLHint:          ev.Usage.TTLHint,
				ProviderCostUSD:  ev.Usage.ProviderCostUSD,
				Known:            ev.Usage.Known,
			})

		case harness.EventDone:
			ts.sessionMu.Lock()
			if ev.SessionID != "" {
				ts.sessionID = ev.SessionID
			}
			sid := ts.sessionID
			ts.sessionMu.Unlock()
			r.rememberSession(ts.threadID, sid)
			ts.endTurn()
			r.settleActive()
			_ = r.send(ctx, &protocol.Done{
				ThreadID: ts.threadID, TurnID: turn,
				SessionID: sid, NumToolCalls: ev.ToolCalls,
			})

		case harness.EventError:
			ts.endTurn()
			r.settleActive()
			_ = r.send(ctx, &protocol.Error{
				ThreadID: ts.threadID, TurnID: turn,
				Code: "harness_error", Message: ev.Error,
			})
		}
	}
	r.log.Info("session ended", "thread", ts.threadID)

	// The event stream closed with a turn still open — the harness process
	// ended after a tool call without emitting Done or Error (a crash, a kill,
	// a usage limit), and was then idle-reaped. Left alone the gateway keeps
	// the turn "running" forever and never frees its concurrency slot, so the
	// runner slowly leaks toward looking permanently at-capacity. Report it so
	// the turn is finalized and the slot released.
	if ts.isBusy() {
		turn := ts.turn()
		ts.endTurn()
		r.settleActive()
		r.log.Warn("session ended with an open turn; reporting error",
			"thread", ts.threadID, "turn", turn)
		_ = r.send(ctx, &protocol.Error{
			ThreadID: ts.threadID, TurnID: turn,
			Code: "session_ended", Message: "the harness session ended before the turn completed",
		})
	}
}

// endSession stops a thread's harness process. The session id survives, so the
// next message resumes rather than starting over.
func (r *Runner) endSession(threadID string) {
	v, ok := r.sessions.Load(threadID)
	if !ok {
		return
	}
	ts := v.(*threadSession)
	ts.sessionMu.Lock()
	if ts.sess != nil {
		_ = ts.sess.Close()
		ts.sess = nil
	}
	// !new means start over, so drop the resume point too.
	ts.sessionID = ""
	ts.sessionMu.Unlock()
	r.rememberSession(threadID, "")
}

// rememberSession persists a thread's resume point, when there is a state dir
// to persist it in.
func (r *Runner) rememberSession(threadID, sessionID string) {
	if err := r.sessionIDs.set(threadID, sessionID); err != nil {
		r.log.Warn("persisting session id failed", "thread", threadID, "err", err)
	}
}

// makeRoom enforces MaxSessions before a new harness process starts.
//
// The cap exists because sessions are reaped by silence, not by count: enough
// concurrent threads inside the idle window will exhaust the box no matter how
// short the timeout is (thirteen resident sessions froze a 16 GB host solid).
// Preference order: evict the longest-idle session that is between turns; a
// session mid-turn is never killed — if every resident session is mid-turn,
// the new one is refused instead, which surfaces in-thread as an error the
// sender can retry, rather than as a dead machine.
func (r *Runner) makeRoom(exclude string) error {
	max := r.opts.MaxSessions
	if max <= 0 {
		return nil
	}

	type candidate struct {
		ts   *threadSession
		idle time.Duration
	}
	running := 0
	var victims []candidate
	r.sessions.Range(func(key, value any) bool {
		ts := value.(*threadSession)
		if key.(string) == exclude {
			return true
		}
		// TryLock, not Lock: a session holding its own mutex is mid-start or
		// mid-teardown. Treat it as resident but not evictable. A plain Lock
		// here can deadlock against that session's own makeRoom.
		if !ts.sessionMu.TryLock() {
			running++
			return true
		}
		alive := ts.sess != nil && ts.sess.Running()
		ts.sessionMu.Unlock()
		if !alive {
			return true
		}
		running++
		if !ts.isBusy() {
			victims = append(victims, candidate{ts, ts.idle()})
		}
		return true
	})

	if running < max {
		return nil
	}

	sort.Slice(victims, func(i, j int) bool { return victims[i].idle > victims[j].idle })
	need := running - max + 1
	for _, v := range victims {
		if need == 0 {
			break
		}
		if !v.ts.sessionMu.TryLock() {
			continue
		}
		// Re-check under the lock: a message may have started a turn since the
		// scan, and killing it mid-turn is the exact bug the idle reaper had.
		if v.ts.isBusy() || v.ts.sess == nil || !v.ts.sess.Running() {
			v.ts.sessionMu.Unlock()
			continue
		}
		r.log.Info("reaping session to make room",
			"thread", v.ts.threadID, "idle", v.ts.idle().Round(time.Second),
			"resident", running, "max", max)
		_ = v.ts.sess.Close()
		v.ts.sess = nil
		v.ts.sessionMu.Unlock()
		need--
	}
	if need > 0 {
		return fmt.Errorf("runner at capacity: %d sessions and all are mid-turn (max %d); wait for one to finish and resend", running, max)
	}
	return nil
}

// sweepIdle reaps sessions that have been quiet.
//
// This is a cost lever as much as a memory one: killing a session forces the
// next turn to re-create the prompt cache, trading cache reads at a fraction of
// input for cache writes at a premium.
func (r *Runner) sweepIdle(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.CleanupThreads()
			r.sessions.Range(func(key, value any) bool {
				ts := value.(*threadSession)
				ts.sessionMu.Lock()
				running := ts.sess != nil && ts.sess.Running()
				ts.sessionMu.Unlock()
				if !running {
					return true
				}
				if ts.idle() < r.opts.IdleTimeout {
					return true
				}
				r.log.Info("reaping idle session",
					"thread", ts.threadID, "idle", ts.idle().Round(time.Second))
				ts.sessionMu.Lock()
				if ts.sess != nil {
					_ = ts.sess.Close()
					ts.sess = nil
				}
				ts.sessionMu.Unlock()
				return true
			})
		}
	}
}

// turnForThread resolves the active turn, used by local helpers that know only
// which thread they belong to.
func (r *Runner) turnForThread(threadID string) string {
	if v, ok := r.sessions.Load(threadID); ok {
		return v.(*threadSession).turn()
	}
	return ""
}

// threadDir is where a thread's uploads and config live.
func (r *Runner) threadDir(threadID string) string {
	return filepath.Join(r.opts.RuntimeRoot, r.opts.Name, "threads", threadDirName(threadID))
}

// CleanupThreads removes spool directories for threads with no live session.
// Upload directories that grow forever were a real defect in the predecessor;
// tying cleanup to the same sweep that reaps sessions closes it.
func (r *Runner) CleanupThreads() {
	root := filepath.Join(r.opts.RuntimeRoot, r.opts.Name, "threads")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	live := map[string]bool{}
	r.sessions.Range(func(key, value any) bool {
		live[threadDirName(key.(string))] = true
		return true
	})
	for _, e := range entries {
		if !e.IsDir() || live[e.Name()] {
			continue
		}
		if strings.TrimSpace(e.Name()) == "" {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
}

// withContext puts the gateway's context header in front of the message text,
// on its own line, so the agent knows where the message came from and who sent
// it. The header is input only: nothing here reaches the surface.
func withContext(msg *protocol.Message) string {
	if msg.Context == "" {
		return msg.Text
	}
	if msg.Text == "" {
		return msg.Context
	}
	return msg.Context + "\n" + msg.Text
}
