package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/analystaio/splitscreen/config"
	"github.com/analystaio/splitscreen/internal/store"
	"github.com/analystaio/splitscreen/protocol"
)

// bundleState is the last bundle pushed to a runner.
type bundleState struct {
	Version int
	Digest  string
}

// BundleRoot is where bundle source files are read from, relative to the config
// file's directory unless absolute.
const BundleRoot = "bundles"

// buildBundle materializes a runner's configuration into a pushable frame.
//
// Contents are interpreted by the harness adapter, not by the gateway: memory
// files and skills mean something to a Claude Code adapter and something else,
// or nothing, to another. The gateway versions and ships them.
func (g *Gateway) buildBundle(runner string) (*protocol.BundlePush, error) {
	cfg := g.cfg.Load()
	rc, ok := cfg.Runners[runner]
	if !ok {
		return nil, fmt.Errorf("gateway: runner %q is not configured", runner)
	}

	push := &protocol.BundlePush{Model: rc.Model}

	if rc.Bundle != "" {
		resolved, err := cfg.ResolveBundle(rc.Bundle)
		if err != nil {
			return nil, err
		}
		push.LooseMCP = !resolved.StrictMCP
		for _, rel := range resolved.Memory {
			content, err := g.readBundleFile(rel)
			if err != nil {
				return nil, err
			}
			push.Files = append(push.Files, protocol.BundleFile{
				Path: filepath.ToSlash(filepath.Join("memory", filepath.Base(rel))),
				// 0600: bundles can carry operator-authored guidance that should
				// not be world-readable on a shared box.
				Content: content, Mode: 0o600,
			})
		}
		for _, rel := range resolved.Skills {
			files, err := g.readBundleTree(rel, "skills")
			if err != nil {
				return nil, err
			}
			push.Files = append(push.Files, files...)
		}

		seenMCP := map[string]bool{}
		for _, name := range resolved.MCP {
			if seenMCP[name] {
				continue
			}
			seenMCP[name] = true
			s, ok := cfg.MCP[name]
			if !ok {
				return nil, fmt.Errorf("gateway: bundle references unknown mcp server %q", name)
			}
			srv := protocol.MCPServer{Name: name}
			switch s.Kind {
			case config.MCPLocal:
				srv.Kind = protocol.MCPLocal
				srv.Command = s.Command
				srv.Args = s.Args
				srv.Env = s.Env
			case config.MCPProxied:
				// Proxied servers carry no command and no credential: the runner
				// reaches them through the gateway, which holds the secret.
				srv.Kind = protocol.MCPProxied
			}
			push.MCP = append(push.MCP, srv)
		}

		if len(resolved.Plugins) > 0 {
			// The manifest is all the runner needs to fetch and load plugins:
			// which ones, and where each marketplace lives at which ref. It is
			// part of the digest, so moving a ref re-pushes the bundle.
			markets := map[string]protocol.Marketplace{}
			for name, m := range cfg.MarketplacesFor(resolved) {
				markets[name] = protocol.Marketplace{Repo: m.Repo, Ref: m.Ref}
			}
			manifest, err := json.MarshalIndent(protocol.PluginManifest{
				Plugins: resolved.Plugins, Marketplaces: markets,
			}, "", "  ")
			if err != nil {
				return nil, err
			}
			push.Files = append(push.Files, protocol.BundleFile{
				Path: protocol.PluginManifestFile, Content: manifest, Mode: 0o600,
			})
		}
	}

	// The harness credential is the one secret that must reach the runner,
	// because the agent process authenticates from there. It is shipped in the
	// bundle and materialized to tmpfs, never to persistent disk.
	if rc.HarnessSecret != "" {
		sec, err := g.secrets.Get(rc.HarnessSecret)
		if err != nil {
			return nil, fmt.Errorf("gateway: harness secret %q for runner %q: %w",
				rc.HarnessSecret, runner, err)
		}
		env := rc.HarnessEnv
		if env == "" {
			env = "ANTHROPIC_API_KEY"
		}
		push.Secrets = map[string]string{env: sec.Value}
	}

	push.Digest = digestBundle(push)
	return push, nil
}

