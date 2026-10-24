package config

import (
	"strings"
	"testing"
)

func cargoTest() ValidationPolicy {
	return ValidationPolicy{Name: "cargo-test", Argv: []string{"cargo", "test"}, CWD: ".", UnobservedDirs: []string{"target"}}
}

func withValidation(ps ...ValidationPolicy) Config {
	c := Defaults()
	c.Run.Validation = ps
	return c
}

func TestValidationPolicyAcceptsExactLiteralDeclarations(t *testing.T) {
	for _, dirs := range [][]string{
		{"target"},
		{"crates/app/target"},
		{"ta*get", "[x]", "dir with space"},
		{"target", "build/gen"},
	} {
		p := cargoTest()
		p.UnobservedDirs = dirs
		if err := withValidation(p).Validate(); err != nil {
			t.Errorf("unobserved_dirs %q rejected: %v", dirs, err)
		}
	}
	p := cargoTest()
	p.CWD = "crates/app"
	if err := withValidation(p).Validate(); err != nil {
		t.Errorf("nested cwd rejected: %v", err)
	}
}

func TestValidationPolicyRejectsInvalidDeclarations(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ValidationPolicy)
		want   string
	}{
		{"empty name", func(p *ValidationPolicy) { p.Name = "" }, "run.validation[0].name must not be empty"},
		{"padded name", func(p *ValidationPolicy) { p.Name = " x" }, "run.validation[0].name"},
		{"empty argv", func(p *ValidationPolicy) { p.Argv = nil }, "run.validation[0].argv"},
		{"empty executable", func(p *ValidationPolicy) { p.Argv = []string{"", "test"} }, "run.validation[0].argv"},
		{"empty cwd", func(p *ValidationPolicy) { p.CWD = "" }, "run.validation[0].cwd"},
		{"escaping cwd", func(p *ValidationPolicy) { p.CWD = "../x" }, "run.validation[0].cwd"},
		{"no dirs", func(p *ValidationPolicy) { p.UnobservedDirs = nil }, "unobserved_dirs must list"},
		{"root", func(p *ValidationPolicy) { p.UnobservedDirs = []string{"."} }, "unobserved_dirs[0]"},
		{"absolute", func(p *ValidationPolicy) { p.UnobservedDirs = []string{"/target"} }, "unobserved_dirs[0]"},
		{"escaping", func(p *ValidationPolicy) { p.UnobservedDirs = []string{"../target"} }, "unobserved_dirs[0]"},
		{"unclean", func(p *ValidationPolicy) { p.UnobservedDirs = []string{"a/./b"} }, "unobserved_dirs[0]"},
		{"trailing slash", func(p *ValidationPolicy) { p.UnobservedDirs = []string{"target/"} }, "unobserved_dirs[0]"},
		{"backslash", func(p *ValidationPolicy) { p.UnobservedDirs = []string{`a\b`} }, "unobserved_dirs[0]"},
		{"padded", func(p *ValidationPolicy) { p.UnobservedDirs = []string{"target "} }, "unobserved_dirs[0]"},
		{"protected git", func(p *ValidationPolicy) { p.UnobservedDirs = []string{".git"} }, "protected"},
		{"protected nested awa", func(p *ValidationPolicy) { p.UnobservedDirs = []string{"x/.awa/y"} }, "protected"},
		{"nested pair", func(p *ValidationPolicy) { p.UnobservedDirs = []string{"target", "target/debug"} }, "overlaps"},
		{"duplicate dir", func(p *ValidationPolicy) { p.UnobservedDirs = []string{"target", "target"} }, "overlaps"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := cargoTest()
			tc.mutate(&p)
			err := withValidation(p).Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want a problem containing %q", err, tc.want)
			}
		})
	}
}

func TestValidationPoliciesSelectingOneInvocationConflict(t *testing.T) {
	other := cargoTest()
	other.Name = "other"
	other.UnobservedDirs = []string{"build"}
	err := withValidation(cargoTest(), other).Validate()
	if err == nil || !strings.Contains(err.Error(), "run.validation[1] conflicts with run.validation[0]") {
		t.Fatalf("Validate() = %v, want a selector conflict", err)
	}

	sameName := cargoTest()
	sameName.Argv = []string{"cargo", "check"}
	if err := withValidation(cargoTest(), sameName).Validate(); err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("Validate() = %v, want a duplicate-name problem", err)
	}

	joined := cargoTest()
	joined.Name = "joined"
	joined.Argv = []string{"cargo test"}
	if err := withValidation(cargoTest(), joined).Validate(); err != nil {
		t.Fatalf("distinct argv vectors conflict: %v", err)
	}
}

func TestSelectValidationMatchesOnlyTheExactInvocation(t *testing.T) {
	run := withValidation(cargoTest()).Run
	if p, ok := run.SelectValidation([]string{"cargo", "test"}, "."); !ok || p.Name != "cargo-test" {
		t.Fatalf("exact invocation selected %v, %v", p, ok)
	}
	for _, miss := range []struct {
		argv []string
		cwd  string
	}{
		{[]string{"cargo", "test", "--release"}, "."},
		{[]string{"cargo"}, "."},
		{[]string{"cargo test"}, "."},
		{[]string{"/usr/bin/cargo", "test"}, "."},
		{[]string{"cargo", "Test"}, "."},
		{[]string{"cargo", "test"}, "crates/app"},
	} {
		if p, ok := run.SelectValidation(miss.argv, miss.cwd); ok {
			t.Errorf("SelectValidation(%q, %q) borrowed policy %q", miss.argv, miss.cwd, p.Name)
		}
	}
}

func TestPathWithinComparesComponents(t *testing.T) {
	for _, tc := range []struct {
		p, dir string
		want   bool
	}{
		{"target", "target", true},
		{"target/debug", "target", true},
		{"target2", "target", false},
		{".", "target", false},
		{"artifacts", "artifacts/cache", false},
		{"artifacts/cache/input", "artifacts/cache", true},
	} {
		if got := PathWithin(tc.p, tc.dir); got != tc.want {
			t.Errorf("PathWithin(%q, %q) = %v, want %v", tc.p, tc.dir, got, tc.want)
		}
	}
}
