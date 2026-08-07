package worktree

import (
	"fmt"
	"sort"
)

// StatField identifies a stat field that a platform may be unable to supply.
// Size, mtime, and mode are always available from os.FileInfo, so only the
// syscall-only fields are tracked here. Recording which fields were omitted lets
// trust-mode comparisons skip them rather than treating an unavailable field as
// a zero: an omission is recorded, never guessed.
type StatField uint8

const (
	FieldCtime StatField = iota
	FieldDev
	FieldIno
	FieldNlink
)

// String renders a StatField as its persisted token. The token set matches the
// SQLite index's omitted-field columns so the JSON checkpoint, the index, and the
// model name the same fields the same way.
func (f StatField) String() string {
	switch f {
	case FieldCtime:
		return "ctime"
	case FieldDev:
		return "dev"
	case FieldIno:
		return "ino"
	case FieldNlink:
		return "nlink"
	default:
		return "unknown"
	}
}

// ParseStatField resolves a persisted token to a StatField, rejecting unknown
// tokens so a corrupt checkpoint cannot inject an omission the model does not
// understand.
func ParseStatField(s string) (StatField, error) {
	switch s {
	case "ctime":
		return FieldCtime, nil
	case "dev":
		return FieldDev, nil
	case "ino":
		return FieldIno, nil
	case "nlink":
		return FieldNlink, nil
	default:
		return 0, fmt.Errorf("invalid stat field %q", s)
	}
}

// FieldSet is a small bitmask of omitted stat fields.
type FieldSet uint8

// knownFields is the mask of all defined StatField bits. A FieldSet with any bit
// outside this mask is invalid — it would carry an omission the model does not
// understand, which comparison would silently ignore.
const knownFields = FieldSet(1<<FieldCtime | 1<<FieldDev | 1<<FieldIno | 1<<FieldNlink)

// KnownFieldsMask is the bit mask of all defined stat-field bits — the same mask
// Valid enforces. It is exported so the persistence layer can pin its DB-level
// CHECK constraint to the exact set of bits the domain understands, keeping the
// stored constraint and the model from drifting apart.
const KnownFieldsMask = uint8(knownFields)

// With returns the set with f marked present (omitted).
func (s FieldSet) With(f StatField) FieldSet { return s | (1 << f) }

// Has reports whether f is in the set (i.e. was omitted).
func (s FieldSet) Has(f StatField) bool { return s&(1<<f) != 0 }

// Union combines two omission sets.
func (s FieldSet) Union(other FieldSet) FieldSet { return s | other }

// Valid reports whether the set contains only defined field bits. It guards
// against an unknown omission entering the model — for example a corrupt value
// read back from the index.
func (s FieldSet) Valid() bool { return s&^knownFields == 0 }

// Tokens returns the persisted tokens of every omitted field, in a stable order,
// for serializing the set into a JSON checkpoint. An empty set yields an empty
// slice rather than nil so the encoder is deterministic across callers.
func (s FieldSet) Tokens() []string {
	tokens := []string{}
	for _, f := range []StatField{FieldCtime, FieldDev, FieldIno, FieldNlink} {
		if s.Has(f) {
			tokens = append(tokens, f.String())
		}
	}
	return tokens
}

// ParseFieldSet rebuilds a FieldSet from its persisted tokens, rejecting unknown
// or duplicate tokens so a corrupt checkpoint cannot reconstruct an invalid set.
func ParseFieldSet(tokens []string) (FieldSet, error) {
	var set FieldSet
	seen := map[StatField]bool{}
	sorted := append([]string(nil), tokens...)
	sort.Strings(sorted)
	for _, t := range sorted {
		f, err := ParseStatField(t)
		if err != nil {
			return 0, err
		}
		if seen[f] {
			return 0, fmt.Errorf("duplicate stat field %q", t)
		}
		seen[f] = true
		set = set.With(f)
	}
	return set, nil
}

// StatSignature is the stat-based checkpoint used to decide whether a file's
// content hash can be reused. Every available field is captured; fields the
// platform cannot provide are listed in Omitted and excluded from comparison.
type StatSignature struct {
	Size    int64
	MtimeNs int64
	CtimeNs int64
	Mode    uint32
	Dev     uint64
	Ino     uint64
	Nlink   uint64
	Omitted FieldSet
}

// EqualNormal reports whether two signatures match under normal trust: size,
// mtime, ctime, mode, dev, ino and nlink must all agree. A field omitted on
// either side is skipped rather than compared, so a platform that cannot supply
// ctime/dev/ino/nlink simply relies on the remaining fields. Any difference in a
// compared field means the file must be re-read and rehashed.
func (s StatSignature) EqualNormal(other StatSignature) bool {
	if s.Size != other.Size || s.MtimeNs != other.MtimeNs || s.Mode != other.Mode {
		return false
	}
	if !s.fieldOmitted(other, FieldCtime) && s.CtimeNs != other.CtimeNs {
		return false
	}
	if !s.fieldOmitted(other, FieldDev) && s.Dev != other.Dev {
		return false
	}
	if !s.fieldOmitted(other, FieldIno) && s.Ino != other.Ino {
		return false
	}
	if !s.fieldOmitted(other, FieldNlink) && s.Nlink != other.Nlink {
		return false
	}
	return true
}

// EqualFast reports whether two signatures match under fast trust, which checks
// only size and mtime. It is deliberately weaker than EqualNormal and must never
// be presented as integrity-grade; scans using it flag the weakness in their
// metadata.
func (s StatSignature) EqualFast(other StatSignature) bool {
	return s.Size == other.Size && s.MtimeNs == other.MtimeNs
}

// fieldOmitted reports whether f was omitted on either signature, in which case
// it cannot participate in comparison.
func (s StatSignature) fieldOmitted(other StatSignature, f StatField) bool {
	return s.Omitted.Has(f) || other.Omitted.Has(f)
}

// HardlinkIdentity is the subset of a stat signature that identifies the
// underlying inode shared by hard-linked paths. Known is false on platforms that
// cannot supply dev/ino/nlink, so callers never mistake zeroes for a real
// identity.
type HardlinkIdentity struct {
	Dev   uint64
	Ino   uint64
	Nlink uint64
	Known bool
}

// HardlinkIdentity derives the hardlink identity from a signature, marking it
// unknown when any of dev/ino/nlink was omitted.
func (s StatSignature) HardlinkIdentity() HardlinkIdentity {
	known := !s.Omitted.Has(FieldDev) && !s.Omitted.Has(FieldIno) && !s.Omitted.Has(FieldNlink)
	return HardlinkIdentity{Dev: s.Dev, Ino: s.Ino, Nlink: s.Nlink, Known: known}
}
