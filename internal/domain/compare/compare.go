// Package compare is the pure domain model for comparing two worktree states.
//
// It takes the manifest content of a left (old) and a right (new) state —
// entries plus skipped inputs — and produces a deterministic change set. It
// performs no I/O and knows nothing about checkpoints, blobs, or the scanner: a
// state is just its entries and skipped inputs. Identity is decided with the
// same semantics the tree hash uses (kind, content hash, permission bits,
// symlink target, traversal provenance), so two states that hash equal produce
// no changes, and a difference the tree hash would notice is reported here.
package compare

import (
	"sort"
	"strings"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
)

// Status classifies a single reported change.
type Status int

const (
	// Added: the path exists only in the right state.
	Added Status = iota
	// Modified: same path and kind, but a different semantic identity.
	Modified
	// Deleted: the path exists only in the left state.
	Deleted
	// Renamed: a delete and an add paired by identical content (or target).
	Renamed
	// TypeChanged: same path, different kind.
	TypeChanged
	// Skipped: the change involves a skipped/unavailable input on a side where
	// no clearer add/modify/delete/type status applies.
	Skipped
)

// Code returns the single-letter status code used in human output.
func (s Status) Code() string {
	switch s {
	case Added:
		return "A"
	case Modified:
		return "M"
	case Deleted:
		return "D"
	case Renamed:
		return "R"
	case TypeChanged:
		return "T"
	case Skipped:
		return "S"
	default:
		return "?"
	}
}

// String returns a stable lowercase token for JSON output.
func (s Status) String() string {
	switch s {
	case Added:
		return "added"
	case Modified:
		return "modified"
	case Deleted:
		return "deleted"
	case Renamed:
		return "renamed"
	case TypeChanged:
		return "type-changed"
	case Skipped:
		return "skipped"
	default:
		return "unknown"
	}
}

// Change is one reported difference between the two states. Old* fields describe
// the left side, New* fields the right side; a side absent for the status leaves
// its fields zero. For modify/type/skipped, OldPath and NewPath are the same
// path. ContentDiffAvailable is true only when there is a meaningful content diff
// to render: a regular file whose content hash actually differs and whose bytes
// are stored as blobs on both sides. It is a domain fact (settled here), not a
// renderer decision. A hash-only or skipped side leaves it false (no bytes), and
// so does a metadata-only modification (same content hash, changed mode or
// traversal) — there is nothing to diff, so it explains itself through Note.
type Change struct {
	Status     Status
	OldPath    worktree.RelPath
	NewPath    worktree.RelPath
	OldKind    worktree.FileKind
	NewKind    worktree.FileKind
	OldContent hashing.ContentHash
	NewContent hashing.ContentHash
	OldStorage worktree.ContentStorageIntent
	NewStorage worktree.ContentStorageIntent
	OldSize    int64
	NewSize    int64
	OldSymlink worktree.SymlinkTarget
	NewSymlink worktree.SymlinkTarget
	// OldMode and NewMode are the regular-file permission bits (0o777) on each
	// side; OldModeSet/NewModeSet report whether that side carries one (a present
	// regular file). The explicit flags are needed because 0000 is valid
	// permission bits, so a zero value cannot mean "absent". Mode is recorded
	// independently of ContentDiffAvailable, so a permission change stays visible
	// even when the content also changed and a textual diff is shown.
	OldMode    uint32
	NewMode    uint32
	OldModeSet bool
	NewModeSet bool
	// ContentDiffAvailable: see the type-level contract above.
	ContentDiffAvailable bool
	// Note carries a short human reason when content is unavailable or the change
	// is a skip (e.g. "hash-only", "skipped: read-error", "traversal changed").
	Note string
}

// ModeChanged reports whether a regular file's permission bits changed between
// the two states — both sides present, regular, and differing. It compares the
// set flags, not zero values, so a change to or from 0000 is detected.
func (c Change) ModeChanged() bool {
	return c.OldModeSet && c.NewModeSet && c.OldMode != c.NewMode
}

// PrimaryPath is the path used to sort and to display the change in name-only
// modes: the new path for everything except a delete, which has only an old path.
func (c Change) PrimaryPath() worktree.RelPath {
	if c.Status == Deleted {
		return c.OldPath
	}
	return c.NewPath
}

