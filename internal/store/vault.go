package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// VaultSecret is one stored secret. Ciphertext and Nonce are opaque here: the
// store never sees a value in the clear.
type VaultSecret struct {
	Name       string
	Owner      string
	Kind       string
	Ciphertext []byte
	Nonce      []byte
	KeyID      string
	Policy     string // JSON
	CreatedAt  time.Time
	UpdatedAt  time.Time
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// ErrSecretOwned is returned when a name is taken by someone else's secret.
var ErrSecretOwned = errors.New("store: that secret name belongs to someone else")

func optTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return nowString(*t)
}

func scanOptTime(ns sql.NullString) *time.Time {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	t := parseTime(ns.String)
	return &t
}

// PutVaultSecret creates a secret, or replaces the value and policy of one the
// same owner already has (rotation). A revoked secret's name can be reused by
// anyone; a live one only by its owner.
func (s *Store) PutVaultSecret(v VaultSecret) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var owner string
	var revoked sql.NullString
	err = tx.QueryRow(`SELECT owner, revoked_at FROM vault_secrets WHERE name = ?`, v.Name).Scan(&owner, &revoked)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	case owner != v.Owner && !revoked.Valid:
		return ErrSecretOwned
	}

	now := s.ts()
	_, err = tx.Exec(`
		INSERT INTO vault_secrets (name, owner, kind, ciphertext, nonce, key_id, policy,
		                           created_at, updated_at, expires_at, last_used_at, revoked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL)
		ON CONFLICT(name) DO UPDATE SET
			owner = excluded.owner, kind = excluded.kind, ciphertext = excluded.ciphertext,
			nonce = excluded.nonce, key_id = excluded.key_id, policy = excluded.policy,
			updated_at = excluded.updated_at, expires_at = excluded.expires_at,
			last_used_at = NULL, revoked_at = NULL`,
		v.Name, v.Owner, v.Kind, v.Ciphertext, v.Nonce, v.KeyID, v.Policy,
		now, now, optTime(v.ExpiresAt))
	if err != nil {
		return fmt.Errorf("store: put vault secret: %w", err)
	}
	return tx.Commit()
}

const vaultCols = `name, owner, kind, ciphertext, nonce, key_id, policy, created_at, updated_at,
	expires_at, last_used_at, revoked_at`

func scanVault(row interface{ Scan(...any) error }) (VaultSecret, error) {
	var v VaultSecret
	var created, updated string
	var exp, used, rev sql.NullString
	if err := row.Scan(&v.Name, &v.Owner, &v.Kind, &v.Ciphertext, &v.Nonce, &v.KeyID, &v.Policy,
		&created, &updated, &exp, &used, &rev); err != nil {
		return v, err
	}
	v.CreatedAt, v.UpdatedAt = parseTime(created), parseTime(updated)
	v.ExpiresAt, v.LastUsedAt, v.RevokedAt = scanOptTime(exp), scanOptTime(used), scanOptTime(rev)
	return v, nil
}

// VaultSecret returns a secret by name, revoked or not.
func (s *Store) VaultSecret(name string) (VaultSecret, error) {
	v, err := scanVault(s.db.QueryRow(`SELECT `+vaultCols+` FROM vault_secrets WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	return v, err
}

// VaultSecrets lists every secret that is not revoked, by name.
func (s *Store) VaultSecrets() ([]VaultSecret, error) {
	rows, err := s.db.Query(`SELECT ` + vaultCols + ` FROM vault_secrets WHERE revoked_at IS NULL ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VaultSecret
	for rows.Next() {
		v, err := scanVault(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// RevokeVaultSecret marks a secret revoked and drops its ciphertext, so a
// revoked value cannot be recovered even with the key.
func (s *Store) RevokeVaultSecret(name string) error {
	res, err := s.db.Exec(`UPDATE vault_secrets SET revoked_at = ?, ciphertext = x'', nonce = x''
		WHERE name = ? AND revoked_at IS NULL`, s.ts(), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchVaultSecret records a use.
func (s *Store) TouchVaultSecret(name string) error {
	_, err := s.db.Exec(`UPDATE vault_secrets SET last_used_at = ? WHERE name = ?`, s.ts(), name)
	return err
}

// VaultIntake is a pending one-time link.
type VaultIntake struct {
	TokenHash   string
	Name        string
	Request     string // JSON: what the agent asked for
	Surface     string
	SurfaceUser string
	Runner      string
	ThreadID    string
	Channel     string
	ThreadTS    string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	UsedAt      *time.Time
}

// CreateVaultIntake stores a pending link.
func (s *Store) CreateVaultIntake(in VaultIntake) error {
	_, err := s.db.Exec(`INSERT INTO vault_intakes (token_hash, name, request, surface, surface_user,
		runner, thread_id, channel, thread_ts, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.TokenHash, in.Name, in.Request, in.Surface, in.SurfaceUser, in.Runner, in.ThreadID,
		in.Channel, in.ThreadTS, s.ts(), nowString(in.ExpiresAt))
	return err
}

// VaultIntake looks a link up by its token hash.
func (s *Store) VaultIntake(tokenHash string) (VaultIntake, error) {
	var in VaultIntake
	var created, expires string
	var thread, ts, used sql.NullString
	err := s.db.QueryRow(`SELECT token_hash, name, request, surface, surface_user, runner, thread_id,
		channel, thread_ts, created_at, expires_at, used_at FROM vault_intakes WHERE token_hash = ?`, tokenHash).
		Scan(&in.TokenHash, &in.Name, &in.Request, &in.Surface, &in.SurfaceUser, &in.Runner, &thread,
			&in.Channel, &ts, &created, &expires, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return in, ErrNotFound
	}
	if err != nil {
		return in, err
	}
	in.ThreadID, in.ThreadTS = thread.String, ts.String
	in.CreatedAt, in.ExpiresAt, in.UsedAt = parseTime(created), parseTime(expires), scanOptTime(used)
	return in, nil
}

// ClaimVaultIntake marks a link used. It succeeds once: a second claim, or a
// claim after expiry, returns ErrNotFound.
func (s *Store) ClaimVaultIntake(tokenHash string) error {
	// Expiry is compared here, not in SQL: RFC 3339 with trimmed fractions does
	// not sort as text.
	in, err := s.VaultIntake(tokenHash)
	if err != nil {
		return err
	}
	if !s.now().Before(in.ExpiresAt) {
		return ErrNotFound
	}
	res, err := s.db.Exec(`UPDATE vault_intakes SET used_at = ? WHERE token_hash = ? AND used_at IS NULL`,
		s.ts(), tokenHash)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// EventRow is one audit record as stored.
type EventRow struct {
	At          time.Time
	Kind        string
	Runner      string
	ThreadID    string
	TurnID      string
	SurfaceUser string
	Detail      string
}

// EventsByKind returns the newest events whose kind starts with prefix.
func (s *Store) EventsByKind(prefix string, limit int) ([]EventRow, error) {
	rows, err := s.db.Query(`SELECT at, kind, COALESCE(runner,''), COALESCE(thread_id,''), COALESCE(turn_id,''),
		COALESCE(surface_user,''), COALESCE(detail,'') FROM events WHERE substr(kind, 1, ?) = ?
		ORDER BY id DESC LIMIT ?`, len(prefix), prefix, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var e EventRow
		var at string
		if err := rows.Scan(&at, &e.Kind, &e.Runner, &e.ThreadID, &e.TurnID, &e.SurfaceUser, &e.Detail); err != nil {
			return nil, err
		}
		e.At = parseTime(at)
		out = append(out, e)
	}
	return out, rows.Err()
}
