// Package cron parses standard 5-field cron expressions and answers "does
// this minute match". That is all a minute-ticker scheduler needs — no Next()
// computation, no seconds field, no @keywords. Written in-house (like the
// GitHub App JWT and the webhook glob matcher) to keep the binary
// dependency-free.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// field bounds, in expression order.
var bounds = [5]struct{ min, max int }{
	{0, 59}, // minute
	{0, 23}, // hour
	{1, 31}, // day of month
	{1, 12}, // month
	{0, 6},  // day of week (Sunday = 0; 7 accepted as Sunday)
}

// Schedule is a parsed cron expression.
type Schedule struct {
	fields [5]uint64 // bitmask of allowed values per field
	// domStar/dowStar record whether the field was "*": vixie cron matches
	// day-of-month OR day-of-week when both are restricted, AND otherwise.
	domStar, dowStar bool
}

// Parse parses a 5-field cron expression: minute, hour, day-of-month, month,
// day-of-week. Each field accepts *, numbers, ranges (a-b), steps (*/n, a-b/n)
// and comma lists. Day-of-week is 0-6 with 7 accepted as Sunday.
func Parse(expr string) (Schedule, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return Schedule{}, fmt.Errorf("cron: want 5 fields (minute hour day month weekday), got %d", len(parts))
	}
	var s Schedule
	for i, part := range parts {
		mask, star, err := parseField(part, bounds[i].min, bounds[i].max)
		if err != nil {
			return Schedule{}, fmt.Errorf("cron: field %d (%q): %w", i+1, part, err)
		}
		s.fields[i] = mask
		switch i {
		case 2:
			s.domStar = star
		case 4:
			s.dowStar = star
		}
	}
	return s, nil
}

// Matches reports whether t (truncated to the minute) satisfies the schedule.
func (s Schedule) Matches(t time.Time) bool {
	if !s.bit(0, t.Minute()) || !s.bit(1, t.Hour()) || !s.bit(3, int(t.Month())) {
		return false
	}
	dom := s.bit(2, t.Day())
	dow := s.bit(4, int(t.Weekday()))
	// Vixie semantics: when both day fields are restricted, either may match;
	// otherwise both must (a "*" always does).
	if !s.domStar && !s.dowStar {
		return dom || dow
	}
	return dom && dow
}

func (s Schedule) bit(field, v int) bool { return s.fields[field]&(1<<uint(v)) != 0 }

// parseField parses one comma-separated field into a bitmask. star reports
// whether the field is exactly "*" (no step).
func parseField(f string, min, max int) (mask uint64, star bool, err error) {
	if f == "*" {
		star = true
	}
	for _, part := range strings.Split(f, ",") {
		spec, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			step, err = strconv.Atoi(stepStr)
			if err != nil || step < 1 {
				return 0, false, fmt.Errorf("bad step %q", stepStr)
			}
		}
		lo, hi := min, max
		if spec != "*" {
			loStr, hiStr, isRange := strings.Cut(spec, "-")
			lo, err = parseValue(loStr, min, max)
			if err != nil {
				return 0, false, err
			}
			if isRange {
				hi, err = parseValue(hiStr, min, max)
				if err != nil {
					return 0, false, err
				}
				if hi < lo {
					return 0, false, fmt.Errorf("range %q is backwards", spec)
				}
			} else if hasStep {
				// "N/step" means "from N to max by step" (vixie extension).
				hi = max
			} else {
				hi = lo
			}
		}
		for v := lo; v <= hi; v += step {
			mask |= 1 << uint(v)
		}
	}
	return mask, star, nil
}

func parseValue(s string, min, max int) (int, error) {
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("bad value %q", s)
	}
	// Day-of-week 7 is Sunday, same as 0 (both POSIX and vixie accept it).
	if v == 7 && min == 0 && max == 6 {
		v = 0
	}
	if v < min || v > max {
		return 0, fmt.Errorf("value %d out of range %d-%d", v, min, max)
	}
	return v, nil
}
