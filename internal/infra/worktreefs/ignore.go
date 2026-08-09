package worktreefs

import (
	"os"
	"path"
	"path/filepath"
	"strings"

	ignore "github.com/sabhiram/go-gitignore"
)

// ignoreEngine evaluates paths against the layered ignore rules. Built-in
// excludes, per-directory .gitignore, and per-directory .awaignore are merged
// into one ordered pattern list per directory — built-in first, then all
// .gitignore files shallow-to-deep, then all .awaignore files — so the highest
// layer and deepest file win, and a higher-layer negation can re-include a path
// a lower layer excluded. Nested patterns are rewritten to root-relative form so
// a single matcher can evaluate them.
//
// gitByDir and awaByDir hold each directory's own rewritten patterns, so they name
// the effective rule boundaries; cache routes a directory to the compiled matcher
// that decides for it, which for a directory adding no patterns of its own is
// literally its nearest ancestor boundary's instance. A compiled matcher is
// immutable, so many directories can share one, and the expensive regexp state
// therefore scales with rule boundaries rather than with directory count.
type ignoreEngine struct {
	useGitignore bool
	useAwaignore bool

	builtin  []string
	gitByDir map[string][]string
	awaByDir map[string][]string
	cache    map[string]*ignore.GitIgnore
}

func newIgnoreEngine(builtinExcludes []string, useGitignore, useAwaignore bool) *ignoreEngine {
	e := &ignoreEngine{
		useGitignore: useGitignore,
		useAwaignore: useAwaignore,
		gitByDir:     map[string][]string{},
		awaByDir:     map[string][]string{},
		cache:        map[string]*ignore.GitIgnore{},
	}
	// Built-in excludes are root-relative as written and need no rewriting.
	for _, line := range builtinExcludes {
		if isPattern(line) {
			e.builtin = append(e.builtin, line)
		}
	}
	return e
}

// loadDirAt reads the ignore files in the directory at absDir, attributing them
// to the virtual project-relative path dirRel ("" for root). absDir and dirRel
// differ when the directory is reached by following a symlink: the files live at
// the real path while their rules apply to the virtual path under the root. It is
// called once per in-scope directory as the walk enters it, before the
// directory's children are evaluated. A read error other than "not found" is
// returned so a permission failure is not silently treated as "no rules".
//
// A load never invalidates anything already cached. A directory's contribution
// only ever appears in the combined matcher of that virtual directory and its
// descendants, and no combined matcher is built before the directory it decides
// for has been loaded: the walk loads the root before deciding anything under it,
// and loads every other directory — ordinary, followed-symlink, or nested inside a
// followed subtree — before evaluating its children, while the directory entry
// itself is decided by its parent's matcher. So a rule found in one subtree can
// never invalidate an already compiled ancestor or sibling matcher, and compiled
// matchers stay immutable for the life of the walk.
func (e *ignoreEngine) loadDirAt(absDir, dirRel string) error {
	if e.useGitignore {
		lines, err := loadIgnoreFile(absDir, dirRel, ".gitignore")
		if err != nil {
			return err
		}
		if len(lines) > 0 {
			e.gitByDir[dirRel] = lines
		}
	}
	if e.useAwaignore {
		lines, err := loadIgnoreFile(absDir, dirRel, ".awaignore")
		if err != nil {
			return err
		}
		if len(lines) > 0 {
			e.awaByDir[dirRel] = lines
		}
	}
	return nil
}

// ignores reports whether rel (a slash path relative to root) is excluded by the
// ignore rules in force for its directory.
// isDir must report whether rel names a directory, because a directory-only pattern
// (e.g. "build/") applies to the directory entry itself — not only its descendants —
// and the underlying matcher only recognizes that when the path is queried in its
// directory form (with a trailing slash). Querying directories with the slash makes a
// dir-only pattern prune the directory (and everything beneath it) regardless of
// whether the directory existed when a baseline was recorded, and keeps a dir-only
// pattern from ever matching a like-named file. A trailing slash never changes how a
// non-dir-only pattern matches, so this is safe for every pattern.
func (e *ignoreEngine) ignores(rel string, isDir bool) bool {
	query := rel
	if isDir {
		query = rel + "/"
	}
	return e.combinedFor(parentDir(rel)).MatchesPath(query)
}

// combinedFor returns the combined matcher deciding for files in directory dirRel.
// A directory that contributes no patterns of its own has exactly the effective
// rules of its parent, so it shares its parent's compiled instance rather than
// compiling an identical one; only a directory that is an effective rule boundary
// — and the root, which carries the built-in layer — compiles its own. The cache
// then maps every directory to a matcher, but distinct matchers exist only per
// boundary.
func (e *ignoreEngine) combinedFor(dirRel string) *ignore.GitIgnore {
	if c, ok := e.cache[dirRel]; ok {
		return c
	}
	var c *ignore.GitIgnore
	if dirRel == "" || e.contributes(dirRel) {
		c = e.compileFor(dirRel)
	} else {
		c = e.combinedFor(parentDir(dirRel))
	}
	e.cache[dirRel] = c
	return c
}

