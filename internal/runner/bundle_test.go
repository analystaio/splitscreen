package runner

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/analystaio/splitscreen/protocol"
)

func testRunner(t *testing.T) *Runner {
	t.Helper()
	root := t.TempDir()
	return &Runner{
		opts: Options{
			Name:        "alpha",
			RuntimeRoot: root,
			SelfPath:    "/usr/bin/splitscreen",
			Cwd:         t.TempDir(),
		},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestSafeRelPath(t *testing.T) {
	// The gateway validates these too. Repeating the check here is the point:
	// a guarantee that depends on one side of a connection is not a guarantee.
	for _, bad := range []string{"", "/etc/passwd", "../escape", "a/../../b", `win\path`} {
		if err := safeRelPath(bad); err == nil {
			t.Errorf("path %q was accepted", bad)
		}
	}
	for _, ok := range []string{"CLAUDE.md", "skills/deploy/SKILL.md", "memory/base.md"} {
		if err := safeRelPath(ok); err != nil {
			t.Errorf("path %q was rejected: %v", ok, err)
		}
	}
}

func TestApplyBundleMaterializes(t *testing.T) {
	r := testRunner(t)

	push := &protocol.BundlePush{
		Version: 3,
		Digest:  "sha256:abc",
		Files: []protocol.BundleFile{
			{Path: "memory/00-base.md", Content: []byte("base rules"), Mode: 0o600},
			{Path: "memory/10-runner.md", Content: []byte("runner rules"), Mode: 0o600},
			{Path: "skills/deploy/SKILL.md", Content: []byte("skill"), Mode: 0o600},
		},
		MCP: []protocol.MCPServer{
			{Name: "fs", Kind: protocol.MCPLocal, Command: "/usr/bin/mcp-fs"},
			{Name: "jira", Kind: protocol.MCPProxied},
		},
		Secrets: map[string]string{"ANTHROPIC_API_KEY": "sk-test"},
	}
	if err := r.applyBundle(push); err != nil {
		t.Fatalf("apply: %v", err)
	}

	dir := r.bundle.ConfigDir()
	if r.bundle.Version() != 3 || r.bundle.Digest() != "sha256:abc" {
		t.Fatalf("bundle state = v%d %s", r.bundle.Version(), r.bundle.Digest())
	}

	// Memory layers are concatenated base-first into the harness's memory file.
	memory, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(memory), "base rules") || !strings.Contains(string(memory), "runner rules") {
		t.Fatalf("assembled memory = %q", memory)
	}
	if strings.Index(string(memory), "base rules") > strings.Index(string(memory), "runner rules") {
		t.Error("the base layer should come first")
	}

	// Files land at 0600: a shared box should not expose operator guidance.
	info, err := os.Stat(filepath.Join(dir, "skills", "deploy", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("skill mode = %o", info.Mode().Perm())
	}

	// Secrets stay in memory, never on disk.
	if got := r.bundle.Secrets()["ANTHROPIC_API_KEY"]; got != "sk-test" {
		t.Errorf("secret = %q", got)
	}
	walkAssertNoSecret(t, dir, "sk-test")
}

func walkAssertNoSecret(t *testing.T, root, secret string) {
	t.Helper()
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		body, rerr := os.ReadFile(p)
		if rerr == nil && strings.Contains(string(body), secret) {
			t.Errorf("secret value was written to %s", p)
		}
		return nil
	})
}

func TestMCPConfigSplitsLocalFromProxied(t *testing.T) {
	r := testRunner(t)
	push := &protocol.BundlePush{
		Version: 1, Digest: "d",
		MCP: []protocol.MCPServer{
			{Name: "fs", Kind: protocol.MCPLocal, Command: "/usr/bin/mcp-fs", Args: []string{"--root", "/srv"}},
			{Name: "jira", Kind: protocol.MCPProxied},
		},
	}
	if err := r.applyBundle(push); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(r.bundle.MCPPath())
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Servers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}

	if doc.Servers["fs"].Command != "/usr/bin/mcp-fs" {
		t.Errorf("local server = %+v", doc.Servers["fs"])
	}
	// A proxied server becomes a shim: the runner never executes it and never
	// holds its credential.
	jira := doc.Servers["jira"]
	if jira.Command != "/usr/bin/splitscreen" || jira.Args[0] != "mcp-shim" {
		t.Errorf("proxied server = %+v", jira)
	}
	// The permission server is always present; it is how every tool decision
	// reaches the gateway.
	perm := doc.Servers["splitscreen"]
	if perm.Args[0] != "permission-shim" {
		t.Fatalf("permission server = %+v", perm)
	}
}

