package worktreefs

import (
	ignore "github.com/sabhiram/go-gitignore"

	"awarer/internal/domain/worktree"
)

// patternSelector is a root-relative pattern list compiled with the same matcher the
// ignore engine uses, so a configured selection reads exactly like an .awaignore file
// at the project root, including that matcher's known departures from Git (its
// syntax-dependent anchoring, a literal "?", silently dropped malformed patterns):
// case-sensitive, later rules overriding earlier ones (a "!" exception un-selects),
// and a matching directory selecting every path below it.
type patternSelector struct {
	m *ignore.GitIgnore
}

// CompilePatterns compiles an ordered root-relative pattern list into a selector.
// Blank and comment lines select nothing, as in an ignore file. An empty list
// selects nothing at all.
func CompilePatterns(patterns []string) worktree.PathSelector {
	var lines []string
	for _, p := range patterns {
		if isPattern(p) {
			lines = append(lines, p)
		}
	}
	if len(lines) == 0 {
		return patternSelector{}
	}
	return patternSelector{m: ignore.CompileIgnoreLines(lines...)}
}

// Selects reports whether the list selects p. It queries the file form of the path:
// a directory-only pattern ("secrets/") still selects every file below it.
func (s patternSelector) Selects(p worktree.RelPath) bool {
	if s.m == nil {
		return false
	}
	return s.m.MatchesPath(p.String())
}
