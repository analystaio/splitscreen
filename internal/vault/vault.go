// Package vault encrypts secrets people hand the gateway, and describes the
// policy each one carries: where it may be sent, how, and on whose behalf.
// See docs/secrets-broker.md.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Sealer encrypts and decrypts values with one key. The secret's name is bound
// in as associated data, so a ciphertext copied onto another row will not open.
type Sealer struct {
	aead cipher.AEAD
	id   string
}

// NewSealer takes the base64 form of a 32-byte key, as stored in the gateway's
// secret backend.
func NewSealer(b64 string) (*Sealer, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("vault: key is not base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("vault: key is %d bytes, want 32 (generate with: head -c32 /dev/urandom | base64)", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(key)
	return &Sealer{aead: aead, id: hex.EncodeToString(sum[:4])}, nil
}

// KeyID names the key without revealing it, so rows record which key sealed them.
func (s *Sealer) KeyID() string { return s.id }

// Seal encrypts value for the secret called name.
func (s *Sealer) Seal(name, value string) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return s.aead.Seal(nil, nonce, []byte(value), []byte(name)), nonce, nil
}

// Open decrypts a value sealed for name.
func (s *Sealer) Open(name string, ciphertext, nonce []byte, keyID string) (string, error) {
	if keyID != s.id {
		return "", fmt.Errorf("vault: secret %q was sealed with key %s, the gateway has %s", name, keyID, s.id)
	}
	if len(nonce) != s.aead.NonceSize() {
		return "", errors.New("vault: secret has no value (revoked?)")
	}
	pt, err := s.aead.Open(nil, nonce, ciphertext, []byte(name))
	if err != nil {
		return "", fmt.Errorf("vault: cannot decrypt %q: %w", name, err)
	}
	return string(pt), nil
}

// Who may use a secret.
const (
	// UsersOwner limits a secret to turns its owner started. It needs a
	// hardened runner to mean anything; elsewhere such a secret refuses.
	UsersOwner = "owner"
	// UsersAny lets any turn on an allowed runner use it.
	UsersAny = "any"
)

// Policy is stored as JSON beside each secret.
type Policy struct {
	// Hosts the secret may be sent to: exact host names, optionally with a
	// port. The first is the default.
	Hosts []string `json:"hosts"`
	// Header and Format put the value in a request header. Format must
	// contain {value}. Defaults: Authorization, "Bearer {value}".
	Header string `json:"header,omitempty"`
	Format string `json:"format,omitempty"`
	// Query, when set, sends the value as this query parameter instead.
	Query string `json:"query,omitempty"`

	Users   string   `json:"users"`
	Runners []string `json:"runners,omitempty"` // empty: every runner with egress
	Methods []string `json:"methods,omitempty"` // empty: every method

	// Purpose is the free-text reason given when the secret was requested.
	Purpose string `json:"purpose,omitempty"`
}

var (
	nameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	hostRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
	headerRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	paramRe  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	methods  = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}
)

// ValidName reports whether s can name a secret. Names appear in URLs and audit.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// ValidHost reports whether h is an exact host name, optionally with a port.
// IP addresses are refused: a secret belongs to a service, not an address.
func ValidHost(h string) bool {
	host, port, err := net.SplitHostPort(h)
	if err != nil {
		host, port = h, ""
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	return net.ParseIP(host) == nil && hostRe.MatchString(host)
}

// Normalize fills defaults and lower-cases what should be.
func (p *Policy) Normalize() {
	for i, h := range p.Hosts {
		p.Hosts[i] = strings.ToLower(strings.TrimSpace(h))
	}
	if p.Query == "" {
		if p.Header == "" {
			p.Header = "Authorization"
		}
		if p.Format == "" {
			p.Format = "Bearer {value}"
		}
	}
	if p.Users == "" {
		p.Users = UsersOwner
	}
	for i, m := range p.Methods {
		p.Methods[i] = strings.ToUpper(strings.TrimSpace(m))
	}
}

// Validate checks a normalized policy.
func (p *Policy) Validate() error {
	var errs []error
	if len(p.Hosts) == 0 {
		errs = append(errs, errors.New("at least one host is required"))
	}
	for _, h := range p.Hosts {
		if !ValidHost(h) {
			errs = append(errs, fmt.Errorf("%q is not a host name (no scheme, path or IP address)", h))
		}
	}
	if p.Query != "" {
		if !paramRe.MatchString(p.Query) {
			errs = append(errs, fmt.Errorf("%q is not a query parameter name", p.Query))
		}
		if p.Header != "" {
			errs = append(errs, errors.New("use a header or a query parameter, not both"))
		}
	} else {
		if !headerRe.MatchString(p.Header) {
			errs = append(errs, fmt.Errorf("%q is not a header name", p.Header))
		}
		if !strings.Contains(p.Format, "{value}") {
			errs = append(errs, errors.New("the header format must contain {value}"))
		}
		if strings.ContainsAny(p.Format, "\r\n") {
			errs = append(errs, errors.New("the header format must be one line"))
		}
	}
	if p.Users != UsersOwner && p.Users != UsersAny {
		errs = append(errs, fmt.Errorf("users must be %q or %q", UsersOwner, UsersAny))
	}
	for _, m := range p.Methods {
		if !slices.Contains(methods, m) {
			errs = append(errs, fmt.Errorf("%q is not an HTTP method", m))
		}
	}
	return errors.Join(errs...)
}

// Encode returns the stored form.
func (p Policy) Encode() string {
	b, _ := json.Marshal(p)
	return string(b)
}

// DecodePolicy parses the stored form.
func DecodePolicy(s string) (Policy, error) {
	var p Policy
	err := json.Unmarshal([]byte(s), &p)
	return p, err
}

// AllowsHost reports whether host is one of the policy's.
func (p Policy) AllowsHost(host string) bool { return slices.Contains(p.Hosts, strings.ToLower(host)) }

// AllowsMethod reports whether method may be used.
func (p Policy) AllowsMethod(m string) bool {
	return len(p.Methods) == 0 || slices.Contains(p.Methods, m)
}

// AllowsRunner reports whether runner may use the secret.
func (p Policy) AllowsRunner(r string) bool {
	return len(p.Runners) == 0 || slices.Contains(p.Runners, r)
}

// HeaderValue is what goes in the header.
func (p Policy) HeaderValue(value string) string {
	return strings.ReplaceAll(p.Format, "{value}", value)
}
