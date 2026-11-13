package cron

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func la(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	return loc
}

func mustParse(t *testing.T, expr string) *Spec {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	return s
}

func TestNext(t *testing.T) {
	utc := time.UTC
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, utc)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		expr, after, want string
	}{
		{"* * * * *", "2026-11-13 10:00", "2026-11-13 10:01"},
		{"0 9 * * *", "2026-11-13 08:59", "2026-11-13 09:00"},
		{"0 9 * * *", "2026-11-13 09:00", "2026-11-14 09:00"},   // strictly after
		{"0 9 * * 1-5", "2026-11-13 09:00", "2026-11-16 09:00"}, // Fri -> Mon
		{"0 9 * * mon-fri", "2026-11-14 12:00", "2026-11-16 09:00"},
		{"*/15 * * * *", "2026-11-13 10:07", "2026-11-13 10:15"},
		{"5/20 * * * *", "2026-11-13 10:26", "2026-11-13 10:45"},
		{"30 8,17 * * *", "2026-11-13 09:00", "2026-11-13 17:30"},
		{"0 0 1 * *", "2026-11-13 00:00", "2026-12-01 00:00"},
		{"0 0 1 jan *", "2026-11-13 00:00", "2027-01-01 00:00"},
		{"0 0 29 2 *", "2026-03-01 00:00", "2028-02-29 00:00"},
		{"0 12 * * 0", "2026-11-13 00:00", "2026-11-15 12:00"},
		{"0 12 * * 7", "2026-11-13 00:00", "2026-11-15 12:00"}, // 7 is Sunday
		// Both day fields restricted: either matches.
		{"0 9 1 * 1", "2026-11-13 00:00", "2026-11-16 09:00"},
		{"0 9 15 * 1", "2026-11-13 00:00", "2026-11-15 09:00"},
	}
	for _, c := range cases {
		got := mustParse(t, c.expr).Next(at(c.after))
		if want := at(c.want); !got.Equal(want) {
			t.Errorf("%q after %s = %s, want %s", c.expr, c.after, got.Format("2006-01-02 15:04 Mon"), c.want)
		}
	}
}

func TestNextKeepsWallClockAcrossDST(t *testing.T) {
	loc := la(t)
	s := mustParse(t, "0 9 * * *")
	// 2026-11-01 is the US fall-back day.
	got := s.Upcoming(time.Date(2026, 10, 30, 12, 0, 0, 0, loc), 3)
	for i, g := range got {
		if g.Hour() != 9 || g.Minute() != 0 {
			t.Errorf("run %d at %s, want 09:00 local", i, g)
		}
	}
	if got[1].Sub(got[0]) != 25*time.Hour {
		t.Errorf("fall-back day gap = %s, want 25h", got[1].Sub(got[0]))
	}
}

func TestSkippedHourDoesNotFireTwiceOrHang(t *testing.T) {
	loc := la(t)
	s := mustParse(t, "30 2 * * *")
	// 2027-03-14 02:30 does not exist in Los Angeles.
	got := s.Upcoming(time.Date(2027, 3, 13, 12, 0, 0, 0, loc), 3)
	if len(got) != 3 {
		t.Fatalf("got %d runs", len(got))
	}
	for i := 1; i < len(got); i++ {
		if !got[i].After(got[i-1]) {
			t.Fatalf("runs not increasing: %v", got)
		}
	}
}

func TestRepeatedHourFiresOnce(t *testing.T) {
	loc := la(t)
	s := mustParse(t, "30 1 * * *")
	got := s.Upcoming(time.Date(2026, 10, 31, 12, 0, 0, 0, loc), 2)
	if got[1].Day() == got[0].Day() {
		t.Fatalf("fired twice on fall-back day: %v", got)
	}
}

func TestParseRejects(t *testing.T) {
	for _, bad := range []string{
		"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *",
		"* * * 13 *", "* * * * 8", "*/0 * * * *", "5-1 * * * *", "x * * * *",
		"0 0 31 2 *", "@daily",
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

func TestCheckInterval(t *testing.T) {
	from := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	for _, ok := range []string{"0 9 * * 1-5", "*/15 * * * *", "0 * * * *", "0 9,17 * * *"} {
		if err := mustParse(t, ok).CheckInterval(from, 15*time.Minute); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"* * * * *", "*/5 * * * *", "0,10 9 * * *", "0,10 9 * * 3"} {
		err := mustParse(t, bad).CheckInterval(from, 15*time.Minute)
		if !errors.Is(err, ErrTooFrequent) {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}

func TestStringNormalizes(t *testing.T) {
	if got := mustParse(t, "  0   9 *  * 1-5 ").String(); got != "0 9 * * 1-5" {
		t.Errorf("String = %q", got)
	}
	_, err := Parse("0 9 * *")
	if err == nil || !strings.Contains(err.Error(), "5") {
		t.Errorf("field-count error unhelpful: %v", err)
	}
}
