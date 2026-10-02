package forge

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

func TestSplitRepo(t *testing.T) {
	if _, _, err := SplitRepo("acme/widgets"); err != nil {
		t.Fatalf("valid repo rejected: %v", err)
	}
	for _, bad := range []string{"widgets", "acme/", "/widgets", "a/b/c", ""} {
		if _, _, err := SplitRepo(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestAppJWTShape(t *testing.T) {
	g, err := NewGitHubApp("123", "456", testKeyPEM(t))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := g.appJWT()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts", len(parts))
	}
	var claims map[string]any
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	if claims["iss"] != "123" {
		t.Errorf("iss = %v", claims["iss"])
	}
	// iat is backdated: GitHub rejects future-dated assertions, and clock skew
	// between a gateway and GitHub is not something we control.
	iat := int64(claims["iat"].(float64))
	if iat > time.Now().Unix() {
		t.Errorf("iat %d is not backdated", iat)
	}
}

// The minted token must be scoped to exactly the repository being accessed —
// that is what stops a runner reaching anything outside its policy.
func TestMintRequestsRepositoryScope(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"ghs_test","expires_at":"2030-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	g, err := NewGitHubApp("123", "456", testKeyPEM(t))
	if err != nil {
		t.Fatal(err)
	}
	g.baseURL = srv.URL

	cred, err := g.Mint(context.Background(), "acme/widgets", ReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Token != "ghs_test" {
		t.Fatalf("token = %q", cred.Token)
	}
	if cred.Username != "x-access-token" {
		t.Fatalf("username = %q", cred.Username)
	}
	repos, _ := gotBody["repositories"].([]any)
	if len(repos) != 1 || repos[0] != "widgets" {
		t.Fatalf("repositories = %v, want exactly [widgets]", gotBody["repositories"])
	}
	if _, narrowed := gotBody["permissions"]; narrowed {
		t.Errorf("a read-write mint narrowed permissions: %v", gotBody["permissions"])
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Errorf("authorization = %q", gotAuth)
	}
}

// A read-only mint asks GitHub for contents:read and nothing else, and is
// cached apart from the read-write token for the same repository.
func TestMintReadOnlyNarrowsPermissions(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		_, _ = w.Write([]byte(`{"token":"ghs_` + fmt.Sprint(len(bodies)) + `","expires_at":"2030-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	g, _ := NewGitHubApp("123", "456", testKeyPEM(t))
	g.baseURL = srv.URL

	ro, err := g.Mint(context.Background(), "acme/plugins", ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	perms, _ := bodies[0]["permissions"].(map[string]any)
	if len(perms) != 2 || perms["contents"] != "read" || perms["metadata"] != "read" {
		t.Fatalf("permissions = %v, want contents:read + metadata:read only", bodies[0]["permissions"])
	}
	rw, _ := g.Mint(context.Background(), "acme/plugins", ReadWrite)
	if rw.Token == ro.Token || len(bodies) != 2 {
		t.Fatalf("read-write mint was served the read-only token (%d mints)", len(bodies))
	}
	again, _ := g.Mint(context.Background(), "acme/plugins", ReadOnly)
	if again.Token != ro.Token || len(bodies) != 2 {
		t.Fatalf("read-only token was not cached (%d mints)", len(bodies))
	}
}

func TestMintCachesUntilNearExpiry(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"token":"ghs_test","expires_at":"2030-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	g, _ := NewGitHubApp("123", "456", testKeyPEM(t))
	g.baseURL = srv.URL

	for i := 0; i < 3; i++ {
		if _, err := g.Mint(context.Background(), "acme/widgets", ReadWrite); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("minted %d times, want 1 — a live token should be reused", calls)
	}

	// A different repository must not reuse the first repository's token.
	if _, err := g.Mint(context.Background(), "acme/other", ReadWrite); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("minted %d times, want 2 — scoping is per repository", calls)
	}
}

func TestMintSurfacesGitHubErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"not installed"}`))
	}))
	defer srv.Close()

	g, _ := NewGitHubApp("123", "456", testKeyPEM(t))
	g.baseURL = srv.URL

	_, err := g.Mint(context.Background(), "acme/widgets", ReadWrite)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("error %q should include GitHub's explanation", err)
	}
}

func TestBadKeyRejected(t *testing.T) {
	if _, err := NewGitHubApp("1", "2", []byte("not a pem")); err == nil {
		t.Fatal("a non-PEM key was accepted")
	}
}
