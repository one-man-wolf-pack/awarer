package run

import (
	"testing"

	"awarer/internal/domain/runcache"
)

func TestCommandMatches(t *testing.T) {
	cargo := []string{"cargo", "xtask", "check"}
	for _, tc := range []struct {
		name   string
		argv   []string
		filter string
		want   bool
	}{
		{"whole executable", cargo, "cargo", true},
		{"partial executable", cargo, "arg", true},
		{"whole argument", cargo, "xtask", true},
		{"inside an argument", cargo, "hec", true},
		{"spans adjacent arguments", cargo, "xtask check", true},
		{"spans executable and argument", cargo, "go xta", true},
		{"whole command line", cargo, "cargo xtask check", true},
		{"non-adjacent arguments do not join", cargo, "cargo check", false},
		{"case differs in executable", cargo, "Cargo", false},
		{"case differs in argument", cargo, "XTASK", false},
		{"absent text", cargo, "fmt", false},
		{"arguments join with one space, not two", cargo, "xtask  check", false},
		{"dot is literal", []string{"go", "test", "./..."}, "./...", true},
		{"dot is not a wildcard", []string{"go", "test", "abc"}, "a.c", false},
		{"star is literal", []string{"ls", "*.go"}, "*.go", true},
		{"star is not a wildcard", []string{"ls", "main.go"}, "*.go", false},
		{"bracket is literal", []string{"grep", "[a-z]+", "f"}, "[a-z]+ f", true},
		{"bracket is not a class", []string{"grep", "b", "f"}, "[a-z]", false},
		{"whitespace inside an argument is kept", []string{"sh", "-c", "a  b"}, "-c a  b", true},
		{"whitespace inside an argument is not collapsed", []string{"sh", "-c", "a  b"}, "a b", false},
		{"quotes are not added", []string{"sh", "-c", "a b"}, `"a b"`, false},
		{"surrounding filter space is not trimmed", cargo, " cargo", false},
		{"raw executable is not searched", cargo, "resolved", false},
		{"empty argv", nil, "cargo", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var e runcache.RunEntry
			e.KeyInput.Command.Argv = tc.argv
			e.KeyInput.Command.RawExecutable = "/resolved/bin/tool"
			if got := commandMatches(e, tc.filter); got != tc.want {
				t.Errorf("commandMatches(%q, %q) = %v, want %v", tc.argv, tc.filter, got, tc.want)
			}
		})
	}
}
