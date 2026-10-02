// Package forge mints short-lived, repository-scoped git credentials.
//
// Runners hold no forge credentials. A local credential helper asks the gateway
// per operation, the gateway checks policy, and the minted token is scoped to
// the single repository being accessed — so a runner physically cannot reach
// repositories outside its policy regardless of what the agent attempts.
package forge

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Credential is a minted git credential.
type Credential struct {
	Username  string
	Token     string
	ExpiresAt time.Time
}

// Access is what a minted credential may do.
type Access int

const (
	// ReadWrite is whatever the provider grants by default: for a GitHub App,
	// the installation's permissions on that one repository.
	ReadWrite Access = iota
	// ReadOnly is repository contents read only — how a runner fetches a plugin
	// marketplace it must not be able to change.
	ReadOnly
)

func (a Access) String() string {
	if a == ReadOnly {
		return "read-only"
	}
	return "read-write"
}

// Provider mints credentials for a repository.
type Provider interface {
	// Mint returns a credential valid for repo ("owner/name") only.
	Mint(ctx context.Context, repo string, access Access) (Credential, error)
	Name() string
}

// ---------------------------------------------------------------------------

// GitHubApp mints installation access tokens scoped to individual repositories.
type GitHubApp struct {
	AppID          string
	InstallationID string
	key            *rsa.PrivateKey
	baseURL        string
	http           *http.Client
	now            func() time.Time

	mu    sync.Mutex
	cache map[string]Credential
}

// NewGitHubApp builds a provider from a PEM-encoded RSA private key.
func NewGitHubApp(appID, installationID string, pemKey []byte) (*GitHubApp, error) {
	key, err := parseRSAKey(pemKey)
	if err != nil {
		return nil, err
	}
	if appID == "" || installationID == "" {
		return nil, errors.New("forge: app id and installation id are required")
	}
	return &GitHubApp{
		AppID:          appID,
		InstallationID: installationID,
		key:            key,
		baseURL:        "https://api.github.com",
		http:           &http.Client{Timeout: 20 * time.Second},
		now:            time.Now,
		cache:          map[string]Credential{},
	}, nil
}

func (g *GitHubApp) Name() string { return "github-app" }

func parseRSAKey(pemKey []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemKey)
	if block == nil {
		return nil, errors.New("forge: private key is not PEM-encoded")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	any, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("forge: parse private key: %w", err)
	}
	k, ok := any.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("forge: private key is %T, want RSA", any)
	}
	return k, nil
}

// appJWT builds the short-lived assertion that authenticates as the App itself.
func (g *GitHubApp) appJWT() (string, error) {
	now := g.now()
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		// Backdate to tolerate clock skew; GitHub rejects future-dated iat.
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": g.AppID,
	}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(hb) + "." + enc.EncodeToString(cb)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, g.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("forge: sign jwt: %w", err)
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// SplitRepo validates and splits an "owner/name" reference.
func SplitRepo(repo string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("forge: %q is not in owner/name form", repo)
	}
	return owner, name, nil
}

// Mint returns an installation token valid only for repo. Tokens live about an
// hour; they are cached in memory until shortly before expiry and never written
// to disk on either side of the connection.
func (g *GitHubApp) Mint(ctx context.Context, repo string, access Access) (Credential, error) {
	_, name, err := SplitRepo(repo)
	if err != nil {
		return Credential{}, err
	}
	// Read-only and read-write tokens for the same repository are different
	// credentials; one must never be served from the other's cache slot.
	key := repo + "#" + access.String()

	g.mu.Lock()
	if c, ok := g.cache[key]; ok && g.now().Add(5*time.Minute).Before(c.ExpiresAt) {
		g.mu.Unlock()
		return c, nil
	}
	g.mu.Unlock()

	jwt, err := g.appJWT()
	if err != nil {
		return Credential{}, err
	}

	req := map[string]any{"repositories": []string{name}}
	if access == ReadOnly {
		// GitHub narrows the token to exactly these permissions.
		req["permissions"] = map[string]string{"contents": "read", "metadata": "read"}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Credential{}, err
	}
	url := fmt.Sprintf("%s/app/installations/%s/access_tokens", g.baseURL, g.InstallationID)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Credential{}, err
	}
	hreq.Header.Set("Authorization", "Bearer "+jwt)
	hreq.Header.Set("Accept", "application/vnd.github+json")
	hreq.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	hreq.Header.Set("Content-Type", "application/json")

	resp, err := g.http.Do(hreq)
	if err != nil {
		return Credential{}, fmt.Errorf("forge: mint token: %w", err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return Credential{}, fmt.Errorf("forge: mint token for %s: github returned %s: %s",
			repo, resp.Status, strings.TrimSpace(string(payload)))
	}

	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return Credential{}, fmt.Errorf("forge: decode token response: %w", err)
	}
	if out.Token == "" {
		return Credential{}, errors.New("forge: github returned an empty token")
	}

	// x-access-token is the username GitHub expects alongside an installation
	// token over HTTPS.
	cred := Credential{Username: "x-access-token", Token: out.Token, ExpiresAt: out.ExpiresAt}
	g.mu.Lock()
	g.cache[key] = cred
	g.mu.Unlock()
	return cred, nil
}

// ---------------------------------------------------------------------------

// StaticToken is a personal access token, for single-user setups that do not
// warrant a GitHub App. It cannot be scoped per repository, so policy is the
// only thing bounding it — which is why the App path is the recommendation.
type StaticToken struct {
	Username string
	Token    string
	Expires  time.Time
}

func (s *StaticToken) Name() string { return "static-token" }

// Mint ignores access: a personal token cannot be narrowed per request, so a
// "read-only" grant from it is read-only by policy alone.
func (s *StaticToken) Mint(_ context.Context, repo string, _ Access) (Credential, error) {
	if _, _, err := SplitRepo(repo); err != nil {
		return Credential{}, err
	}
	user := s.Username
	if user == "" {
		user = "x-access-token"
	}
	return Credential{Username: user, Token: s.Token, ExpiresAt: s.Expires}, nil
}
