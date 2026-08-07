// Package worktree is the pure domain model for a scanned working tree.
//
// It owns the value objects the scanner produces — normalized paths, file
// kinds, stat signatures, entries, skipped inputs, scan identity and results —
// and the canonical, deterministic tree-hash construction. It also declares the
// ports (Walker, Hasher consumers, Index) the application scanner depends on, so
// the filesystem, hashing library, and SQLite index are all injected rather than
// imported. The package performs no I/O.
package worktree

import (
	"fmt"
	"path/filepath"
	"strings"
)

// RelPath is a project-relative path normalized to forward slashes with case
// preserved exactly. It never escapes the project root and is never absolute, so
// downstream code can treat it as a safe key and sort it deterministically.
type RelPath struct {
	p string
}

// ParseRelPath normalizes and validates a project-relative path. It accepts
// either OS-native or slash separators, rejects absolute paths, the root itself
// ("." or ""), and any path that escapes the root via "..". Case is preserved.
func ParseRelPath(raw string) (RelPath, error) {
	if raw == "" {
		return RelPath{}, fmt.Errorf("invalid path: empty")
	}
	if filepath.IsAbs(raw) || strings.HasPrefix(raw, "/") {
		return RelPath{}, fmt.Errorf("invalid path %q: must be relative to the project root", raw)
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.ToSlash(raw)))
	if clean == "." || clean == "" {
		return RelPath{}, fmt.Errorf("invalid path %q: must name an entry below the root", raw)
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return RelPath{}, fmt.Errorf("invalid path %q: must not escape the project root", raw)
	}
	return RelPath{p: clean}, nil
}

// String returns the normalized slash-separated path.
func (r RelPath) String() string { return r.p }

// IsZero reports whether r is the zero (unset) path.
func (r RelPath) IsZero() bool { return r.p == "" }

// Less orders paths by their raw UTF-8 bytes, giving a stable total order
// independent of filesystem walk order. The tree hash relies on this.
func (r RelPath) Less(other RelPath) bool { return r.p < other.p }

// Equal reports byte-for-byte path equality (case-sensitive).
func (r RelPath) Equal(other RelPath) bool { return r.p == other.p }