func TestThreadMCPCarriesTheThread(t *testing.T) {
	r := testRunner(t)
	if err := r.applyBundle(&protocol.BundlePush{
		Version: 1, Digest: "d",
		MCP: []protocol.MCPServer{{Name: "jira", Kind: protocol.MCPProxied}},
	}); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "mcp.json")
	if err := r.writeThreadMCP(out, "slack:C1:T1"); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(out)
	if !strings.Contains(string(body), "slack:C1:T1") {
		t.Fatalf("thread id missing from shim args:\n%s", body)
	}
	// Only shims get the thread; a local server's argv must be untouched.
	var doc struct {
		Servers map[string]struct {
			Args []string `json:"args"`
		} `json:"mcpServers"`
	}
	_ = json.Unmarshal(body, &doc)
	for _, a := range doc.Servers["splitscreen"].Args {
		if a == "--thread" {
			return
		}
	}
	t.Fatal("the permission shim did not receive a thread")
}

func TestApplyBundleRejectsEscapingPaths(t *testing.T) {
	r := testRunner(t)
	err := r.applyBundle(&protocol.BundlePush{
		Version: 1, Digest: "d",
		Files: []protocol.BundleFile{{Path: "../../escaped", Content: []byte("x")}},
	})
	if err == nil {
		t.Fatal("a path escaping the bundle root was accepted")
	}
}

func TestBuildEnvIsAnAllowlist(t *testing.T) {
	r := testRunner(t)
	// A denylist would let this through; an allowlist cannot.
	t.Setenv("CLAUDE_CODE_SOMETHING_NEW", "leaked")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "leaked")
	t.Setenv("PATH", "/usr/bin")

	r.bundle.mu.Lock()
	r.bundle.secrets = map[string]string{"ANTHROPIC_API_KEY": "sk-test"}
	r.bundle.mu.Unlock()

	env := r.buildEnv("/run/splitscreen/alpha/config")
	joined := strings.Join(env, "\n")

	if strings.Contains(joined, "leaked") {
		t.Errorf("an unlisted variable reached the harness environment:\n%s", joined)
	}
	if !strings.Contains(joined, "PATH=/usr/bin") {
		t.Error("PATH should be passed through")
	}
	if !strings.Contains(joined, "ANTHROPIC_API_KEY=sk-test") {
		t.Error("the harness credential should be injected")
	}
	if !strings.Contains(joined, "CLAUDE_CONFIG_DIR=/run/splitscreen/alpha/config") {
		t.Error("the config dir should be set")
	}
}

// Subscription-authenticated harnesses keep credentials on disk and refresh them
// in place. The gateway ships nothing, so the runner must expose the existing
// file inside the config directory it points the harness at — otherwise the
// harness reports "not logged in" with no clue why.
func TestHostCredentialsAreLinked(t *testing.T) {
	r := testRunner(t)
	credDir := t.TempDir()
	cred := filepath.Join(credDir, ".credentials.json")
	if err := os.WriteFile(cred, []byte(`{"token":"original"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r.opts.HarnessCredentials = cred

	if err := r.applyBundle(&protocol.BundlePush{Version: 1, Digest: "d"}); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(r.bundle.ConfigDir(), ".credentials.json")
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("credentials were not exposed: %v", err)
	}
	// A symlink, not a copy: the harness rotates this file, and a copy would go
	// stale and fail much later as an unexplained auth error.
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("credentials were copied rather than linked")
	}

	if err := os.WriteFile(cred, []byte(`{"token":"rotated"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "rotated") {
		t.Fatalf("a rotation did not propagate: %s", got)
	}

	// The config directory is rebuilt wholesale on every push, so the link has to
	// be re-created each time.
	if err := r.applyBundle(&protocol.BundlePush{Version: 2, Digest: "d2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(r.bundle.ConfigDir(), ".credentials.json")); err != nil {
		t.Fatalf("the link did not survive a second bundle push: %v", err)
	}
}

func TestMissingHostCredentialsIsAnError(t *testing.T) {
	r := testRunner(t)
	r.opts.HarnessCredentials = "/nonexistent/.credentials.json"
	// Fail loudly at materialization rather than letting the harness start and
	// report a confusing login error on the first real request.
	if err := r.applyBundle(&protocol.BundlePush{Version: 1, Digest: "d"}); err == nil {
		t.Fatal("a missing credentials file was accepted")
	}
}
