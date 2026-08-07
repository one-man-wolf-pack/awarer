package worktree

import (
	"fmt"

	"awarer/internal/domain/hashing"
)

// ContentStorageIntent records how a node's content relates to the blob store. It is
// the scan-time classification, so StorageBlob expresses intent ("this content would
// be stored") rather than a completed write; materializing the blob is the
// checkpoint's separate step.
type ContentStorageIntent int

const (
	// StorageBlob: content is (or would be) stored as a blob.
	StorageBlob ContentStorageIntent = iota
	// StorageHashOnly: content is hashed and included in the tree, but no blob
	// is stored (large-file hash-only policy).
	StorageHashOnly
	// StorageSkipped: content was not hashed; the node is a skipped input.
	StorageSkipped
	// StorageInlineSymlinkTarget: the "content" is the symlink target string.
	StorageInlineSymlinkTarget
	// StorageNone: no content applies (e.g. a directory).
	StorageNone
)

// ParseContentStorageIntent resolves a persisted token.
func ParseContentStorageIntent(s string) (ContentStorageIntent, error) {
	switch s {
	case "blob":
		return StorageBlob, nil
	case "hash-only":
		return StorageHashOnly, nil
	case "skipped":
		return StorageSkipped, nil
	case "inline-symlink-target":
		return StorageInlineSymlinkTarget, nil
	case "none":
		return StorageNone, nil
	default:
		return 0, fmt.Errorf("invalid content storage intent %q", s)
	}
}

func (c ContentStorageIntent) String() string {
	switch c {
	case StorageBlob:
		return "blob"
	case StorageHashOnly:
		return "hash-only"
	case StorageSkipped:
		return "skipped"
	case StorageInlineSymlinkTarget:
		return "inline-symlink-target"
	case StorageNone:
		return "none"
	default:
		return "unknown"
	}
}

// Valid reports whether c is a known intent.
func (c ContentStorageIntent) Valid() bool {
	return c >= StorageBlob && c <= StorageNone
}

// SymlinkTarget is the raw, unresolved target of a symbolic link as read with
// os.Readlink. It is recorded verbatim — the link is identified by where it
// points, not by what lives there.
type SymlinkTarget struct {
	target string
}

// NewSymlinkTarget wraps a non-empty link target.
func NewSymlinkTarget(target string) (SymlinkTarget, error) {
	if target == "" {
		return SymlinkTarget{}, fmt.Errorf("invalid symlink target: empty")
	}
	return SymlinkTarget{target: target}, nil
}

// String returns the raw target.
func (s SymlinkTarget) String() string { return s.target }

// IsZero reports whether no target is set.
func (s SymlinkTarget) IsZero() bool { return s.target == "" }

// TraversalInfo describes how a node was reached when follow_symlinks is on. For
// nodes reached directly it is the zero value (Followed false).
type TraversalInfo struct {
	// Followed is true when the node was reached by traversing one or more
	// symlinks.
	Followed bool
	// SourcePath is the symlink path through which the node was reached.
	SourcePath RelPath
	// Depth is the number of symlink hops taken to reach the node.
	Depth int
}

// Validate enforces the provenance invariant: a node reached through a symlink
// names its source symlink and a positive hop depth, and a node reached directly
// carries neither. This keeps the "how was this reached" provenance in the type
// rather than in a comment.
func (t TraversalInfo) Validate() error {
	if t.Followed {
		if t.SourcePath.IsZero() {
			return fmt.Errorf("followed node has no source symlink path")
		}
		if t.Depth <= 0 {
			return fmt.Errorf("followed node has non-positive depth %d", t.Depth)
		}
		return nil
	}
	if !t.SourcePath.IsZero() {
		return fmt.Errorf("directly-reached node has a source symlink path %q", t.SourcePath)
	}
	if t.Depth != 0 {
		return fmt.Errorf("directly-reached node has non-zero depth %d", t.Depth)
	}
	return nil
}

// Entry is one node of a scanned tree, sorted by Path. Content is the zero
// ContentHash for kinds that carry no content (directories, symlinks). Its fields
// are exported so the persistence and tree-hash layers can read them, but the
// New*Entry constructors are the intended way to build one: they bind storage and
// content to the kind so an entry cannot be assembled in an impossible state (a
// directory with a content hash, a regular file with no hash, a symlink with no
// target). TreeReducer.Add re-checks these invariants as it folds each record, so a
// hand-assembled entry that bypassed a constructor fails at the hashing and
// persistence boundary rather than being stored.
type Entry struct {
	Path      RelPath
	Kind      FileKind
	Content   hashing.ContentHash
	Storage   ContentStorageIntent
	Stat      StatSignature
	Symlink   SymlinkTarget
	Traversal TraversalInfo
}

