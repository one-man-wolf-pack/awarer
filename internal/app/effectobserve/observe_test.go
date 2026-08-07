package effectobserve_test

import (
	"errors"
	"testing"

	"awarer/internal/app/effectobserve"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/runcache"
	"awarer/internal/infra/blake3hash"
)

// fakeWalker returns a scripted set of discovered roots and can inject a failure.
type fakeWalker struct {
	discovered []effectobserve.DiscoveredRoot
	err        error
}

func (w *fakeWalker) Discover(project string, watchNames []string) ([]effectobserve.DiscoveredRoot, error) {
	if w.err != nil {
		return nil, w.err
	}
	return w.discovered, nil
}

func subHash(t *testing.T, h hashing.Hasher, s string) hashing.TreeHash {
	t.Helper()
	return h.HashBytes([]byte(s))
}

func TestObserveRejectsEmptyWatchSet(t *testing.T) {
	// The watch set is non-empty product policy, so an empty one at this boundary is a
	// wiring or caller bug. It must fail loudly rather than yield an identity no
	// execution can produce — and a list of nothing but blanks canonicalizes to the
	// same empty set.
	h := blake3hash.New()
	for _, roots := range [][]string{nil, {}, {"", ""}} {
		obs, _, err := effectobserve.New(&fakeWalker{}, h).Observe("/proj", roots)
		if err == nil {
			t.Errorf("Observe(%q) must reject an empty watch set, got %v", roots, obs.Status())
		}
	}
}

func TestObserveFailClosedIsUnavailable(t *testing.T) {
	h := blake3hash.New()
	svc := effectobserve.New(&fakeWalker{err: errors.New("unreadable")}, h)
	obs, _, err := svc.Observe("/proj", []string{"build", "target"})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.Status() != runcache.EffectUnavailable {
		t.Fatalf("status = %v, want unavailable", obs.Status())
	}
	if obs.SafeForReuse() {
		t.Error("unavailable effect must not be safe for reuse")
	}
	if obs.RootCount() != 2 {
		t.Errorf("root count = %d, want 2", obs.RootCount())
	}
}

func TestObserveDeterministicRegardlessOfDiscoveryOrder(t *testing.T) {
	h := blake3hash.New()
	a := &fakeWalker{discovered: []effectobserve.DiscoveredRoot{
		{Path: "build", SubHash: subHash(t, h, "b"), Entries: 3},
		{Path: "packages/api/target", SubHash: subHash(t, h, "t"), Entries: 5},
	}}
	b := &fakeWalker{discovered: []effectobserve.DiscoveredRoot{
		{Path: "packages/api/target", SubHash: subHash(t, h, "t"), Entries: 5},
		{Path: "build", SubHash: subHash(t, h, "b"), Entries: 3},
	}}
	oa, _, _ := effectobserve.New(a, h).Observe("/proj", []string{"build", "target"})
	ob, _, _ := effectobserve.New(b, h).Observe("/proj", []string{"build", "target"})
	if oa.Signature().String() != ob.Signature().String() {
		t.Errorf("signature depends on discovery order: %s vs %s", oa.Signature(), ob.Signature())
	}
}

func TestObserveNestedRootChangesSignature(t *testing.T) {
	h := blake3hash.New()
	base := &fakeWalker{discovered: []effectobserve.DiscoveredRoot{
		{Path: "packages/api/target", SubHash: subHash(t, h, "v1"), Entries: 1},
	}}
	changed := &fakeWalker{discovered: []effectobserve.DiscoveredRoot{
		{Path: "packages/api/target", SubHash: subHash(t, h, "v2"), Entries: 1},
	}}
	oa, _, _ := effectobserve.New(base, h).Observe("/proj", []string{"target"})
	ob, _, _ := effectobserve.New(changed, h).Observe("/proj", []string{"target"})
	if oa.Signature().String() == ob.Signature().String() {
		t.Error("a changed nested watched directory must change the effect signature")
	}
}

func TestObserveDisappearedRootChangesSignature(t *testing.T) {
	h := blake3hash.New()
	present := &fakeWalker{discovered: []effectobserve.DiscoveredRoot{
		{Path: "dist", SubHash: subHash(t, h, "x"), Entries: 1},
	}}
	gone := &fakeWalker{discovered: nil}
	withPresent, _, _ := effectobserve.New(present, h).Observe("/proj", []string{"dist"})
	withGone, _, _ := effectobserve.New(gone, h).Observe("/proj", []string{"dist"})
	if withPresent.Signature().String() == withGone.Signature().String() {
		t.Error("a watched directory disappearing must change the signature")
	}
}

