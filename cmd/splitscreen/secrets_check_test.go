package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avarant/splitscreen/config"
	"github.com/avarant/splitscreen/internal/secrets"
)

const secretsConfig = `
gateway:
  secrets_dir: SECRETS
runners:
  alpha:
    display: { name: "Alpha" }
    cwd: /srv/alpha
    harness: claude-code
    harness_secret: claude-token
  box-template:
    display: { name: "Box" }
    cwd: /srv/box
    harness: claude-code
    harness_secret: claude-token
routes:
  - { channel: C1, runner: alpha }
`

func secretsFixture(t *testing.T, present ...string) (cfgPath string, cfg *config.Config, sec secrets.Backend) {
	t.Helper()
	dir := t.TempDir()
	sdir := filepath.Join(dir, "secrets")
	if err := os.Mkdir(sdir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range present {
		if err := os.WriteFile(filepath.Join(sdir, name), []byte("v"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath = filepath.Join(dir, "splitscreen.yaml")
	if err := os.WriteFile(cfgPath, []byte(strings.Replace(secretsConfig, "SECRETS", sdir, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	d, err := secrets.NewDirBackend(sdir)
	if err != nil {
		t.Fatal(err)
	}
	return cfgPath, cfg, secrets.Chain{d}
}

// A template (or any runner) with no enrollment token is a runner that cannot
// connect — not a gateway that cannot start. One missing box token took every
// runner down on 2026-09-28.
func TestMissingRunnerTokenDoesNotBlockStartup(t *testing.T) {
	_, cfg, sec := secretsFixture(t, "runner-alpha", "claude-token")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := verifySecrets(cfg, sec, log); err != nil {
		t.Fatalf("startup refused over an unenrolled runner: %v", err)
	}
	rep := checkSecrets(cfg, sec)
	if rep.Unenrolled["box-template"] != "runner-box-template" || len(rep.Fatal) != 0 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestMissingSharedSecretStillBlocksStartup(t *testing.T) {
	_, cfg, sec := secretsFixture(t, "runner-alpha", "runner-box-template")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := verifySecrets(cfg, sec, log)
	if err == nil || !strings.Contains(err.Error(), "claude-token") {
		t.Fatalf("a missing harness secret must stay fatal, got %v", err)
	}
}

// config check --resolve fails exactly when startup would, so a deploy can
// pre-flight a restart instead of discovering the problem by crash-looping.
func TestConfigCheckResolveMatchesStartup(t *testing.T) {
	path, _, _ := secretsFixture(t, "runner-alpha", "claude-token")
	if err := runCmd(t, "config", "check", "--resolve", "-c", path); err != nil {
		t.Fatalf("--resolve failed over an unenrolled runner: %v", err)
	}

	path, _, _ = secretsFixture(t, "runner-alpha", "runner-box-template")
	err := runCmd(t, "config", "check", "--resolve", "-c", path)
	if err == nil || !strings.Contains(err.Error(), "claude-token") {
		t.Fatalf("--resolve should fail on a missing harness secret, got %v", err)
	}
	// Without --resolve it is a syntax/semantics check only, as before.
	if err := runCmd(t, "config", "check", "-c", path); err != nil {
		t.Fatalf("plain check: %v", err)
	}
}
