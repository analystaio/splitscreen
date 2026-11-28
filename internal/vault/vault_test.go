package vault

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func testKey(t *testing.T) string {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k)
}

func TestSealRoundTrip(t *testing.T) {
	s, err := NewSealer(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	ct, nonce, err := s.Seal("vendor", "sk_live_123")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ct), "sk_live_123") {
		t.Fatal("ciphertext contains the value")
	}
	got, err := s.Open("vendor", ct, nonce, s.KeyID())
	if err != nil || got != "sk_live_123" {
		t.Fatalf("open: %q, %v", got, err)
	}
	// Bound to its name: a row copied under another name does not open.
	if _, err := s.Open("other", ct, nonce, s.KeyID()); err == nil {
		t.Fatal("opened under another name")
	}
	// A different key does not open it, and says so before trying.
	s2, _ := NewSealer(testKey(t))
	if _, err := s2.Open("vendor", ct, nonce, s.KeyID()); err == nil || !strings.Contains(err.Error(), "sealed with key") {
		t.Fatalf("other key: %v", err)
	}
	// A revoked row has no nonce.
	if _, err := s.Open("vendor", nil, nil, s.KeyID()); err == nil {
		t.Fatal("opened an empty row")
	}
}

func TestNewSealerRejectsBadKeys(t *testing.T) {
	for _, k := range []string{"", "not base64!", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := NewSealer(k); err == nil {
			t.Errorf("key %q accepted", k)
		}
	}
}

func TestPolicy(t *testing.T) {
	p := Policy{Hosts: []string{" API.Vendor.com "}, Methods: []string{"get"}}
	p.Normalize()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if p.Header != "Authorization" || p.HeaderValue("k") != "Bearer k" || p.Users != UsersOwner {
		t.Fatalf("defaults: %+v", p)
	}
	if !p.AllowsHost("api.vendor.com") || p.AllowsHost("evil.com") || !p.AllowsMethod("GET") || p.AllowsMethod("POST") {
		t.Fatal("allow checks")
	}

	bad := []Policy{
		{},
		{Hosts: []string{"https://api.vendor.com"}},
		{Hosts: []string{"api.vendor.com/v1"}},
		{Hosts: []string{"10.0.0.1"}},
		{Hosts: []string{"localhost"}},
		{Hosts: []string{"api.vendor.com:99999"}},
		{Hosts: []string{"api.vendor.com"}, Format: "Bearer"},
		{Hosts: []string{"api.vendor.com"}, Format: "a {value}\r\nX-Evil: 1"},
		{Hosts: []string{"api.vendor.com"}, Header: "Bad Header"},
		{Hosts: []string{"api.vendor.com"}, Users: "everyone"},
		{Hosts: []string{"api.vendor.com"}, Methods: []string{"TRACE"}},
		{Hosts: []string{"api.vendor.com"}, Query: "key", Header: "X-Key"},
	}
	for i, b := range bad {
		b.Normalize()
		if err := b.Validate(); err == nil {
			t.Errorf("case %d validated: %+v", i, b)
		}
	}

	q := Policy{Hosts: []string{"maps.example.com:8443"}, Query: "key", Users: UsersAny}
	q.Normalize()
	if err := q.Validate(); err != nil || q.Header != "" {
		t.Fatalf("query policy: %v %+v", err, q)
	}
	round, err := DecodePolicy(q.Encode())
	if err != nil || round.Query != "key" || round.Users != UsersAny {
		t.Fatalf("encode round trip: %+v %v", round, err)
	}
}

func TestValidName(t *testing.T) {
	for _, n := range []string{"stripe", "vendor-api", "a_b-1"} {
		if !ValidName(n) {
			t.Errorf("%q refused", n)
		}
	}
	for _, n := range []string{"", "Stripe", "-x", "a/b", "a b", strings.Repeat("a", 70)} {
		if ValidName(n) {
			t.Errorf("%q accepted", n)
		}
	}
}