// contributes reports whether dirRel is an effective rule boundary: its own ignore
// files supplied at least one pattern. Comment-only and empty files leave no
// contribution behind (loadDirAt stores nothing for them), so they are not
// boundaries.
func (e *ignoreEngine) contributes(dirRel string) bool {
	if _, ok := e.gitByDir[dirRel]; ok {
		return true
	}
	_, ok := e.awaByDir[dirRel]
	return ok
}

// parentDir returns the virtual parent of a slash-separated project-relative path:
// "a/b" yields "a" and a top-level "a" yields the project root "". It is the single
// spelling of that step, used both to find the directory deciding for a path and to
// walk a directory up to its nearest ancestor rule boundary.
//
// The root is its own parent — parentDir("") is "" — so it is a fixed point, not a
// terminator. Anything walking up with it must stop at the root on its own;
// combinedFor does that by always compiling the root rather than resolving past it.
func parentDir(rel string) string {
	if d := path.Dir(rel); d != "." {
		return d
	}
	return ""
}

// compileFor builds the combined matcher for a boundary directory from the
// built-in, .gitignore, and .awaignore contributions of dirRel and all its
// ancestors, in precedence order. The whole ordered chain is compiled afresh rather
// than appended to the parent's matcher: a local .gitignore must still precede an
// inherited .awaignore, which appending would invert.
func (e *ignoreEngine) compileFor(dirRel string) *ignore.GitIgnore {
	chain := ancestorChain(dirRel)

	lines := append([]string(nil), e.builtin...)
	for _, d := range chain {
		lines = append(lines, e.gitByDir[d]...)
	}
	for _, d := range chain {
		lines = append(lines, e.awaByDir[d]...)
	}

	return ignore.CompileIgnoreLines(lines...)
}

// ancestorChain returns dirRel and all its ancestors, shallowest first, ending
// with dirRel. For "a/b" it yields ["", "a", "a/b"].
func ancestorChain(dirRel string) []string {
	if dirRel == "" {
		return []string{""}
	}
	parts := strings.Split(dirRel, "/")
	chain := make([]string, 0, len(parts)+1)
	chain = append(chain, "")
	cur := ""
	for _, p := range parts {
		if cur == "" {
			cur = p
		} else {
			cur = cur + "/" + p
		}
		chain = append(chain, cur)
	}
	return chain
}

// loadIgnoreFile reads one ignore file and returns its pattern lines. dirRel is the
// directory holding the file ("" for root); patterns are rewritten to be relative to
// the project root so a single combined matcher can evaluate them. Blank and comment
// lines are dropped, so a file carrying none leaves no contribution at all.
func loadIgnoreFile(absDir, dirRel, name string) ([]string, error) {
	// absDir is an OS filesystem path, so join with filepath; the slash-separated
	// path helpers elsewhere are only for project-relative keys.
	data, err := os.ReadFile(filepath.Join(absDir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var lines []string
	for _, raw := range strings.Split(string(data), "\n") {
		if !isPattern(raw) {
			continue
		}
		lines = append(lines, rewritePattern(dirRel, raw))
	}
	return lines, nil
}

// isPattern reports whether a raw ignore line carries a pattern (not blank, not a
// comment).
func isPattern(raw string) bool {
	t := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
	return t != "" && !strings.HasPrefix(t, "#")
}

// rewritePattern rewrites a gitignore pattern from the directory dirRel to a
// root-relative pattern. Root patterns are returned unchanged. For nested
// directories, anchored patterns (a leading or internal slash) are prefixed with
// the directory; unanchored patterns (a bare glob) are made to match at any
// depth under the directory via "/**/". Negation and directory-only ("/")
// suffixes are preserved.
func rewritePattern(dirRel, raw string) string {
	line := strings.TrimRight(raw, "\r")
	if dirRel == "" {
		return line
	}
	body := line
	negate := strings.HasPrefix(body, "!")
	if negate {
		body = body[1:]
	}
	dirOnly := strings.HasSuffix(body, "/")
	core := strings.TrimSuffix(body, "/")
	anchored := strings.HasPrefix(core, "/") || strings.Contains(core, "/")
	core = strings.TrimPrefix(core, "/")

	var rebuilt string
	if anchored {
		rebuilt = dirRel + "/" + core
	} else {
		rebuilt = dirRel + "/**/" + core
	}
	if dirOnly {
		rebuilt += "/"
	}
	if negate {
		rebuilt = "!" + rebuilt
	}
	return rebuilt
}
