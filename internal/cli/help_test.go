package cli

import (
	"strings"
	"testing"

	"awarer/internal/app/help"
)

// These tests cover help ROUTING and process behavior: which page a spelling reaches,
// what the listing projects from the catalog, and how the command fails. What a page
// says is the corpus's business, not this file's.
//
// The help command must work with no project on disk. Every test here uses the
// bare run(...) helper (no initProject), which exercises help from a directory
// that has no .awa/ — proving help never discovers a root, loads config, or
// takes a lock.

func TestHelpIndexRunsOutsideProject(t *testing.T) {
	code, stdout, stderr := run("help")
	if code != int(ExitSuccess) {
		t.Fatalf("awa help exit = %d, want %d; stderr=%q", code, ExitSuccess, stderr)
	}
	if stderr != "" {
		t.Errorf("awa help stderr = %q, want empty", stderr)
	}
	// Anchor on each topic's Summary, not its short slug: slugs like "run" and
	// "diff" leak into the surrounding prose/examples, so a slug substring would
	// pass even if the topic row were dropped. Summaries appear only in the row.
	for _, topic := range help.Topics() {
		if !strings.Contains(stdout, topic.Summary) {
			t.Errorf("awa help index missing topic %q (summary %q)\n%s", topic.Slug, topic.Summary, stdout)
		}
	}
}

func TestHelpTopicsListsCanonicalAndAliases(t *testing.T) {
	code, stdout, stderr := run("help", "topics")
	if code != int(ExitSuccess) {
		t.Fatalf("awa help topics exit = %d, want %d; stderr=%q", code, ExitSuccess, stderr)
	}
	// Anchor on Summary, not the short slug, for the same leak reason as the
	// index test.
	for _, topic := range help.Topics() {
		if !strings.Contains(stdout, topic.Summary) {
			t.Errorf("awa help topics missing canonical topic %q (summary %q)\n%s", topic.Slug, topic.Summary, stdout)
		}
	}
	// Each alias must appear in an "aliases:" group, not merely somewhere in the
	// output (short aliases like "cache" leak into summaries otherwise).
	for alias := range help.Aliases() {
		if !containsAliasInGroup(stdout, alias) {
			t.Errorf("awa help topics missing alias %q in an aliases group\n%s", alias, stdout)
		}
	}
}

// containsAliasInGroup reports whether alias appears on a line that also renders
// the "aliases:" label — i.e. it is listed as an alias, not just present as an
// incidental substring of some summary.
func containsAliasInGroup(out, alias string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "aliases:") && strings.Contains(line, alias) {
			return true
		}
	}
	return false
}

func TestHelpAliasesRouteToCanonicalPage(t *testing.T) {
	// Iterate the production alias table so every current and future alias is
	// covered end-to-end; the authoritative alias set is contract-tested in the
	// help package (TestAliasesResolveToCanonicalTopic).
	for alias, canonical := range help.Aliases() {
		code, aliasOut, _ := run("help", alias)
		if code != int(ExitSuccess) {
			t.Errorf("awa help %s exit = %d, want %d", alias, code, ExitSuccess)
			continue
		}
		// An alias must render byte-for-byte the same page as its canonical name.
		_, canonicalOut, _ := run("help", canonical)
		if aliasOut != canonicalOut {
			t.Errorf("awa help %s did not render the same page as awa help %s\n--- alias ---\n%s\n--- canonical ---\n%s", alias, canonical, aliasOut, canonicalOut)
		}
	}
}

func TestHelpUnknownTopicIsUsageError(t *testing.T) {
	code, stdout, stderr := run("help", "definitely-not-a-topic")
	if code != int(ExitUsageError) {
		t.Fatalf("awa help <unknown> exit = %d, want %d", code, ExitUsageError)
	}
	if stdout != "" {
		t.Errorf("awa help <unknown> stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "run 'awa help topics'") {
		t.Errorf("awa help <unknown> stderr must suggest 'awa help topics'\n%s", stderr)
	}
}

func TestHelpRejectsExtraArguments(t *testing.T) {
	code, _, stderr := run("help", "agents", "extra")
	if code != int(ExitUsageError) {
		t.Fatalf("awa help agents extra exit = %d, want %d", code, ExitUsageError)
	}
	if !strings.Contains(stderr, "unexpected argument") {
		t.Errorf("stderr = %q, want it to reject the extra argument", stderr)
	}
}

// help has no flags; a leading-dash token must report as an unknown flag (like
// every other command), not as an unknown topic.
func TestHelpRejectsFlagTokenAsUnknownFlag(t *testing.T) {
	code, stdout, stderr := run("help", "--nope")
	if code != int(ExitUsageError) {
		t.Fatalf("awa help --nope exit = %d, want %d", code, ExitUsageError)
	}
	if stdout != "" {
		t.Errorf("awa help --nope stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, `unknown flag "--nope" for help`) {
		t.Errorf("stderr = %q, want it to report an unknown flag", stderr)
	}
}
