package gateway

import (
	"testing"

	"github.com/analystaio/splitscreen/protocol"
)

// The digest decides whether a bundle is pushed at all, so anything that
// changes what the runner should do has to be in it. A model left out of the
// digest is worse than an unsupported field: config claims one model, the
// runner keeps running another, and nothing reports a conflict.
func TestDigestCoversTheModel(t *testing.T) {
	base := &protocol.BundlePush{Files: []protocol.BundleFile{{Path: "memory/a.md", Content: []byte("x")}}}
	withModel := &protocol.BundlePush{
		Files: []protocol.BundleFile{{Path: "memory/a.md", Content: []byte("x")}},
		Model: "claude-opus-5",
	}

	if digestBundle(base) == digestBundle(withModel) {
		t.Fatal("setting a model did not change the digest; the push would be skipped as redundant")
	}

	changed := &protocol.BundlePush{
		Files: []protocol.BundleFile{{Path: "memory/a.md", Content: []byte("x")}},
		Model: "claude-sonnet-5",
	}
	if digestBundle(withModel) == digestBundle(changed) {
		t.Fatal("changing the model did not change the digest")
	}
}

// Same inputs must hash the same, or every reconnect looks like drift and
// re-pushes.
func TestDigestIsStable(t *testing.T) {
	mk := func() *protocol.BundlePush {
		return &protocol.BundlePush{
			Files: []protocol.BundleFile{{Path: "memory/a.md", Content: []byte("x")}},
			Model: "claude-opus-5",
		}
	}
	if digestBundle(mk()) != digestBundle(mk()) {
		t.Fatal("digest is not stable across identical bundles")
	}
}
