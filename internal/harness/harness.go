// Package harness abstracts the agent implementations a runner drives.
//
// This is the second of the system's two plug points, symmetric with surface
// adapters on the chat side. Adapters drive a harness as a subprocess rather
// than linking an SDK: subprocess-driving is what generalizes across harnesses,
// which an in-process callback by definition does not.
package harness

import (
	"context"
	"fmt"
	"sync"
)

// EventKind identifies what a harness reported.
type EventKind string

const (
	EventText EventKind = "text"
	// EventThinking is reasoning the model produced on the way to its answer.
	// Separate from EventText because the two diverge at the surface: text is
	// the answer, thinking is an intermediate step.
	EventThinking EventKind = "thinking"
	EventToolUse  EventKind = "tool_use"
	EventToolEnd  EventKind = "tool_end"
	EventUsage    EventKind = "usage"
	EventSession  EventKind = "session"
	EventDone     EventKind = "done"
	EventError    EventKind = "error"
)

// Usage is normalized token accounting.
//
// Adapters that cannot report usage must leave Known false rather than
// reporting zeros: a rollup showing $0 for an un-instrumented harness is worse
// than one showing nothing.
type Usage struct {
	Model            string
	InputTokens      int64
	CacheWriteTokens int64
	CacheReadTokens  int64
	OutputTokens     int64
	TTLHint          string
	ProviderCostUSD  *float64
	Known            bool
}

// Event is one thing a harness said.
type Event struct {
	Kind    EventKind
	Text    string
	Tool    string
	CallID  string
	Summary string
	OK      bool
	Error   string
	// DurationMS is how long a tool call took, measured by the adapter between
	// the call and its result.
	DurationMS int64
	SessionID  string
	Usage      Usage
	ToolCalls  int
}

// Image is an inline image passed to the harness.
type Image struct {
	Mime string
	Data []byte
}

// Input is one user message.
type Input struct {
	Text   string
	Images []Image
}

// SessionConfig describes the process a session runs as.
type SessionConfig struct {
	// Cwd is the working tree the agent operates on.
	Cwd string
	// ConfigDir is the materialized bundle root, on tmpfs.
	ConfigDir string
	// Env is the complete environment for the child, constructed from scratch.
	// An allowlist built here cannot fail open the way filtering the parent's
	// environment does every time a new variable appears.
	Env []string
	// Model is the model id to run, empty meaning the harness keeps its own
	// default. Adapters that cannot select a model ignore it.
	Model string
	// ResumeID continues a previous session, transparently, after an idle kill.
	ResumeID string
	// MCPConfigPath is the assembled server set. Adapters should pass a strict
	// flag so nothing leaks in from user or project scope.
	MCPConfigPath string
	// PermissionTool is the tool the harness must call for permission
	// decisions, rather than prompting or auto-approving.
	PermissionTool string
}

// Session is one live harness conversation.
type Session interface {
	// Send delivers a user message.
	Send(ctx context.Context, in Input) error
	// Events yields harness output until the session ends.
	Events() <-chan Event
	// SessionID is the harness's own identifier, persisted so a killed session
	// can be resumed.
	SessionID() string
	// Close terminates the session.
	Close() error
	// Running reports whether the process is alive.
	Running() bool
}

// Adapter starts sessions for one harness.
type Adapter interface {
	Name() string
	// Start launches a session.
	Start(ctx context.Context, cfg SessionConfig) (Session, error)
	// DefaultCredentialEnv is the environment variable this harness reads its
	// credential from, used when a runner does not override it.
	DefaultCredentialEnv() string
	// PermissionToolName is the fully-qualified tool the harness calls for
	// permission decisions.
	PermissionToolName() string
}

var (
	mu       sync.RWMutex
	adapters = map[string]Adapter{}
)

// Register makes an adapter available by name.
func Register(a Adapter) {
	mu.Lock()
	defer mu.Unlock()
	adapters[a.Name()] = a
}

// Get looks up a registered adapter.
func Get(name string) (Adapter, error) {
	mu.RLock()
	defer mu.RUnlock()
	a, ok := adapters[name]
	if !ok {
		return nil, fmt.Errorf("harness: no adapter named %q (have %v)", name, names())
	}
	return a, nil
}

func names() []string {
	out := make([]string, 0, len(adapters))
	for n := range adapters {
		out = append(out, n)
	}
	return out
}

// Names lists registered adapters.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	return names()
}
