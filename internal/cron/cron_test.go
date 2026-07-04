package cron

import (
	"testing"
	"time"
)

// at builds a time with the given calendar parts (local zone, seconds zero).
func at(year int, month time.Month, day, hour, min int) time.Time {
	return time.Date(year, month, day, hour, min, 0, 0, time.Local)
}

func TestMatches(t *testing.T) {
	// 2026-07-04 is a Saturday.
	sat := at(2026, time.July, 4, 3, 30)

	cases := []struct {
		expr string
		t    time.Time
		want bool
	}{
		{"* * * * *", sat, true},
		{"30 3 * * *", sat, true},
		{"31 3 * * *", sat, false},
		{"30 4 * * *", sat, false},
		{"*/15 * * * *", at(2026, time.July, 4, 3, 45), true},
		{"*/15 * * * *", at(2026, time.July, 4, 3, 40), false},
		{"0-29/10 * * * *", at(2026, time.July, 4, 3, 20), true},
		{"0-29/10 * * * *", at(2026, time.July, 4, 3, 40), false},
		{"30 3 4 7 *", sat, true},  // exact date
		{"30 3 5 7 *", sat, false}, // wrong day
		{"30 3 * 8 *", sat, false}, // wrong month
		{"30 3 * * 6", sat, true},  // Saturday
		{"30 3 * * 0", sat, false}, // not Sunday
		{"30 3 * * 7", sat, false}, // 7 = Sunday, still Saturday
		{"30 3 * * 1-5", sat, false},
		{"0,30 * * * *", sat, true},
		{"15,45 * * * *", sat, false},
		// Vixie OR: dom and dow both restricted -> either matches.
		{"30 3 4 * 1", sat, true},  // dom matches, dow (Monday) doesn't
		{"30 3 1 * 6", sat, true},  // dow matches, dom doesn't
		{"30 3 1 * 1", sat, false}, // neither matches
		// dom restricted, dow "*": dom must match.
		{"30 3 1 * *", sat, false},
		// "N/step" runs from N to max.
		{"20/10 * * * *", at(2026, time.July, 4, 3, 50), true},
		{"20/10 * * * *", at(2026, time.July, 4, 3, 10), false},
	}
	for _, tc := range cases {
		s, err := Parse(tc.expr)
		if err != nil {
			t.Errorf("Parse(%q): %v", tc.expr, err)
			continue
		}
		if got := s.Matches(tc.t); got != tc.want {
			t.Errorf("%q at %s = %v, want %v", tc.expr, tc.t, got, tc.want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	bad := []string{
		"",
		"* * * *",      // 4 fields
		"* * * * * *",  // 6 fields
		"60 * * * *",   // minute out of range
		"* 24 * * *",   // hour out of range
		"* * 0 * *",    // dom out of range
		"* * 32 * *",   // dom out of range
		"* * * 13 *",   // month out of range
		"* * * * 8",    // dow out of range (7 is ok, 8 is not)
		"a * * * *",    // not a number
		"10-5 * * * *", // backwards range
		"*/0 * * * *",  // zero step
		"*/x * * * *",  // bad step
		"1--2 * * * *", // mangled range
		"5, * * * *",   // empty list entry
	}
	for _, expr := range bad {
		if _, err := Parse(expr); err == nil {
			t.Errorf("Parse(%q) accepted, want error", expr)
		}
	}
}

func TestParseAccepts(t *testing.T) {
	good := []string{
		"* * * * *",
		"0 0 * * *",
		"*/5 * * * *",
		"0 3 * * 0",
		"0 3 * * 7",
		"30 2 1,15 * *",
		"0 0 1 1 *",
		"0 9-17/2 * * 1-5",
	}
	for _, expr := range good {
		if _, err := Parse(expr); err != nil {
			t.Errorf("Parse(%q): %v", expr, err)
		}
	}
}
