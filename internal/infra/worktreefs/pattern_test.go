package worktreefs

import (
	"path"
	"slices"
	"strings"
	"testing"

	gi "github.com/git-pkgs/gitignore"

	"awarer/internal/domain/config"
	"awarer/internal/domain/pattern"
	"awarer/internal/domain/worktree"
)

type ignoreSource struct {
	dir   string
	lines string
}

func rootSource(lines string) []ignoreSource { return []ignoreSource{{lines: lines}} }

func engineOver(t *testing.T, excludes []string, sources []ignoreSource, warn func(pattern.Diagnostic)) *ignoreEngine {
	t.Helper()
	e, err := newIgnoreEngine(pattern.KeyRules("scope.extra_excludes", excludes), false, true, warn)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sources {
		rules, err := e.vet(fileRules(path.Join(s.dir, ".awaignore"), s.dir, s.lines))
		if err != nil {
			t.Fatal(err)
		}
		if len(rules) > 0 {
			e.awaByDir[s.dir] = rules
		}
	}
	return e
}

// The expectations are Git's own answers for the same rules and paths
// (git check-ignore --no-index over untracked files, case folding off), recorded
// when the matching foundation was selected.
func TestScanMatchesGit(t *testing.T) {
	cases := []struct {
		name    string
		sources []ignoreSource
		path    string
		isDir   bool
		want    bool
	}{
		{"question", rootSource("a?b"), "axb", false, true},
		{"question-byte-width", rootSource("a?b"), "aéb", false, false},
		{"internal-slash-root", rootSource("foo/bar"), "foo/bar", false, true},
		{"internal-slash-nested", rootSource("foo/bar"), "deep/foo/bar", false, false},
		{"leading-slash-root", rootSource("/cache"), "cache", false, true},
		{"leading-slash-deep", rootSource("/cache"), "deep/cache/child", false, false},
		{"bare-anydepth", rootSource("cache"), "deep/cache/child", false, true},
		{"negated-class", rootSource("[!a]"), "b", false, true},
		{"negated-class-self", rootSource("[!a]"), "a", false, false},
		{"caret-class", rootSource("[^a]"), "b", false, true},
		{"posix-digit", rootSource("[[:digit:]]"), "3", false, true},
		{"range-class", rootSource("x[a-c]"), "xb", false, true},
		{"bracket-first-close", rootSource("[]]"), "]", false, true},
		{"bracket-escaped-close", rootSource(`[\]]`), "]", false, true},
		{"escaped-question", rootSource(`a\?b`), "a?b", false, true},
		{"escaped-question-not-wild", rootSource(`a\?b`), "axb", false, false},
		{"escaped-star", rootSource(`a\*b`), "a*b", false, true},
		{"escaped-star-not-wild", rootSource(`a\*b`), "axb", false, false},
		{"escaped-hash", rootSource(`\#file`), "#file", false, true},
		{"escaped-bang", rootSource(`\!file`), "!file", false, true},
		{"comment", rootSource("#file"), "#file", false, false},
		{"escaped-space", rootSource(`file\ `), "file ", false, true},
		{"trailing-space-trimmed", rootSource("file  "), "file", false, true},
		{"escaped-backslash-then-space", rootSource(`a\\ `), `a\`, false, true},
		{"escaped-backslash-then-space-literal", rootSource(`a\\ `), `a\ `, false, false},
		{"leading-space", rootSource(" file"), " file", false, true},
		{"plus-literal", rootSource("a+b"), "a+b", false, true},
		{"paren-literal", rootSource("a(b"), "a(b", false, true},
		{"dot-literal", rootSource("a.b"), "axb", false, false},
		{"brace-literal", rootSource("a{b,c}"), "ab", false, false},
		{"escaped-backslash", rootSource(`a\\`), `a\`, false, true},
		{"dir-only-file", rootSource("cache/"), "cache", false, false},
		{"dir-only-dir", rootSource("cache/"), "cache", true, true},
		{"dir-only-descendant", rootSource("cache/"), "deep/cache/x", false, true},
		{"double-star-leading", rootSource("**/foo"), "a/b/foo", false, true},
		{"double-star-zero", rootSource("a/**/b"), "a/b", false, true},
		{"double-star-repeat", rootSource("a/**/b/c"), "a/b/b/c", false, true},
		{"double-star-trailing", rootSource("a/**"), "a/x/y", false, true},
		{"double-star-trailing-self", rootSource("a/**"), "a", true, false},
		{"ordinary-double-star", rootSource("a**b"), "axxb", false, true},
		{"ordinary-double-star-no-slash", rootSource("a**b"), "a/b", false, false},
		{"star-no-slash", rootSource("a*b"), "a/b", false, false},
		{"ordered-leaf-exception", rootSource("*.env\n!public.env"), "public.env", false, false},
		{"ordered-overwrite-reselect", rootSource("*.env\n!public.env\npublic.env"), "public.env", false, true},
		{"pruned-parent-child-exception", rootSource("secrets/\n!secrets/public.txt"), "secrets/public.txt", false, true},
		{"negated-dir-reopens", rootSource("build/\n!build/"), "build/x", false, false},
		{"case-sensitive", rootSource("*.TXT"), "a.txt", false, false},
		{"terminal-escape", rootSource(`file\`), "file", false, false},
		{"terminal-escape-literal-backslash", rootSource(`file\`), `file\`, false, false},
		{"literal-backslash-control", rootSource(`file\\`), `file\`, false, true},
		{"nested-direct", []ignoreSource{{"virtual", "foo/bar"}}, "virtual/foo/bar", false, true},
		{"nested-deeper", []ignoreSource{{"virtual", "foo/bar"}}, "virtual/deeper/foo/bar", false, false},
		{"nested-outside", []ignoreSource{{"virtual", "foo/bar"}}, "foo/bar", false, false},
		{"nested-bare-deeper", []ignoreSource{{"virtual", "cache"}}, "virtual/a/cache", false, true},
		{"nested-containing-dir", []ignoreSource{{"virtual", "virtual"}}, "virtual", true, false},
		{"nested-leading-slash", []ignoreSource{{"virtual", "/x"}}, "virtual/a/x", false, false},
		{"nested-escaped-literal", []ignoreSource{{"v", `a\?b`}}, "v/axb", false, false},
		{"nested-base-glob-chars", []ignoreSource{{"a[1]", "x"}}, "a[1]/x", false, true},
		{"nested-base-glob-chars-other", []ignoreSource{{"a[1]", "x"}}, "a1/x", false, false},
		{"nested-leading-space", []ignoreSource{{"v", " f"}}, "v/ f", false, true},
		{"nested-escaped-bang", []ignoreSource{{"v", `\!f`}}, "v/!f", false, true},
		{"nested-deeper-overrides", []ignoreSource{{"", "*.log"}, {"v", "!keep.log"}}, "v/keep.log", false, false},
		{"nested-cannot-reopen-pruned", []ignoreSource{{"", "v/"}, {"v", "!keep.log"}}, "v/keep.log", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := engineOver(t, nil, c.sources, nil)
			if got := e.mustIgnore(c.path, c.isDir); got != c.want {
				t.Errorf("ignores(%q, dir=%v) = %v, want %v", c.path, c.isDir, got, c.want)
			}
		})
	}
}

func TestNeverMatchingRulesAreLocatedAndInert(t *testing.T) {
	cases := []struct {
		pattern string
		reason  string
		probes  []string
	}{
		{"[abc", "unclosed bracket expression", []string{"a", "[abc", "b"}},
		{"x[", "unclosed bracket expression", []string{"x[", "x"}},
		{"[[:nope:]]", "unknown POSIX class [:nope:]", []string{"a", "[[:nope:]]", ":"}},
		{`file\`, danglingEscape, []string{"file", `file\`}},
		{`a\\\`, danglingEscape, []string{`a\`, `a\\`}},
		{"!", "empty pattern", []string{"x", "!"}},
		{"/", "empty pattern", []string{"x"}},
		{"![abc", "unclosed bracket expression", []string{"a", "[abc"}},
	}
	for _, c := range cases {
		t.Run(c.pattern, func(t *testing.T) {
			var diags []pattern.Diagnostic
			e := engineOver(t, nil, rootSource("keep\n"+c.pattern+"\n*.log"), func(d pattern.Diagnostic) { diags = append(diags, d) })
			want := []pattern.Diagnostic{{Origin: pattern.FileLine(".awaignore", 2), Text: c.pattern, Reason: c.reason}}
			if !slices.Equal(diags, want) {
				t.Fatalf("diagnostics = %+v, want %+v", diags, want)
			}
			for _, p := range c.probes {
				if e.mustIgnore(p, false) {
					t.Errorf("never-matching %q matched %q", c.pattern, p)
				}
			}
			if !e.mustIgnore("keep", false) || !e.mustIgnore("a.log", false) {
				t.Error("a never-matching rule disabled its valid neighbours")
			}
		})
	}
}

func TestIgnoreFileFramingPrecedesInterpretation(t *testing.T) {
	var diags []pattern.Diagnostic
	file := "# note\\\r\n" + ".env\\\r\n" + "*.log\r\n" + "cr\r\r\n" + "   \n" + "last"
	e := engineOver(t, nil, rootSource(file), func(d pattern.Diagnostic) { diags = append(diags, d) })

	want := []pattern.Diagnostic{{Origin: pattern.FileLine(".awaignore", 2), Text: `.env\`, Reason: danglingEscape}}
	if !slices.Equal(diags, want) {
		t.Fatalf("diagnostics = %+v, want only the terminal escape exposed by CRLF framing: %+v", diags, want)
	}
	for path, wantIgnored := range map[string]bool{
		"a.log": true, "a.log\r": false, "cr\r": true, "cr": false, "last": true, ".env": false, `.env\`: false,
	} {
		if got := e.mustIgnore(path, false); got != wantIgnored {
			t.Errorf("ignores(%q) = %v, want %v", path, got, wantIgnored)
		}
	}
}

// Without awa's lexical adapter the library selects a literal-backslash name for a
// terminal escape, keeps an unescaped space after a backslash pair, and drops a
// literal CR. Each is a wrong selection, so the adapter is not removable while this
// holds.
func TestLibraryStillNeedsTheLexicalAdapter(t *testing.T) {
	raw := func(text string) *gi.Matcher {
		m := gi.New("")
		m.AddPatterns([]byte(text), "")
		return m
	}
	if m := raw(`file\`); len(m.Errors()) != 0 || !m.MatchPath(`file\`, false) {
		t.Error("the library now rejects a terminal escape: gitTrim's dangling check may be removable")
	}
	if !raw(`a\\ `).MatchPath(`a\ `, false) {
		t.Error("the library now trims by escape pairs: gitTrim's space rule may be removable")
	}
	if m := raw("a\r"); !m.MatchPath("a", false) || m.MatchPath("a\r", false) {
		t.Error("the library now keeps a terminal CR: the CR adaptation may be removable")
	}
}

func TestConfiguredElementsKeepLiteralCR(t *testing.T) {
	cases := []struct {
		name, raw, selected, control string
	}{
		{"repeat-trailing", "a\r\r", "a", "a\r"},
		{"internal", "a\rb", "a\rb", "ab"},
		{"cr-before-space", "a\r ", "a\r", "a"},
		{"repeat-cr-before-spaces", "a\r\r  ", "a\r\r", "a\r"},
		{"escaped-cr-before-space", "a\\\r ", "a\r", "a"},
		{"paired-escape-before-cr", "a\\\\\r ", "a\\\r", "a\\"},
		{"escaped-space-after-cr", "a\r\\ ", "a\r ", "a\r"},
		{"internal-and-trailing", "a\rb\r\r", "a\rb", "ab"},
	}
	for _, key := range []string{"scope.extra_excludes", "history.extra_excludes", "run.extra_excludes"} {
		for _, c := range cases {
			t.Run(key+"/"+c.name, func(t *testing.T) {
				e, err := newIgnoreEngine(pattern.KeyRules(key, []string{c.raw}), false, false, func(d pattern.Diagnostic) {
					t.Errorf("unexpected diagnostic %v", d)
				})
				if err != nil {
					t.Fatal(err)
				}
				if !e.mustIgnore(c.selected, false) || e.mustIgnore(c.control, false) {
					t.Errorf("%q must exclude %q and not %q", c.raw, c.selected, c.control)
				}
			})
		}
	}
}

func TestConfiguredElementCarrierEdges(t *testing.T) {
	var diags []pattern.Diagnostic
	rules := pattern.KeyRules("scope.extra_excludes", []string{"#comment\\\r\r", " \r\r", "file\\\r\r", "ok"})
	e, err := newIgnoreEngine(rules, false, false, func(d pattern.Diagnostic) { diags = append(diags, d) })
	if err != nil {
		t.Fatal(err)
	}
	want := []pattern.Diagnostic{{Origin: pattern.KeyElement("scope.extra_excludes", 2), Text: "file\\\r\r", Reason: danglingEscape}}
	if !slices.Equal(diags, want) {
		t.Errorf("diagnostics = %+v, want %+v", diags, want)
	}
	if e.mustIgnore("file", false) || !e.mustIgnore("ok", false) {
		t.Error("inert and never-matching elements must not change their neighbours")
	}

	_, err = newIgnoreEngine(pattern.KeyRules("run.extra_excludes", []string{"ok", "a\nb"}), false, false, nil)
	if err == nil || !strings.Contains(err.Error(), "run.extra_excludes[1]") {
		t.Errorf("an element holding a line feed compiled: %v", err)
	}
}

func TestSourcePriorityAndOverridableExcludes(t *testing.T) {
	e := engineOver(t, []string{"*.log", "gen/"}, []ignoreSource{
		{"", "!keep.log\n!gen/\n*.tmp"},
		{"sub", "!x.tmp\ndeep.txt"},
	}, nil)
	for p, want := range map[string]bool{
		"a.log": true, "keep.log": false, "sub/keep.log": false,
		"a.tmp": true, "sub/x.tmp": false, "sub/y.tmp": true, "x.tmp": true,
		"sub/deep.txt": true, "deep.txt": false,
	} {
		if got := e.mustIgnore(p, false); got != want {
			t.Errorf("ignores(%q) = %v, want %v", p, got, want)
		}
	}
	if e.mustIgnore("gen", true) || e.mustIgnore("gen/out.bin", false) {
		t.Error("an ignore-file negation must re-include a configured directory exclude")
	}
}

func retentionOf(t *testing.T, patterns ...string) worktree.PathSelector {
	t.Helper()
	s, diags, err := CompilePatterns(pattern.KeyRules("checkpoint.hash_only_patterns", patterns))
	if err != nil || len(diags) != 0 {
		t.Fatalf("CompilePatterns(%q) = %v, %v", patterns, diags, err)
	}
	return s
}

func TestRetentionSelectsWithoutPruning(t *testing.T) {
	env := config.HashOnlyDefaults()
	cases := []struct {
		name     string
		patterns []string
		path     string
		want     bool
	}{
		{"dir-selects-descendant", []string{"secrets/"}, "secrets/a/b.txt", true},
		{"bare-dir-selects-descendant", []string{"secrets"}, "x/secrets/b.txt", true},
		{"child-exception-unselects", []string{"secrets/", "!secrets/public.txt"}, "secrets/public.txt", false},
		{"child-exception-sibling-kept", []string{"secrets/", "!secrets/public.txt"}, "secrets/private.txt", true},
		{"negative-dir-exception", []string{"secrets/", "!secrets/pub/"}, "secrets/pub/a.txt", false},
		{"negative-dir-exception-sibling", []string{"secrets/", "!secrets/pub/"}, "secrets/priv/a.txt", true},
		{"positive-dir-after-negative", []string{"secrets/", "!secrets/pub/", "secrets/pub/keys/"}, "secrets/pub/keys/k", true},
		{"leaf-after-negative-dir", []string{"!secrets/pub/", "secrets/pub/k"}, "secrets/pub/k", true},
		{"name-then-negative-dir", []string{"*.env", "!fixtures/"}, "fixtures/a.env", false},
		{"name-then-negative-dir-deep", []string{"*.env", "!fixtures/"}, "x/fixtures/y/a.env", false},
		{"ordered-overwrite", []string{"*.env", "!public.env", "public.env"}, "public.env", true},
		{"order-is-meaning", []string{"!a.txt", "*.txt"}, "a.txt", true},
		{"dir-only-not-file", []string{"secrets/"}, "secrets", false},
		{"escaped-literal", []string{`a\?b`}, "axb", false},
		{"question", []string{"a?c"}, "abc", true},
		{"anchored-not-deep", []string{"foo/*.env"}, "x/foo/a.env", false},
		{"anchored-root", []string{"foo/*.env"}, "foo/a.env", true},
		{"leading-slash", []string{"/top.env"}, "x/top.env", false},
		{"comment-and-blank", []string{"# comment", "  "}, "# comment", false},
		{"empty-list", nil, ".env", false},
		{"env-default", env, ".env", true},
		{"env-default-deep", env, "a/b/.env", true},
		{"env-default-dotted", env, "app/.env.local", true},
		{"env-default-suffix", env, "svc/prod.env", true},
		{"env-default-dir", env, ".env.d/inner", true},
		{"env-default-example", env, ".env.example", false},
		{"env-default-template", env, "pkg/.env.template", false},
		{"env-default-case", env, ".ENV", false},
		{"env-default-envrc", env, ".envrc", false},
		{"env-default-unrelated", env, "app/env.go", false},
		{"env-default-bak", env, "prod.env.bak", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := worktree.ParseRelPath(c.path)
			if err != nil {
				t.Fatal(err)
			}
			if got := retentionOf(t, c.patterns...).Selects(p); got != c.want {
				t.Errorf("%q selects %q = %v, want %v", c.patterns, c.path, got, c.want)
			}
		})
	}
}

func TestCompilePatternsReportsNeverMatchingElements(t *testing.T) {
	_, diags, err := CompilePatterns(pattern.KeyRules("checkpoint.hash_only_patterns", []string{"*.env", "[abc", `x\`, "!public.env"}))
	if err != nil {
		t.Fatal(err)
	}
	want := []pattern.Diagnostic{
		{Origin: pattern.KeyElement("checkpoint.hash_only_patterns", 1), Text: "[abc", Reason: "unclosed bracket expression"},
		{Origin: pattern.KeyElement("checkpoint.hash_only_patterns", 2), Text: `x\`, Reason: danglingEscape},
	}
	if !slices.Equal(diags, want) {
		t.Errorf("diagnostics = %+v, want %+v", diags, want)
	}
}

func TestUnexpectedCompilerErrorIsNotAWarning(t *testing.T) {
	for _, message := range []string{"unclosed bracket expression", "empty pattern", "unknown POSIX class [:x:]"} {
		if !neverMatching(message) {
			t.Errorf("%q must stay an accepted never-matching rule", message)
		}
	}
	for _, message := range []string{"", "pattern too long", "internal error"} {
		if neverMatching(message) {
			t.Errorf("%q must fail the operation, not become a warning", message)
		}
	}
}