// digestBundle hashes the pushable content so runner-side drift is detectable.
// Secret values are hashed by name only: the digest must be safe to log and to
// show in status output.
func digestBundle(b *protocol.BundlePush) string {
	h := sha256.New()
	// The model is part of the digest because the digest is what decides whether
	// to push at all. Left out, changing the model in config would compute an
	// unchanged digest, the push would be skipped as redundant, and the runner
	// would keep running the old model with the config claiming otherwise.
	fmt.Fprintf(h, "model:%s\x00", b.Model)
	// Likewise the MCP mode: it changes which tools a session has.
	fmt.Fprintf(h, "loose_mcp:%v\x00", b.LooseMCP)
	files := append([]protocol.BundleFile(nil), b.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for _, f := range files {
		fmt.Fprintf(h, "file:%s:%o:", f.Path, f.Mode)
		h.Write(f.Content)
		h.Write([]byte{0})
	}
	servers := append([]protocol.MCPServer(nil), b.MCP...)
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	for _, s := range servers {
		fmt.Fprintf(h, "mcp:%s:%s:%s:%v\x00", s.Name, s.Kind, s.Command, s.Args)
	}
	names := make([]string, 0, len(b.Secrets))
	for k := range b.Secrets {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, n := range names {
		// Name and a salted hash of the value: a rotated secret changes the
		// digest (so it is repushed) without the digest revealing anything.
		vh := sha256.Sum256([]byte("splitscreen-bundle-secret\x00" + b.Secrets[n]))
		fmt.Fprintf(h, "secret:%s:%s\x00", n, hex.EncodeToString(vh[:8]))
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func (g *Gateway) bundleBase() string {
	if g.cfgPath == "" {
		return BundleRoot
	}
	return filepath.Join(filepath.Dir(g.cfgPath), BundleRoot)
}

func (g *Gateway) resolveBundlePath(rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return rel, nil
	}
	base := g.bundleBase()
	full := filepath.Join(base, rel)
	// Keep bundle sources inside the bundle root: the config is operator
	// authored and can be wrong, and this path feeds a file read.
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	absFull, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	if absFull != absBase && !strings.HasPrefix(absFull, absBase+string(os.PathSeparator)) {
		return "", fmt.Errorf("gateway: bundle path %q escapes %s", rel, base)
	}
	return absFull, nil
}

func (g *Gateway) readBundleFile(rel string) ([]byte, error) {
	p, err := g.resolveBundlePath(rel)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("gateway: bundle file %q: %w", rel, err)
	}
	return b, nil
}

// readBundleTree reads a directory of files (a skill, typically) into bundle
// entries under prefix.
func (g *Gateway) readBundleTree(rel, prefix string) ([]protocol.BundleFile, error) {
	root, err := g.resolveBundlePath(rel)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("gateway: bundle tree %q: %w", rel, err)
	}
	name := filepath.Base(rel)
	if !info.IsDir() {
		content, err := os.ReadFile(root)
		if err != nil {
			return nil, err
		}
		return []protocol.BundleFile{{
			Path: filepath.ToSlash(filepath.Join(prefix, name)), Content: content, Mode: 0o600,
		}}, nil
	}

	var out []protocol.BundleFile
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		content, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		relPath, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		out = append(out, protocol.BundleFile{
			Path:    filepath.ToSlash(filepath.Join(prefix, name, relPath)),
			Content: content,
			Mode:    0o600,
		})
		return nil
	})
	return out, err
}

// pushBundle sends a runner its configuration if it has changed.
//
// The bundle version is part of the session key: when it changes, live sessions
// are marked stale so the change is announced rather than discovered.
func (g *Gateway) pushBundle(ctx context.Context, c *Conn) error {
	push, err := g.buildBundle(c.runner)
	if err != nil {
		return err
	}

	g.bundlesMu.Lock()
	prev := g.bundles[c.runner]

	// The decision to skip rests on what the RUNNER reports having, not on the
	// gateway's own cache. A runner's config lives on tmpfs; a reboot wipes it
	// without producing anything the gateway could distinguish from an ordinary
	// reconnect. Trusting the cache here is how a runner ends up connected,
	// healthy, and completely unconfigured.
	if c.BundleDigest() == push.Digest {
		version := prev.Version
		if v := c.BundleVersion(); v > version {
			// It survived a gateway restart with a higher number; keep numbering
			// monotonic from what actually exists in the field.
			version = v
		}
		if version == 0 {
			version = 1
		}
		g.bundles[c.runner] = bundleState{Version: version, Digest: push.Digest}
		g.bundlesMu.Unlock()
		push.Version = version
		c.setBundle(version, push.Digest)
		return nil
	}

	base := prev.Version
	if v := c.BundleVersion(); v > base {
		base = v
	}
	version := base + 1
	if prev.Digest == push.Digest && prev.Version > 0 {
		// Same content the gateway last built — the runner simply lost it, which
		// is not a configuration change. Re-send at the existing version so live
		// sessions are not marked stale and told to restart for nothing.
		version = prev.Version
	}
	next := bundleState{Version: version, Digest: push.Digest}
	g.bundles[c.runner] = next
	g.bundlesMu.Unlock()

	push.Version = next.Version
	if err := c.Send(push); err != nil {
		return err
	}
	c.setBundle(next.Version, push.Digest)

	g.log.Info("bundle pushed", "runner", c.runner, "version", next.Version,
		"digest", next.Digest, "files", len(push.Files), "mcp", len(push.MCP))
	_ = g.store.Log(store.Event{
		Kind: "bundle.pushed", Runner: c.runner,
		Detail: map[string]any{"version": next.Version, "digest": next.Digest},
	})

	if prev.Version > 0 {
		n, err := g.store.MarkRunnerThreadsStale(c.runner, next.Version)
		if err != nil {
			g.log.Error("marking threads stale failed", "runner", c.runner, "err", err)
		} else if n > 0 {
			g.log.Info("threads marked stale by config change", "runner", c.runner, "threads", n)
		}
	}
	return nil
}
