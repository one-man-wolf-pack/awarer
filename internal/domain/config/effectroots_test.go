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

func TestExtraEffectRootsMustBeDirectoryNames(t *testing.T) {
	// extra_effect_roots is a list of directory names matched by basename wherever
	// they appear, so a path, "." / "..", or an empty entry must be rejected — anything
	// with a separator would be a silent no-op the observer never matches.
	// Rejection is OS-independent: path/volume spellings in either OS's syntax, dot
	// segments, and leading/trailing whitespace are all silent no-ops the observer
	// would never match, so they must be rejected on every platform.
	for _, bad := range []string{
		"generated/cache", "build/out", "a/b/c", // unix path
		`generated\cache`, `C:\tmp`, "C:", // windows path/volume
		"..", ".", "", // dot segments / empty
		" build", "build ", "\tbuild", // leading/trailing whitespace
	} {
		cfg := Defaults()
		cfg.Run.WatchedEffectRoots = []string{bad}
		if err := cfg.Validate(); err == nil {
			t.Errorf("extra_effect_roots %q must be rejected (not a single portable directory name)", bad)
		}
	}

	// A plain directory name is accepted.
	cfg := Defaults()
	cfg.Run.WatchedEffectRoots = []string{"generated", ".cache"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("directory-name extra_effect_roots must be accepted: %v", err)
	}
}
