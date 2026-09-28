package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/avarant/splitscreen/config"
)

const templateConfig = `# Boxes are copies of box-template.
gateway:
  listen: 127.0.0.1:8443
  secrets_dir: SECRETS

runners:
  # Explains the template; must not be copied into every box.
  box-template:
    display: { name: "Box", icon: ":package:" }
    host: template
    cwd: /var/www/ksdm.box
    harness: claude-code
    token_secret: runner-box-template
    idle: 45m
    wake: { region: us-east-2 }
    policy:
      auto_approve: true
      deny: ["Bash(terraform apply*)"]
  staging:
    display: { name: "Staging" }
    cwd: /srv/staging
    harness: claude-code

routes:
  - { channel: C111, runner: staging }
`

func writeTemplateConfig(t *testing.T) (cfgPath, secretsDir string) {
	t.Helper()
	dir := t.TempDir()
	secretsDir = filepath.Join(dir, "secrets")
	if err := os.Mkdir(secretsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath = filepath.Join(dir, "splitscreen.yaml")
	body := strings.Replace(templateConfig, "SECRETS", secretsDir, 1)
	if err := os.WriteFile(cfgPath, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	return cfgPath, secretsDir
}

func loadConfig(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config no longer loads: %v", err)
	}
	return cfg
}

func TestRunnerAddCopiesTemplateAndAppliesOverrides(t *testing.T) {
	path, _ := writeTemplateConfig(t)

	err := runCmd(t, "runner", "add", "box-foo", "--template", "box-template",
		"--set", "host=i-0123456789abcdef0",
		"--set", "display.name=Box foo",
		"--set", "max_concurrent=3",
		"--set", "wake.ec2_instance=i-0123456789abcdef0",
		"--set", "wake.region=us-east-2",
		"-c", path)
	if err != nil {
		t.Fatalf("runner add: %v", err)
	}

	cfg := loadConfig(t, path)
	r, ok := cfg.Runners["box-foo"]
	if !ok {
		t.Fatal("box-foo was not added")
	}
	if r.Host != "i-0123456789abcdef0" || r.Display.Name != "Box foo" || r.Display.Icon != ":package:" {
		t.Errorf("overrides or inherited display wrong: %+v", r)
	}
	if r.MaxConcurrent != 3 {
		t.Errorf("max_concurrent = %d, want 3 (typed as a number)", r.MaxConcurrent)
	}
	if r.Idle.String() != "45m0s" || !r.Policy.AutoApprove || len(r.Policy.Deny) != 1 {
		t.Errorf("template settings not inherited: %+v", r)
	}
	if !r.Wakeable() || r.Wake.Region != "us-east-2" {
		t.Errorf("wake not set: %+v", r.Wake)
	}
	// The template's own token secret must never be shared with a copy.
	if got := r.EffectiveTokenSecret("box-foo"); got != "runner-box-foo" {
		t.Errorf("token secret = %q, want runner-box-foo", got)
	}

	body, _ := os.ReadFile(path)
	if strings.Count(string(body), "Explains the template") != 1 {
		t.Errorf("template comment was duplicated or lost:\n%s", body)
	}
	if !strings.Contains(string(body), "Boxes are copies of box-template") {
		t.Errorf("file comment lost:\n%s", body)
	}
}

func TestRunnerAddRefusesExistingAndInvalid(t *testing.T) {
	path, _ := writeTemplateConfig(t)
	before, _ := os.ReadFile(path)

	cases := [][]string{
		{"runner", "add", "staging", "--template", "box-template"},                                                             // name taken
		{"runner", "add", "box-x", "--template", "nope"},                                                                       // unknown template
		{"runner", "add", "box-x", "--template", "box-template", "--set", "host=i-0123456789abcdef0", "--set", "cwd=relative"}, // fails validation
		{"runner", "add", "box-x", "--template", "box-template", "--set", "host=i-0123456789abcdef0", "--set", "wake.ec2_instance=bogus"},
		{"runner", "add", "box-x", "--template", "box-template", "--set", "host=i-0123456789abcdef0", "--set", "policy.deny=x"},  // not a scalar
		{"runner", "add", "box-x", "--template", "box-template", "--set", "host=i-0123456789abcdef0", "--set", "nonsense_key=1"}, // unknown key
		{"runner", "add", "Box_X", "--template", "box-template"},                                                                 // bad slug
	}
	for _, args := range cases {
		if err := runCmd(t, append(args, "-c", path)...); err == nil {
			t.Errorf("%v: expected an error", args)
		}
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Errorf("a refused edit modified the file:\n%s", after)
	}
}

func TestRunnerAddRefusesShadowingSecretFile(t *testing.T) {
	path, secrets := writeTemplateConfig(t)
	stale := filepath.Join(secrets, "runner-box-foo")
	if err := os.WriteFile(stale, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runCmd(t, "runner", "add", "box-foo", "--template", "box-template", "--set", "host=i-0123456789abcdef0", "-c", path)
	if err == nil || !strings.Contains(err.Error(), "shadow") {
		t.Fatalf("expected a shadowing error, got %v", err)
	}
	if _, ok := loadConfig(t, path).Runners["box-foo"]; ok {
		t.Error("runner was added despite the error")
	}
}

func TestRunnerRemoveDropsRoutesAndSecret(t *testing.T) {
	path, secrets := writeTemplateConfig(t)
	if err := runCmd(t, "runner", "add", "box-foo", "--template", "box-template", "--set", "host=i-0123456789abcdef0", "-c", path); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []string{"C222", "C333"} {
		if err := runCmd(t, "route", "add", ch, "box-foo", "-c", path); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(secrets, "runner-box-foo")
	if err := os.WriteFile(secret, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := runCmd(t, "runner", "remove", "box-foo", "-c", path); err != nil {
		t.Fatalf("runner remove: %v", err)
	}
	cfg := loadConfig(t, path)
	if _, ok := cfg.Runners["box-foo"]; ok {
		t.Error("runner still present")
	}
	for _, rt := range cfg.Routes {
		if rt.Runner == "box-foo" {
			t.Errorf("route %s still points at the removed runner", rt.Channel)
		}
	}
	if r, ok := cfg.RunnerFor("C111", false); !ok || r != "staging" {
		t.Error("an unrelated route was disturbed")
	}
	if _, err := os.Stat(secret); !os.IsNotExist(err) {
		t.Error("the runner's secret file was not deleted")
	}

	if err := runCmd(t, "runner", "remove", "box-foo", "-c", path); err == nil {
		t.Error("removing a missing runner should fail without --missing-ok")
	}
	if err := runCmd(t, "runner", "remove", "box-foo", "--missing-ok", "-c", path); err != nil {
		t.Errorf("--missing-ok: %v", err)
	}
}

// A control plane may register two boxes at once. Without the lock both edits
// read the same file and the second rename silently discards the first.
func TestRunnerAddConcurrentEditsAllLand(t *testing.T) {
	path, _ := writeTemplateConfig(t)

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n*2)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("box-%d", i)
			errs <- runCmd(t, "runner", "add", name, "--template", "box-template", "--set", "host=i-0123456789abcdef0", "-c", path)
			errs <- runCmd(t, "route", "add", fmt.Sprintf("C9%02d", i), name, "-c", path)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg := loadConfig(t, path)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("box-%d", i)
		if _, ok := cfg.Runners[name]; !ok {
			t.Errorf("%s was lost", name)
		}
		if r, ok := cfg.RunnerFor(fmt.Sprintf("C9%02d", i), false); !ok || r != name {
			t.Errorf("route for %s was lost", name)
		}
	}
}

func TestEnrollTokenFromStdin(t *testing.T) {
	path, secrets := writeTemplateConfig(t)
	if err := runCmd(t, "runner", "add", "box-foo", "--template", "box-template", "--set", "host=i-0123456789abcdef0", "-c", path); err != nil {
		t.Fatal(err)
	}

	token := strings.Repeat("k", 43)
	withStdin(t, token+"\n", func() {
		if err := runCmd(t, "enroll", "box-foo", "--write", "--token-stdin", "--print-token", "-c", path); err != nil {
			t.Fatalf("enroll: %v", err)
		}
	})
	got, err := os.ReadFile(filepath.Join(secrets, "runner-box-foo"))
	if err != nil || string(got) != token {
		t.Fatalf("stored %q, %v; want the stdin token", got, err)
	}

	withStdin(t, "short", func() {
		if err := runCmd(t, "enroll", "box-foo", "--write", "--force", "--token-stdin", "-c", path); err == nil {
			t.Error("a short token was accepted")
		}
	})
}

func withStdin(t *testing.T, content string, fn func()) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = old; f.Close() }()
	fn()
}

