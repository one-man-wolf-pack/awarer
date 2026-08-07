// Package config defines the domain model for awa configuration.
//
// It owns the valid shape of a configuration: closed enums, parsed byte sizes
// and durations, and the section structs that group them. Values enter only
// through the ParseX constructors, so an invalid enum, size, or duration can
// never reach an application use case as raw, unchecked data. The package is
// pure — it performs no I/O and knows nothing about TOML or the filesystem.
package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// DiffAlgorithm selects the text diff engine used to render a content diff.
// Histogram is the built-in default: it anchors on rare lines for cleaner,
// code-oriented hunks, falling back to Myers locally for regions it cannot
// anchor. Myers stays available as the stable baseline and explicit option. It
// is a closed enum so an invalid algorithm string cannot reach the diff engine:
// config and CLI parse raw input through ParseDiffAlgorithm before it crosses
// the boundary.
type DiffAlgorithm int

const (
	DiffMyers DiffAlgorithm = iota
	DiffHistogram
)

// DefaultDiffAlgorithm is the built-in default when neither config nor a CLI
// flag selects an algorithm. It is Histogram, the better human-facing default
// now that the bounded hybrid keeps Myers as a local fallback for unsuitable
// regions; --algorithm myers and [diff].algorithm = "myers" still force Myers.
func DefaultDiffAlgorithm() DiffAlgorithm { return DiffHistogram }

// ParseDiffAlgorithm resolves a config/CLI string to a DiffAlgorithm, rejecting
// unknown values rather than silently falling back to an algorithm the caller
// did not ask for.
func ParseDiffAlgorithm(s string) (DiffAlgorithm, error) {
	switch s {
	case "myers":
		return DiffMyers, nil
	case "histogram":
		return DiffHistogram, nil
	default:
		return 0, fmt.Errorf("invalid diff algorithm %q: want myers or histogram", s)
	}
}

func (a DiffAlgorithm) String() string {
	switch a {
	case DiffMyers:
		return "myers"
	case DiffHistogram:
		return "histogram"
	default:
		return "unknown"
	}
}

// Valid reports whether a is a known algorithm. Go lets any int be cast to the
// type, so the domain checks validity rather than trusting construction.
func (a DiffAlgorithm) Valid() bool {
	return a == DiffMyers || a == DiffHistogram
}

// TimeDisplay selects how human output renders timestamps. It governs human output
// only; JSON always uses machine timestamps. It is a closed enum so an invalid
// value cannot reach the renderer.
type TimeDisplay int

const (
	// TimeRelative renders ages relative to now ("12m ago"). It is the default.
	TimeRelative TimeDisplay = iota
	// TimeLocal renders absolute local time with offset.
	TimeLocal
	// TimeUTC renders absolute UTC time.
	TimeUTC
)

// DefaultTimeDisplay is the built-in default when neither config nor a CLI flag
// selects a time display: relative time, the most scannable for recent activity.
func DefaultTimeDisplay() TimeDisplay { return TimeRelative }

// ParseTimeDisplay resolves a config/CLI string to a TimeDisplay, rejecting
// unknown values rather than silently falling back.
func ParseTimeDisplay(s string) (TimeDisplay, error) {
	switch s {
	case "relative":
		return TimeRelative, nil
	case "local":
		return TimeLocal, nil
	case "utc":
		return TimeUTC, nil
	default:
		return 0, fmt.Errorf("invalid time display %q: want relative, local, or utc", s)
	}
}

func (d TimeDisplay) String() string {
	switch d {
	case TimeRelative:
		return "relative"
	case TimeLocal:
		return "local"
	case TimeUTC:
		return "utc"
	default:
		return "unknown"
	}
}

// Valid reports whether d is a known time display.
func (d TimeDisplay) Valid() bool {
	return d == TimeRelative || d == TimeLocal || d == TimeUTC
}

// TrustMode selects how aggressively awa trusts the worktree index. The default
// is normal.
type TrustMode int

const (
	TrustNormal TrustMode = iota
	TrustStrict
	TrustFast
)

// ParseTrustMode resolves a config string to a TrustMode.
func ParseTrustMode(s string) (TrustMode, error) {
	switch s {
	case "normal":
		return TrustNormal, nil
	case "strict":
		return TrustStrict, nil
	case "fast":
		return TrustFast, nil
	default:
		return 0, fmt.Errorf("invalid trust_mode %q: want normal, strict, or fast", s)
	}
}

func (m TrustMode) String() string {
	switch m {
	case TrustNormal:
		return "normal"
	case TrustStrict:
		return "strict"
	case TrustFast:
		return "fast"
	default:
		return "unknown"
	}
}

// Valid reports whether m is a known trust mode.
func (m TrustMode) Valid() bool {
	return m == TrustNormal || m == TrustStrict || m == TrustFast
}

// LargeFilePolicy decides what awa does with files larger than max_file_size.
// The default is hash-only.
type LargeFilePolicy int

const (
	LargeFileHashOnly LargeFilePolicy = iota
	LargeFileStore
	LargeFileSkip
)