// Hardlink returns the entry's hardlink identity, derived from its stat signature.
// It is computed rather than stored so it can never disagree with Stat.
func (e Entry) Hardlink() HardlinkIdentity { return e.Stat.HardlinkIdentity() }

// NewRegularEntry builds a regular-file entry. storage must be StorageBlob or
// StorageHashOnly, and content must be a real hash. Mode and hardlink identity are
// read from the stat signature, so they cannot disagree with it. The result is
// shape-validated, so an invalid traversal or unknown omitted stat field is
// rejected at construction, not only later at tree construction.
func NewRegularEntry(path RelPath, content hashing.ContentHash, storage ContentStorageIntent, stat StatSignature, traversal TraversalInfo) (Entry, error) {
	e := Entry{
		Path:      path,
		Kind:      KindRegular,
		Content:   content,
		Storage:   storage,
		Stat:      stat,
		Traversal: traversal,
	}
	if err := e.Validate(); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// NewDirEntry builds a directory entry, which carries only its path, kind, and
// traversal provenance.
func NewDirEntry(path RelPath, traversal TraversalInfo) (Entry, error) {
	e := Entry{Path: path, Kind: KindDir, Storage: StorageNone, Traversal: traversal}
	if err := e.Validate(); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// NewSymlinkEntry builds a symlink entry, identified by its target.
func NewSymlinkEntry(path RelPath, target SymlinkTarget, stat StatSignature, traversal TraversalInfo) (Entry, error) {
	e := Entry{
		Path:      path,
		Kind:      KindSymlink,
		Storage:   StorageInlineSymlinkTarget,
		Stat:      stat,
		Symlink:   target,
		Traversal: traversal,
	}
	if err := e.Validate(); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// Validate checks every entry invariant: path, stat-omission validity, traversal
// provenance, and the per-kind binding of storage and content. The constructors
// call it too, so an entry cannot be assembled in an impossible state and cannot
// reach a boundary in one either — tree construction and persistence both re-run
// it. Special files and unknown kinds are never entries (they are skipped inputs),
// so only regular files, directories, and symlinks are accepted.
func (e Entry) Validate() error {
	if e.Path.IsZero() {
		return fmt.Errorf("tree entry has no path")
	}
	if !e.Stat.Omitted.Valid() {
		return fmt.Errorf("tree entry %q has unknown omitted stat fields", e.Path)
	}
	// A negative size is an impossible state from a corrupt or hand-edited record
	// (the scanner never produces one); reject it so it cannot yield a negative
	// total_bytes downstream. Directories carry a zero stat, so this never trips them.
	if e.Stat.Size < 0 {
		return fmt.Errorf("tree entry %q has negative size %d", e.Path, e.Stat.Size)
	}
	if err := e.Traversal.Validate(); err != nil {
		return fmt.Errorf("tree entry %q: %w", e.Path, err)
	}
	switch e.Kind {
	case KindRegular:
		if e.Content.IsZero() {
			return fmt.Errorf("regular entry %q has no content hash", e.Path)
		}
		if e.Storage != StorageBlob && e.Storage != StorageHashOnly {
			return fmt.Errorf("regular entry %q has storage %s, want blob or hash-only", e.Path, e.Storage)
		}
	case KindDir:
		if !e.Content.IsZero() {
			return fmt.Errorf("directory entry %q must not carry a content hash", e.Path)
		}
		if e.Storage != StorageNone {
			return fmt.Errorf("directory entry %q has storage %s, want none", e.Path, e.Storage)
		}
	case KindSymlink:
		if e.Symlink.IsZero() {
			return fmt.Errorf("symlink entry %q has no target", e.Path)
		}
		if !e.Content.IsZero() {
			return fmt.Errorf("symlink entry %q must not carry a content hash", e.Path)
		}
		if e.Storage != StorageInlineSymlinkTarget {
			return fmt.Errorf("symlink entry %q has storage %s, want inline-symlink-target", e.Path, e.Storage)
		}
	default:
		return fmt.Errorf("tree entry %q has invalid kind %v", e.Path, e.Kind)
	}
	return nil
}
