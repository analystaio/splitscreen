package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/analystaio/splitscreen/protocol"
)

// Plugins: a bundle may enable Claude Code plugins from marketplaces that live
// in git repositories. The runner fetches each marketplace at exactly the ref
// the gateway pinned, with a read-only credential the gateway mints, and every
// session loads the enabled plugins from that checkout (--plugin-dir). Nothing
// is installed into the harness's own config, so there is no plugin cache to
// drift and no background update to move a pinned version.
//
// Fetching needs the gateway (for the credential), and bundles are applied on
// the connection's read loop, which must not block on a reply it would itself
// have to read. So the sync runs beside it, and a session that starts before it
// finishes waits for it, briefly.

const (
	// pluginSyncTimeout bounds one marketplace fetch.
	pluginSyncTimeout = 2 * time.Minute
	// pluginWait bounds how long a starting session waits for a sync in
	// progress before starting without plugins rather than not at all.
	pluginWait = 3 * time.Minute
)

// gitBase is where marketplace repositories are fetched from. A var so tests
// can point it at a local directory.
var gitBase = "https://github.com/"

// pluginState is the outcome of the latest sync, swapped as a unit.
type pluginState struct {
	ready chan struct{} // closed when dirs is final for this bundle
	dirs  []string
}

type pluginSet struct {
	mu  sync.Mutex
	cur *pluginState
}

func (p *pluginSet) reset(pending bool) *pluginState {
	st := &pluginState{ready: make(chan struct{})}
	if !pending {
		close(st.ready)
	}
	p.mu.Lock()
	p.cur = st
	p.mu.Unlock()
	return st
}

