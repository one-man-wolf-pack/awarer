package cli

import (
	"fmt"
	"time"

	domainconfig "awarer/internal/domain/config"
)

// FormatTime renders t for human output under the given display policy, relative
// to now. JSON output never calls this — it always uses machine timestamps. The
// clock is injected (now is passed in, never read from the wall clock here) so the
// relative form is deterministic in tests.
func FormatTime(d domainconfig.TimeDisplay, t, now time.Time) string {
	switch d {
	case domainconfig.TimeUTC:
		return t.UTC().Format("2006-01-02 15:04:05Z")
	case domainconfig.TimeLocal:
		return t.Local().Format("2006-01-02 15:04:05 -0700")
	default:
		return relativeTime(t, now)
	}
}

// relativeTime renders the signed gap between t and now as a compact age, e.g.
// "just now", "12m ago", "2h ago", "3d ago", or, for a future time, "in 5m". It
// is coarse by design (one unit), optimized for scanning a log, not for precision.
func relativeTime(t, now time.Time) string {
	d := now.Sub(t)
	future := d < 0
	if future {
		d = -d
	}
	if d < 5*time.Second {
		return "just now"
	}
	var n int
	var unit string
	switch {
	case d < time.Minute:
		n, unit = int(d/time.Second), "s"
	case d < time.Hour:
		n, unit = int(d/time.Minute), "m"
	case d < 24*time.Hour:
		n, unit = int(d/time.Hour), "h"
	case d < 7*24*time.Hour:
		n, unit = int(d/(24*time.Hour)), "d"
	default:
		n, unit = int(d/(7*24*time.Hour)), "w"
	}
	if future {
		return fmt.Sprintf("in %d%s", n, unit)
	}
	return fmt.Sprintf("%d%s ago", n, unit)
}

// timeDisplayFromFlag resolves the effective time display: the --time flag value
// when set, otherwise the config default. The flag is parsed at the CLI boundary,
// so by here it is a validated string or empty.
func timeDisplayFromFlag(flagValue string, cfg domainconfig.Config) (domainconfig.TimeDisplay, error) {
	if flagValue == "" {
		return cfg.UI.Time, nil
	}
	return domainconfig.ParseTimeDisplay(flagValue)
}
