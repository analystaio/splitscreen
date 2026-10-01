package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/analystaio/splitscreen/config"
)

const baseConfig = `# Splitscreen configuration.
#
# This comment must survive an edit — it carries most of the file's explanation.
gateway:
  listen: 127.0.0.1:8443

runners:
  alpha:
    display: { name: "Alpha" }   # persona shown on every message
    cwd: /srv/alpha
    harness: claude-code
  beta:
    display: { name: "Beta" }
    cwd: /srv/beta
    harness: claude-code

routes:
  - { channel: C111, runner: alpha }
  - { dm: true, runner: beta }
`

func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "splitscreen.yaml")
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	return path
}

func runCmd(t *testing.T, args ...string) error {
	t.Helper()
	root := rootCommand()
	root.SetArgs(args)
	// io.Discard, not os.NewFile(0, ...): that wraps fd 0, and its finalizer
	// closes whatever fd 0 has become by the time it runs.
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	return root.Execute()
}

func TestRouteAddPreservesComments(t *testing.T) {
	path := writeConfigFile(t, baseConfig)

	if err := runCmd(t, "route", "add", "C222", "beta", "-c", path); err != nil {
		t.Fatalf("route add: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Editing the node tree rather than round-tripping structs is what keeps
	// the file readable; losing comments would make the tool unusable on a real
	// config nobody wants to re-annotate.
	if !strings.Contains(string(body), "must survive an edit") {
		t.Errorf("top-level comment was lost:\n%s", body)
	}
	if !strings.Contains(string(body), "persona shown on every message") {
		t.Errorf("inline comment was lost:\n%s", body)
	}

	cfg, err := config.Parse(body)
	if err != nil {
		t.Fatalf("result does not parse: %v", err)
	}
	if r, ok := cfg.RunnerFor("C222", false); !ok || r != "beta" {
		t.Fatalf("new route resolves to %q,%v", r, ok)
	}
	if r, ok := cfg.RunnerFor("C111", false); !ok || r != "alpha" {
		t.Fatalf("existing route broke: %q,%v", r, ok)
	}
}

func TestRouteAddPreservesFileMode(t *testing.T) {
	path := writeConfigFile(t, baseConfig)
	if err := runCmd(t, "route", "add", "C222", "beta", "-c", path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The atomic replace must not widen permissions on a file that may sit
	// beside secrets.
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o, want 640", info.Mode().Perm())
	}
}

func TestRouteAddRejectsDuplicateChannel(t *testing.T) {
	path := writeConfigFile(t, baseConfig)
	before, _ := os.ReadFile(path)

	err := runCmd(t, "route", "add", "C111", "beta", "-c", path)
	if err == nil {
		t.Fatal("a second claim on one channel was accepted")
	}
	if !strings.Contains(err.Error(), "exactly one runner") {
		t.Errorf("error = %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("a rejected edit modified the file")
	}
}

func TestRouteAddRejectsUnknownRunner(t *testing.T) {
	path := writeConfigFile(t, baseConfig)
	err := runCmd(t, "route", "add", "C222", "ghost", "-c", path)
	if err == nil || !strings.Contains(err.Error(), "no runner named") {
		t.Fatalf("error = %v", err)
	}
}

func TestRouteAddRejectsSecondDMRoute(t *testing.T) {
	path := writeConfigFile(t, baseConfig)
	err := runCmd(t, "route", "add", "alpha", "--dm", "-c", path)
	if err == nil || !strings.Contains(err.Error(), "one DM surface") {
		t.Fatalf("error = %v", err)
	}
}

// A config that is already broken must be reported as such, rather than having
// an edit layered on top of it.
func TestRouteAddRefusesToEditABrokenConfig(t *testing.T) {
	path := writeConfigFile(t, "runners:\n  alpha: { cwd: relative, harness: h }\n")
	err := runCmd(t, "route", "add", "C222", "alpha", "-c", path)
	if err == nil || !strings.Contains(err.Error(), "not valid") {
		t.Fatalf("error = %v", err)
	}
}

func TestRouteRemove(t *testing.T) {
	path := writeConfigFile(t, baseConfig)
	if err := runCmd(t, "route", "remove", "C111", "-c", path); err != nil {
		t.Fatalf("route remove: %v", err)
	}
	body, _ := os.ReadFile(path)
	cfg, err := config.Parse(body)
	if err != nil {
		t.Fatalf("result does not parse: %v", err)
	}
	if _, ok := cfg.RunnerFor("C111", false); ok {
		t.Error("the route was not removed")
	}
	// beta still has the DM route, so it is not orphaned; alpha now is, which
	// the validator reports rather than silently accepting.
	if _, ok := cfg.RunnerFor("", true); !ok {
		t.Error("the DM route was collateral damage")
	}
}

func TestRouteRemoveUnknownChannel(t *testing.T) {
	path := writeConfigFile(t, baseConfig)
	err := runCmd(t, "route", "remove", "C999", "-c", path)
	if err == nil || !strings.Contains(err.Error(), "no route for channel") {
		t.Fatalf("error = %v", err)
	}
}

func TestEnrollWriteStoresTheGatewayHalf(t *testing.T) {
	dir := t.TempDir()
	secretsDir := filepath.Join(dir, "secrets")
	if err := os.Mkdir(secretsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "splitscreen.yaml")
	body := strings.Replace(baseConfig, "  listen: 127.0.0.1:8443",
		"  listen: 127.0.0.1:8443\n  secrets_dir: "+secretsDir, 1)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := runCmd(t, "enroll", "alpha", "--write", "-c", path); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	dest := filepath.Join(secretsDir, "runner-alpha")
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("secret was not written: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
	token, _ := os.ReadFile(dest)
	if len(token) < 32 {
		t.Errorf("token looks too short: %q", token)
	}
	// No trailing newline: the gateway compares bytes, and a stray newline is
	// the classic cause of an opaque authentication failure.
	if strings.ContainsAny(string(token), "\r\n") {
		t.Error("the stored token contains a newline")
	}

	// Re-enrolling must not silently invalidate the runner that holds the old
	// token.
	err = runCmd(t, "enroll", "alpha", "--write", "-c", path)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("overwrite was not guarded: %v", err)
	}
	if err := runCmd(t, "enroll", "alpha", "--write", "--force", "-c", path); err != nil {
		t.Fatalf("forced enroll: %v", err)
	}
	replaced, _ := os.ReadFile(dest)
	if string(replaced) == string(token) {
		t.Error("--force did not mint a new token")
	}
}

func TestEnrollWriteRequiresAConfiguredRunner(t *testing.T) {
	dir := t.TempDir()
	secretsDir := filepath.Join(dir, "secrets")
	_ = os.Mkdir(secretsDir, 0o700)
	path := filepath.Join(dir, "splitscreen.yaml")
	body := strings.Replace(baseConfig, "  listen: 127.0.0.1:8443",
		"  listen: 127.0.0.1:8443\n  secrets_dir: "+secretsDir, 1)
	_ = os.WriteFile(path, []byte(body), 0o600)

	// The gateway rejects a hello from an unconfigured runner, so enrolling one
	// only sets up a confusing failure later.
	err := runCmd(t, "enroll", "ghost", "--write", "-c", path)
	if err == nil || !strings.Contains(err.Error(), "no runner named") {
		t.Fatalf("error = %v", err)
	}
}
