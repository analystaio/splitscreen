package gateway

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/analystaio/splitscreen/config"
	"github.com/analystaio/splitscreen/internal/forge"
	"github.com/analystaio/splitscreen/protocol"
)

type recordingForge struct {
	mu    sync.Mutex
	mints []string
}

func (f *recordingForge) Name() string { return "test" }
func (f *recordingForge) Mint(_ context.Context, repo string, a forge.Access) (forge.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mints = append(f.mints, repo+" "+a.String())
	return forge.Credential{Username: "x-access-token", Token: "tok-" + a.String()}, nil
}

// alpha's bundle enables a plugin from acme/plugins; its forge policy is
// acme/widgets only.
func marketplaceHarness(t *testing.T) (*harness, *recordingForge) {
	t.Helper()
	h := newHarness(t)
	cfg := h.gw.cfg.Load()
	cfg.Marketplaces = map[string]*config.Marketplace{"acme": {Repo: "acme/plugins", Ref: "v1"}}
	cfg.Bundles = map[string]*config.Bundle{"b": {Plugins: []string{"tools@acme"}}}
	cfg.Runners["alpha"].Bundle = "b"
	f := &recordingForge{}
	h.gw.forge = f
	return h, f
}

func credential(t *testing.T, h *harness, repo string) *protocol.CredentialGrant {
	t.Helper()
	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)
	send(t, ws, &protocol.CredentialRequest{RequestID: "c1", Kind: protocol.CredentialForge, Resource: repo})
	return readFrame[*protocol.CredentialGrant](t, ws)
}

func TestMarketplaceRepoGetsAReadOnlyCredential(t *testing.T) {
	h, f := marketplaceHarness(t)
	g := credential(t, h, "acme/plugins")
	if g.Denied || g.Value != "tok-read-only" {
		t.Fatalf("grant = %+v, want a read-only token", g)
	}
	if len(f.mints) != 1 || f.mints[0] != "acme/plugins read-only" {
		t.Fatalf("mints = %v", f.mints)
	}
}

func TestPolicyRepoStillGetsReadWrite(t *testing.T) {
	h, f := marketplaceHarness(t)
	if g := credential(t, h, "acme/widgets"); g.Denied || g.Value != "tok-read-write" {
		t.Fatalf("grant = %+v", g)
	}
	if f.mints[0] != "acme/widgets read-write" {
		t.Fatalf("mints = %v", f.mints)
	}
}

// A marketplace another runner uses, or none at all, grants nothing.
func TestUnusedMarketplaceRepoIsDenied(t *testing.T) {
	h, f := marketplaceHarness(t)
	h.gw.cfg.Load().Bundles["b"].Plugins = nil
	if g := credential(t, h, "acme/plugins"); !g.Denied {
		t.Fatalf("grant = %+v, want denied", g)
	}
	if len(f.mints) != 0 {
		t.Fatalf("minted %v for a denied request", f.mints)
	}
}

// The runner learns where each marketplace is, at which ref, from the bundle.
func TestBundleCarriesMarketplacePins(t *testing.T) {
	h, _ := marketplaceHarness(t)
	push, err := h.gw.buildBundle("alpha")
	if err != nil {
		t.Fatal(err)
	}
	var m protocol.PluginManifest
	for _, f := range push.Files {
		if f.Path == protocol.PluginManifestFile {
			if err := json.Unmarshal(f.Content, &m); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(m.Plugins) != 1 || m.Plugins[0] != "tools@acme" || m.Marketplaces["acme"] != (protocol.Marketplace{Repo: "acme/plugins", Ref: "v1"}) {
		t.Fatalf("manifest = %+v", m)
	}

	before := push.Digest
	h.gw.cfg.Load().Marketplaces["acme"].Ref = "v2"
	push2, _ := h.gw.buildBundle("alpha")
	if push2.Digest == before {
		t.Fatal("moving the ref did not change the digest; the runner would keep v1")
	}
}
