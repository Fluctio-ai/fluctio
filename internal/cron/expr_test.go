package cron

import (
	"testing"
	"time"
)

func TestNextOccurrenceWeekdayFields(t *testing.T) {
	// Friday 2026-11-06 08:00 UTC — past Friday's 07:30, so a weekday
	// rule must roll to next Monday.
	after := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	cases := []struct{ expr, want string }{
		{"30 7 * * 1-5", "2026-11-09 07:30"}, // Mon-Fri
		{"30 7 * * MON-FRI", "2026-11-09 07:30"},
		{"30 7 * * mon-fri", "2026-11-09 07:30"},
		{"30 7 * * 1,3,5", "2026-11-09 07:30"}, // list
		{"30 7 * * 2,4", "2026-11-10 07:30"},   // list → Tuesday
		{"30 7 * * 1-5/2", "2026-11-09 07:30"}, // range with step → Mon/Wed/Fri
		{"30 7 * * 6", "2026-11-07 07:30"},     // Saturday
		{"30 7 * * SAT", "2026-11-07 07:30"},
		{"30 7 * * 0", "2026-11-08 07:30"}, // Sunday as 0
		{"30 7 * * 7", "2026-11-08 07:30"}, // Sunday as 7
		{"30 7 * * SUN", "2026-11-08 07:30"},
		{"30 7 * * 1-5,0", "2026-11-08 07:30"}, // weekdays + Sunday
		{"30 7 * * *", "2026-11-07 07:30"},
	}
	for _, c := range cases {
		got := NextOccurrenceIn(c.expr, after, time.UTC).Format("2006-01-02 15:04")
		if got != c.want {
			t.Errorf("%q: next = %s, want %s", c.expr, got, c.want)
		}
	}
}

func TestNextOccurrenceBeyond48Hours(t *testing.T) {
	after := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC) // Friday
	cases := []struct{ expr, want string }{
		{"30 7 * * 1", "2026-11-09 07:30"},  // weekly, 3 days out
		{"0 9 1 * *", "2026-12-01 09:00"},   // monthly, 25 days out
		{"0 9 1 JAN *", "2027-01-01 09:00"}, // yearly, ~2 months out
	}
	for _, c := range cases {
		got := NextOccurrenceIn(c.expr, after, time.UTC).Format("2006-01-02 15:04")
		if got != c.want {
			t.Errorf("%q: next = %s, want %s", c.expr, got, c.want)
		}
	}
}

func TestNextOccurrenceDayOfMonthOrWeekday(t *testing.T) {
	// Standard cron: with both day fields restricted, either may match.
	// Monday 2026-11-30 10:00 → the 1st of December wins over the next
	// Monday.
	after := time.Date(2026, 11, 30, 10, 0, 0, 0, time.UTC)
	got := NextOccurrenceIn("0 9 1 * 1", after, time.UTC).Format("2006-01-02 15:04")
	if got != "2026-12-01 09:00" {
		t.Errorf("got %s, want 2026-12-01 09:00", got)
	}
}

func TestValidate(t *testing.T) {
	valid := []string{
		"* * * * *",
		"0 9 * * *",
		"30 7 * * 1-5",
		"30 7 * * MON-FRI",
		"*/15 9-18 * * 1,2,3",
		"0 0 1 JAN-DEC *",
		"0 0 * * 0,7",
	}
	for _, s := range valid {
		if err := Validate(s); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", s, err)
		}
	}
	invalid := []string{
		"",
		"0 9 * *",
		"0 9 * * * *",
		"60 9 * * *",
		"0 24 * * *",
		"0 0 0 * *",
		"0 9 * * 8",
		"0 9 * * FOO",
		"0 9 * * 1-",
		"*/0 * * * *",
		"0 9 * * 1-5x",
	}
	for _, s := range invalid {
		if err := Validate(s); err == nil {
			t.Errorf("Validate(%q) = nil, want error", s)
		}
	}
}