// Summary counts changes by status.
type Summary struct {
	Added       int `json:"added"`
	Modified    int `json:"modified"`
	Deleted     int `json:"deleted"`
	Renamed     int `json:"renamed"`
	TypeChanged int `json:"type_changed"`
	Skipped     int `json:"skipped"`
}

// Total returns the number of changes summarized.
func (s Summary) Total() int {
	return s.Added + s.Modified + s.Deleted + s.Renamed + s.TypeChanged + s.Skipped
}

// Add folds one change into the counts. It is the incremental counterpart of
// summarize, so a streaming renderer can accumulate bounded summary stats from a
// change cursor without retaining the changes it counts.
func (s *Summary) Add(c Change) {
	switch c.Status {
	case Added:
		s.Added++
	case Modified:
		s.Modified++
	case Deleted:
		s.Deleted++
	case Renamed:
		s.Renamed++
	case TypeChanged:
		s.TypeChanged++
	case Skipped:
		s.Skipped++
	}
}

// ChangeSet is the result of comparing two states: the sorted changes, their
// summary counts, and the rename-detection outcome (whether rename pairing was
// requested and whether the bounded presentation policy actually applied it).
type ChangeSet struct {
	Changes         []Change
	Summary         Summary
	RenameDetection RenameDetection
}

// Options tunes a comparison.
type Options struct {
	// DetectRenames pairs a unique deleted+added regular file (or symlink) with
	// identical content (or target) into a single rename.
	DetectRenames bool
	// PathFilters narrows the reported changes to the named paths and their
	// descendants. Empty means no filtering. The paths are project-root-relative.
	PathFilters []worktree.RelPath
}

// node is a path's presence on one side: either an entry or a skipped input.
type node struct {
	skipped bool
	entry   worktree.Entry
	skip    worktree.SkippedInput
}

// comparePresent compares a node present on both sides, returning whether it
// changed and the change record. Equal nodes are omitted.
func comparePresent(l, r node) (Change, bool) {
	switch {
	case !l.skipped && !r.skipped:
		return compareEntries(l.entry, r.entry)
	case l.skipped && r.skipped:
		if skipEqual(l.skip, r.skip) {
			return Change{}, false
		}
		return skipChange(l, r), true
	default:
		// entry <-> skipped transition: a readable file became unavailable, or an
		// unavailable input became readable. Reported as S; no clearer status.
		return skipChange(l, r), true
	}
}

func compareEntries(l, r worktree.Entry) (Change, bool) {
	if l.Kind != r.Kind {
		return Change{
			Status:     TypeChanged,
			OldPath:    l.Path,
			NewPath:    r.Path,
			OldKind:    l.Kind,
			NewKind:    r.Kind,
			OldContent: l.Content,
			NewContent: r.Content,
			OldStorage: l.Storage,
			NewStorage: r.Storage,
			OldSize:    l.Stat.Size,
			NewSize:    r.Stat.Size,
			OldSymlink: l.Symlink,
			NewSymlink: r.Symlink,
		}, true
	}
	if entryIdentityEqual(l, r) {
		return Change{}, false
	}
	ch := Change{
		Status:     Modified,
		OldPath:    l.Path,
		NewPath:    r.Path,
		OldKind:    l.Kind,
		NewKind:    r.Kind,
		OldContent: l.Content,
		NewContent: r.Content,
		OldStorage: l.Storage,
		NewStorage: r.Storage,
		OldSize:    l.Stat.Size,
		NewSize:    r.Stat.Size,
		OldSymlink: l.Symlink,
		NewSymlink: r.Symlink,
	}
	if l.Kind == worktree.KindRegular {
		// Mode is recorded structurally and independently of the content diff, so a
		// permission change stays visible even alongside a content edit.
		ch.OldMode, ch.OldModeSet = l.Stat.Mode&0o777, true
		ch.NewMode, ch.NewModeSet = r.Stat.Mode&0o777, true
		// A content diff is only possible when the content hash actually differs
		// (and both sides are stored as blobs). A regular modify whose content hash
		// is unchanged is a metadata-only change — mode bits or traversal — with
		// nothing to diff, so it must not claim content-diff availability.
		contentDiffers := l.Content != r.Content
		ch.ContentDiffAvailable = contentDiffers &&
			l.Storage == worktree.StorageBlob && r.Storage == worktree.StorageBlob
		switch {
		case !contentDiffers:
			// Metadata-only: mode is already in OldMode/NewMode, so Note only needs
			// to name a traversal change (the other thing entry identity covers).
			ch.Note = traversalChangeNote(l, r)
		case !ch.ContentDiffAvailable:
			ch.Note = "hash-only"
		}
	}
	return ch, true
}