// ParseLargeFilePolicy resolves a config string to a LargeFilePolicy.
func ParseLargeFilePolicy(s string) (LargeFilePolicy, error) {
	switch s {
	case "hash-only":
		return LargeFileHashOnly, nil
	case "store":
		return LargeFileStore, nil
	case "skip":
		return LargeFileSkip, nil
	default:
		return 0, fmt.Errorf("invalid large_file_policy %q: want hash-only, store, or skip", s)
	}
}

func (p LargeFilePolicy) String() string {
	switch p {
	case LargeFileHashOnly:
		return "hash-only"
	case LargeFileStore:
		return "store"
	case LargeFileSkip:
		return "skip"
	default:
		return "unknown"
	}
}

// Valid reports whether p is a known large-file policy.
func (p LargeFilePolicy) Valid() bool {
	return p == LargeFileHashOnly || p == LargeFileStore || p == LargeFileSkip
}

// ByteSize is a size in bytes parsed from human strings such as "50MiB".
type ByteSize int64

const (
	kiB = 1024
	miB = 1024 * kiB
	giB = 1024 * miB
	tiB = 1024 * giB
)

// byteUnits is ordered from largest to smallest so String picks the coarsest
// unit that divides the value evenly, and longest-suffix-first so ParseByteSize
// matches "KiB" before "B".
var byteUnits = []struct {
	suffix string
	scale  int64
}{
	{"TiB", tiB},
	{"GiB", giB},
	{"MiB", miB},
	{"KiB", kiB},
	{"B", 1},
}

// ParseByteSize parses a binary byte-size string such as "50MiB" or "1024B".
// A bare number is interpreted as bytes. Negative values are rejected.
func ParseByteSize(s string) (ByteSize, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return 0, fmt.Errorf("invalid byte size %q: empty", s)
	}
	for _, u := range byteUnits {
		if strings.HasSuffix(raw, u.suffix) {
			num := strings.TrimSpace(strings.TrimSuffix(raw, u.suffix))
			n, err := strconv.ParseInt(num, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid byte size %q: %w", s, err)
			}
			if n < 0 {
				return 0, fmt.Errorf("invalid byte size %q: must not be negative", s)
			}
			if n > math.MaxInt64/u.scale {
				return 0, fmt.Errorf("invalid byte size %q: too large", s)
			}
			return ByteSize(n * u.scale), nil
		}
	}
	// No recognized suffix: accept a bare number as bytes.
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid byte size %q: want a number with an optional B/KiB/MiB/GiB/TiB suffix", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("invalid byte size %q: must not be negative", s)
	}
	return ByteSize(n), nil
}

// String renders the size using the coarsest binary unit that divides it
// evenly, so a value parsed from "50MiB" renders back as "50MiB".
func (b ByteSize) String() string {
	if b == 0 {
		return "0B"
	}
	for _, u := range byteUnits {
		if int64(b)%u.scale == 0 {
			return strconv.FormatInt(int64(b)/u.scale, 10) + u.suffix
		}
	}
	return strconv.FormatInt(int64(b), 10) + "B"
}

// Bytes returns the size as a count of bytes.
func (b ByteSize) Bytes() int64 { return int64(b) }

// Duration is a time span parsed from human strings such as "7d" or "14d".
type Duration time.Duration

// durationUnits is ordered largest-to-smallest for the same reasons as byteUnits.
// Days are an awa-specific extension to Go durations; retention is day-granular
// ("7d", "14d"), so days are the coarsest unit (weeks would render "7d" as "1w").
var durationUnits = []struct {
	suffix string
	scale  time.Duration
}{
	{"d", 24 * time.Hour},
	{"h", time.Hour},
	{"m", time.Minute},
	{"s", time.Second},
}

// ParseDuration parses a duration string with a single unit suffix, such as
// "7d", "14d", "12h", or "30s". Negative values are rejected.
func ParseDuration(s string) (Duration, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return 0, fmt.Errorf("invalid duration %q: empty", s)
	}
	for _, u := range durationUnits {
		if strings.HasSuffix(raw, u.suffix) {
			num := strings.TrimSpace(strings.TrimSuffix(raw, u.suffix))
			n, err := strconv.ParseInt(num, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid duration %q: %w", s, err)
			}
			if n < 0 {
				return 0, fmt.Errorf("invalid duration %q: must not be negative", s)
			}
			if n > math.MaxInt64/int64(u.scale) {
				return 0, fmt.Errorf("invalid duration %q: too large", s)
			}
			return Duration(time.Duration(n) * u.scale), nil
		}
	}
	return 0, fmt.Errorf("invalid duration %q: want a number with a d/h/m/s suffix", s)
}

// String renders the duration using the coarsest unit that divides it evenly,
// so a value parsed from "7d" renders back as "7d".
func (d Duration) String() string {
	if d == 0 {
		return "0s"
	}
	td := time.Duration(d)
	for _, u := range durationUnits {
		if td%u.scale == 0 {
			return strconv.FormatInt(int64(td/u.scale), 10) + u.suffix
		}
	}
	return td.String()
}

// Duration returns the value as a standard library duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }
