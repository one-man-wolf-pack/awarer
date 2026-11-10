package config

import (
	"strings"
	"testing"
)

func nugetRestore() EnvRequirement {
	return EnvRequirement{Argv: []string{"dotnet", "restore"}, CWD: ".", Names: []string{"NUGET_PACKAGES"}}
}

func withRequiredEnv(ds ...EnvRequirement) Config {
	c := Defaults()
	c.Run.RequiredEnv = ds
	return c
}

func TestRequiredEnvAcceptsBaselineAllowlistedAndValidationSharedSelectors(t *testing.T) {
	d := nugetRestore()
	// A baseline name and a default-allowlisted name are both requirable: requiring a
	// variable the child already inherits is a precondition, not redundant permission.
	d.Names = []string{"NUGET_PACKAGES", "HOME", "CI"}
	c := withRequiredEnv(d)
	// The same invocation may also carry a validation policy; neither needs the other.
	c.Run.Validation = []ValidationPolicy{{Name: "restore", Argv: []string{"dotnet", "restore"}, CWD: ".", UnobservedDirs: []string{"obj"}}}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid declaration rejected: %v", err)
	}
	nested := nugetRestore()
	nested.CWD = "src/app"
	if err := withRequiredEnv(nugetRestore(), nested).Validate(); err != nil {
		t.Fatalf("same argv in another cwd rejected: %v", err)
	}
}

func TestRequiredEnvRejectsInvalidDeclarations(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*EnvRequirement)
		want   string
	}{
		{"empty argv", func(d *EnvRequirement) { d.Argv = nil }, "run.required_env[0].argv"},
		{"empty executable", func(d *EnvRequirement) { d.Argv = []string{"", "x"} }, "run.required_env[0].argv"},
		{"nul argv", func(d *EnvRequirement) { d.Argv = []string{"a\x00b"} }, "argv[0] must not contain NUL"},
		{"empty cwd", func(d *EnvRequirement) { d.CWD = "" }, "run.required_env[0].cwd"},
		{"escaping cwd", func(d *EnvRequirement) { d.CWD = "../x" }, "run.required_env[0].cwd"},
		{"no names", func(d *EnvRequirement) { d.Names = nil }, "names must list at least one"},
		{"blank name", func(d *EnvRequirement) { d.Names = []string{""} }, "names[0] must not be empty"},
		{"assignment", func(d *EnvRequirement) { d.Names = []string{"A=b"} }, "not name=value"},
		{"nul name", func(d *EnvRequirement) { d.Names = []string{"A\x00"} }, "must not contain NUL"},
		{"reserved", func(d *EnvRequirement) { d.Names = []string{"awa_run"} }, "reserved by awa"},
		{"case duplicate", func(d *EnvRequirement) { d.Names = []string{"Token", "TOKEN"} }, "names[1] \"TOKEN\" duplicates names[0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := nugetRestore()
			tc.mutate(&d)
			err := withRequiredEnv(d).Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestRequiredEnvRejectsDuplicateSelectors(t *testing.T) {
	other := nugetRestore()
	other.Names = []string{"OTHER"}
	err := withRequiredEnv(nugetRestore(), other).Validate()
	if err == nil || !strings.Contains(err.Error(), "run.required_env[1] conflicts with run.required_env[0]") {
		t.Fatalf("Validate() = %v, want a selector conflict", err)
	}
}

func TestSelectRequiredEnvMatchesOnlyTheExactInvocation(t *testing.T) {
	second := EnvRequirement{Argv: []string{"go", "test", "./..."}, CWD: "svc", Names: []string{"GOFLAGS"}}
	r := withRequiredEnv(nugetRestore(), second).Run
	d, i, ok := r.SelectRequiredEnv([]string{"go", "test", "./..."}, "svc")
	if !ok || i != 1 || d.Names[0] != "GOFLAGS" {
		t.Fatalf("SelectRequiredEnv = %v, %d, %t; want the second declaration", d, i, ok)
	}
	for _, miss := range []struct {
		argv []string
		cwd  string
	}{
		{[]string{"dotnet", "restore", "--force"}, "."},
		{[]string{"dotnet"}, "."},
		{[]string{"/usr/bin/dotnet", "restore"}, "."},
		{[]string{"dotnet restore"}, "."},
		{[]string{"Dotnet", "restore"}, "."},
		{[]string{"dotnet", "restore"}, "src"},
		{[]string{"go", "test", "./..."}, "."},
	} {
		if _, _, ok := r.SelectRequiredEnv(miss.argv, miss.cwd); ok {
			t.Errorf("SelectRequiredEnv(%q, %q) matched; want no declaration", miss.argv, miss.cwd)
		}
	}
	// The returned declaration is a copy: mutating it cannot change config.
	d.Names[0] = "MUTATED"
	if r.RequiredEnv[1].Names[0] != "GOFLAGS" {
		t.Fatal("SelectRequiredEnv returned shared backing storage")
	}
}
