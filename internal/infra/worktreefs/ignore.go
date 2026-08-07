package worktreefs

import (
	"os"
	"path"
	"path/filepath"
	"strings"

	ignore "github.com/sabhiram/go-gitignore"
)

// layer identifies an ignore precedence band. Higher values win.
type layer int

const (
	layerBuiltin   layer = iota // configured/built-in excludes (lowest)
	layerGitignore              // per-directory .gitignore
	layerAwaignore              // per-directory .awaignore (overrides gitignore)
	layerCLI                    // command-line include/exclude (highest, future)
)

func (l layer) String() string {
	switch l {
	case layerBuiltin:
		return "builtin-exclude"
	case layerGitignore:
		return ".gitignore"
	case layerAwaignore:
		return ".awaignore"
	case layerCLI:
		return "cli"
	default:
		return "unknown"
	}
}

// Decision is the result of evaluating a path against the ignore rules. Rule
// explains which rule decided, for diagnostics ("why is this path
// included/excluded?").
type Decision struct {
	Ignored bool
	Rule    Rule
}

// Rule names the deciding ignore rule, or reports that no rule matched.
type Rule struct {
	Matched bool
	Layer   layer
	Source  string // ".gitignore" at a dir, or "config" for built-in excludes
	Pattern string // original pattern text
	LineNo  int    // 1-based line within Source, when known
	Negated bool   // whether the deciding pattern re-included the path
}

// patternMeta records where a compiled pattern came from, so a decision can be
// explained in terms of the original ignore file and line.
type patternMeta struct {
	layer   layer
	source  string
	pattern string
	lineNo  int
}

// rewritten is a directory's contribution to one layer: rewritten, root-relative
// pattern lines and their parallel metadata (1:1 with lines).
type rewritten struct {
	lines []string
	meta  []patternMeta
}

// compiled is a combined matcher for a directory plus the metadata needed to
// explain its decisions. meta is aligned 1:1 with the lines compiled, so a
// matched pattern's LineNo indexes it directly.
type compiled struct {
	matcher *ignore.GitIgnore
	meta    []patternMeta
}

// ignoreEngine evaluates paths against the layered ignore rules. Built-in
// excludes, per-directory .gitignore, and per-directory .awaignore are merged
// into one ordered pattern list per directory — built-in first, then all
// .gitignore files shallow-to-deep, then all .awaignore files — so the highest
// layer and deepest file win, and a higher-layer negation can re-include a path
// a lower layer excluded. Nested patterns are rewritten to root-relative form so
// a single matcher can evaluate them.
type ignoreEngine struct {
	useGitignore bool
	useAwaignore bool

	builtin  rewritten
	gitByDir map[string]rewritten
	awaByDir map[string]rewritten
	cache    map[string]*compiled
}

