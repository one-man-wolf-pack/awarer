package gitmeta

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
)

// envValue returns the last assignment of name in env, which is the one a process
// takes: environment blocks are last-wins, and the pin relies on that.
func envValue(env []string, name string) (string, bool) {
	value, found := "", false
	for _, e := range env {
		if n, v, ok := strings.Cut(e, "="); ok && n == name {
			value, found = v, true
		}
	}
	return value, found
}

// TestGitInvocationsPinTheMessageLocale checks the environment a git invocation is
// actually given, not merely that a helper exists.
//
// awa classifies two ordinary, benign git outcomes — an unborn branch and "not a
// repository" — by matching git's English stderr. Under a translated locale those
// messages change, and a benign absence would be reported as a genuine git failure. So
// every invocation this provider makes must run with git's message locale pinned.
//
// Only awa's own git subprocesses are pinned. This is not the wrapped-command
// environment: a child of `awa run` inherits the caller's locale exactly.
func TestGitInvocationsPinTheMessageLocale(t *testing.T) {
	p := &Provider{root: t.TempDir()}
	cmd := p.gitCommand(context.Background(), "rev-parse", "--is-inside-work-tree")

	if cmd.Env == nil {
		t.Fatal("git ran with a nil environment: it would inherit the caller's locale and translate the messages awa classifies")
	}
	if got, ok := envValue(cmd.Env, "LC_ALL"); !ok || got != "C" {
		t.Errorf("LC_ALL = %q (present=%v), want C", got, ok)
	}
	// GNU gettext ignores LANGUAGE once the locale is C, so clearing it is belt and
	// braces — but it keeps the intent explicit if that precedence ever changes.
	if got, ok := envValue(cmd.Env, "LANGUAGE"); !ok || got != "" {
		t.Errorf("LANGUAGE = %q (present=%v), want present and empty", got, ok)
	}
}

// TestGitInvocationsPinBeatsTheCallersLocale proves the pin wins rather than merely
// being present: a caller running under a translated locale must not have that value
// survive into the invocation.
func TestGitInvocationsPinBeatsTheCallersLocale(t *testing.T) {
	t.Setenv("LC_ALL", "fr_FR.UTF-8")
	t.Setenv("LANG", "fr_FR.UTF-8")
	t.Setenv("LANGUAGE", "fr")

	p := &Provider{root: t.TempDir()}
	cmd := p.gitCommand(context.Background(), "status", "-z")

	if got, _ := envValue(cmd.Env, "LC_ALL"); got != "C" {
		t.Errorf("LC_ALL = %q, want the pin C to win over the caller's value", got)
	}
	if got, _ := envValue(cmd.Env, "LANGUAGE"); got != "" {
		t.Errorf("LANGUAGE = %q, want the pin (empty) to win over the caller's value", got)
	}
}

// TestGitInvocationsInheritEverythingElse pins the other half of the decision: only the
// message locale is overridden. git still needs the caller's real environment to find
// its configuration, credential helpers, and PATH, so this is a narrow correction, not
// a sanitized environment like the one `awa run` builds for a wrapped command.
func TestGitInvocationsInheritEverythingElse(t *testing.T) {
	t.Setenv("AWA_GITMETA_PROBE", "inherited")

	p := &Provider{root: t.TempDir()}
	cmd := p.gitCommand(context.Background(), "rev-parse", "HEAD")

	if got, ok := envValue(cmd.Env, "AWA_GITMETA_PROBE"); !ok || got != "inherited" {
		t.Errorf("AWA_GITMETA_PROBE = %q (present=%v), want it inherited", got, ok)
	}
	if _, ok := envValue(cmd.Env, "PATH"); !ok && len(os.Environ()) > 0 {
		t.Error("PATH was not inherited; git would not be found through the caller's PATH")
	}
}

// TestGitInvocationsCarryTheProjectRoot guards the property the pin sits next to: the
// invocation is rooted at the project, never at the caller's working directory.
func TestGitInvocationsCarryTheProjectRoot(t *testing.T) {
	root := t.TempDir()
	p := &Provider{root: root}
	cmd := p.gitCommand(context.Background(), "rev-parse", "HEAD")

	want := []string{"git", "-C", root, "rev-parse", "HEAD"}
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("args = %v, want %v", cmd.Args, want)
	}
}

// TestProseClassifiersDependOnTheEnglishMessages is the reason the pin exists, stated as
// a test. The classifiers match git's English wording, so a translated message is not
// recognized — which under an unpinned locale would turn "this project has no commits
// yet" into a reported git failure.
//
// This is the mechanistic proof: it does not need a localized git installed, because
// the defect is in the classifier's contract, not in any particular translation.
func TestProseClassifiersDependOnTheEnglishMessages(t *testing.T) {
	for _, tc := range []struct {
		name       string
		english    string
		translated string
		classify   func(string) bool
	}{
		{
			name:       "unborn branch",
			english:    "fatal: your current branch 'main' does not have any commits yet",
			translated: "fatal : la branche actuelle 'main' n'a encore aucun commit",
			classify:   isUnbornBranch,
		},
		{
			name:       "not a repository",
			english:    "fatal: not a git repository (or any of the parent directories): .git",
			translated: "fatal : ce n'est pas un dépôt git (ni aucun des répertoires parents) : .git",
			classify:   isNotARepo,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.classify(tc.english) {
				t.Fatalf("the English message is not recognized: %q", tc.english)
			}
			if tc.classify(tc.translated) {
				t.Fatalf("the translated message was recognized; this test no longer shows why the locale pin is required")
			}
		})
	}
}
