package runcache_test

import (
	"strings"
	"testing"

	"awarer/internal/domain/runcache"
	"awarer/internal/infra/blake3hash"
)

func validationScope(t *testing.T, label string, dirs ...string) *runcache.ValidationScope {
	t.Helper()
	v, err := runcache.NewValidationScope(label, dirs)
	if err != nil {
		t.Fatalf("NewValidationScope: %v", err)
	}
	return &v
}

func TestValidationKeyBindsDirectoriesButNotTheLabel(t *testing.T) {
	h := blake3hash.New()
	ordinary := baseKeyInput(t)

	waived := baseKeyInput(t)
	waived.Validation = validationScope(t, "cargo-test", "target")
	renamed := baseKeyInput(t)
	renamed.Validation = validationScope(t, "renamed", "target")
	wider := baseKeyInput(t)
	wider.Validation = validationScope(t, "cargo-test", "target", "build")

	if waived.Compute(h) == ordinary.Compute(h) {
		t.Error("a waived run keys like an ordinary one: an old result would gain a waiver")
	}
	if waived.Compute(h) != renamed.Compute(h) {
		t.Error("renaming the policy changed the key")
	}
	if wider.Compute(h) == waived.Compute(h) {
		t.Error("a changed directory set reused the old key")
	}

	if cmp := runcache.CompareKeyInputs(waived, renamed); !cmp.Equal() {
		t.Errorf("label-only change compares as %+v", cmp.Differences)
	}
	cmp := runcache.CompareKeyInputs(ordinary, waived)
	if cmp.Equal() || cmp.PrimaryReason() != runcache.DiffScopeChanged || cmp.Differences[0].Field != "validation" {
		t.Fatalf("ordinary vs waived compares as %+v", cmp.Differences)
	}
	if got := runcache.ReasonFromDiffCode(cmp.PrimaryReason()); got != runcache.ReasonConfigMismatch {
		t.Errorf("reason = %q, want the existing config-mismatch", got)
	}
}

func TestValidationScopeIsCanonicalAndValidated(t *testing.T) {
	v := validationScope(t, "p", "target", "build")
	if strings.Join(v.UnobservedDirs, ",") != "build,target" {
		t.Errorf("dirs = %v, want sorted", v.UnobservedDirs)
	}
	valid := func() runcache.KeyInput {
		in := baseKeyInput(t)
		in.Effect = observedEffect(t, blake3hash.New(), "effect")
		return in
	}
	control := valid()
	control.Validation = v
	if err := control.Validate(); err != nil {
		t.Fatalf("a valid waived key input is rejected: %v", err)
	}
	for _, bad := range []runcache.ValidationScope{
		{Label: "", UnobservedDirs: []string{"target"}},
		{Label: "p"},
		{Label: "p", UnobservedDirs: []string{"target", "build"}},
		{Label: "p", UnobservedDirs: []string{"target", "target/x"}},
		{Label: "p", UnobservedDirs: []string{".git"}},
		{Label: "p", UnobservedDirs: []string{"../x"}},
	} {
		in := valid()
		in.Validation = &bad
		if err := in.Validate(); err == nil {
			t.Errorf("key input with validation %+v validated", bad)
		}
	}
}
