package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/analystaio/splitscreen/internal/secrets"
)

// recordingSecrets wraps a backend and records invalidations.
type recordingSecrets struct {
	secrets.Backend
	mu          sync.Mutex
	invalidated []string
}

func (r *recordingSecrets) Invalidate(name string) {
	r.mu.Lock()
	r.invalidated = append(r.invalidated, name)
	r.mu.Unlock()
}

// A reload that adds or removes a runner drops that runner's cached enrollment
// secret, so a name reused by a new machine is not judged against the previous
// machine's token — or against a miss cached before the token was written.
func TestReloadInvalidatesChangedRunnerSecrets(t *testing.T) {
	h := newHarness(t)
	rec := &recordingSecrets{Backend: h.gw.secrets}
	h.gw.secrets = rec

	path := filepath.Join(t.TempDir(), "splitscreen.yaml")
	h.gw.cfgPath = path
	withBox := strings.Replace(testConfig, "routes:", `  box-foo:
    display: { name: "Box" }
    cwd: /tmp
    harness: claude-code
routes:`, 1)
	if err := os.WriteFile(path, []byte(withBox), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.gw.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := strings.Join(rec.invalidated, ","); got != "runner-box-foo" {
		t.Fatalf("invalidated %q after adding box-foo, want runner-box-foo", got)
	}

	rec.invalidated = nil
	if err := os.WriteFile(path, []byte(testConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.gw.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := strings.Join(rec.invalidated, ","); got != "runner-box-foo" {
		t.Fatalf("invalidated %q after removing box-foo, want runner-box-foo", got)
	}
}

// Removing a runner revokes it: the live connection is closed and the same
// token is refused from then on, because an unconfigured runner cannot
// authenticate whatever it presents.
func TestRemovedRunnerIsDisconnectedAndRefused(t *testing.T) {
	h := newHarness(t)
	path := filepath.Join(t.TempDir(), "splitscreen.yaml")
	h.gw.cfgPath = path

	ws := h.connect(t, "s3cret")
	eventually(t, "alpha registered", func() bool { _, ok := h.gw.hub.Get("alpha"); return ok })

	other := strings.Replace(testConfig, "alpha", "beta", -1)
	t.Setenv("SPLITSCREEN_SECRET_RUNNER_BETA", "x")
	if err := os.WriteFile(path, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.gw.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		if _, _, err := ws.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatal("the removed runner's connection was not closed")
			}
			break
		}
	}

	ws2 := h.connect(t, "s3cret")
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	for {
		_, _, err := ws2.Read(ctx2)
		if err == nil {
			continue
		}
		if ctx2.Err() != nil {
			t.Fatal("a removed runner's reconnect was not refused")
		}
		if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
			t.Fatalf("expected a refusal, got a normal close: %v", err)
		}
		break
	}
}

// A failed authentication drops the cached secret, so a token written after a
// runner's first (failed) attempt is read on its next one, not after the TTL.
func TestFailedAuthInvalidatesCachedSecret(t *testing.T) {
	h := newHarness(t)
	rec := &recordingSecrets{Backend: h.gw.secrets}
	h.gw.secrets = rec

	ws := h.connect(t, "wrong")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		if _, _, err := ws.Read(ctx); err != nil {
			break
		}
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if strings.Join(rec.invalidated, ",") != "runner-alpha" {
		t.Fatalf("invalidated %v after a failed auth, want runner-alpha", rec.invalidated)
	}
}
