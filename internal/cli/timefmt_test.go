package cli

import (
	"testing"
	"time"

	domainconfig "awarer/internal/domain/config"
)

func TestRelativeTime(t *testing.T) {
	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		t    time.Time
		want string
	}{
		{"just now", now.Add(-2 * time.Second), "just now"},
		{"seconds", now.Add(-30 * time.Second), "30s ago"},
		{"minutes", now.Add(-12 * time.Minute), "12m ago"},
		{"hours", now.Add(-2 * time.Hour), "2h ago"},
		{"days", now.Add(-3 * 24 * time.Hour), "3d ago"},
		{"future", now.Add(5 * time.Minute), "in 5m"},
	}
	for _, c := range cases {
		if got := relativeTime(c.t, now); got != c.want {
			t.Errorf("%s: relativeTime = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestFormatTimeModes(t *testing.T) {
	now := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	at := now.Add(-90 * time.Minute)

	if got := FormatTime(domainconfig.TimeRelative, at, now); got != "1h ago" {
		t.Errorf("relative = %q, want %q", got, "1h ago")
	}
	if got := FormatTime(domainconfig.TimeUTC, at, now); got != "2026-06-28 10:30:00Z" {
		t.Errorf("utc = %q, want %q", got, "2026-06-28 10:30:00Z")
	}
	// Local depends on the test machine's zone; assert it is absolute (not relative).
	if got := FormatTime(domainconfig.TimeLocal, at, now); got == "1h ago" {
		t.Errorf("local = %q, want an absolute time", got)
	}
}

func TestTimeDisplayFromFlag(t *testing.T) {
	cfg := domainconfig.Defaults()
	cfg.UI.Time = domainconfig.TimeUTC
	// No flag → config default.
	if got, err := timeDisplayFromFlag("", cfg); err != nil || got != domainconfig.TimeUTC {
		t.Errorf("no flag = %v, %v, want utc", got, err)
	}
	// Flag overrides config.
	if got, err := timeDisplayFromFlag("relative", cfg); err != nil || got != domainconfig.TimeRelative {
		t.Errorf("flag override = %v, %v, want relative", got, err)
	}
	if _, err := timeDisplayFromFlag("bogus", cfg); err == nil {
		t.Error("an unknown --time value must be rejected")
	}
}
