package worktreefs

import (
	"testing"

	"awarer/internal/domain/config"
	"awarer/internal/domain/worktree"
)

// TestCompilePatternsUsesTheIgnoreLanguage pins the selection semantics at the matcher
// owner: the default env list and a directory pattern with an ordered exception.
func TestCompilePatternsUsesTheIgnoreLanguage(t *testing.T) {
	cases := []struct {
		patterns []string
		path     string
		want     bool
	}{
		{config.HashOnlyDefaults(), ".env", true},
		{config.HashOnlyDefaults(), "a/b/.env", true},
		{config.HashOnlyDefaults(), ".env.production", true},
		{config.HashOnlyDefaults(), "svc/prod.env", true},
		{config.HashOnlyDefaults(), ".env.d/inner", true},
		{config.HashOnlyDefaults(), ".env.example", false},
		{config.HashOnlyDefaults(), "pkg/.env.template", false},
		{config.HashOnlyDefaults(), ".ENV", false},
		{config.HashOnlyDefaults(), ".envrc", false},
		{config.HashOnlyDefaults(), "env", false},
		{config.HashOnlyDefaults(), "prod.env.bak", false},
		{[]string{"secrets/", "!secrets/public.txt"}, "secrets/a", true},
		{[]string{"secrets/", "!secrets/public.txt"}, "x/secrets/deep/b", true},
		{[]string{"secrets/", "!secrets/public.txt"}, "secrets/public.txt", false},
		{[]string{"secrets/", "!secrets/public.txt"}, "secrets.txt", false},
		{[]string{"!a.txt", "*.txt"}, "a.txt", true}, // order is meaning
		{[]string{"/top.env"}, "x/top.env", false},
		// Documented limitations of the shared ignore matcher, pinned so the shipped
		// pattern guidance stays true: anchoring depends on pattern syntax, "?" is a
		// literal, and a malformed pattern is dropped without an error.
		{[]string{"foo/*.env"}, "x/foo/a.env", false},
		{[]string{"a?c"}, "abc", false},
		{[]string{"["}, "[", false},
		{[]string{}, ".env", false},
		{[]string{"# comment", "  "}, "# comment", false},
	}
	for _, c := range cases {
		p, err := worktree.ParseRelPath(c.path)
		if err != nil {
			t.Fatal(err)
		}
		got := CompilePatterns(c.patterns).Selects(p)
		if got != c.want {
			t.Errorf("%v selects %q = %v, want %v", c.patterns, c.path, got, c.want)
		}
	}
}
