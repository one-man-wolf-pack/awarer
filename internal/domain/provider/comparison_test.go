package provider_test

import (
	"testing"
	"time"

	"awarer/internal/domain/provider"
)

// Mutation proofs:
//   - break Complete's relationship mapping (e.g. add RelIncomparable to the complete
//     set) -> TestComparisonCompleteProjection goes red (see its own note).
//   - in Classify, return VerdictEqual on tree-hash equality before checking
//     provablyCompatible -> TestClassifyHashEqualDifferentPolicyIsIncomparable goes red.
//   - in provablyCompatible, drop the scanConfig equality -> TestClassifyDifferentScanConfig
//     goes red.
//   - relax sameImmutableRecord to ignore the run selector, or move the
//     sameImmutableRecord check after provablyCompatible ->
//     TestClassifyRunBeforeVsAfterChangedNeedsMerge / TestClassifyRunSelfIsEqual go red.

func cpIdent(t *testing.T, id string, treeNibble string, scan provider.ScanIdentity) provider.Identity {
	t.Helper()
	i, err := provider.NewCheckpointIdentity(cpID(t, id), treeHash(t, treeNibble), scan, time.Now().UTC(), boundary(t, 0))
	if err != nil {
		t.Fatalf("NewCheckpointIdentity: %v", err)
	}
	return i
}

// runIdent builds a run-observation identity with a complete scan policy identity:
// cfgNibble selects the persisted scan_config_hash, so tests can build
// run observations whose policies are provably compatible (equal cfgNibble) or
// provably distinct (different cfgNibble).
func runIdent(t *testing.T, before bool, id, treeNibble, cfgNibble string) provider.Identity {
	t.Helper()
	i, err := provider.NewRunObservationIdentity(before, runID(t, id), treeHash(t, treeNibble), validScan(t, cfgNibble), time.Time{}, false, boundary(t, 0))
	if err != nil {
		t.Fatalf("NewRunObservationIdentity: %v", err)
	}
	return i
}

// TestNewCountsRejectsNegativeAndRoutesEveryCount pins both of the constructor's
// obligations: it rejects a negative value in every position, and it stores each argument
// in its own field. The accepted values are pairwise distinct on purpose — with equal
// counts a constructor that stored an argument in the wrong field, or an accessor that
// returned a sibling's value, would still satisfy every assertion. The five counts are the
// whole change cardinality, so their sum is asserted against the independent literal sum of
// the same distinct values rather than against a value-owned total.
func TestNewCountsRejectsNegativeAndRoutesEveryCount(t *testing.T) {
	negatives := map[string][5]int{
		"added":        {-1, 0, 0, 0, 0},
		"modified":     {0, -1, 0, 0, 0},
		"deleted":      {0, 0, -1, 0, 0},
		"type-changed": {0, 0, 0, -1, 0},
		"skipped":      {0, 0, 0, 0, -1},
	}
	for name, v := range negatives {
		if _, err := provider.NewCounts(v[0], v[1], v[2], v[3], v[4]); err == nil {
			t.Errorf("NewCounts with a negative %s count: expected an error", name)
		}
	}

	c, err := provider.NewCounts(1, 2, 3, 4, 5)
	if err != nil {
		t.Fatalf("NewCounts: %v", err)
	}
	got := [5]int{c.Added(), c.Modified(), c.Deleted(), c.TypeChanged(), c.Skipped()}
	if want := [5]int{1, 2, 3, 4, 5}; got != want {
		t.Errorf("accessors = %v, want %v (a=1 m=2 d=3 t=4 s=5)", got, want)
	}
	sum := c.Added() + c.Modified() + c.Deleted() + c.TypeChanged() + c.Skipped()
	if sum != 1+2+3+4+5 {
		t.Errorf("counts sum = %d, want %d: every per-kind count contributes to the change cardinality", sum, 1+2+3+4+5)
	}
}

func TestEqualComparison(t *testing.T) {
	l := cpIdent(t, "a", "1", validScan(t, "a"))
	r := cpIdent(t, "b", "1", validScan(t, "a"))
	cmp := provider.Equal(l, r)
	if cmp.Relationship() != provider.RelEqual {
		t.Errorf("rel = %v, want equal", cmp.Relationship())
	}
	if _, ok := cmp.Counts(); ok {
		t.Error("equal comparison must not carry counts")
	}
	if _, ok := cmp.Reason(); ok {
		t.Error("equal comparison must not carry a reason")
	}
	if _, ok := cmp.Left(); !ok {
		t.Error("equal comparison must carry the left operand")
	}
	if !cmp.Complete() {
		t.Error("equal comparison must be complete")
	}
}

