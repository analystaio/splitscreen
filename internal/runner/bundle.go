package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/avarant/splitscreen/protocol"
)

// materialized is the on-disk form of a pushed bundle.
type materialized struct {
	mu sync.RWMutex

	version   int
	digest    string
	configDir string
	mcpPath   string
	model     string
	// secrets are held in memory and written only to tmpfs. They are never
	// logged and never persisted.
	secrets map[string]string
}

func (m *materialized) Version() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.version
}

func (m *materialized) Digest() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.digest
}

func (m *materialized) ConfigDir() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.configDir
}

func (m *materialized) Model() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.model
}

func (m *materialized) MCPPath() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.mcpPath
}

func (m *materialized) Secrets() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]string, len(m.secrets))
	for k, v := range m.secrets {
		out[k] = v
	}
	return out
}

// applyBundle writes a pushed bundle to the runtime directory.
//
// The runtime root should be tmpfs (/run/splitscreen by default): nothing here
// belongs on persistent disk, and a reboot should leave no trace. Ephemeral
// custody shortens the window a credential exists on the box; it does not
// isolate it from another process running as the same user.
func (r *Runner) applyBundle(push *protocol.BundlePush) error {
	root := filepath.Join(r.opts.RuntimeRoot, r.opts.Name)
	configDir := filepath.Join(root, "config")

	// Replace atomically-ish: build beside the live directory, then swap. A
	// half-written config directory would be worse than a stale one.
	staging := configDir + ".new"
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("runner: clear staging dir: %w", err)
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return fmt.Errorf("runner: create staging dir: %w", err)
	}

	for _, f := range push.Files {
		// The gateway validated these, but the check is repeated here: a
		// guarantee that depends on one side of a connection is not a guarantee.
		if err := safeRelPath(f.Path); err != nil {
			return fmt.Errorf("runner: refusing bundle file %q: %w", f.Path, err)
		}
		dest := filepath.Join(staging, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return err
		}
		mode := os.FileMode(f.Mode)
		if mode == 0 {
			mode = 0o600
		}
		mode &= 0o777
		if err := os.WriteFile(dest, f.Content, mode); err != nil {
			return fmt.Errorf("runner: write bundle file %q: %w", f.Path, err)
		}
	}

	if r.opts.StateDir != "" {
		if err := r.prepareState(staging); err != nil {
			return err
		}
	}

	// Memory files are concatenated into the harness's user-memory file. The
	// gateway keeps them as separate reviewable sources; the harness wants one.
	if err := assembleMemory(staging, r.localMemoryPath()); err != nil {
		return err
	}

	mcpPath := filepath.Join(staging, "mcp.json")
	if err := r.writeMCPConfig(mcpPath, push.MCP); err != nil {
		return err
	}

	if err := r.linkHostCredentials(staging); err != nil {
		return err
	}

	old := configDir + ".old"
	_ = os.RemoveAll(old)
	if _, err := os.Stat(configDir); err == nil {
		if err := os.Rename(configDir, old); err != nil {
			return fmt.Errorf("runner: rotate config dir: %w", err)
		}
	}
	if err := os.Rename(staging, configDir); err != nil {
		return fmt.Errorf("runner: install config dir: %w", err)
	}
	_ = os.RemoveAll(old)

	r.bundle.mu.Lock()
	r.bundle.version = push.Version
	r.bundle.digest = push.Digest
	r.bundle.model = push.Model
	r.bundle.configDir = configDir
	r.bundle.mcpPath = filepath.Join(configDir, "mcp.json")
	r.bundle.secrets = make(map[string]string, len(push.Secrets))
	for k, v := range push.Secrets {
		r.bundle.secrets[k] = v
	}
	r.bundle.mu.Unlock()

	r.log.Info("bundle materialized",
		"version", push.Version, "digest", push.Digest,
		"files", len(push.Files), "mcp", len(push.MCP), "dir", configDir)
	return nil
}

// linkHostCredentials exposes a pre-existing on-host credentials file inside the
// materialized config directory.
//
// A symlink rather than a copy, deliberately: subscription credentials are
// refreshed in place by the harness itself, and a copy would go stale the first
// time a token rotated — failing much later as an unexplained "not logged in".
// It is re-created on every materialization because the config directory is
// rebuilt wholesale on each bundle push.
func (r *Runner) linkHostCredentials(root string) error {
	src := r.opts.HarnessCredentials
	if src == "" {
		return nil
	}
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("runner: harness credentials %q: %w", src, err)
	}
	dest := filepath.Join(root, ".credentials.json")
	_ = os.Remove(dest)
	if err := os.Symlink(src, dest); err != nil {
		return fmt.Errorf("runner: link harness credentials: %w", err)
	}
	return nil
}

// assembleMemory concatenates memory/*.md into CLAUDE.md, base layer first.
//
// With a durable notes file (localPath non-empty), a section pointing at it —
// and its contents, if any — follows the bundle's memory. The pointer is
// there even when the file does not exist yet, because the agent can only keep
// a note somewhere it has been told about.
func assembleMemory(root, localPath string) error {
	dir := filepath.Join(root, bundleMemoryDir)
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var b strings.Builder
	for _, n := range names {
		content, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return err
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.Write(content)
	}

	if localPath != "" {
		local, err := os.ReadFile(localPath)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "%s\n\nEverything above is rebuilt from the deployment bundle, so edits to this file are lost. "+
			"To keep a note across restarts and redeploys, add it to `%s`; it is appended here at the start of every session.\n",
			durableNotesHeader, localPath)
		if len(strings.TrimSpace(string(local))) > 0 {
			b.WriteString("\n")
			b.Write(local)
		}
	}

	if b.Len() == 0 {
		return nil
	}
	return writeFileAtomic(filepath.Join(root, assembledMemory), []byte(b.String()), 0o600)
}

// writeMCPConfig assembles the final server set.
//
// The runner owns assembly: the bundle contributes servers, and the runner adds
// its own control-plane servers on top. Proxied servers become shim commands
// that speak back over the unix socket, so the runner never holds their
// credentials.
func (r *Runner) writeMCPConfig(path string, servers []protocol.MCPServer) error {
	socket := SocketPath(r.opts.RuntimeRoot, r.opts.Name)
	out := map[string]any{}

	for _, s := range servers {
		switch s.Kind {
		case protocol.MCPLocal:
			entry := map[string]any{"command": s.Command}
			if len(s.Args) > 0 {
				entry["args"] = s.Args
			}
			if len(s.Env) > 0 {
				entry["env"] = s.Env
			}
			out[s.Name] = entry
		case protocol.MCPProxied:
			out[s.Name] = map[string]any{
				"command": r.opts.SelfPath,
				"args":    []string{"mcp-shim", "--socket", socket, "--server", s.Name},
			}
		}
	}

	// The permission-prompt server is always present: it is how every tool
	// decision reaches the gateway.
	out["splitscreen"] = map[string]any{
		"command": r.opts.SelfPath,
		"args":    []string{"permission-shim", "--socket", socket},
	}

	body, err := json.MarshalIndent(map[string]any{"mcpServers": out}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

func safeRelPath(p string) error {
	if p == "" {
		return fmt.Errorf("empty path")
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) {
		return fmt.Errorf("must be a relative slash-separated path")
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("escapes the bundle root")
	}
	return nil
}
