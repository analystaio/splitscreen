package store

import (
	"errors"
	"testing"
	"time"
)

func TestVaultSecretLifecycle(t *testing.T) {
	s := open(t)
	v := VaultSecret{Name: "vendor", Owner: "U1", Kind: "static", Ciphertext: []byte{1, 2}, Nonce: []byte{3}, KeyID: "k", Policy: `{}`}
	if err := s.PutVaultSecret(v); err != nil {
		t.Fatal(err)
	}
	// Someone else cannot take a live name.
	other := v
	other.Owner = "U2"
	if err := s.PutVaultSecret(other); !errors.Is(err, ErrSecretOwned) {
		t.Fatalf("other owner: %v", err)
	}
	// The owner can rotate it.
	v.Ciphertext = []byte{9}
	if err := s.PutVaultSecret(v); err != nil {
		t.Fatal(err)
	}
	got, err := s.VaultSecret("vendor")
	if err != nil || string(got.Ciphertext) != "\x09" || got.Owner != "U1" {
		t.Fatalf("got %+v %v", got, err)
	}
	if err := s.TouchVaultSecret("vendor"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.VaultSecret("vendor"); got.LastUsedAt == nil {
		t.Fatal("use not recorded")
	}
	list, _ := s.VaultSecrets()
	if len(list) != 1 {
		t.Fatalf("list: %d", len(list))
	}

	// Revoking drops the ciphertext, hides it from the list, and frees the name.
	if err := s.RevokeVaultSecret("vendor"); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeVaultSecret("vendor"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second revoke: %v", err)
	}
	got, _ = s.VaultSecret("vendor")
	if got.RevokedAt == nil || len(got.Ciphertext) != 0 || len(got.Nonce) != 0 {
		t.Fatalf("revoked row kept its value: %+v", got)
	}
	if list, _ := s.VaultSecrets(); len(list) != 0 {
		t.Fatal("revoked secret listed")
	}
	if err := s.PutVaultSecret(other); err != nil {
		t.Fatalf("reuse of a revoked name: %v", err)
	}
}

func TestVaultIntakeClaimsOnce(t *testing.T) {
	s := open(t)
	now := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })
	in := VaultIntake{TokenHash: "h", Name: "vendor", Request: `{}`, Surface: "slack", SurfaceUser: "U1",
		Runner: "r", Channel: "C1", ExpiresAt: now.Add(15 * time.Minute)}
	if err := s.CreateVaultIntake(in); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimVaultIntake("h"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimVaultIntake("h"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second claim: %v", err)
	}
	if err := s.ClaimVaultIntake("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}

	in.TokenHash = "h2"
	if err := s.CreateVaultIntake(in); err != nil {
		t.Fatal(err)
	}
	now = now.Add(16 * time.Minute)
	if err := s.ClaimVaultIntake("h2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired claim: %v", err)
	}
}