func TestDifferent(t *testing.T) {
	l := cpIdent(t, "a", "1", validScan(t, "a"))
	r := cpIdent(t, "b", "2", validScan(t, "a"))
	counts, _ := provider.NewCounts(1, 0, 0, 0, 0)
	cmp := provider.Different(l, r, counts)
	got, ok := cmp.Counts()
	if !ok || got.Added() != 1 {
		t.Errorf("counts = (%+v,%v)", got, ok)
	}
	if !cmp.Complete() {
		t.Error("different comparison must be complete")
	}
}

func TestIncomparableAndUnavailableRequireReason(t *testing.T) {
	l := cpIdent(t, "a", "1", validScan(t, "a"))
	r := cpIdent(t, "b", "2", validScan(t, "b"))
	if _, err := provider.Incomparable(l, r, provider.Reason("bogus")); err == nil {
		t.Error("Incomparable must reject an invalid reason")
	}
	cmp, err := provider.Incomparable(l, r, provider.ReasonIncompatibleIdentityPolicy)
	if err != nil {
		t.Fatalf("Incomparable: %v", err)
	}
	if _, ok := cmp.Counts(); ok {
		t.Error("incomparable must not carry counts")
	}
	if reason, ok := cmp.Reason(); !ok || reason != provider.ReasonIncompatibleIdentityPolicy {
		t.Errorf("reason = (%v,%v)", reason, ok)
	}
	// Unavailable accepts a missing operand.
	u, err := provider.ComparisonUnavailable(&l, nil, provider.ReasonNotFound)
	if err != nil {
		t.Fatalf("ComparisonUnavailable: %v", err)
	}
	if _, ok := u.Right(); ok {
		t.Error("unavailable comparison with a nil right operand must report it absent")
	}
	if _, err := provider.ComparisonUnavailable(&l, nil, provider.Reason("")); err == nil {
		t.Error("ComparisonUnavailable must reject an invalid reason")
	}
}

func TestClassifySameCheckpointIsEqual(t *testing.T) {
	// Same checkpoint id on both sides: provably the same immutable record -> equal,
	// even without draining a manifest.
	l := cpIdent(t, "same", "1", validScan(t, "a"))
	r := cpIdent(t, "same", "1", validScan(t, "a"))
	if v := provider.Classify(l, r); v != provider.VerdictEqual {
		t.Errorf("Classify same checkpoint = %v, want equal", v)
	}
}

func TestClassifyRunSelfIsEqual(t *testing.T) {
	// The same immutable run observation (same id + selector, and therefore the same
	// single scan policy it recorded) compared with itself is equal by record identity:
	// sameImmutableRecord short-circuits before any manifest is drained, so a self-compare
	// is equal without draining. A run observation has exactly one policy identity, so both
	// operands carry it.
	l := runIdent(t, true, "run1", "9", "a")
	r := runIdent(t, true, "run1", "9", "a")
	if v := provider.Classify(l, r); v != provider.VerdictEqual {
		t.Errorf("Classify run self = %v, want equal", v)
	}
}

func TestClassifyRunBeforeVsAfterUnchangedIsEqual(t *testing.T) {
	// before vs after of the same run are different observations (not the same record),
	// but both carry the run's persisted scan policy identity. When the run did not
	// mutate its inputs the two tree hashes match, so with proven-compatible policy
	// the pair is equal.
	before := runIdent(t, true, "run1", "9", "a")
	after := runIdent(t, false, "run1", "9", "a")
	if v := provider.Classify(before, after); v != provider.VerdictEqual {
		t.Errorf("Classify run before/after unchanged = %v, want equal", v)
	}
}

func TestClassifyRunBeforeVsAfterChangedNeedsMerge(t *testing.T) {
	// before vs after of a run that changed its inputs: different tree hashes under one
	// shared, known scan policy -> needs the streaming merge for counts, never equal.
	// This also guards sameImmutableRecord's selector distinction: were it relaxed to
	// treat before and after as the same record, this would wrongly report equal.
	before := runIdent(t, true, "run1", "9", "a")
	after := runIdent(t, false, "run1", "8", "a")
	if v := provider.Classify(before, after); v != provider.VerdictNeedsMerge {
		t.Errorf("Classify run before/after changed = %v, want needs-merge", v)
	}
}

