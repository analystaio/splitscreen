// Package store is the gateway's durable state: routing bindings, the audit
// log, and usage accounting.
package store

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver; keeps static builds static
)

//go:embed schema.sql
var schema string

// ErrNotFound is returned when a lookup misses.
var ErrNotFound = errors.New("store: not found")

// Store wraps the gateway database. The gateway is the only writer, so the
// connection pool is capped at one writer to avoid SQLITE_BUSY entirely.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open initializes the database at path, applying the schema if needed. Use
// ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)&_pragma=synchronous(NORMAL)"
	if path == ":memory:" {
		dsn = "file::memory:?cache=shared&_pragma=busy_timeout(5000)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: apply schema: %w", err)
	}
	if _, err := db.Exec(
		`INSERT OR IGNORE INTO schema_migrations(version, applied_at) VALUES (1, ?)`,
		nowString(time.Now()),
	); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: record migration: %w", err)
	}
	return &Store{db: db, now: time.Now}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// SetClock overrides time for deterministic tests.
func (s *Store) SetClock(f func() time.Time) { s.now = f }

func nowString(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
func (s *Store) ts() string        { return nowString(s.now()) }

// ---------------------------------------------------------------------------
// Threads
// ---------------------------------------------------------------------------

// Thread is a conversation bound to a runner.
type Thread struct {
	ID             string
	Surface        string
	Channel        string
	Runner         string
	SessionID      string
	BundleVersion  int
	Stale          bool
	CreatedAt      time.Time
	LastActivityAt time.Time
}

// BindThread returns the existing binding for a thread, or creates one against
// runner. Bindings are sticky: an existing thread keeps its runner even after
// routing changes, because its session lives on that runner's disk.
func (s *Store) BindThread(id, surface, channel, runner string) (*Thread, bool, error) {
	existing, err := s.Thread(id)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}
	now := s.ts()
	_, err = s.db.Exec(`
		INSERT INTO threads (id, surface, channel, runner, created_at, last_activity_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		id, surface, channel, runner, now, now)
	if err != nil {
		return nil, false, fmt.Errorf("store: bind thread: %w", err)
	}
	t, err := s.Thread(id)
	return t, true, err
}

func (s *Store) Thread(id string) (*Thread, error) {
	row := s.db.QueryRow(`
		SELECT id, surface, channel, runner, COALESCE(session_id,''), bundle_version,
		       stale, created_at, last_activity_at
		FROM threads WHERE id = ?`, id)
	var t Thread
	var stale int
	var created, last string
	err := row.Scan(&t.ID, &t.Surface, &t.Channel, &t.Runner, &t.SessionID,
		&t.BundleVersion, &stale, &created, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: thread: %w", err)
	}
	t.Stale = stale != 0
	t.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	t.LastActivityAt, _ = time.Parse(time.RFC3339Nano, last)
	return &t, nil
}

func (s *Store) TouchThread(id string) error {
	_, err := s.db.Exec(`UPDATE threads SET last_activity_at = ? WHERE id = ?`, s.ts(), id)
	return err
}

func (s *Store) SetThreadSession(id, sessionID string) error {
	_, err := s.db.Exec(`UPDATE threads SET session_id = ? WHERE id = ?`, sessionID, id)
	return err
}

// RebindThread moves a thread to a different runner, abandoning the old
// session. Only reached through an explicit user command.
func (s *Store) RebindThread(id, runner string) error {
	_, err := s.db.Exec(
		`UPDATE threads SET runner = ?, session_id = NULL, stale = 0 WHERE id = ?`,
		runner, id)
	return err
}

// ClearSession drops the session id, so the next message starts fresh.
func (s *Store) ClearSession(id string) error {
	_, err := s.db.Exec(`UPDATE threads SET session_id = NULL, stale = 0 WHERE id = ?`, id)
	return err
}

// MarkRunnerThreadsStale flags every live thread on a runner as running against
// an older bundle. Config changes are announced rather than discovered.
func (s *Store) MarkRunnerThreadsStale(runner string, version int) (int64, error) {
	res, err := s.db.Exec(
		`UPDATE threads SET stale = 1 WHERE runner = ? AND bundle_version < ? AND stale = 0`,
		runner, version)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) SetThreadBundleVersion(id string, version int) error {
	_, err := s.db.Exec(`UPDATE threads SET bundle_version = ?, stale = 0 WHERE id = ?`, version, id)
	return err
}

// ---------------------------------------------------------------------------
// Turns
// ---------------------------------------------------------------------------

const (
	TurnRunning = "running"
	TurnDone    = "done"
	TurnError   = "error"
)

type Turn struct {
	ID           string
	ThreadID     string
	Channel      string
	Runner       string
	SurfaceUser  string
	Status       string
	StartedAt    time.Time
	NumToolCalls int
}

func (s *Store) StartTurn(t Turn) error {
	_, err := s.db.Exec(`
		INSERT INTO turns (id, thread_id, channel, runner, surface_user, status, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.ThreadID, t.Channel, t.Runner, t.SurfaceUser, TurnRunning, s.ts())
	if err != nil {
		return fmt.Errorf("store: start turn: %w", err)
	}
	return nil
}

// Usage records token counters for a turn. Cost is computed by the caller from
// a versioned price table; runners never price their own work.
type Usage struct {
	Model            string
	InputTokens      int64
	CacheWriteTokens int64
	CacheReadTokens  int64
	OutputTokens     int64
	TTLHint          string
	Known            bool
	ComputedUSD      *float64
	ReportedUSD      *float64
	PriceTableVer    string
}

func (s *Store) RecordUsage(turnID string, u Usage) error {
	_, err := s.db.Exec(`
		UPDATE turns SET
			model = ?, input_tokens = ?, cache_write_tokens = ?, cache_read_tokens = ?,
			output_tokens = ?, ttl_hint = ?, usage_known = ?,
			cost_computed_usd = ?, cost_reported_usd = ?, price_table_version = ?
		WHERE id = ?`,
		nullString(u.Model), u.InputTokens, u.CacheWriteTokens, u.CacheReadTokens,
		u.OutputTokens, nullString(u.TTLHint), boolInt(u.Known),
		u.ComputedUSD, u.ReportedUSD, nullString(u.PriceTableVer), turnID)
	if err != nil {
		return fmt.Errorf("store: record usage: %w", err)
	}
	return nil
}

func (s *Store) FinishTurn(turnID, status, errMsg string, durationMS int64, toolCalls int) error {
	_, err := s.db.Exec(`
		UPDATE turns SET status = ?, error = ?, ended_at = ?, duration_ms = ?, num_tool_calls = ?
		WHERE id = ?`,
		status, nullString(errMsg), s.ts(), durationMS, toolCalls, turnID)
	return err
}

// FailOpenTurns marks every running turn as errored. Called at gateway
// startup: a restart empties the in-memory turn map, and a Done frame for an
// unknown turn is dropped, so a row still running at boot can never finish —
// left alone it sits as "running" forever and poisons any liveness query.
func (s *Store) FailOpenTurns(reason string) (int64, error) {
	res, err := s.db.Exec(
		`UPDATE turns SET status = ?, error = ?, ended_at = ? WHERE status = ?`,
		TurnError, reason, s.ts(), TurnRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) IncrementToolCalls(turnID string) error {
	_, err := s.db.Exec(`UPDATE turns SET num_tool_calls = num_tool_calls + 1 WHERE id = ?`, turnID)
	return err
}

// CostRow is one line of a cost rollup.
type CostRow struct {
	Key          string
	Turns        int64
	Input        int64
	CacheWrite   int64
	CacheRead    int64
	Output       int64
	CostUSD      float64
	UnknownTurns int64
}

// CostByRunner rolls up spend since `since`. UnknownTurns is reported
// separately: a report showing $0 for an un-instrumented harness is worse than
// one showing nothing.
func (s *Store) CostByRunner(since time.Time) ([]CostRow, error) {
	return s.costBy("runner", since)
}

func (s *Store) CostByChannel(since time.Time) ([]CostRow, error) {
	return s.costBy("channel", since)
}

func (s *Store) CostByUser(since time.Time) ([]CostRow, error) {
	return s.costBy("surface_user", since)
}

func (s *Store) costBy(column string, since time.Time) ([]CostRow, error) {
	// column is not user input: it comes from the three wrappers above.
	q := fmt.Sprintf(`
		SELECT %s AS k,
		       COUNT(*),
		       COALESCE(SUM(input_tokens),0), COALESCE(SUM(cache_write_tokens),0),
		       COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(output_tokens),0),
		       COALESCE(SUM(cost_computed_usd),0),
		       COALESCE(SUM(CASE WHEN usage_known = 0 THEN 1 ELSE 0 END),0)
		FROM turns WHERE started_at >= ?
		GROUP BY k ORDER BY 7 DESC, 2 DESC`, column)
	rows, err := s.db.Query(q, nowString(since))
	if err != nil {
		return nil, fmt.Errorf("store: cost rollup: %w", err)
	}
	defer rows.Close()
	var out []CostRow
	for rows.Next() {
		var r CostRow
		if err := rows.Scan(&r.Key, &r.Turns, &r.Input, &r.CacheWrite, &r.CacheRead,
			&r.Output, &r.CostUSD, &r.UnknownTurns); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TopThreads returns the most expensive threads since `since`.
func (s *Store) TopThreads(since time.Time, limit int) ([]CostRow, error) {
	rows, err := s.db.Query(`
		SELECT thread_id, COUNT(*), COALESCE(SUM(cost_computed_usd),0)
		FROM turns WHERE started_at >= ?
		GROUP BY thread_id ORDER BY 3 DESC LIMIT ?`, nowString(since), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CostRow
	for rows.Next() {
		var r CostRow
		if err := rows.Scan(&r.Key, &r.Turns, &r.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

type Event struct {
	Kind        string
	Runner      string
	ThreadID    string
	TurnID      string
	SurfaceUser string
	Detail      any
}

// Log appends to the audit stream. Failures are returned rather than swallowed:
// a gateway that cannot audit should be visibly broken.
func (s *Store) Log(e Event) error {
	var detail *string
	if e.Detail != nil {
		b, err := json.Marshal(e.Detail)
		if err != nil {
			return fmt.Errorf("store: marshal event detail: %w", err)
		}
		str := string(b)
		detail = &str
	}
	_, err := s.db.Exec(`
		INSERT INTO events (at, kind, runner, thread_id, turn_id, surface_user, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		s.ts(), e.Kind, nullString(e.Runner), nullString(e.ThreadID),
		nullString(e.TurnID), nullString(e.SurfaceUser), detail)
	if err != nil {
		return fmt.Errorf("store: log event: %w", err)
	}
	return nil
}

func (s *Store) RecordPermissionRequest(requestID, threadID, turnID, runner, tool, input string) error {
	_, err := s.db.Exec(`
		INSERT INTO permissions (request_id, thread_id, turn_id, runner, tool, input, requested_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		requestID, nullString(threadID), nullString(turnID), runner, tool, nullString(input), s.ts())
	return err
}

func (s *Store) RecordPermissionDecision(requestID, decision, decidedBy, reason string, policyDenied bool) error {
	_, err := s.db.Exec(`
		UPDATE permissions SET decision = ?, decided_by = ?, reason = ?, policy_denied = ?, decided_at = ?
		WHERE request_id = ?`,
		decision, nullString(decidedBy), nullString(reason), boolInt(policyDenied), s.ts(), requestID)
	return err
}

type BlobRecord struct {
	ID          string
	Direction   string
	ThreadID    string
	TurnID      string
	Runner      string
	Name        string
	Mime        string
	Size        int64
	SHA256      string
	SurfaceUser string
	OK          bool
	Error       string
}

func (s *Store) RecordBlob(b BlobRecord) error {
	_, err := s.db.Exec(`
		INSERT INTO blobs (id, direction, thread_id, turn_id, runner, name, mime, size,
		                   sha256, surface_user, ok, error, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET ok = excluded.ok, error = excluded.error`,
		b.ID, b.Direction, nullString(b.ThreadID), nullString(b.TurnID), nullString(b.Runner),
		b.Name, nullString(b.Mime), b.Size, nullString(b.SHA256), nullString(b.SurfaceUser),
		boolInt(b.OK), nullString(b.Error), s.ts())
	return err
}

type CredentialRecord struct {
	RequestID   string
	Runner      string
	Kind        string
	Resource    string
	ThreadID    string
	TurnID      string
	SurfaceUser string
	Granted     bool
	Reason      string
	ExpiresAt   *time.Time
}

func (s *Store) RecordCredential(c CredentialRecord) error {
	var exp *string
	if c.ExpiresAt != nil {
		e := nowString(*c.ExpiresAt)
		exp = &e
	}
	_, err := s.db.Exec(`
		INSERT INTO credential_grants (request_id, runner, kind, resource, thread_id, turn_id,
		                               surface_user, granted, reason, expires_at, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.RequestID, c.Runner, c.Kind, nullString(c.Resource), nullString(c.ThreadID),
		nullString(c.TurnID), nullString(c.SurfaceUser), boolInt(c.Granted),
		nullString(c.Reason), exp, s.ts())
	return err
}

type MCPRecord struct {
	CallID      string
	Runner      string
	ThreadID    string
	TurnID      string
	SurfaceUser string
	Server      string
	Tool        string
	Args        string
	OK          bool
	Error       string
	DurationMS  int64
}

func (s *Store) RecordMCPCall(m MCPRecord) error {
	_, err := s.db.Exec(`
		INSERT INTO mcp_calls (call_id, runner, thread_id, turn_id, surface_user, server, tool,
		                       args, ok, error, duration_ms, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(call_id) DO UPDATE SET ok = excluded.ok, error = excluded.error,
		                                   duration_ms = excluded.duration_ms`,
		m.CallID, nullString(m.Runner), nullString(m.ThreadID), nullString(m.TurnID),
		nullString(m.SurfaceUser), m.Server, m.Tool, nullString(m.Args),
		boolInt(m.OK), nullString(m.Error), m.DurationMS, s.ts())
	return err
}

// ---------------------------------------------------------------------------
// Offline queue
// ---------------------------------------------------------------------------

// Enqueue persists a frame for a runner that is not currently connected. The
// gateway writes here before dispatch, so a runner restart cannot silently eat
// traffic.
func (s *Store) Enqueue(runner, threadID string, frame []byte) error {
	_, err := s.db.Exec(
		`INSERT INTO queued_messages (runner, thread_id, frame, at) VALUES (?, ?, ?, ?)`,
		runner, threadID, string(frame), s.ts())
	return err
}

type QueuedMessage struct {
	ID       int64
	ThreadID string
	Frame    []byte
}

func (s *Store) Dequeue(runner string, limit int) ([]QueuedMessage, error) {
	rows, err := s.db.Query(
		`SELECT id, thread_id, frame FROM queued_messages WHERE runner = ? ORDER BY id LIMIT ?`,
		runner, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueuedMessage
	for rows.Next() {
		var m QueuedMessage
		var frame string
		if err := rows.Scan(&m.ID, &m.ThreadID, &frame); err != nil {
			return nil, err
		}
		m.Frame = []byte(frame)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) DeleteQueued(id int64) error {
	_, err := s.db.Exec(`DELETE FROM queued_messages WHERE id = ?`, id)
	return err
}

// PurgeQueue deletes everything queued for a runner, returning how many rows
// went. Used when a runner is removed: nothing will ever drain its queue.
func (s *Store) PurgeQueue(runner string) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM queued_messages WHERE runner = ?`, runner)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) QueueDepth(runner string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM queued_messages WHERE runner = ?`, runner).Scan(&n)
	return n, err
}

// ---------------------------------------------------------------------------

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
