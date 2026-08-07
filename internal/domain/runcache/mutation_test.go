package runcache_test

import (
	"testing"

	"awarer/internal/domain/runcache"
)

func TestMutationStatusCacheability(t *testing.T) {
	cases := []struct {
		name       string
		outcome    runcache.MutationOutcome
		cacheable  bool
		changed    bool
		scanFailed bool
		reason     runcache.MismatchReason
	}{
		{"unobserved", runcache.MutationUnobserved, false, false, false, ""},
		{"unchanged", runcache.MutationUnchanged, true, false, false, ""},
		{"changed", runcache.MutationChanged, false, true, false, runcache.ReasonMutatedState},
		{"scan-failed", runcache.MutationScanFailed, false, false, true, runcache.ReasonPostScanFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := runcache.NewMutationStatus(c.outcome)
			if err != nil {
				t.Fatalf("NewMutationStatus(%v): %v", c.outcome, err)
			}
			if got := m.CacheableUnderMutation(); got != c.cacheable {
				t.Errorf("CacheableUnderMutation = %v, want %v", got, c.cacheable)
			}
			if got := m.Changed(); got != c.changed {
				t.Errorf("Changed = %v, want %v", got, c.changed)
			}
			if got := m.ScanFailed(); got != c.scanFailed {
				t.Errorf("ScanFailed = %v, want %v", got, c.scanFailed)
			}
			if got := m.Reason(); got != c.reason {
				t.Errorf("Reason = %q, want %q", got, c.reason)
			}
		})
	}
}

func TestMutationStatusRejectsUnknownOutcome(t *testing.T) {
	if _, err := runcache.NewMutationStatus(runcache.MutationOutcome(99)); err == nil {
		t.Fatal("NewMutationStatus accepted an unknown outcome")
	}
}

func TestRunReusabilityValidStates(t *testing.T) {
	r := runcache.Reusable()
	if !r.IsReusable() {
		t.Error("Reusable() is not reusable")
	}
	if r.Reason() != "" {
		t.Errorf("reusable verdict carries reason %q", r.Reason())
	}

	nr, err := runcache.NotReusable(runcache.ReasonInputTreeDiffers)
	if err != nil {
		t.Fatalf("NotReusable: %v", err)
	}
	if nr.IsReusable() {
		t.Error("NotReusable verdict reports reusable")
	}
	if nr.Reason() != runcache.ReasonInputTreeDiffers {
		t.Errorf("reason = %q, want %q", nr.Reason(), runcache.ReasonInputTreeDiffers)
	}
}

func TestNotReusableRequiresReason(t *testing.T) {
	if _, err := runcache.NotReusable(""); err == nil {
		t.Fatal("NotReusable accepted an empty reason")
	}
}

// TestMismatchReasonsIsTheClosedVocabulary proves the published enumeration and
// the parser are one vocabulary rather than two lists that happen to agree: every
// enumerated reason parses back to itself, none is empty or duplicated, and the
// count is pinned so adding a reason is a deliberate edit that every consumer
// walking the vocabulary (including the documentation coverage matrix) sees.
func TestMismatchReasonsIsTheClosedVocabulary(t *testing.T) {
	reasons := runcache.MismatchReasons()
	if len(reasons) != 20 {
		t.Errorf("MismatchReasons() has %d entries, want 20 — update the documentation coverage matrix too", len(reasons))
	}
	seen := map[runcache.MismatchReason]bool{}
	for _, r := range reasons {
		if r == "" {
			t.Error("MismatchReasons() contains an empty reason")
			continue
		}
		if seen[r] {
			t.Errorf("MismatchReasons() lists %q twice", r)
		}
		seen[r] = true
		got, err := runcache.ParseMismatchReason(r.String())
		if err != nil {
			t.Errorf("enumerated reason %q does not parse: %v", r, err)
			continue
		}
		if got != r {
			t.Errorf("ParseMismatchReason(%q) = %q, want the same token", r, got)
		}
	}
}

// TestMismatchReasonsIsCopySafe proves a consumer cannot reorder or truncate the
// catalog every other consumer reads.
func TestMismatchReasonsIsCopySafe(t *testing.T) {
	first := runcache.MismatchReasons()
	original := first[0]
	first[0] = "mutated"
	if got := runcache.MismatchReasons()[0]; got != original {
		t.Errorf("MismatchReasons() shares its backing array: first entry is now %q", got)
	}
}

