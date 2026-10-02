package runner

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/analystaio/splitscreen/protocol"
)

// A marketplace in a real git repository: tag v1 has plugins a and b, tag v2
// changes a and drops b. Fetching uses the same code as production, against a
// local path instead of the forge.
func marketplaceRepo(t *testing.T) (base, repo string) {
	t.Helper()
	base = t.TempDir() + "/"
	repo = "acme/plugins"
	dir := filepath.Join(base, repo)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git("init", "-q", "-b", "main")
	write(".claude-plugin/marketplace.json", `{"name":"acme","owner":{"name":"a"},"plugins":[
		{"name":"a","source":"./plugins/a"},{"name":"b","source":"./plugins/b"},
		{"name":"escape","source":"../../etc"},{"name":"remote","source":{"source":"github","repo":"x/y"}}]}`)
	write("plugins/a/skills/s/SKILL.md", "v1")
	write("plugins/b/skills/s/SKILL.md", "b")
	git("add", "-A")
	git("commit", "-q", "-m", "v1")
	git("tag", "v1")
	write("plugins/a/skills/s/SKILL.md", "v2")
	git("rm", "-q", "-r", "plugins/b")
	git("commit", "-q", "-am", "v2")
	git("tag", "v2")
	return base, repo
}

func pluginRunner(t *testing.T, base string) (*Runner, *[]string) {
	t.Helper()
	old := gitBase
	gitBase = base
	t.Cleanup(func() { gitBase = old })
	var asked []string
	r := &Runner{
		opts: Options{Name: "r1", RuntimeRoot: t.TempDir(), StateDir: t.TempDir()},
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		credentialHook: func(_ context.Context, repo string) (CredentialResult, error) {
			asked = append(asked, repo)
			return CredentialResult{}, nil
		},
	}
	return r, &asked
}

func syncWith(t *testing.T, r *Runner, plugins []string, ref string) []string {
	t.Helper()
	cfgDir := t.TempDir()
	m := `{"plugins":["` + strings.Join(plugins, `","`) + `"],"marketplaces":{"acme":{"repo":"acme/plugins","ref":"` + ref + `"}}}`
	if err := os.WriteFile(filepath.Join(cfgDir, protocol.PluginManifestFile), []byte(m), 0o600); err != nil {
		t.Fatal(err)
	}
	st := r.plugins.reset(true)
	r.syncPlugins(context.Background(), cfgDir, st)
	dirs, err := r.plugins.wait(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

func skillText(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "skills", "s", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPluginsLoadAtThePinnedRef(t *testing.T) {
	base, _ := marketplaceRepo(t)
	r, asked := pluginRunner(t, base)

	dirs := syncWith(t, r, []string{"a@acme", "b@acme"}, "v1")
	if len(dirs) != 2 || skillText(t, dirs[0]) != "v1" || skillText(t, dirs[1]) != "b" {
		t.Fatalf("v1 dirs = %v", dirs)
	}
	if len(*asked) != 1 || (*asked)[0] != "acme/plugins" {
		t.Fatalf("credential requests = %v, want one for acme/plugins", *asked)
	}
	// The checkout lives in the state dir, so it survives a restart.
	if !strings.HasPrefix(dirs[0], r.opts.StateDir) {
		t.Errorf("checkout %s is not under the state dir", dirs[0])
	}

	// Moving the ref moves the plugins; b no longer exists at v2 and is skipped.
	dirs = syncWith(t, r, []string{"a@acme", "b@acme"}, "v2")
	if len(dirs) != 1 || skillText(t, dirs[0]) != "v2" {
		t.Fatalf("v2 dirs = %v", dirs)
	}
}

// Only relative sources inside the checkout load: nothing that escapes it, and
// nothing fetched from elsewhere at an unpinned version.
func TestPluginSourcesMustStayInTheCheckout(t *testing.T) {
	base, _ := marketplaceRepo(t)
	r, _ := pluginRunner(t, base)
	if dirs := syncWith(t, r, []string{"escape@acme", "remote@acme", "missing@acme"}, "v1"); len(dirs) != 0 {
		t.Fatalf("dirs = %v, want none", dirs)
	}
}

// With the forge unreachable, a checkout of the same ref is reused; a
// different ref is not silently substituted.
func TestPluginFetchFailureReusesOnlyTheSameRef(t *testing.T) {
	base, _ := marketplaceRepo(t)
	r, _ := pluginRunner(t, base)
	if dirs := syncWith(t, r, []string{"a@acme"}, "v1"); len(dirs) != 1 {
		t.Fatalf("dirs = %v", dirs)
	}
	gitBase = t.TempDir() + "/" // the forge is gone
	if dirs := syncWith(t, r, []string{"a@acme"}, "v1"); len(dirs) != 1 || skillText(t, dirs[0]) != "v1" {
		t.Fatalf("same ref offline: dirs = %v", dirs)
	}
	if dirs := syncWith(t, r, []string{"a@acme"}, "v2"); len(dirs) != 0 {
		t.Fatalf("different ref offline must load nothing, got %v", dirs)
	}
}

func TestNoManifestMeansNoPlugins(t *testing.T) {
	r, asked := pluginRunner(t, t.TempDir()+"/")
	st := r.plugins.reset(true)
	r.syncPlugins(context.Background(), t.TempDir(), st)
	dirs, err := r.plugins.wait(context.Background(), time.Second)
	if err != nil || dirs != nil || len(*asked) != 0 {
		t.Fatalf("dirs=%v err=%v asked=%v", dirs, err, *asked)
	}
}

// A session that starts mid-sync waits for it, but not forever.
func TestPluginWaitIsBounded(t *testing.T) {
	var p pluginSet
	p.reset(true) // never completes
	if _, err := p.wait(context.Background(), 20*time.Millisecond); err == nil {
		t.Fatal("wait on an unfinished sync should time out")
	}
	p.reset(false)
	if dirs, err := p.wait(context.Background(), time.Second); err != nil || dirs != nil {
		t.Fatalf("finished empty sync: dirs=%v err=%v", dirs, err)
	}
}
