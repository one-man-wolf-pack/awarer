// Package manifestjsonl is the shared, verified codec for a worktree manifest
// persisted as JSONL — one canonical record per line. It is the single source of
// truth for the on-disk record format so that checkpoints and recorded runs encode
// their manifests identically, and a manifest from either store can be compared
// against the other (and against a live "now" scan) without format drift.
//
// Encoding and decoding go through the worktree value-object constructors, so a
// round-trip re-validates every invariant and a corrupt or hand-edited line is
// rejected at decode rather than trusted. Each store wraps the structural errors
// this package returns in its own ErrCorruptStore sentinel (see Stream.Corrupt).
package manifestjsonl

import (
	"fmt"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
)

// TraversalDoc is the persisted form of a followed-symlink traversal note.
type TraversalDoc struct {
	SourcePath string `json:"source_path"`
	Depth      int    `json:"depth"`
}

// EntryDoc is the persisted form of one manifest entry. Exactly one node kind is
// described per record; the per-kind shape is enforced on decode.
type EntryDoc struct {
	Path           string        `json:"path"`
	Kind           string        `json:"kind"`
	ContentHash    string        `json:"content_hash,omitempty"`
	ContentStorage string        `json:"content_storage"`
	Size           int64         `json:"size"`
	Mode           uint32        `json:"mode"`
	MtimeNs        int64         `json:"mtime_ns"`
	CtimeNs        *int64        `json:"ctime_ns,omitempty"`
	Dev            *uint64       `json:"dev,omitempty"`
	Ino            *uint64       `json:"ino,omitempty"`
	Nlink          *uint64       `json:"nlink,omitempty"`
	OmittedStat    []string      `json:"omitted_stat_fields,omitempty"`
	SymlinkTarget  string        `json:"symlink_target,omitempty"`
	Traversal      *TraversalDoc `json:"traversal,omitempty"`
}

// SkippedDoc is the persisted form of one skipped input.
type SkippedDoc struct {
	Path          string        `json:"path"`
	Kind          string        `json:"kind"`
	Reason        string        `json:"reason"`
	Size          int64         `json:"size"`
	Mode          uint32        `json:"mode"`
	MtimeNs       int64         `json:"mtime_ns"`
	CtimeNs       *int64        `json:"ctime_ns,omitempty"`
	Dev           *uint64       `json:"dev,omitempty"`
	Ino           *uint64       `json:"ino,omitempty"`
	Nlink         *uint64       `json:"nlink,omitempty"`
	OmittedStat   []string      `json:"omitted_stat_fields,omitempty"`
	OSError       string        `json:"os_error,omitempty"`
	SymlinkTarget string        `json:"symlink_target,omitempty"`
	Traversal     *TraversalDoc `json:"traversal,omitempty"`
}

// Line is one record of a manifest.jsonl file: exactly one of Entry or Skipped is
// set. The discriminated wrapper keeps an entry and a skip from sharing the
// overlapping "path"/"kind" fields and makes "exactly one" checkable on read.
type Line struct {
	Entry   *EntryDoc   `json:"entry,omitempty"`
	Skipped *SkippedDoc `json:"skipped,omitempty"`
}

// EncodeEntry renders a worktree entry as its persisted doc.
func EncodeEntry(e worktree.Entry) EntryDoc {
	d := EntryDoc{
		Path:           e.Path.String(),
		Kind:           e.Kind.String(),
		ContentHash:    e.Content.String(),
		ContentStorage: e.Storage.String(),
		SymlinkTarget:  e.Symlink.String(),
		Traversal:      EncodeTraversal(e.Traversal),
	}
	// Directories carry no stat in the domain. The scalar stat fields (size, mode,
	// mtime_ns) have no omitempty, so they still serialize as zeros; what matters,
	// and what decode enforces, is that no non-zero scalar and no optional stat field
	// (ctime/dev/ino/nlink, omitted-set) is written for a directory.
	if e.Kind != worktree.KindDir {
		applyStatToEntry(&d, e.Stat)
	}
	return d
}