// persistedReasonTokens restates the current vocabulary as literal strings, on
// purpose. It is an independent oracle, not a convenience: a decoder test that only
// walked MismatchReasons() would follow the catalog anywhere it went, so dropping a
// reason would silently take its test with it — and the records already on disk
// carrying that token would stop decoding with nothing gone red. Spelled out here,
// removing a reason from the catalog fails this test by name.
var persistedReasonTokens = []string{
	"input-tree-differs",
	"mutated-state",
	"effect-state-differs",
	"effect-state-unavailable",
	"fast-trust-mode",
	"expired",
	"env-mismatch",
	"cwd-mismatch",
	"config-mismatch",
	"platform-mismatch",
	"payload-missing",
	"corrupt",
	"post-scan-failed",
	"non-cacheable-policy",
	"record-only",
	"no-cache",
	"stdin-not-keyed",
	"skipped-inputs",
	"capture-disabled",
	"failed-not-cached",
}

func TestParseMismatchReasonAcceptsOnlyTheCurrentVocabulary(t *testing.T) {
	// Every token a record can carry decodes to itself.
	for _, tok := range persistedReasonTokens {
		got, err := runcache.ParseMismatchReason(tok)
		if err != nil {
			t.Errorf("ParseMismatchReason(%q): %v — a record carrying this token no longer decodes", tok, err)
			continue
		}
		if got.String() != tok {
			t.Errorf("ParseMismatchReason(%q) = %q, want the same token", tok, got)
		}
	}
	// The oracle and the catalog describe the same set, in both directions.
	catalog := map[string]bool{}
	for _, r := range runcache.MismatchReasons() {
		catalog[r.String()] = true
	}
	for _, tok := range persistedReasonTokens {
		if !catalog[tok] {
			t.Errorf("%q is no longer in the published catalog; records carrying it are now undecodable", tok)
		}
		delete(catalog, tok)
	}
	for tok := range catalog {
		t.Errorf("catalog publishes %q, which this test does not cover; add it above deliberately", tok)
	}
	// Anything outside the catalog is rejected. The parser is a membership test over
	// the published vocabulary and nothing else: it holds no map of spellings this
	// project once used, so a record carrying one fails decoding rather than being
	// quietly translated into a reason it does not literally state.
	for _, unknown := range []string{"not-a-reason", "", "INPUT-TREE-DIFFERS", "input_tree_differs"} {
		if _, err := runcache.ParseMismatchReason(unknown); err == nil {
			t.Errorf("ParseMismatchReason(%q) was accepted, want rejection", unknown)
		}
	}
}

func TestReasonFromDiffCode(t *testing.T) {
	cases := []struct {
		code runcache.DiffCode
		want runcache.MismatchReason
	}{
		{runcache.DiffInputTreeChanged, runcache.ReasonInputTreeDiffers},
		{runcache.DiffCWDChanged, runcache.ReasonCWDMismatch},
		{runcache.DiffEnvChanged, runcache.ReasonEnvMismatch},
		{runcache.DiffRunConfigChanged, runcache.ReasonConfigMismatch},
		{runcache.DiffCommandChanged, runcache.ReasonConfigMismatch},
		{runcache.DiffPlatformChanged, runcache.ReasonPlatformMismatch},
		{runcache.DiffCode(""), runcache.ReasonConfigMismatch},
	}
	for _, c := range cases {
		if got := runcache.ReasonFromDiffCode(c.code); got != c.want {
			t.Errorf("ReasonFromDiffCode(%q) = %q, want %q", c.code, got, c.want)
		}
	}
}

func TestReasonCanSampleChangedPaths(t *testing.T) {
	// Only input-tree-differs can produce a meaningful changed-input-path sample; every
	// other reason (a record-only or mutating run never re-keyed, an effect-state change
	// over a different scope, a corrupt/missing payload, a policy disqualification)
	// cannot, so the near-miss surfaces must not pay for a comparison for those.
	if !runcache.ReasonCanSampleChangedPaths(runcache.ReasonInputTreeDiffers) {
		t.Error("input-tree-differs must be sampleable")
	}
	cannot := []runcache.MismatchReason{
		runcache.ReasonRecordOnly,
		runcache.ReasonEffectStateUnavailable,
		runcache.ReasonEffectStateDiffers,
		runcache.ReasonMutatedState,
		runcache.ReasonCorrupt,
		runcache.ReasonPayloadMissing,
		runcache.ReasonPostScanFailed,
		runcache.ReasonExpired,
		runcache.ReasonEnvMismatch,
		runcache.ReasonCWDMismatch,
		runcache.ReasonConfigMismatch,
		runcache.ReasonPlatformMismatch,
		runcache.ReasonNonCacheablePolicy,
		runcache.ReasonNoCache,
		runcache.ReasonStdinNotKeyed,
		runcache.ReasonSkippedInputs,
		runcache.ReasonCaptureDisabled,
		runcache.ReasonFailedNotCached,
		runcache.ReasonFastTrustMode,
		runcache.MismatchReason(""),
	}
	for _, r := range cannot {
		if runcache.ReasonCanSampleChangedPaths(r) {
			t.Errorf("reason %q must not be sampleable", r)
		}
	}
}
