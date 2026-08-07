package worktree

import "fmt"

// FileKind classifies a worktree node. The set is closed by Parse and Valid and
// re-checked at every boundary, so a kind outside this set is rejected rather
// than acted on — Go cannot make the enum exhaustive at compile time, so the
// guarantee is upheld by validation, not the type alone.
type FileKind int

const (
	// KindRegular is an ordinary file whose contents are hashed.
	KindRegular FileKind = iota
	// KindDir is a directory; it contributes structure but no content.
	KindDir
	// KindSymlink is a symbolic link recorded by its target, not followed by
	// default.
	KindSymlink
	// KindSpecial is anything else (device, socket, fifo): never hashed, always
	// recorded as a skipped input.
	KindSpecial
)

// ParseFileKind resolves a persisted token to a FileKind.
func ParseFileKind(s string) (FileKind, error) {
	switch s {
	case "file":
		return KindRegular, nil
	case "dir":
		return KindDir, nil
	case "symlink":
		return KindSymlink, nil
	case "special":
		return KindSpecial, nil
	default:
		return 0, fmt.Errorf("invalid file kind %q", s)
	}
}

func (k FileKind) String() string {
	switch k {
	case KindRegular:
		return "file"
	case KindDir:
		return "dir"
	case KindSymlink:
		return "symlink"
	case KindSpecial:
		return "special"
	default:
		return "unknown"
	}
}

// Valid reports whether k is a known kind.
func (k FileKind) Valid() bool {
	return k == KindRegular || k == KindDir || k == KindSymlink || k == KindSpecial
}
