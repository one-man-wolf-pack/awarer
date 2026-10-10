package worktreefs

import (
	"os"
	"path"
	"path/filepath"
	"strings"

	gi "github.com/git-pkgs/gitignore"

	"awarer/internal/domain/pattern"
)

// ignoreEngine evaluates paths against the layered ignore rules. Built-in
// excludes, per-directory .gitignore, and per-directory .awaignore are merged
// into one ordered rule list per directory — built-in first, then all
// .gitignore files shallow-to-deep, then all .awaignore files — so the highest
// layer and deepest file win, and a higher-layer negation can re-include a path
// a lower layer excluded. Every rule keeps its declaring directory as a literal
// base, so a single matcher evaluates the whole chain.
//
// gitByDir and awaByDir hold each directory's own matching rules, so they name
// the effective rule boundaries; cache routes a directory to the compiled matcher
// that decides for it, which for a directory adding no rules of its own is
// literally its nearest ancestor boundary's instance. A compiled matcher is
// immutable, so many directories can share one, and compiled state therefore
// scales with rule boundaries rather than with directory count.
type ignoreEngine struct {
	useGitignore bool
	useAwaignore bool
	warn         func(pattern.Diagnostic)

	builtin  []compilerRule
	gitByDir map[string][]compilerRule
	awaByDir map[string][]compilerRule
	cache    map[string]*gi.Matcher
}

func newIgnoreEngine(excludes []pattern.Rule, useGitignore, useAwaignore bool, warn func(pattern.Diagnostic)) (*ignoreEngine, error) {
	e := &ignoreEngine{
		useGitignore: useGitignore,
		useAwaignore: useAwaignore,
		warn:         warn,
		gitByDir:     map[string][]compilerRule{},
		awaByDir:     map[string][]compilerRule{},
		cache:        map[string]*gi.Matcher{},
	}
	builtin, err := e.vet(excludes)
	if err != nil {
		return nil, err
	}
	e.builtin = builtin
	return e, nil
}

func (e *ignoreEngine) vet(rules []pattern.Rule) ([]compilerRule, error) {
	vetted, diags, err := vetRules(rules)
	if err != nil {
		return nil, err
	}
	if e.warn != nil {
		for _, d := range diags {
			e.warn(d)
		}
	}
	return vetted, nil
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
		rules, err := e.loadIgnoreFile(absDir, dirRel, ".gitignore")
		if err != nil {
			return err
		}
		if len(rules) > 0 {
			e.gitByDir[dirRel] = rules
		}
	}
	if e.useAwaignore {
		rules, err := e.loadIgnoreFile(absDir, dirRel, ".awaignore")
		if err != nil {
			return err
		}
		if len(rules) > 0 {
			e.awaByDir[dirRel] = rules
		}
	}
	return nil
}

// ignores reports whether rel (a slash path relative to root) is excluded by the
// ignore rules in force for its directory. isDir must report whether rel names a
// directory: a directory-only pattern (e.g. "build/") prunes the directory entry
// itself and never matches a like-named file.
func (e *ignoreEngine) ignores(rel string, isDir bool) (bool, error) {
	m, err := e.combinedFor(parentDir(rel))
	if err != nil {
		return false, err
	}
	return m.MatchPath(rel, isDir), nil
}

// combinedFor returns the combined matcher deciding for files in directory dirRel.
// A directory that contributes no patterns of its own has exactly the effective
// rules of its parent, so it shares its parent's compiled instance rather than
// compiling an identical one; only a directory that is an effective rule boundary
// — and the root, which carries the built-in layer — compiles its own. The cache
// then maps every directory to a matcher, but distinct matchers exist only per
// boundary.
func (e *ignoreEngine) combinedFor(dirRel string) (*gi.Matcher, error) {
	if c, ok := e.cache[dirRel]; ok {
		return c, nil
	}
	var c *gi.Matcher
	var err error
	if dirRel == "" || e.contributes(dirRel) {
		c, err = e.compileFor(dirRel)
	} else {
		c, err = e.combinedFor(parentDir(dirRel))
	}
	if err != nil {
		return nil, err
	}
	e.cache[dirRel] = c
	return c, nil
}

// contributes reports whether dirRel is an effective rule boundary: its own ignore
// files supplied at least one matching rule. Files holding only blank, comment, or
// never-matching lines leave no contribution behind (loadDirAt stores nothing for
// them), so they are not boundaries.
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
func (e *ignoreEngine) compileFor(dirRel string) (*gi.Matcher, error) {
	chain := ancestorChain(dirRel)

	rules := append([]compilerRule(nil), e.builtin...)
	for _, d := range chain {
		rules = append(rules, e.gitByDir[d]...)
	}
	for _, d := range chain {
		rules = append(rules, e.awaByDir[d]...)
	}

	return aggregate(rules)
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

// loadIgnoreFile reads one ignore file and returns its matching rules, declared at
// the virtual directory dirRel ("" for root). A file carrying none leaves no
// contribution at all.
func (e *ignoreEngine) loadIgnoreFile(absDir, dirRel, name string) ([]compilerRule, error) {
	// absDir is an OS filesystem path, so join with filepath; the slash-separated
	// path helpers elsewhere are only for project-relative keys.
	data, err := os.ReadFile(filepath.Join(absDir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return e.vet(fileRules(path.Join(dirRel, name), dirRel, string(data)))
}

func fileRules(file, base, data string) []pattern.Rule {
	lines := strings.Split(data, "\n")
	rules := make([]pattern.Rule, len(lines))
	for i, line := range lines {
		rules[i] = pattern.Rule{Text: strings.TrimSuffix(line, "\r"), Base: base, Origin: pattern.FileLine(file, i+1)}
	}
	return rules
}