func newIgnoreEngine(builtinExcludes []string, useGitignore, useAwaignore bool) *ignoreEngine {
	e := &ignoreEngine{
		useGitignore: useGitignore,
		useAwaignore: useAwaignore,
		gitByDir:     map[string]rewritten{},
		awaByDir:     map[string]rewritten{},
		cache:        map[string]*compiled{},
	}
	// Built-in excludes are root-relative as written and need no rewriting.
	for i, line := range builtinExcludes {
		if !isPattern(line) {
			continue
		}
		e.builtin.lines = append(e.builtin.lines, line)
		e.builtin.meta = append(e.builtin.meta, patternMeta{
			layer: layerBuiltin, source: "config", pattern: line, lineNo: i + 1,
		})
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
func (e *ignoreEngine) loadDirAt(absDir, dirRel string) error {
	if e.useGitignore {
		r, err := loadIgnoreFile(absDir, dirRel, ".gitignore", layerGitignore)
		if err != nil {
			return err
		}
		if len(r.lines) > 0 {
			e.gitByDir[dirRel] = r
			e.invalidateCache()
		}
	}
	if e.useAwaignore {
		r, err := loadIgnoreFile(absDir, dirRel, ".awaignore", layerAwaignore)
		if err != nil {
			return err
		}
		if len(r.lines) > 0 {
			e.awaByDir[dirRel] = r
			e.invalidateCache()
		}
	}
	return nil
}

// invalidateCache drops cached combined matchers. Directories are loaded before
// their children are evaluated, and a directory's own file is loaded after the
// directory itself was decided, so the small cost of recompiling on demand keeps
// the logic simple and correct.
func (e *ignoreEngine) invalidateCache() {
	e.cache = map[string]*compiled{}
}

// decide evaluates rel (a slash path relative to root) for a file or directory.
// isDir must report whether rel names a directory, because a directory-only pattern
// (e.g. "build/") applies to the directory entry itself — not only its descendants —
// and the underlying matcher only recognizes that when the path is queried in its
// directory form (with a trailing slash). Querying directories with the slash makes a
// dir-only pattern prune the directory (and everything beneath it) regardless of
// whether the directory existed when a baseline was recorded, and keeps a dir-only
// pattern from ever matching a like-named file. A trailing slash never changes how a
// non-dir-only pattern matches, so this is safe for every pattern.
func (e *ignoreEngine) decide(rel string, isDir bool) Decision {
	dirRel := path.Dir(rel)
	if dirRel == "." {
		dirRel = ""
	}
	c := e.combinedFor(dirRel)
	query := rel
	if isDir {
		query = rel + "/"
	}
	matched, pat := c.matcher.MatchesPathHow(query)
	if pat == nil {
		return Decision{Ignored: false, Rule: Rule{Matched: false}}
	}
	m := patternMeta{}
	if idx := pat.LineNo - 1; idx >= 0 && idx < len(c.meta) {
		m = c.meta[idx]
	}
	return Decision{
		Ignored: matched,
		Rule: Rule{
			Matched: true, Layer: m.layer, Source: m.source,
			Pattern: m.pattern, LineNo: m.lineNo, Negated: pat.Negate,
		},
	}
}

// combinedFor returns the cached combined matcher for files in directory dirRel,
// building it from the built-in, .gitignore, and .awaignore contributions of
// dirRel and all its ancestors, in precedence order.
func (e *ignoreEngine) combinedFor(dirRel string) *compiled {
	if c, ok := e.cache[dirRel]; ok {
		return c
	}
	chain := ancestorChain(dirRel)

	lines := append([]string(nil), e.builtin.lines...)
	meta := append([]patternMeta(nil), e.builtin.meta...)
	for _, d := range chain {
		if r, ok := e.gitByDir[d]; ok {
			lines = append(lines, r.lines...)
			meta = append(meta, r.meta...)
		}
	}
	for _, d := range chain {
		if r, ok := e.awaByDir[d]; ok {
			lines = append(lines, r.lines...)
			meta = append(meta, r.meta...)
		}
	}

	c := &compiled{matcher: ignore.CompileIgnoreLines(lines...), meta: meta}
	e.cache[dirRel] = c
	return c
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

// loadIgnoreFile reads and rewrites one ignore file. dirRel is the directory
// holding the file ("" for root); patterns are rewritten to be relative to the
// project root so a single combined matcher can evaluate them.
func loadIgnoreFile(absDir, dirRel, name string, l layer) (rewritten, error) {
	// absDir is an OS filesystem path, so join with filepath; the slash-separated
	// path helpers elsewhere are only for project-relative keys.
	data, err := os.ReadFile(filepath.Join(absDir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return rewritten{}, nil
		}
		return rewritten{}, err
	}
	source := name
	if dirRel != "" {
		source = dirRel + "/" + name
	}
	var r rewritten
	for i, raw := range strings.Split(string(data), "\n") {
		if !isPattern(raw) {
			continue
		}
		line := rewritePattern(dirRel, raw)
		r.lines = append(r.lines, line)
		r.meta = append(r.meta, patternMeta{
			layer: l, source: source, pattern: strings.TrimRight(raw, "\r"), lineNo: i + 1,
		})
	}
	return r, nil
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
