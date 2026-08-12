package config

import (
	"slices"
	"testing"
)

func TestEffectiveEffectRootsIsAdditiveAndFresh(t *testing.T) {
	got := EffectiveEffectRoots([]string{"custom-out"})
	// The built-in defaults are all present, followed by the user's additions.
	for _, want := range EffectRootDefaults() {
		if !slices.Contains(got, want) {
			t.Errorf("effective effect roots missing built-in %q: %v", want, got)
		}
	}
	if !slices.Contains(got, "custom-out") {
		t.Errorf("effective effect roots missing the user addition: %v", got)
	}

	// The result is a fresh slice: mutating it must not perturb the defaults.
	got[0] = "mutated"
	if slices.Contains(EffectRootDefaults(), "mutated") {
		t.Error("EffectiveEffectRoots returned a slice aliasing the shared defaults")
	}
}

func TestEffectRootDefaultsSubsetOfGeneratedOutputDirs(t *testing.T) {
	generated := GeneratedOutputDirs()
	for _, d := range EffectRootDefaults() {
		if !generated[d] {
			t.Errorf("watched effect root %q is not in GeneratedOutputDirs", d)
		}
	}
}

// TestRunBaselineExcludesCoveredByEffectRoots locks the correctness invariant: every
// directory the run input scan is blind to (a baseline exclude) must be watched as an
// effect root, so its deletion/change can never leave a reusable hit stale. The only
// documented exception is the protected excludes (.git/.awa), which awa never observes.
func TestRunBaselineExcludesCoveredByEffectRoots(t *testing.T) {
	watched := make(map[string]bool)
	for _, d := range EffectRootDefaults() {
		watched[d] = true
	}
	protected := make(map[string]bool)
	for _, d := range ProtectedExcludes() {
		protected[d] = true
	}
	for _, d := range BaselineExcludes() {
		if protected[d] {
			continue // documented exception: VCS/awa state is outside the guarantee
		}
		if !watched[d] {
			t.Errorf("baseline exclude %q is hidden from the run input scan but not watched as an effect root: "+
				"its change would be invisible to cache correctness (add it to effect roots or document why it is safe)", d)
		}
	}
}

func TestExtraEffectRootsAcceptNamesAndExactPaths(t *testing.T) {
	for _, bad := range []string{
		`generated\cache`, `C:\tmp`, "C:",
		"..", ".", "",
		" build", "build ", "\tbuild",
		" artifacts/bin", "artifacts/bin ",
		"/abs/bin", "artifacts/", "a//b",
		"./a", "a/./b", "../a", "a/../b",
		".awa/store", "a/.git",
	} {
		cfg := Defaults()
		cfg.Run.WatchedEffectRoots = []string{bad}
		if err := cfg.Validate(); err == nil {
			t.Errorf("extra_effect_roots %q must be rejected (not a portable name or clean project-relative path)", bad)
		}
	}

	// ".git" and "artifacts/ bin" are the two boundaries a plausible cleanup would move
	// into the rejected list above: the bare name stays accepted for v0.1.x configs, and
	// the padding rule stops at the value boundary.
	for _, good := range []string{"generated", ".cache", ".git", "artifacts/bin", "a/b/c", "artifacts/ bin"} {
		cfg := Defaults()
		cfg.Run.WatchedEffectRoots = []string{good}
		if err := cfg.Validate(); err != nil {
			t.Errorf("extra_effect_roots %q must be accepted: %v", good, err)
		}
	}
}