// traversalChangeNote names a traversal-provenance change for a regular-file
// modification whose content hash and mode are unchanged. Mode changes are
// carried structurally (OldMode/NewMode), so they are not repeated here.
func traversalChangeNote(l, r worktree.Entry) string {
	if l.Traversal != r.Traversal {
		return "traversal changed"
	}
	return ""
}

// entryIdentityEqual mirrors the fields the tree hash folds into an entry's
// identity (worktree.entryRecord): kind plus, per kind, content hash and
// permission bits, or symlink target, and always the traversal provenance.
// Volatile stat and the content-storage class are deliberately excluded, so a
// blob-vs-hash-only policy change is not a worktree change.
func entryIdentityEqual(l, r worktree.Entry) bool {
	if l.Kind != r.Kind {
		return false
	}
	if l.Traversal != r.Traversal {
		return false
	}
	switch l.Kind {
	case worktree.KindRegular:
		return l.Content == r.Content && (l.Stat.Mode&0o777) == (r.Stat.Mode&0o777)
	case worktree.KindDir:
		return true
	case worktree.KindSymlink:
		return l.Symlink == r.Symlink
	default:
		return false
	}
}

// skipEqual mirrors worktree.skippedRecord: kind, reason, size, target, mtime,
// permission bits, and traversal provenance.
func skipEqual(l, r worktree.SkippedInput) bool {
	return l.Kind == r.Kind &&
		l.Reason == r.Reason &&
		l.Size == r.Size &&
		l.Symlink == r.Symlink &&
		l.Stat.MtimeNs == r.Stat.MtimeNs &&
		(l.Stat.Mode&0o777) == (r.Stat.Mode&0o777) &&
		l.Traversal == r.Traversal
}

func deletedChange(n node) Change {
	if n.skipped {
		return Change{
			Status:  Deleted,
			OldPath: n.skip.Path,
			OldKind: n.skip.Kind,
			OldSize: n.skip.Size,
			Note:    "skipped: " + n.skip.Reason.String(),
		}
	}
	e := n.entry
	ch := Change{
		Status:     Deleted,
		OldPath:    e.Path,
		OldKind:    e.Kind,
		OldContent: e.Content,
		OldStorage: e.Storage,
		OldSize:    e.Stat.Size,
		OldSymlink: e.Symlink,
	}
	if e.Kind == worktree.KindRegular {
		ch.OldMode, ch.OldModeSet = e.Stat.Mode&0o777, true
		ch.ContentDiffAvailable = e.Storage == worktree.StorageBlob
		if !ch.ContentDiffAvailable {
			ch.Note = "hash-only"
		}
	}
	return ch
}

func addedChange(n node) Change {
	if n.skipped {
		return Change{
			Status:  Added,
			NewPath: n.skip.Path,
			NewKind: n.skip.Kind,
			NewSize: n.skip.Size,
			Note:    "skipped: " + n.skip.Reason.String(),
		}
	}
	e := n.entry
	ch := Change{
		Status:     Added,
		NewPath:    e.Path,
		NewKind:    e.Kind,
		NewContent: e.Content,
		NewStorage: e.Storage,
		NewSize:    e.Stat.Size,
		NewSymlink: e.Symlink,
	}
	if e.Kind == worktree.KindRegular {
		ch.NewMode, ch.NewModeSet = e.Stat.Mode&0o777, true
		ch.ContentDiffAvailable = e.Storage == worktree.StorageBlob
		if !ch.ContentDiffAvailable {
			ch.Note = "hash-only"
		}
	}
	return ch
}

