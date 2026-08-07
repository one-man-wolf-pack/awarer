package runevidence

import "fmt"

// Presence is the metadata-only readability of one captured output stream, obtained
// by stat without reading payload bytes. It is a closed set: present, missing, or
// unreadable. Missing is calm and typed (the payload was removed or never landed);
// unreadable is a distinct I/O fact; neither is corruption of the enclosing record.
type Presence int

const (
	// PresencePresent: the stream's payload file exists and can be opened. It is
	// deliberately iota+1, so the zero value is an invalid presence rather than a
	// silent "present" — a struct built without NewOutputInspectability fails Valid()
	// instead of masquerading as a landed payload.
	PresencePresent Presence = iota + 1
	// PresenceMissing: the payload file is absent (removed or never written).
	PresenceMissing
	// PresenceUnreadable: the payload file exists but cannot be opened/stat'd.
	PresenceUnreadable
)

// String renders the wire token for a presence value.
func (p Presence) String() string {
	switch p {
	case PresencePresent:
		return "present"
	case PresenceMissing:
		return "missing"
	case PresenceUnreadable:
		return "unreadable"
	default:
		return fmt.Sprintf("presence(%d)", int(p))
	}
}

// Valid reports whether p is a defined presence value.
func (p Presence) Valid() bool {
	return p == PresencePresent || p == PresenceMissing || p == PresenceUnreadable
}

// Integrity is whether a stream's captured bytes have been verified against their
// recorded hash during this inspection. Metadata-only inspection never reads bytes, so it
// is always Unverified; only an explicit output-selection path (--tail/--grep) that opens
// and hashes the same descriptor proves Verified. A successful stat is deliberately never
// called "verified". It is the strongest verification the invocation performed on the
// stream: an unselected stream stays Unverified even when its sibling was read.
type Integrity int

const (
	// IntegrityUnverified: the captured bytes were not read or hashed this invocation.
	IntegrityUnverified Integrity = iota
	// IntegrityVerified: the captured bytes were opened and hash-verified against the
	// record this invocation (an explicit --tail/--grep read of the stream).
	IntegrityVerified
)

// String renders the wire token for an integrity value.
func (i Integrity) String() string {
	switch i {
	case IntegrityUnverified:
		return "unverified"
	case IntegrityVerified:
		return "verified"
	default:
		return fmt.Sprintf("integrity(%d)", int(i))
	}
}

// OutputInspectability is the metadata-only inspectability of one captured stream:
// its presence/readability (obtained without reading payload bytes) and its content
// integrity, which is always Unverified for a metadata inspection. The byte counts,
// truncation policy, and content hash live on the run record's OutputCapture; this
// value adds only the no-byte-read inspectability.
type OutputInspectability struct {
	presence  Presence
	integrity Integrity
}

// NewOutputInspectability builds a metadata-only inspectability from a presence
// classification. Integrity is fixed Unverified: a metadata inspection reads no
// payload bytes and therefore cannot assert content integrity.
func NewOutputInspectability(p Presence) (OutputInspectability, error) {
	if !p.Valid() {
		return OutputInspectability{}, fmt.Errorf("output inspectability has invalid presence %d", int(p))
	}
	return OutputInspectability{presence: p, integrity: IntegrityUnverified}, nil
}

// Valid reports whether the inspectability is a defined value object. Only its
// presence can be undefined (the zero-value struct), since IntegrityUnverified is the
// sole integrity value; a zero-value OutputInspectability is therefore invalid.
func (o OutputInspectability) Valid() bool { return o.presence.Valid() }

// Presence returns the stream's no-byte-read presence classification.
func (o OutputInspectability) Presence() Presence { return o.presence }

// Integrity returns the stream's content-integrity status (always Unverified for a
// metadata inspection).
func (o OutputInspectability) Integrity() Integrity { return o.integrity }
