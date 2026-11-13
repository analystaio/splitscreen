// Package mcpproxy executes credential-bearing MCP calls on the gateway's
// behalf.
//
// The split is: does a server need the runner's filesystem, or does it need a
// credential? Filesystem servers run on the runner. Credentialed servers run
// here, so the runner never sees the token, every call is audited with the
// requesting human attached, and destructive tools can be denied irrespective
// of what the agent intends.
package mcpproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// AuthKind selects how a server's credential is presented.
type AuthKind string

const (
	AuthNone   AuthKind = "none"
	AuthBearer AuthKind = "bearer"
	// AuthBasic is email:token, which is how Atlassian API tokens authenticate.
	AuthBasic AuthKind = "basic"
)

// Server describes one proxied MCP endpoint.
type Server struct {
	Name string
	URL  string
	Auth AuthKind
	// User is the basic-auth username (an account email, typically).
	User string
	// Secret is the resolved credential value. It exists only in gateway
	// memory; it is never sent to a runner.
	Secret string
	// Deny lists tool names this server may never invoke, enforced before the
	// call leaves the gateway.
	Deny []string
}

// Result is a proxied call's outcome.
type Result struct {
	Payload json.RawMessage
	Err     error
}

// Proxy invokes tools on configured servers.
type Proxy struct {
	mu      sync.RWMutex
	servers map[string]Server
	http    *http.Client
	seq     atomic.Int64
}

func New() *Proxy {
	return &Proxy{
		servers: map[string]Server{},
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

// Configure replaces the server set. Called on config reload.
func (p *Proxy) Configure(servers []Server) {
	m := make(map[string]Server, len(servers))
	for _, s := range servers {
		m[s.Name] = s
	}
	p.mu.Lock()
	p.servers = m
	p.mu.Unlock()
}

// Has reports whether a server is proxied here.
func (p *Proxy) Has(name string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.servers[name]
	return ok
}

// Names lists configured servers.
func (p *Proxy) Names() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, 0, len(p.servers))
	for n := range p.servers {
		out = append(out, n)
	}
	return out
}

type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data,omitempty"`
	} `json:"error,omitempty"`
}

// ListTool is the reserved tool name that means "enumerate this server's
// tools". The protocol carries one MCP frame type so that every call has a
// single audited path; discovery rides on it under this name and is translated
// to the proper JSON-RPC method here.
const ListTool = "tools/list"

// Call invokes a tool. The caller has already resolved and recorded who asked;
// this function is responsible for the credential and for the server-side deny
// list.
func (p *Proxy) Call(ctx context.Context, server, tool string, args json.RawMessage) (json.RawMessage, error) {
	p.mu.RLock()
	s, ok := p.servers[server]
	p.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("mcpproxy: server %q is not configured on the gateway", server)
	}
	// Discovery is never denied: hiding a tool's existence while still refusing
	// to run it produces a much more confusing failure than an honest refusal.
	if tool != ListTool {
		for _, d := range s.Deny {
			if matchTool(d, tool) {
				return nil, fmt.Errorf("mcpproxy: tool %q on %q is denied by gateway policy", tool, server)
			}
		}
	}

	method := "tools/call"
	var params any = map[string]any{"name": tool}
	if tool == ListTool {
		// Discovery is a distinct JSON-RPC method, not a tool named after one.
		method = ListTool
		params = map[string]any{}
	} else if len(args) > 0 {
		params.(map[string]any)["arguments"] = json.RawMessage(args)
	}
	body, err := json.Marshal(jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      p.seq.Add(1),
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	switch s.Auth {
	case AuthBearer:
		req.Header.Set("Authorization", "Bearer "+s.Secret)
	case AuthBasic:
		req.SetBasicAuth(s.User, s.Secret)
	case AuthNone, "":
	default:
		return nil, fmt.Errorf("mcpproxy: server %q has unknown auth kind %q", server, s.Auth)
	}

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcpproxy: call %s/%s: %w", server, tool, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("mcpproxy: read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("mcpproxy: %s/%s: %s: %s", server, tool, resp.Status,
			strings.TrimSpace(truncate(string(raw), 500)))
	}

	payload := raw
	// Streamable-HTTP endpoints answer with SSE framing even for unary calls.
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/event-stream") {
		payload = extractSSEData(raw)
	}

	var rpc jsonRPCResponse
	if err := json.Unmarshal(payload, &rpc); err != nil {
		return nil, fmt.Errorf("mcpproxy: decode response from %s: %w", server, err)
	}
	if rpc.Error != nil {
		return nil, fmt.Errorf("mcpproxy: %s/%s returned error %d: %s",
			server, tool, rpc.Error.Code, rpc.Error.Message)
	}
	return rpc.Result, nil
}

// extractSSEData pulls the last data: payload out of an SSE body.
func extractSSEData(raw []byte) []byte {
	var last []byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if after, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			last = bytes.TrimSpace(after)
		}
	}
	if last == nil {
		return raw
	}
	return last
}

// MatchTool reports whether a deny rule covers a tool, for servers the gateway
// runs itself.
func MatchTool(rule, tool string) bool { return matchTool(rule, tool) }

// matchTool supports an exact name or a trailing wildcard.
func matchTool(rule, tool string) bool {
	if prefix, ok := strings.CutSuffix(rule, "*"); ok {
		return strings.HasPrefix(tool, prefix)
	}
	return rule == tool
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