// dirs returns the plugin directories for a new session, waiting up to wait
// for a sync in progress.
func (p *pluginSet) wait(ctx context.Context, wait time.Duration) ([]string, error) {
	p.mu.Lock()
	st := p.cur
	p.mu.Unlock()
	if st == nil {
		return nil, nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-st.ready:
		return st.dirs, nil
	case <-t.C:
		return nil, errors.New("plugins still syncing")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// readPluginManifest returns the bundle's plugin manifest, or nil if it has
// none (or enables no plugins).
func readPluginManifest(configDir string) (*protocol.PluginManifest, error) {
	raw, err := os.ReadFile(filepath.Join(configDir, protocol.PluginManifestFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m protocol.PluginManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("runner: parse %s: %w", protocol.PluginManifestFile, err)
	}
	if len(m.Plugins) == 0 {
		return nil, nil
	}
	return &m, nil
}

// syncPlugins fetches every marketplace the bundle uses and publishes the
// enabled plugins' directories. Failures are logged and reported to the
// gateway; the sessions that follow run without the affected plugins.
func (r *Runner) syncPlugins(ctx context.Context, configDir string, st *pluginState) {
	defer close(st.ready)
	m, err := readPluginManifest(configDir)
	if err != nil || m == nil {
		if err != nil {
			r.reportPluginError(ctx, err)
		}
		return
	}

	checkouts := map[string]string{}
	for name, mp := range m.Marketplaces {
		dir, err := r.syncMarketplace(ctx, name, mp)
		if err != nil {
			r.reportPluginError(ctx, fmt.Errorf("marketplace %s (%s@%s): %w", name, mp.Repo, mp.Ref, err))
			continue
		}
		checkouts[name] = dir
	}

	var dirs []string
	for _, id := range m.Plugins {
		plugin, market, ok := splitPluginID(id)
		if !ok {
			r.reportPluginError(ctx, fmt.Errorf("plugin %q is not name@marketplace", id))
			continue
		}
		root, ok := checkouts[market]
		if !ok {
			continue // its marketplace failed and was reported
		}
		dir, err := pluginDir(root, plugin)
		if err != nil {
			r.reportPluginError(ctx, fmt.Errorf("plugin %s: %w", id, err))
			continue
		}
		dirs = append(dirs, dir)
	}
	st.dirs = dirs
	r.log.Info("plugins ready", "plugins", len(dirs), "enabled", len(m.Plugins))
}

func (r *Runner) reportPluginError(ctx context.Context, err error) {
	r.log.Error("plugins unavailable", "err", err)
	_ = r.send(ctx, &protocol.Error{Code: "plugins_unavailable", Message: err.Error()})
}

func splitPluginID(id string) (name, market string, ok bool) {
	i := strings.LastIndex(id, "@")
	if i <= 0 || i == len(id)-1 {
		return "", "", false
	}
	return id[:i], id[i+1:], true
}

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// syncMarketplace makes <cache>/marketplaces/<name> a checkout of repo at ref.
func (r *Runner) syncMarketplace(ctx context.Context, name string, mp protocol.Marketplace) (string, error) {
	if !protocol.ValidSlug(name) {
		return "", fmt.Errorf("bad marketplace name %q", name)
	}
	// Persistent when there is a state dir, so a restart with an unreachable
	// forge still has the last checkout of the same ref.
	root := r.opts.StateDir
	if root == "" {
		root = filepath.Join(r.opts.RuntimeRoot, r.opts.Name)
	}
	dir := filepath.Join(root, "marketplaces", name)
	marker := filepath.Join(dir, ".git", "splitscreen-ref")

	have, _ := os.ReadFile(marker)
	haveRef := strings.TrimSpace(string(have))
	if haveRef == mp.Repo+"@"+mp.Ref && fullSHA.MatchString(mp.Ref) {
		return dir, nil // a commit never moves
	}

	err := r.fetchMarketplace(ctx, dir, mp)
	if err != nil {
		if haveRef == mp.Repo+"@"+mp.Ref {
			r.log.Warn("marketplace fetch failed; using the checkout of the same ref",
				"marketplace", name, "err", err)
			return dir, nil
		}
		return "", err
	}
	if err := os.WriteFile(marker, []byte(mp.Repo+"@"+mp.Ref+"\n"), 0o600); err != nil {
		return "", err
	}
	return dir, nil
}

func (r *Runner) fetchMarketplace(ctx context.Context, dir string, mp protocol.Marketplace) error {
	cred, err := r.marketplaceCredential(ctx, mp.Repo)
	if err != nil {
		return fmt.Errorf("credential: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, pluginSyncTimeout)
	defer cancel()

	// The token rides an HTTP header set through the environment, so it is in
	// neither argv (visible in ps) nor any file. A repository whose remote is
	// not on gitBase (a test's local path) simply ignores it.
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"),
		"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
	}
	if cred.Password != "" {
		basic := base64.StdEncoding.EncodeToString([]byte(cred.Username + ":" + cred.Password))
		env = append(env, "GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http."+gitBase+".extraheader",
			"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+basic)
	}
	git := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			msg := strings.TrimSpace(string(out))
			if len(msg) > 500 {
				msg = msg[len(msg)-500:]
			}
			return fmt.Errorf("git %s: %w: %s", args[0], err, msg)
		}
		return nil
	}

	url := gitBase + mp.Repo
	if strings.HasPrefix(gitBase, "https://") {
		url += ".git"
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := git("init", "-q", dir); err != nil {
			return err
		}
	}
	// set-url rather than add: the repo may have been re-pointed in config.
	if git("-C", dir, "remote", "set-url", "origin", url) != nil {
		if err := git("-C", dir, "remote", "add", "origin", url); err != nil {
			return err
		}
	}
	// "--" ends options: config validation keeps refs plain, and this keeps a
	// ref from ever being read as a flag even if that check were bypassed.
	if err := git("-C", dir, "fetch", "-q", "--depth", "1", "--no-tags", "origin", "--", mp.Ref); err != nil {
		return err
	}
	if err := git("-C", dir, "checkout", "-q", "--force", "--detach", "FETCH_HEAD"); err != nil {
		return err
	}
	return git("-C", dir, "clean", "-q", "-ffdx")
}

// marketplaceCredential asks the gateway for a read-only token; a var-backed
// hook so tests can run without one.
func (r *Runner) marketplaceCredential(ctx context.Context, repo string) (CredentialResult, error) {
	if r.credentialHook != nil {
		return r.credentialHook(ctx, repo)
	}
	return r.requestCredential(ctx, repo)
}

// pluginDir resolves a plugin's directory from the marketplace manifest. Only
// relative sources inside the checkout are accepted: a source pointing at
// another repository would load code nobody pinned.
func pluginDir(root, plugin string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, ".claude-plugin", "marketplace.json"))
	if err != nil {
		return "", fmt.Errorf("read marketplace.json: %w", err)
	}
	var mk struct {
		Plugins []struct {
			Name   string          `json:"name"`
			Source json.RawMessage `json:"source"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &mk); err != nil {
		return "", fmt.Errorf("parse marketplace.json: %w", err)
	}
	for _, p := range mk.Plugins {
		if p.Name != plugin {
			continue
		}
		var rel string
		if err := json.Unmarshal(p.Source, &rel); err != nil {
			return "", errors.New("only relative-path plugin sources are supported")
		}
		dir := filepath.Clean(filepath.Join(root, rel))
		if dir != root && !strings.HasPrefix(dir, root+string(filepath.Separator)) {
			return "", fmt.Errorf("source %q escapes the marketplace", rel)
		}
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			return "", fmt.Errorf("source %q is not a directory in the checkout", rel)
		}
		return dir, nil
	}
	return "", errors.New("not in the marketplace at this ref")
}
