// Package cron parses standard five-field cron expressions and computes when
// they next fire.
//
// It is deliberately the classic Vixie dialect and nothing more: minute, hour,
// day of month, month, day of week, with lists, ranges, steps and three-letter
// names. No seconds field, no @every, no "L" or "#". Schedules are written by an
// agent from a person's plain-language request and shown back to that person
// as concrete times, so a small, unsurprising dialect is worth more than a rich
// one.
package cron

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Spec is a parsed expression. Each field is a bitset of the values it allows.
type Spec struct {
	expr   string
	minute uint64 // 0-59
	hour   uint64 // 0-23
	dom    uint64 // 1-31
	month  uint64 // 1-12
	dow    uint64 // 0-6, Sunday is 0
	// Vixie cron: when both day fields are restricted, a day matches if EITHER
	// does. "0 9 1 * 1" is the 1st of the month and every Monday, not Mondays
	// that fall on the 1st.
	domStar, dowStar bool
}

type field struct {
	name     string
	min, max int
	names    map[string]int
}

var (
	minuteField = field{name: "minute", min: 0, max: 59}
	hourField   = field{name: "hour", min: 0, max: 23}
	domField    = field{name: "day of month", min: 1, max: 31}
	monthField  = field{name: "month", min: 1, max: 12, names: map[string]int{
		"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	}}
	// 7 is accepted as Sunday, as most crons do, and folded onto 0.
	dowField = field{name: "day of week", min: 0, max: 7, names: map[string]int{
		"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
	}}
)

// Parse reads a five-field expression.
func Parse(expr string) (*Spec, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron: %q has %d fields; want 5 (minute hour day-of-month month day-of-week)", expr, len(fields))
	}
	s := &Spec{expr: strings.Join(fields, " ")}
	var err error
	if s.minute, err = minuteField.parse(fields[0]); err != nil {
		return nil, err
	}
	if s.hour, err = hourField.parse(fields[1]); err != nil {
		return nil, err
	}
	if s.dom, err = domField.parse(fields[2]); err != nil {
		return nil, err
	}
	if s.month, err = monthField.parse(fields[3]); err != nil {
		return nil, err
	}
	if s.dow, err = dowField.parse(fields[4]); err != nil {
		return nil, err
	}
	if s.dow&(1<<7) != 0 {
		s.dow = s.dow&^(1<<7) | 1
	}
	s.domStar = isStar(fields[2])
	s.dowStar = isStar(fields[4])
	// "0 0 31 2 *" parses but never fires. Catch it here rather than letting
	// Next search four years and give up.
	if s.Next(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)).IsZero() {
		return nil, fmt.Errorf("cron: %q never fires", expr)
	}
	return s, nil
}

// String returns the expression, normalized to single spaces.
func (s *Spec) String() string { return s.expr }

func isStar(f string) bool { return f == "*" || f == "?" }

func (f field) parse(text string) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(text, ",") {
		b, err := f.parsePart(part)
		if err != nil {
			return 0, err
		}
		bits |= b
	}
	return bits, nil
}

func (f field) parsePart(part string) (uint64, error) {
	rng, stepText, hasStep := strings.Cut(part, "/")
	step := 1
	if hasStep {
		n, err := strconv.Atoi(stepText)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("cron: bad step %q in %s field", stepText, f.name)
		}
		step = n
	}

	var lo, hi int
	switch {
	case rng == "*" || rng == "?":
		lo, hi = f.min, f.max
		if f.name == dowField.name {
			hi = 6 // a star over days of week must not count Sunday twice
		}
	case strings.Contains(rng, "-"):
		a, b, _ := strings.Cut(rng, "-")
		var err error
		if lo, err = f.value(a); err != nil {
			return 0, err
		}
		if hi, err = f.value(b); err != nil {
			return 0, err
		}
		if lo > hi {
			return 0, fmt.Errorf("cron: range %q runs backwards in %s field", rng, f.name)
		}
	default:
		v, err := f.value(rng)
		if err != nil {
			return 0, err
		}
		lo, hi = v, v
		if hasStep {
			hi = f.max // "5/15" means from 5 to the end, every 15
		}
	}

	var bits uint64
	for v := lo; v <= hi; v += step {
		bits |= 1 << uint(v)
	}
	return bits, nil
}