func TestObserveWatchNameFoldedEvenWithoutMatch(t *testing.T) {
	h := blake3hash.New()
	// No directories match, but the configured watch names still shape the identity,
	// so changing the watch list changes the signature even before anything exists.
	one, _, _ := effectobserve.New(&fakeWalker{}, h).Observe("/proj", []string{"dist"})
	two, _, _ := effectobserve.New(&fakeWalker{}, h).Observe("/proj", []string{"dist", "coverage"})
	if one.Signature().String() == two.Signature().String() {
		t.Error("changing the watch-name list must change the signature")
	}
}

// TestObserveReportOverBudgetNamesRoot proves the diagnostic side channel names the
// huge root when a Discover fails closed on the entry budget, while the observation
// identity stays Unavailable (fail-closed) exactly as before.
func TestObserveReportOverBudgetNamesRoot(t *testing.T) {
	h := blake3hash.New()
	over := &fakeWalker{err: &effectobserve.OverBudgetError{Path: "target", Limit: 100_000}}
	obs, report, err := effectobserve.New(over, h).Observe("/proj", []string{"target"})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.Status() != runcache.EffectUnavailable {
		t.Fatalf("status = %v, want unavailable", obs.Status())
	}
	fact, ok := report.OverBudget()
	if !ok {
		t.Fatal("expected an over-budget fact in the report")
	}
	if fact.Path != "target" || fact.Limit != 100_000 {
		t.Errorf("over-budget fact = %+v, want {target 100000}", fact)
	}
	if _, ok := report.LargestRoot(); ok {
		t.Error("over-budget report must not also carry a largest-root fact")
	}
}

// TestObserveReportPlainFailureCarriesNoEvidence proves a non-budget Discover failure
// (an unreadable directory) still fails closed but carries no root evidence — only a
// budget overrun names a root.
func TestObserveReportPlainFailureCarriesNoEvidence(t *testing.T) {
	h := blake3hash.New()
	svc := effectobserve.New(&fakeWalker{err: errors.New("unreadable")}, h)
	obs, report, err := svc.Observe("/proj", []string{"target"})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.Status() != runcache.EffectUnavailable {
		t.Fatalf("status = %v, want unavailable", obs.Status())
	}
	_, hasOverBudget := report.OverBudget()
	_, hasLargestRoot := report.LargestRoot()
	if hasOverBudget || hasLargestRoot {
		t.Errorf("plain failure must carry no evidence, got %+v", report)
	}
}

// TestObserveReportLargestRoot proves the success path reports the biggest fully-observed
// root with its true entry count.
func TestObserveReportLargestRoot(t *testing.T) {
	h := blake3hash.New()
	w := &fakeWalker{discovered: []effectobserve.DiscoveredRoot{
		{Path: "build", SubHash: subHash(t, h, "b"), Entries: 3},
		{Path: "node_modules", SubHash: subHash(t, h, "n"), Entries: 4200},
		{Path: "dist", SubHash: subHash(t, h, "d"), Entries: 12},
	}}
	obs, report, err := effectobserve.New(w, h).Observe("/proj", []string{"build", "node_modules", "dist"})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.Status() != runcache.EffectObserved {
		t.Fatalf("status = %v, want observed", obs.Status())
	}
	fact, ok := report.LargestRoot()
	if !ok {
		t.Fatal("expected a largest-root fact")
	}
	if fact.Path != "node_modules" || fact.Entries != 4200 {
		t.Errorf("largest-root fact = %+v, want {node_modules 4200}", fact)
	}
	if _, ok := report.OverBudget(); ok {
		t.Error("success report must not carry an over-budget fact")
	}
}

// The diagnostic report is a pure side channel: computing it changes nothing keyed, so
// the folded effect signature is unaffected by it. Two identical discoveries fold to the
// same signature even though the report names the largest root.
func TestObserveSignatureIndependentOfReport(t *testing.T) {
	h := blake3hash.New()
	discovered := []effectobserve.DiscoveredRoot{
		{Path: "target", SubHash: subHash(t, h, "t"), Entries: 90_000},
	}
	obs, report, err := effectobserve.New(&fakeWalker{discovered: discovered}, h).Observe("/proj", []string{"target"})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	obs2, _, err := effectobserve.New(&fakeWalker{discovered: discovered}, h).Observe("/proj", []string{"target"})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.Signature().String() != obs2.Signature().String() {
		t.Errorf("signature not deterministic: %s vs %s", obs.Signature(), obs2.Signature())
	}
	if fact, ok := report.LargestRoot(); !ok || fact.Entries != 90_000 {
		t.Fatalf("expected largest-root fact with 90000 entries, got %+v", report)
	}
}