// skipChange builds an S record for a both-present transition involving a skip.
// It carries whatever metadata each side has so output can explain the change.
func skipChange(l, r node) Change {
	ch := Change{Status: Skipped}
	if l.skipped {
		ch.OldPath = l.skip.Path
		ch.OldKind = l.skip.Kind
		ch.OldSize = l.skip.Size
		ch.OldSymlink = l.skip.Symlink
	} else {
		ch.OldPath = l.entry.Path
		ch.OldKind = l.entry.Kind
		ch.OldContent = l.entry.Content
		ch.OldStorage = l.entry.Storage
		ch.OldSize = l.entry.Stat.Size
		ch.OldSymlink = l.entry.Symlink
	}
	if r.skipped {
		ch.NewPath = r.skip.Path
		ch.NewKind = r.skip.Kind
		ch.NewSize = r.skip.Size
		ch.NewSymlink = r.skip.Symlink
		ch.Note = "skipped: " + r.skip.Reason.String()
	} else {
		ch.NewPath = r.entry.Path
		ch.NewKind = r.entry.Kind
		ch.NewContent = r.entry.Content
		ch.NewStorage = r.entry.Storage
		ch.NewSize = r.entry.Stat.Size
		ch.NewSymlink = r.entry.Symlink
		ch.Note = "now readable"
	}
	return ch
}

// detectRenames pairs a unique deleted regular file with a unique added regular
// file of identical non-zero content (and likewise symlinks by identical
// target). Ambiguous identities — more than one delete or add sharing an
// identity — are left as separate delete/add to avoid false renames.
func detectRenames(changes []Change) []Change {
	delByKey := map[string][]int{}
	addByKey := map[string][]int{}
	for i, c := range changes {
		switch c.Status {
		case Deleted:
			if k, ok := renameKey(c.OldKind, c.OldContent, c.OldSymlink); ok {
				delByKey[k] = append(delByKey[k], i)
			}
		case Added:
			if k, ok := renameKey(c.NewKind, c.NewContent, c.NewSymlink); ok {
				addByKey[k] = append(addByKey[k], i)
			}
		}
	}

	consumed := make(map[int]bool)
	var renames []Change
	for k, dels := range delByKey {
		adds := addByKey[k]
		if len(dels) != 1 || len(adds) != 1 {
			continue // ambiguous: leave as delete/add
		}
		d := changes[dels[0]]
		a := changes[adds[0]]
		consumed[dels[0]] = true
		consumed[adds[0]] = true
		renames = append(renames, Change{
			Status:     Renamed,
			OldPath:    d.OldPath,
			NewPath:    a.NewPath,
			OldKind:    d.OldKind,
			NewKind:    a.NewKind,
			OldContent: d.OldContent,
			NewContent: a.NewContent,
			OldStorage: d.OldStorage,
			NewStorage: a.NewStorage,
			OldSize:    d.OldSize,
			NewSize:    a.NewSize,
			OldSymlink: d.OldSymlink,
			NewSymlink: a.NewSymlink,
			// A conservative rename pairs identical content, so there is nothing to
			// diff; rename metadata is shown without a content diff.
			ContentDiffAvailable: false,
		})
	}

	out := make([]Change, 0, len(changes))
	for i, c := range changes {
		if consumed[i] {
			continue
		}
		out = append(out, c)
	}
	return append(out, renames...)
}

// renameKey returns a pairing key for a rename candidate, or false if the node
// is not a regular file with non-zero content or a symlink with a target.
func renameKey(kind worktree.FileKind, content hashing.ContentHash, target worktree.SymlinkTarget) (string, bool) {
	switch kind {
	case worktree.KindRegular:
		if content.IsZero() {
			return "", false
		}
		return "f:" + content.String(), true
	case worktree.KindSymlink:
		if target.IsZero() {
			return "", false
		}
		return "s:" + target.String(), true
	default:
		return "", false
	}
}

// matchesAny reports whether a change touches any filter path. A filter matches
// the exact path or any descendant, so a directory filter includes its subtree
// and a file filter matches that file — one rule covers both.
func matchesAny(c Change, filters []worktree.RelPath) bool {
	for _, f := range filters {
		if pathMatches(c.OldPath, f) || pathMatches(c.NewPath, f) {
			return true
		}
	}
	return false
}

func pathMatches(p, filter worktree.RelPath) bool {
	if p.IsZero() {
		return false
	}
	if p.Equal(filter) {
		return true
	}
	return strings.HasPrefix(p.String(), filter.String()+"/")
}

func sortChanges(changes []Change) {
	sort.Slice(changes, func(i, j int) bool {
		pi := changes[i].PrimaryPath()
		pj := changes[j].PrimaryPath()
		if !pi.Equal(pj) {
			return pi.Less(pj)
		}
		return changes[i].Status < changes[j].Status
	})
}

func summarize(changes []Change) Summary {
	var s Summary
	for _, c := range changes {
		s.Add(c)
	}
	return s
}