func TestScalarTag(t *testing.T) {
	cases := map[string]string{
		"4": "!!int", "true": "!!bool", "45m": "!!str", "i-0abc": "!!str",
		":package:": "!!str", "claude-opus-5": "!!str", "Box foo": "!!str", "": "!!str",
	}
	for in, want := range cases {
		if got := scalarTag(in); got != want {
			t.Errorf("scalarTag(%q) = %s, want %s", in, got, want)
		}
	}
}

// The control plane passes only --set host=i-…: the template's wake block makes
// the copy wakeable, targeting the copy's own host and never anything the
// template named.
func TestRunnerAddWakesItsOwnHost(t *testing.T) {
	path, _ := writeTemplateConfig(t)
	if err := runCmd(t, "runner", "add", "box-foo", "--template", "box-template",
		"--set", "host=i-0123456789abcdef0", "-c", path); err != nil {
		t.Fatalf("runner add: %v", err)
	}
	r := loadConfig(t, path).Runners["box-foo"]
	if !r.Wakeable() || r.Wake.EC2Instance != "i-0123456789abcdef0" || r.Wake.Region != "us-east-2" {
		t.Fatalf("wake = %+v, want the copy's host in the template's region", r.Wake)
	}

	// A host inherited from the template must never become a wake target.
	if err := runCmd(t, "runner", "add", "box-inh", "--template", "box-template", "-c", path); err == nil {
		t.Fatal("expected an error when the copy names no machine of its own")
	}

	// A host that is not an instance id cannot be woken: refuse, don't guess.
	before, _ := os.ReadFile(path)
	if err := runCmd(t, "runner", "add", "box-bar", "--template", "box-template",
		"--set", "host=somebox", "-c", path); err == nil {
		t.Fatal("expected an error for a wake template with a non-instance host")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("a refused add modified the file")
	}

	// An explicit instance wins over the host.
	if err := runCmd(t, "runner", "add", "box-baz", "--template", "box-template",
		"--set", "host=somebox", "--set", "wake.ec2_instance=i-0fedcba9876543210", "-c", path); err != nil {
		t.Fatal(err)
	}
	if got := loadConfig(t, path).Runners["box-baz"].Wake.EC2Instance; got != "i-0fedcba9876543210" {
		t.Errorf("wake instance = %q", got)
	}
}