func (f field) value(text string) (int, error) {
	if v, ok := f.names[strings.ToLower(text)]; ok {
		return v, nil
	}
	v, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("cron: %q is not a valid %s", text, f.name)
	}
	if v < f.min || v > f.max {
		return 0, fmt.Errorf("cron: %d is out of range for %s (%d-%d)", v, f.name, f.min, f.max)
	}
	return v, nil
}

func (s *Spec) dayMatches(t time.Time) bool {
	dom := s.dom&(1<<uint(t.Day())) != 0
	dow := s.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dow
	case s.dowStar:
		return dom
	default:
		return dom || dow
	}
}

// searchYears bounds Next. Anything that parses fires at least once in a leap
// cycle, so this only matters for expressions Parse already refuses.
const searchYears = 5

// Next returns the first time strictly after `after` that the expression
// matches, in after's location. It returns the zero time if there is none.
//
// Times are computed on the wall clock of that location. A time skipped by a
// daylight-saving jump does not exist and is skipped; a time that occurs twice
// fires once, on its first occurrence.
func (s *Spec) Next(after time.Time) time.Time {
	loc := after.Location()
	t := after.Truncate(time.Minute).Add(time.Minute)
	// Work in wall-clock fields and rebuild with time.Date, which normalizes
	// overflow. Each step jumps to the start of the next candidate unit rather
	// than walking minute by minute.
	limit := t.Year() + searchYears
	for t.Year() <= limit {
		if s.month&(1<<uint(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
			continue
		}
		if !s.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
			continue
		}
		if s.hour&(1<<uint(t.Hour())) == 0 {
			next := time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc)
			if !next.After(t) { // a DST fall-back hour repeats; step past it
				next = t.Add(time.Hour).Truncate(time.Hour)
			}
			t = next
			continue
		}
		if s.minute&(1<<uint(t.Minute())) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		if !t.After(after) {
			// Only reachable when a DST fall-back rebuilt t onto the earlier
			// copy of a repeated hour.
			t = t.Add(time.Minute)
			continue
		}
		if repeated(t) {
			// The second pass through a fall-back hour: this wall-clock time
			// already fired an hour ago.
			t = t.Add(time.Duration(60-t.Minute()) * time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

// repeated reports that t's wall-clock time also occurred an hour earlier,
// which is true only in the second copy of a daylight-saving fall-back hour.
func repeated(t time.Time) bool {
	e := t.Add(-time.Hour)
	return e.Hour() == t.Hour() && e.Day() == t.Day()
}

// Upcoming returns the next n fire times after `after`.
func (s *Spec) Upcoming(after time.Time, n int) []time.Time {
	out := make([]time.Time, 0, n)
	t := after
	for range n {
		t = s.Next(t)
		if t.IsZero() {
			break
		}
		out = append(out, t)
	}
	return out
}

// ErrTooFrequent is returned by CheckInterval.
var ErrTooFrequent = errors.New("cron: fires too often")

// CheckInterval refuses an expression that ever fires twice within min of
// itself, looking far enough ahead to see a full week of its pattern.
func (s *Spec) CheckInterval(from time.Time, min time.Duration) error {
	prev := s.Next(from)
	end := prev.AddDate(0, 0, 8)
	for i := 0; i < 5000 && !prev.IsZero() && prev.Before(end); i++ {
		next := s.Next(prev)
		if next.IsZero() {
			return nil
		}
		if gap := next.Sub(prev); gap < min {
			return fmt.Errorf("%w: %q runs %s apart (%s and %s); the minimum is %s",
				ErrTooFrequent, s.expr, gap, prev.Format("Mon 15:04"), next.Format("15:04"), min)
		}
		prev = next
	}
	return nil
}
