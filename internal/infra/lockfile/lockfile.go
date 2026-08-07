// Package lockfile parses the small, versioned lock records awa writes under
// .awa/locks and classifies them as active or stale.
//
// Writers and collectors take presence/collector locks (see exclusive_unix.go and
// exclusive_other.go), but doctor must still be honest about what it finds on disk:
// only a record whose schema this build recognizes can be parsed and classified. An
// unrecognized file is reported as unknown and is never a repair target. A recognized
// record is removed only when it is provably stale — its owner process is gone on this
// host, or it is older than a documented threshold with no active-owner evidence. All
// lock writers encode records with Encode so they parse through the same boundary.
package lockfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// schemaVersion is the lock record schema this build understands. A record stamped
// with a different version is unrecognized, not corrupt: doctor reports it as
// unknown rather than deleting it.
const schemaVersion = 1

// DefaultStaleAfter is the documented age past which a recognized lock record with
// no active-owner evidence is considered stale and safe to remove. It is generous:
// a lock should never legitimately outlive a single long command by this much.
const DefaultStaleAfter = 24 * time.Hour

// ErrUnsupportedSchema reports a lock file whose schema_version this build does not
// recognize. Callers treat it as an unknown lock (a warning), not corruption.
var ErrUnsupportedSchema = errors.New("unsupported lock schema version")

// Status is the classification of a recognized lock record.
type Status int

const (
	// StatusActive means the lock appears to belong to a live owner: its process
	// is running on this host, or it is recent enough that no owner evidence makes
	// it safe to remove.
	StatusActive Status = iota + 1
	// StatusStale means the lock is safe to remove: its owner process is gone on
	// this host, or it is older than the stale threshold with no active owner.
	StatusStale
)

func (s Status) String() string {
	switch s {
	case StatusActive:
		return "active"
	case StatusStale:
		return "stale"
	default:
		return "unknown"
	}
}

// Record is a parsed lock record. It is built only through Parse, so an
// unrecognized schema or a malformed timestamp never reaches classification.
type Record struct {
	Operation string
	PID       int
	Hostname  string
	CreatedAt time.Time
}

// doc is the persisted JSON shape.
type doc struct {
	SchemaVersion int    `json:"schema_version"`
	Operation     string `json:"operation"`
	PID           int    `json:"pid"`
	Hostname      string `json:"hostname"`
	CreatedAt     string `json:"created_at"`
}

// Parse decodes a lock record strictly: unknown fields are rejected (a hand-edit
// or a record from another format is not silently accepted), there must be exactly
// one object, the schema version must match, the operation must be one this build
// recognizes, and created_at must be a valid RFC3339 timestamp. A wrong schema
// version returns ErrUnsupportedSchema so the caller can distinguish "not ours" from
// "malformed".
func Parse(data []byte) (Record, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var d doc
	if err := dec.Decode(&d); err != nil {
		return Record{}, fmt.Errorf("decoding lock record: %w", err)
	}
	// There must be exactly one object: a second decode must hit end of input.
	// Anything else — a second value, or trailing bytes that are not even valid
	// JSON (a truncated record, an appended token) — is trailing data. Checking
	// only for a successful second decode (err == nil) would accept a valid object
	// followed by garbage, since that garbage fails the second decode; requiring
	// io.EOF closes that hole and matches the checkpoint/run/manifest decoders.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Record{}, fmt.Errorf("decoding lock record: trailing data")
	}
	if d.SchemaVersion != schemaVersion {
		return Record{}, fmt.Errorf("%w: %d (want %d)", ErrUnsupportedSchema, d.SchemaVersion, schemaVersion)
	}
	if d.Operation == "" {
		return Record{}, fmt.Errorf("lock record missing operation")
	}
	// The operation is a closed, validated vocabulary. An unrecognized one — a
	// third-party file that merely happens to use this schema, or a lock written by a
	// newer awa with an operation this build does not know — must not be accepted as
	// one of our locks: it is reported as unknown (so it blocks gc and is never
	// auto-removed) rather than trusted on caller discipline.
	if !Operation(d.Operation).Valid() {
		return Record{}, fmt.Errorf("lock record has unrecognized operation %q", d.Operation)
	}
	if d.PID <= 0 {
		return Record{}, fmt.Errorf("lock record has non-positive pid %d", d.PID)
	}
	if d.Hostname == "" {
		return Record{}, fmt.Errorf("lock record missing hostname")
	}
	created, err := time.Parse(time.RFC3339Nano, d.CreatedAt)
	if err != nil {
		return Record{}, fmt.Errorf("lock record has invalid created_at: %w", err)
	}
	return Record{
		Operation: d.Operation,
		PID:       d.PID,
		Hostname:  d.Hostname,
		CreatedAt: created,
	}, nil
}

// Encode renders a record as schema-versioned JSON bytes, for writers and test
// fixtures so they cross the same boundary readers do.
func Encode(r Record) ([]byte, error) {
	d := doc{
		SchemaVersion: schemaVersion,
		Operation:     r.Operation,
		PID:           r.PID,
		Hostname:      r.Hostname,
		CreatedAt:     r.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	out, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding lock record: %w", err)
	}
	return append(out, '\n'), nil
}

// Classify decides whether a recognized record is active or stale. localHostname
// is this machine's hostname and alive reports whether a local pid is running;
// both are injected so the decision is testable without real processes. A record
// is stale when it belongs to this host and its process is not running (the owner
// is provably gone), or when it is older than staleAfter regardless of host (the
// documented threshold for a lock with no active-owner evidence). Otherwise it is
// active: a running owner on this host, or a record too recent to judge from
// another host, is left alone.
func (r Record) Classify(now time.Time, staleAfter time.Duration, localHostname string, alive func(pid int) bool) Status {
	if r.Hostname == localHostname {
		if !alive(r.PID) {
			return StatusStale
		}
		return StatusActive
	}
	if now.Sub(r.CreatedAt) > staleAfter {
		return StatusStale
	}
	return StatusActive
}