// DecodeEntry parses a persisted entry doc into a validated worktree entry.
func DecodeEntry(d EntryDoc) (worktree.Entry, error) {
	// Decoding is parse, not normalization: reject impossible raw shapes (a
	// directory carrying a content hash, a symlink without a target, a skipped
	// record masquerading as an entry) before constructing a domain entry, rather
	// than silently dropping the offending fields.
	if err := validateEntryShape(d); err != nil {
		return worktree.Entry{}, err
	}
	path, err := worktree.ParseRelPath(d.Path)
	if err != nil {
		return worktree.Entry{}, err
	}
	kind, err := worktree.ParseFileKind(d.Kind)
	if err != nil {
		return worktree.Entry{}, err
	}
	// Directories carry no domain stat; validateEntryShape has already rejected any
	// non-zero scalar stat field and any optional stat field on them, so only build a
	// signature for the kinds that have one.
	var stat worktree.StatSignature
	if kind != worktree.KindDir {
		stat, err = BuildStat(d.Size, d.Mode, d.MtimeNs, d.CtimeNs, d.Dev, d.Ino, d.Nlink, d.OmittedStat)
		if err != nil {
			return worktree.Entry{}, err
		}
	}
	traversal, err := DecodeTraversal(d.Traversal)
	if err != nil {
		return worktree.Entry{}, err
	}
	switch kind {
	case worktree.KindRegular:
		content, err := hashing.ParseContentHash(d.ContentHash)
		if err != nil {
			return worktree.Entry{}, err
		}
		storage, err := worktree.ParseContentStorageIntent(d.ContentStorage)
		if err != nil {
			return worktree.Entry{}, err
		}
		return worktree.NewRegularEntry(path, content, storage, stat, traversal)
	case worktree.KindDir:
		return worktree.NewDirEntry(path, traversal)
	case worktree.KindSymlink:
		target, err := worktree.NewSymlinkTarget(d.SymlinkTarget)
		if err != nil {
			return worktree.Entry{}, err
		}
		return worktree.NewSymlinkEntry(path, target, stat, traversal)
	default:
		return worktree.Entry{}, fmt.Errorf("entry %q has non-entry kind %q", d.Path, d.Kind)
	}
}

// validateEntryShape rejects raw manifest-entry shapes that cannot describe a
// real node, per kind, with an error that names the path and the offending
// category so later tooling (and CLI output) can point at the bad record.
func validateEntryShape(d EntryDoc) error {
	switch d.Kind {
	case worktree.KindRegular.String():
		if d.ContentHash == "" {
			return entryShapeErr(d.Path, d.Kind, "missing content_hash")
		}
		if d.ContentStorage != worktree.StorageBlob.String() && d.ContentStorage != worktree.StorageHashOnly.String() {
			return entryShapeErr(d.Path, d.Kind, fmt.Sprintf("content_storage %q must be %q or %q", d.ContentStorage, worktree.StorageBlob, worktree.StorageHashOnly))
		}
		if d.SymlinkTarget != "" {
			return entryShapeErr(d.Path, d.Kind, "must not have a symlink_target")
		}
	case worktree.KindDir.String():
		if d.ContentHash != "" {
			return entryShapeErr(d.Path, d.Kind, "must not have a content_hash")
		}
		if d.SymlinkTarget != "" {
			return entryShapeErr(d.Path, d.Kind, "must not have a symlink_target")
		}
		if d.ContentStorage != worktree.StorageNone.String() {
			return entryShapeErr(d.Path, d.Kind, fmt.Sprintf("content_storage %q must be %q", d.ContentStorage, worktree.StorageNone))
		}
		// A directory carries no stat in the domain. Its scalar stat fields must be
		// zero (the schema always serializes them) and it must carry no optional stat
		// field; a record with a non-zero scalar or any optional field is corrupt or
		// hand-edited, not a directory the constructor would reduce to zero on re-encode.
		if hasNonZeroStat(d) {
			return entryShapeErr(d.Path, d.Kind, "must not carry non-zero or optional stat fields")
		}
	case worktree.KindSymlink.String():
		if d.ContentHash != "" {
			return entryShapeErr(d.Path, d.Kind, "must not have a content_hash")
		}
		if d.SymlinkTarget == "" {
			return entryShapeErr(d.Path, d.Kind, "missing symlink_target")
		}
		if d.ContentStorage != worktree.StorageInlineSymlinkTarget.String() {
			return entryShapeErr(d.Path, d.Kind, fmt.Sprintf("content_storage %q must be %q", d.ContentStorage, worktree.StorageInlineSymlinkTarget))
		}
	default:
		// Special files and unknown kinds are never manifest entries; a skipped
		// record decoded here also lands in this branch or fails a kind check above.
		return entryShapeErr(d.Path, d.Kind, "is not a valid manifest entry kind")
	}
	return nil
}

