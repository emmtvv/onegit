package ci

import (
	"testing"
	"time"
)

func TestCronNext(t *testing.T) {
	base := time.Date(2026, 10, 4, 10, 17, 42, 0, time.UTC) // a Sunday
	for _, c := range []struct {
		expr, tz string
		want     string
	}{
		{"* * * * *", "", "2026-10-04T10:18:00Z"},
		{"*/15 * * * *", "", "2026-10-04T10:30:00Z"},
		{"0 3 * * *", "", "2026-10-05T03:00:00Z"},
		{"@hourly", "", "2026-10-04T11:00:00Z"},
		{"30 9 * * mon-fri", "", "2026-10-05T09:30:00Z"},
		{"0 0 1 jan *", "", "2027-01-01T00:00:00Z"},
		{"0 12 * * 7", "", "2026-10-04T12:00:00Z"}, // 7 = Sunday
		{"0 0 13 * 5", "", "2026-10-09T00:00:00Z"}, // the 13th OR a Friday
		{"5,10 1-2 * * *", "", "2026-10-05T01:05:00Z"},
		{"0 9 * * *", "Europe/Moscow", "2026-10-05T06:00:00Z"}, // 09:00 MSK = 06:00 UTC
		{"0 * * * *", "Asia/Kolkata", "2026-10-04T10:30:00Z"},  // UTC+5:30: the next local full hour
	} {
		cr, err := ParseCron(c.expr, c.tz)
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		if got := cr.Next(base).UTC().Format(time.RFC3339); got != c.want {
			t.Errorf("%s (%s): next = %s, want %s", c.expr, c.tz, got, c.want)
		}
	}
	if cr, _ := ParseCron("0 0 30 2 *", ""); !cr.Next(base).IsZero() {
		t.Error("February 30th matched")
	}
	for _, bad := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "*/0 * * * *", "5-1 * * * *", "* * * foo *"} {
		if _, err := ParseCron(bad, ""); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if _, err := ParseCron("* * * * *", "Mars/Olympus"); err == nil {
		t.Error("unknown time zone accepted")
	}
}