func TestClassifyProvenCompatibleEqualHash(t *testing.T) {
	scan := validScan(t, "a")
	l := cpIdent(t, "a", "1", scan)
	r := cpIdent(t, "b", "1", scan) // distinct records, same scan identity + tree hash
	if v := provider.Classify(l, r); v != provider.VerdictEqual {
		t.Errorf("Classify proven-compatible equal hash = %v, want equal", v)
	}
}

func TestClassifyProvenCompatibleDifferentHashNeedsMerge(t *testing.T) {
	scan := validScan(t, "a")
	l := cpIdent(t, "a", "1", scan)
	r := cpIdent(t, "b", "2", scan)
	if v := provider.Classify(l, r); v != provider.VerdictNeedsMerge {
		t.Errorf("Classify proven-compatible different hash = %v, want needs-merge", v)
	}
}

func TestClassifyHashEqualDifferentPolicyIsIncomparable(t *testing.T) {
	// The strict rule: equal tree-hash bytes alone do not prove equality across
	// different observation policies. A run observation against a distinct checkpoint
	// with the SAME tree hash but a different scan_config_hash must be incomparable,
	// never equal — the run's policy is now known, so the incompatibility is proven,
	// not merely unproven.
	run := runIdent(t, true, "run1", "1", "a")     // scan config "a"
	cp := cpIdent(t, "cp", "1", validScan(t, "c")) // same tree "1", scan config "c"
	if run.TreeHash() != cp.TreeHash() {
		t.Fatal("precondition: tree hashes should be equal for this test")
	}
	if v := provider.Classify(run, cp); v != provider.VerdictIncomparable {
		t.Errorf("Classify hash-equal-different-policy = %v, want incomparable", v)
	}
}

func TestClassifyDifferentScanConfig(t *testing.T) {
	l := cpIdent(t, "a", "1", validScan(t, "a"))
	r := cpIdent(t, "b", "1", validScan(t, "c")) // same hash, different scan config
	if v := provider.Classify(l, r); v != provider.VerdictIncomparable {
		t.Errorf("Classify different scan config = %v, want incomparable", v)
	}
}

func TestClassifyDistinctRecordsNeedProof(t *testing.T) {
	// Two distinct run records with the same tree hash but different scan policies are
	// not the same record and not provably compatible -> incomparable regardless of the
	// matching hash bytes.
	l := runIdent(t, true, "run1", "1", "a")
	r := runIdent(t, true, "run2", "1", "b")
	if v := provider.Classify(l, r); v != provider.VerdictIncomparable {
		t.Errorf("Classify distinct run records = %v, want incomparable", v)
	}
}

func TestClassifyDistinctRunsProvenCompatible(t *testing.T) {
	// The converse of TestClassifyDistinctRecordsNeedProof: two distinct run records
	// under the same known scan policy with the same tree hash are provably compatible
	// and equal — the enrichment that lets independently named run observations compare
	// once their policies are actually compatible.
	l := runIdent(t, true, "run1", "1", "a")
	r := runIdent(t, false, "run2", "1", "a")
	if v := provider.Classify(l, r); v != provider.VerdictEqual {
		t.Errorf("Classify proven-compatible distinct runs = %v, want equal", v)
	}
}

// TestComparisonCompleteProjection pins the relationship-to-completeness projection:
// equal and different are complete observations; incomparable and unavailable are not.
// The oracle is a fixed expectation per relationship, independent of the derivation.
//
// Mutation proof: breaking Complete's mapping (e.g. adding RelIncomparable to the
// complete set, or dropping RelDifferent) turns this test red.
func TestComparisonCompleteProjection(t *testing.T) {
	l := cpIdent(t, "a", "1", validScan(t, "a"))
	r := cpIdent(t, "b", "2", validScan(t, "a"))
	counts, _ := provider.NewCounts(1, 0, 0, 0, 0)
	incomp, _ := provider.Incomparable(l, r, provider.ReasonIncompatibleIdentityPolicy)
	unavail, _ := provider.ComparisonUnavailable(&l, nil, provider.ReasonNotFound)

	cases := map[string]struct {
		cmp  provider.Comparison
		want bool
	}{
		"equal":        {provider.Equal(l, r), true},
		"different":    {provider.Different(l, r, counts), true},
		"incomparable": {incomp, false},
		"unavailable":  {unavail, false},
	}
	for name, c := range cases {
		if got := c.cmp.Complete(); got != c.want {
			t.Errorf("%s: Complete() = %v, want %v", name, got, c.want)
		}
	}
}