// hasNonZeroStat reports whether a raw entry carries any non-zero scalar stat field
// or any optional stat field. A directory's scalar fields are allowed to be zero
// (the schema always serializes them), but anything beyond that is stat a directory
// cannot have.
func hasNonZeroStat(d EntryDoc) bool {
	return d.Size != 0 || d.Mode != 0 || d.MtimeNs != 0 ||
		d.CtimeNs != nil || d.Dev != nil || d.Ino != nil || d.Nlink != nil ||
		len(d.OmittedStat) != 0
}

func entryShapeErr(path, kind, msg string) error {
	return fmt.Errorf("invalid manifest entry %q (kind %q): %s", path, kind, msg)
}

// EncodeSkipped renders a skipped input as its persisted doc.
func EncodeSkipped(s worktree.SkippedInput) SkippedDoc {
	d := SkippedDoc{
		Path:          s.Path.String(),
		Kind:          s.Kind.String(),
		Reason:        s.Reason.String(),
		OSError:       s.OSError,
		SymlinkTarget: s.Symlink.String(),
		Traversal:     EncodeTraversal(s.Traversal),
	}
	applyStatToSkipped(&d, s.Stat)
	// SkippedInput carries its own size, which may be set even when the node
	// could not be stat'd, so it overrides the stat-derived size.
	d.Size = s.Size
	return d
}

// DecodeSkipped parses a persisted skipped doc into a validated skipped input.
func DecodeSkipped(d SkippedDoc) (worktree.SkippedInput, error) {
	path, err := worktree.ParseRelPath(d.Path)
	if err != nil {
		return worktree.SkippedInput{}, err
	}
	reason, err := worktree.ParseSkippedReason(d.Reason)
	if err != nil {
		return worktree.SkippedInput{}, err
	}
	// Decode is a parse, not a normalization: the persisted kind must agree with the
	// kind the reason implies, so a hand-edited record like {kind:"file",
	// reason:"special-file"} is rejected rather than silently re-derived from reason.
	kind, err := worktree.ParseFileKind(d.Kind)
	if err != nil {
		return worktree.SkippedInput{}, err
	}
	if kind != reason.Kind() {
		return worktree.SkippedInput{}, fmt.Errorf("invalid skipped record %q: kind %q does not match reason %q (kind %q)", d.Path, d.Kind, d.Reason, reason.Kind())
	}
	stat, err := BuildStat(d.Size, d.Mode, d.MtimeNs, d.CtimeNs, d.Dev, d.Ino, d.Nlink, d.OmittedStat)
	if err != nil {
		return worktree.SkippedInput{}, err
	}
	traversal, err := DecodeTraversal(d.Traversal)
	if err != nil {
		return worktree.SkippedInput{}, err
	}
	var target worktree.SymlinkTarget
	if d.SymlinkTarget != "" {
		target, err = worktree.NewSymlinkTarget(d.SymlinkTarget)
		if err != nil {
			return worktree.SkippedInput{}, err
		}
	}
	return worktree.NewSkippedInput(path, reason, d.Size, stat, d.OSError, target, traversal)
}

