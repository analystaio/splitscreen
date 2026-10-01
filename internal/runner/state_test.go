package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/analystaio/splitscreen/protocol"
)

func statefulRunner(t *testing.T) *Runner {
	t.Helper()
	r := testRunner(t)
	r.opts.StateDir = filepath.Join(t.TempDir(), "state")
	idx, err := loadSessionIndex(r.opts.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	r.sessionIDs = idx
	return r
}

func push(version int, files ...protocol.BundleFile) *protocol.BundlePush {
	return &protocol.BundlePush{Version: version, Digest: "sha256:x", Files: files}
}

func bf(path, content string) protocol.BundleFile {
	return protocol.BundleFile{Path: path, Content: []byte(content), Mode: 0o600}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestStateSurvivesBundlePush(t *testing.T) {
	r := statefulRunner(t)
	state := r.opts.StateDir

	if err := r.applyBundle(push(1,
		bf("memory/00-base.md", "base rules"),
		bf("skills/deploy/SKILL.md", "deploy v1"),
		bf("skills/retired/SKILL.md", "old"),
	)); err != nil {
		t.Fatal(err)
	}
	cfg := r.bundle.ConfigDir()

	// The harness writes memory and a transcript under projects/, creates a
	// skill of its own, and edits a bundle skill in place.
	mem := filepath.Join(cfg, "projects", "-var-www-app", "memory", "MEMORY.md")
	if err := os.MkdirAll(filepath.Dir(mem), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mem, []byte("learned a thing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg, "skills", "mine"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "skills", "mine", "SKILL.md"), []byte("runtime skill"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "skills", "deploy", "SKILL.md"), []byte("edited at runtime"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Next push: deploy changes, retired is dropped.
	if err := r.applyBundle(push(2,
		bf("memory/00-base.md", "base rules v2"),
		bf("skills/deploy/SKILL.md", "deploy v2"),
	)); err != nil {
		t.Fatal(err)
	}
	cfg = r.bundle.ConfigDir()

	if got := mustRead(t, filepath.Join(cfg, "projects", "-var-www-app", "memory", "MEMORY.md")); got != "learned a thing" {
		t.Errorf("project memory = %q; it should survive a push", got)
	}
	if got := mustRead(t, filepath.Join(cfg, "skills", "mine", "SKILL.md")); got != "runtime skill" {
		t.Errorf("runtime skill = %q; it should survive a push", got)
	}
	if got := mustRead(t, filepath.Join(cfg, "skills", "deploy", "SKILL.md")); got != "deploy v2" {
		t.Errorf("bundle skill = %q; the bundle must win", got)
	}
	if exists(filepath.Join(cfg, "skills", "retired")) {
		t.Error("a skill the bundle stopped shipping is still installed")
	}
	// All of it lives on the persistent side.
	if !exists(filepath.Join(state, "skills", "mine", "SKILL.md")) ||
		!exists(filepath.Join(state, "projects", "-var-www-app", "memory", "MEMORY.md")) {
		t.Error("state was not written through to the state dir")
	}
	// Rotating the old config dir away must not have followed the links.
	if !exists(filepath.Join(state, "projects")) {
		t.Error("removing the old config dir deleted the state it linked to")
	}
}

func TestDurableNotesAppendedAfterBundleMemory(t *testing.T) {
	r := statefulRunner(t)
	local := filepath.Join(r.opts.StateDir, "CLAUDE.local.md")

	if err := r.applyBundle(push(1, bf("memory/00-base.md", "base rules"))); err != nil {
		t.Fatal(err)
	}
	cfg := r.bundle.ConfigDir()
	md := mustRead(t, filepath.Join(cfg, "CLAUDE.md"))
	if !strings.HasPrefix(md, "base rules") || !strings.Contains(md, local) {
		t.Fatalf("CLAUDE.md should carry the bundle memory, then point at the notes file:\n%s", md)
	}

	// A note added between pushes reaches the next session.
	if err := os.WriteFile(local, []byte("the staging DB is slow on Mondays"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.refreshMemory(cfg); err != nil {
		t.Fatal(err)
	}
	md = mustRead(t, filepath.Join(cfg, "CLAUDE.md"))
	base := strings.Index(md, "base rules")
	note := strings.Index(md, "slow on Mondays")
	if base != 0 || note <= base {
		t.Fatalf("notes should follow the bundle memory:\n%s", md)
	}
}

func TestNoStateDirKeepsRuntimeEphemeral(t *testing.T) {
	r := testRunner(t)
	if err := r.applyBundle(push(1, bf("memory/00-base.md", "base"), bf("skills/s/SKILL.md", "x"))); err != nil {
		t.Fatal(err)
	}
	cfg := r.bundle.ConfigDir()
	for _, name := range []string{"projects", "skills"} {
		if fi, err := os.Lstat(filepath.Join(cfg, name)); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			t.Errorf("%s is a symlink without a state dir", name)
		}
	}
	if md := mustRead(t, filepath.Join(cfg, "CLAUDE.md")); md != "base" {
		t.Errorf("CLAUDE.md = %q; nothing should be appended without a state dir", md)
	}
}

func TestSessionIDsPersistAcrossRestart(t *testing.T) {
	r := statefulRunner(t)
	r.rememberSession("slack:C1:T1", "sess-1")
	r.rememberSession("slack:C1:T2", "sess-2")
	r.rememberSession("slack:C1:T2", "") // !new

	idx, err := loadSessionIndex(r.opts.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := idx.get("slack:C1:T1"); got != "sess-1" {
		t.Errorf("T1 resume id = %q after reload", got)
	}
	if got := idx.get("slack:C1:T2"); got != "" {
		t.Errorf("T2 resume id = %q; !new should have cleared it", got)
	}

	// Without a state dir nothing is written anywhere.
	eph, _ := loadSessionIndex("")
	if err := eph.set("t", "s"); err != nil {
		t.Fatal(err)
	}
}

// Switching an existing tmpfs runner to a state dir keeps what it had.
func TestStateAdoptsEphemeralProjects(t *testing.T) {
	r := testRunner(t)
	if err := r.applyBundle(push(1, bf("memory/00-base.md", "base"))); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(r.bundle.ConfigDir(), "projects", "p", "abc.jsonl")
	if err := os.MkdirAll(filepath.Dir(old), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("transcript"), 0o600); err != nil {
		t.Fatal(err)
	}

	r.opts.StateDir = filepath.Join(t.TempDir(), "state")
	if err := r.applyBundle(push(2, bf("memory/00-base.md", "base"))); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, filepath.Join(r.bundle.ConfigDir(), "projects", "p", "abc.jsonl")); got != "transcript" {
		t.Errorf("transcript = %q after switching to a state dir", got)
	}
}
