package worktreefs

import "strings"

// includeFilter implements scope inclusion. The default scope ["."] includes the
// whole tree; a narrower scope restricts the walk to the listed paths (and the
// directories on the way to them, so the walk can descend to reach them).
type includeFilter struct {
	all      bool
	includes []string
}

// newIncludeFilter builds a filter from an already-canonical include list (see
// config.Scope.CanonicalInclude), so the walker and the config hash agree on the
// effective scope. A canonical list of ["."], or an empty one, means the whole
// tree.
func newIncludeFilter(canonicalInclude []string) includeFilter {
	if len(canonicalInclude) == 0 {
		return includeFilter{all: true}
	}
	for _, inc := range canonicalInclude {
		if inc == "." {
			return includeFilter{all: true}
		}
	}
	return includeFilter{includes: canonicalInclude}
}

// overridesExcludes reports whether an explicit, narrowed scope is in effect.
// When it is, explicit includes take higher priority than configurable and
// default excludes, so the walker bypasses the ignore engine for in-scope
// paths — a path the user deliberately scoped to is scanned even if a baseline,
// history, .awaignore, or .gitignore rule would otherwise drop it. The whole-tree default
// scope (["."]) is not an explicit include, so it leaves the ignore engine in
// force; protected paths (.git/.awa) are a separate hard boundary applied before
// this and are never overridden.
func (f includeFilter) overridesExcludes() bool {
	return !f.all
}

// allows reports whether rel is in the configured scope. A directory is allowed
// if it lies within an include root or is an ancestor of one (so the walk can
// reach deeper includes); a file is allowed only if it lies within an include
// root.
func (f includeFilter) allows(rel string, isDir bool) bool {
	if f.all {
		return true
	}
	for _, inc := range f.includes {
		if rel == inc || strings.HasPrefix(rel, inc+"/") {
			return true
		}
		if isDir && strings.HasPrefix(inc, rel+"/") {
			return true
		}
	}
	return false
}
