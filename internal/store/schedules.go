package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Schedule states.
const (
	SchedulePending = "pending"
	ScheduleActive  = "active"
	SchedulePaused  = "paused"
)

// Schedule modes: where a run posts.
const (
	// ScheduleModeChannel starts a new thread in the channel for every run, on a
	// fresh session.
	ScheduleModeChannel = "channel"
	// ScheduleModeThread runs in the thread the schedule was created in, so each
	// run continues that conversation.
	ScheduleModeThread = "thread"
)

// Schedule is a prompt run on a cron expression.
type Schedule struct {
	ID             string
	Name           string
	Surface        string
	Channel        string
	Thread         string
	IsDM           bool
	Mode           string
	Runner         string
	Creator        string
	CreatorDisplay string
	Cron           string
	Timezone       string
	Prompt         string
	State          string
	CardID         string
	NextRunAt      time.Time // zero when not scheduled
	LastRunAt      time.Time
	LastResult     string
	Failures       int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

const scheduleCols = `id, name, surface, channel, thread, is_dm, mode, runner, creator,
	creator_display, cron, timezone, prompt, state, card_id, COALESCE(next_run_at,''),
	COALESCE(last_run_at,''), last_result, failures, created_at, updated_at`

func scanSchedule(row interface{ Scan(...any) error }) (*Schedule, error) {
	var s Schedule
	var dm int
	var next, last, created, updated string
	err := row.Scan(&s.ID, &s.Name, &s.Surface, &s.Channel, &s.Thread, &dm, &s.Mode,
		&s.Runner, &s.Creator, &s.CreatorDisplay, &s.Cron, &s.Timezone, &s.Prompt,
		&s.State, &s.CardID, &next, &last, &s.LastResult, &s.Failures, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: schedule: %w", err)
	}
	s.IsDM = dm != 0
	s.NextRunAt = parseTime(next)
	s.LastRunAt = parseTime(last)
	s.CreatedAt = parseTime(created)
	s.UpdatedAt = parseTime(updated)
	return &s, nil
}

func parseTime(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, v)
	return t
}

func timeOrNull(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return nowString(t)
}

// CreateSchedule inserts a schedule. ID, CreatedAt and UpdatedAt are filled by
// the caller and the store respectively.
func (s *Store) CreateSchedule(sc *Schedule) error {
	now := s.ts()
	_, err := s.db.Exec(`
		INSERT INTO schedules (id, name, surface, channel, thread, is_dm, mode, runner,
			creator, creator_display, cron, timezone, prompt, state, card_id,
			next_run_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sc.ID, sc.Name, sc.Surface, sc.Channel, sc.Thread, boolInt(sc.IsDM), sc.Mode,
		sc.Runner, sc.Creator, sc.CreatorDisplay, sc.Cron, sc.Timezone, sc.Prompt,
		sc.State, sc.CardID, timeOrNull(sc.NextRunAt), now, now)
	if err != nil {
		return fmt.Errorf("store: create schedule: %w", err)
	}
	return nil
}

// UpdateSchedule writes every mutable field back.
func (s *Store) UpdateSchedule(sc *Schedule) error {
	res, err := s.db.Exec(`
		UPDATE schedules SET name = ?, mode = ?, cron = ?, timezone = ?, prompt = ?,
			state = ?, card_id = ?, next_run_at = ?, last_run_at = ?, last_result = ?,
			failures = ?, updated_at = ?
		WHERE id = ?`,
		sc.Name, sc.Mode, sc.Cron, sc.Timezone, sc.Prompt, sc.State, sc.CardID,
		timeOrNull(sc.NextRunAt), timeOrNull(sc.LastRunAt), sc.LastResult, sc.Failures,
		s.ts(), sc.ID)
	if err != nil {
		return fmt.Errorf("store: update schedule: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Schedule reads one schedule.
func (s *Store) Schedule(id string) (*Schedule, error) {
	return scanSchedule(s.db.QueryRow(`SELECT `+scheduleCols+` FROM schedules WHERE id = ?`, id))
}

// DeleteSchedule removes a schedule.
func (s *Store) DeleteSchedule(id string) error {
	res, err := s.db.Exec(`DELETE FROM schedules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete schedule: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DueSchedules returns active schedules whose next run is at or before now.
//
// next_run_at is always a whole minute, which RFC3339Nano writes with no
// fraction, so now is formatted the same way: comparing "…:00Z" against
// "…:00.123Z" as strings would put the fractionless one last.
func (s *Store) DueSchedules(now time.Time) ([]*Schedule, error) {
	return s.querySchedules(`SELECT `+scheduleCols+` FROM schedules
		WHERE state = ? AND next_run_at IS NOT NULL AND next_run_at <= ?
		ORDER BY next_run_at`, ScheduleActive, now.UTC().Format("2006-01-02T15:04:05Z"))
}

// SchedulesFor lists the schedules a person created, or that live in a channel,
// oldest first. Either filter may be empty.
func (s *Store) SchedulesFor(surface, creator, channel string) ([]*Schedule, error) {
	return s.querySchedules(`SELECT `+scheduleCols+` FROM schedules
		WHERE surface = ? AND ((? != '' AND creator = ?) OR (? != '' AND channel = ?))
		ORDER BY created_at`, surface, creator, creator, channel, channel)
}

// AllSchedules lists every schedule, oldest first.
func (s *Store) AllSchedules() ([]*Schedule, error) {
	return s.querySchedules(`SELECT ` + scheduleCols + ` FROM schedules ORDER BY created_at`)
}

// CountSchedulesBy counts a person's schedules in every state.
func (s *Store) CountSchedulesBy(surface, creator string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM schedules WHERE surface = ? AND creator = ?`,
		surface, creator).Scan(&n)
	return n, err
}

func (s *Store) querySchedules(q string, args ...any) ([]*Schedule, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: schedules: %w", err)
	}
	defer rows.Close()
	var out []*Schedule
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// SettleInterruptedRuns marks every run still recorded as "running" as cut
// short. Called at gateway startup, which fails every open turn: a run's own
// result can then never arrive to replace "running" on its card.
func (s *Store) SettleInterruptedRuns(reason string) (int64, error) {
	res, err := s.db.Exec(`UPDATE schedules SET last_result = ?, updated_at = ? WHERE last_result = 'running'`,
		reason, s.ts())
	if err != nil {
		return 0, fmt.Errorf("store: settle schedule runs: %w", err)
	}
	return res.RowsAffected()
}
