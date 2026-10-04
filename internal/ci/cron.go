package ci

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a parsed five-field cron expression (minute hour day-of-month
// month day-of-week) in a time zone. When both day fields are restricted a
// day matches if either does, as in Vixie cron.
type Cron struct {
	minute, hour, dom, month, dow uint64 // bit sets
	domStar, dowStar              bool
	loc                           *time.Location
}

var cronMacros = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *",
	"@weekly": "0 0 * * 0", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
}

var monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
var dayNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

// ParseCron parses an expression; tz is an IANA zone name ("" = UTC).
func ParseCron(expr, tz string) (*Cron, error) {
	loc := time.UTC
	if tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			return nil, fmt.Errorf("unknown time zone %q", tz)
		}
		loc = l
	}
	expr = strings.TrimSpace(expr)
	if m, ok := cronMacros[strings.ToLower(expr)]; ok {
		expr = m
	}
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron %q: want 5 fields (minute hour day month weekday)", expr)
	}
	c := &Cron{loc: loc}
	var err error
	if c.minute, err = cronField(f[0], 0, 59, nil); err != nil {
		return nil, fmt.Errorf("cron %q: minute: %w", expr, err)
	}
	if c.hour, err = cronField(f[1], 0, 23, nil); err != nil {
		return nil, fmt.Errorf("cron %q: hour: %w", expr, err)
	}
	if c.dom, err = cronField(f[2], 1, 31, nil); err != nil {
		return nil, fmt.Errorf("cron %q: day of month: %w", expr, err)
	}
	if c.month, err = cronField(f[3], 1, 12, monthNames); err != nil {
		return nil, fmt.Errorf("cron %q: month: %w", expr, err)
	}
	if c.dow, err = cronField(f[4], 0, 7, dayNames); err != nil {
		return nil, fmt.Errorf("cron %q: day of week: %w", expr, err)
	}
	if c.dow&(1<<7) != 0 { // 7 is Sunday too
		c.dow |= 1
	}
	c.domStar, c.dowStar = f[2] == "*" || f[2] == "?", f[4] == "*" || f[4] == "?"
	return c, nil
}

func cronField(s string, lo, hi int, names map[string]int) (uint64, error) {
	var bits uint64
	num := func(v string) (int, error) {
		if n, ok := names[strings.ToLower(v)]; ok {
			return n, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			return 0, fmt.Errorf("%q is not in %d-%d", v, lo, hi)
		}
		return n, nil
	}
	for _, part := range strings.Split(s, ",") {
		rng, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("bad step %q", stepStr)
			}
			step = n
		}
		var from, to int
		switch {
		case rng == "*" || rng == "?":
			from, to = lo, hi
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err error
			if from, err = num(a); err != nil {
				return 0, err
			}
			if to, err = num(b); err != nil {
				return 0, err
			}
			if from > to {
				return 0, fmt.Errorf("bad range %q", rng)
			}
		default:
			n, err := num(rng)
			if err != nil {
				return 0, err
			}
			from, to = n, n
			if hasStep {
				to = hi
			}
		}
		for i := from; i <= to; i += step {
			bits |= 1 << uint(i)
		}
	}
	return bits, nil
}

func (c *Cron) dayMatches(t time.Time) bool {
	dom := c.dom&(1<<uint(t.Day())) != 0
	dow := c.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dow
	case c.dowStar:
		return dom
	}
	return dom || dow
}

// Next returns the first matching minute strictly after t, or the zero time
// if none exists within five years (e.g. "0 0 30 2 *").
func (c *Cron) Next(t time.Time) time.Time {
	t = t.In(c.loc)
	t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute()+1, 0, 0, c.loc)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if c.month&(1<<uint(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, c.loc)
			continue
		}
		if !c.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, c.loc)
			continue
		}
		if c.hour&(1<<uint(t.Hour())) == 0 {
			// Not Truncate: it works in absolute time, wrong for zones
			// offset by a fraction of an hour.
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, c.loc)
			continue
		}
		if c.minute&(1<<uint(t.Minute())) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}