// EncodeTraversal renders a traversal note, or nil when the node was not followed.
func EncodeTraversal(t worktree.TraversalInfo) *TraversalDoc {
	if !t.Followed {
		return nil
	}
	return &TraversalDoc{SourcePath: t.SourcePath.String(), Depth: t.Depth}
}

// DecodeTraversal parses a traversal note, or the zero value when absent.
func DecodeTraversal(t *TraversalDoc) (worktree.TraversalInfo, error) {
	if t == nil {
		return worktree.TraversalInfo{}, nil
	}
	src, err := worktree.ParseRelPath(t.SourcePath)
	if err != nil {
		return worktree.TraversalInfo{}, err
	}
	return worktree.TraversalInfo{Followed: true, SourcePath: src, Depth: t.Depth}, nil
}

// applyStatToEntry writes a stat signature into an entry doc, emitting the
// platform-optional fields as pointers that are nil exactly when the field was
// omitted, so an omission is encoded as absence rather than a zero value.
func applyStatToEntry(d *EntryDoc, s worktree.StatSignature) {
	d.Size = s.Size
	d.Mode = s.Mode
	d.MtimeNs = s.MtimeNs
	d.CtimeNs, d.Dev, d.Ino, d.Nlink = optionalStatFields(s)
	d.OmittedStat = s.Omitted.Tokens()
}

func applyStatToSkipped(d *SkippedDoc, s worktree.StatSignature) {
	d.Size = s.Size
	d.Mode = s.Mode
	d.MtimeNs = s.MtimeNs
	d.CtimeNs, d.Dev, d.Ino, d.Nlink = optionalStatFields(s)
	d.OmittedStat = s.Omitted.Tokens()
}

func optionalStatFields(s worktree.StatSignature) (ctime *int64, dev, ino, nlink *uint64) {
	if !s.Omitted.Has(worktree.FieldCtime) {
		v := s.CtimeNs
		ctime = &v
	}
	if !s.Omitted.Has(worktree.FieldDev) {
		v := s.Dev
		dev = &v
	}
	if !s.Omitted.Has(worktree.FieldIno) {
		v := s.Ino
		ino = &v
	}
	if !s.Omitted.Has(worktree.FieldNlink) {
		v := s.Nlink
		nlink = &v
	}
	return
}

// BuildStat reconstructs a stat signature from the persisted scalar and optional
// fields, rejecting any present/omitted mismatch so a corrupt record is never
// silently normalized into a different stat than it claims.
func BuildStat(size int64, mode uint32, mtime int64, ctime *int64, dev, ino, nlink *uint64, omittedTokens []string) (worktree.StatSignature, error) {
	omitted, err := worktree.ParseFieldSet(omittedTokens)
	if err != nil {
		return worktree.StatSignature{}, err
	}
	// A platform-optional field is present in the document exactly when it is not
	// in the omitted set. Rejecting either mismatch — present-but-omitted or
	// missing-but-not-omitted — keeps a corrupt checkpoint from being silently
	// normalized into a different stat than it claims.
	for _, f := range []struct {
		field   worktree.StatField
		present bool
		name    string
	}{
		{worktree.FieldCtime, ctime != nil, "ctime_ns"},
		{worktree.FieldDev, dev != nil, "dev"},
		{worktree.FieldIno, ino != nil, "ino"},
		{worktree.FieldNlink, nlink != nil, "nlink"},
	} {
		if omitted.Has(f.field) && f.present {
			return worktree.StatSignature{}, fmt.Errorf("stat field %s is present but marked omitted", f.name)
		}
		if !omitted.Has(f.field) && !f.present {
			return worktree.StatSignature{}, fmt.Errorf("stat field %s is missing but not marked omitted", f.name)
		}
	}
	s := worktree.StatSignature{Size: size, Mode: mode, MtimeNs: mtime, Omitted: omitted}
	if ctime != nil {
		s.CtimeNs = *ctime
	}
	if dev != nil {
		s.Dev = *dev
	}
	if ino != nil {
		s.Ino = *ino
	}
	if nlink != nil {
		s.Nlink = *nlink
	}
	return s, nil
}
