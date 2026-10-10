package worktreefs

import (
	"fmt"
	"strings"

	gi "github.com/git-pkgs/gitignore"

	"awarer/internal/domain/pattern"
	"awarer/internal/domain/worktree"
)

type compilerRule struct {
	text   string
	base   string
	negate bool
}

const danglingEscape = "trailing backslash"

// neverMatching names the library's compile errors for patterns Git accepts and
// never matches. Any other compile error is a failure of the operation, not an
// accepted rule.
func neverMatching(message string) bool {
	return message == "unclosed bracket expression" ||
		message == "empty pattern" ||
		strings.HasPrefix(message, "unknown POSIX class [")
}

func vetRule(r pattern.Rule) (rule compilerRule, diag *pattern.Diagnostic, ok bool, err error) {
	text := r.Text
	if !r.Origin.InFile() {
		text = strings.TrimRight(text, "\r")
	}
	if strings.Contains(text, "\n") {
		return compilerRule{}, nil, false, fmt.Errorf("%s: pattern %q contains a line feed", r.Origin, r.Text)
	}
	if text == "" || text[0] == '#' {
		return compilerRule{}, nil, false, nil
	}
	text, dangling := gitTrim(text)
	if dangling {
		return compilerRule{}, &pattern.Diagnostic{Origin: r.Origin, Text: r.Text, Reason: danglingEscape}, false, nil
	}
	if text == "" {
		return compilerRule{}, nil, false, nil
	}
	// The library (github.com/git-pkgs/gitignore v1.3.0) frames its input as file
	// lines and drops a final CR before trimming spaces, so a pattern ending in a
	// literal CR would match the name without it. One unescaped space after that CR
	// is removed by the library's own space trim and leaves the CR in the pattern.
	// Remove when AddPatterns accepts a single pattern without line framing.
	if strings.HasSuffix(text, "\r") {
		text += " "
	}
	probe := gi.New("")
	probe.AddPatterns([]byte(text), r.Base)
	if errs := probe.Errors(); len(errs) > 0 {
		if !neverMatching(errs[0].Message) {
			return compilerRule{}, nil, false, fmt.Errorf("%s: compiling pattern %q: %s", r.Origin, r.Text, errs[0].Message)
		}
		return compilerRule{}, &pattern.Diagnostic{Origin: r.Origin, Text: r.Text, Reason: errs[0].Message}, false, nil
	}
	return compilerRule{text: text, base: r.Base, negate: text[0] == '!'}, nil, true, nil
}

func vetRules(rules []pattern.Rule) ([]compilerRule, []pattern.Diagnostic, error) {
	var out []compilerRule
	var diags []pattern.Diagnostic
	for _, r := range rules {
		rule, diag, ok, err := vetRule(r)
		if err != nil {
			return nil, nil, err
		}
		if diag != nil {
			diags = append(diags, *diag)
		}
		if ok {
			out = append(out, rule)
		}
	}
	return out, diags, nil
}

// gitTrim removes trailing unescaped spaces the way Git does and reports a final
// unpaired backslash, which Git never matches.
//
// The library (github.com/git-pkgs/gitignore v1.3.0) gets both wrong: it keeps the
// space of `a\\ `, whose backslashes are a pair, and it compiles a terminal escape
// into a pattern matching a literal backslash name. Normalizing here keeps both out
// of its input. Remove when the library trims by escape pairs and rejects a
// terminal escape.
func gitTrim(text string) (trimmed string, dangling bool) {
	firstTrailingSpace := -1
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case ' ':
			if firstTrailingSpace < 0 {
				firstTrailingSpace = i
			}
		case '\\':
			i++
			if i == len(text) {
				return text, true
			}
			firstTrailingSpace = -1
		default:
			firstTrailingSpace = -1
		}
	}
	if firstTrailingSpace >= 0 {
		return text[:firstTrailingSpace], false
	}
	return text, false
}

func aggregate(rules []compilerRule) (*gi.Matcher, error) {
	m := gi.New("")
	for _, r := range rules {
		m.AddPatterns([]byte(r.text), r.base)
	}
	if errs := m.Errors(); len(errs) > 0 {
		return nil, fmt.Errorf("compiling pattern %q: %s", errs[0].Pattern, errs[0].Message)
	}
	return m, nil
}

func CheckPatterns(rules []pattern.Rule) ([]pattern.Diagnostic, error) {
	_, diags, err := vetRules(rules)
	return diags, err
}

func RootDiagnostics(root string, rules []pattern.Rule, useGitignore, useAwaignore bool) ([]pattern.Diagnostic, error) {
	var diags []pattern.Diagnostic
	e, err := newIgnoreEngine(rules, useGitignore, useAwaignore, func(d pattern.Diagnostic) { diags = append(diags, d) })
	if err != nil {
		return nil, err
	}
	if err := e.loadDirAt(root, ""); err != nil {
		return nil, fmt.Errorf("reading root ignore files: %w", err)
	}
	return diags, nil
}

type selectorRule struct {
	m      *gi.Matcher
	negate bool
}

type patternSelector struct {
	rules []selectorRule
}

func CompilePatterns(rules []pattern.Rule) (worktree.PathSelector, []pattern.Diagnostic, error) {
	vetted, diags, err := vetRules(rules)
	if err != nil {
		return nil, nil, err
	}
	s := patternSelector{rules: make([]selectorRule, 0, len(vetted))}
	for _, r := range vetted {
		m, err := aggregate([]compilerRule{r})
		if err != nil {
			return nil, nil, err
		}
		s.rules = append(s.rules, selectorRule{m: m, negate: r.negate})
	}
	return s, diags, nil
}

// Selects applies the last rule matching p or one of its directories.
//
// Each rule is its own matcher because the library's aggregate match treats an
// excluded directory as final for everything below it, which would keep a file
// selected after a later exception. A single positive rule already covers the
// descendants of a directory it matches; a single negated rule does not, so its
// ancestors are asked explicitly.
func (s patternSelector) Selects(p worktree.RelPath) bool {
	path := p.String()
	for i := len(s.rules) - 1; i >= 0; i-- {
		r := s.rules[i]
		if r.m.MatchDetail(path).Matched {
			return !r.negate
		}
		if !r.negate {
			continue
		}
		for j := 0; j < len(path); j++ {
			if path[j] == '/' && r.m.MatchDetail(path[:j+1]).Matched {
				return false
			}
		}
	}
	return false
}
