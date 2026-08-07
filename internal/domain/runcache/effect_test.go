package runcache_test

import (
	"testing"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/runcache"
)

func effectSig(t *testing.T) runcache.EffectHash {
	t.Helper()
	h := newHasher(t)
	return runcache.EffectHashFromTree(h.HashBytes([]byte("effect")))
}

// observedEffect builds the effect identity a real execution produces, labelled so a
// fixture can vary the signature without varying anything else. Every KeyInput
// carries one: production always observes the non-empty built-in watch set.
func observedEffect(t testing.TB, h hashing.Hasher, label string) runcache.EffectObservation {
	t.Helper()
	o, err := runcache.ObservedEffect(runcache.EffectHashFromTree(h.HashBytes([]byte(label))), 1)
	if err != nil {
		t.Fatalf("ObservedEffect(%q): %v", label, err)
	}
	return o
}

func TestObservedEffectRequiresSignature(t *testing.T) {
	if _, err := runcache.ObservedEffect(runcache.EffectHash{}, 1); err == nil {
		t.Error("observed effect without a signature must be rejected")
	}
	if _, err := runcache.ObservedEffect(effectSig(t), 0); err == nil {
		t.Error("observed effect covering zero roots must be rejected")
	}
	if _, err := runcache.ObservedEffect(effectSig(t), 2); err != nil {
		t.Errorf("a valid observed effect was rejected: %v", err)
	}
}

func TestZeroEffectObservationIsInvalid(t *testing.T) {
	// A forgotten initialization must not read as a cacheable identity: the zero value
	// is the "never observed" gap, and every execution observes the non-empty watch set.
	var zero runcache.EffectObservation
	if err := zero.Validate(); err == nil {
		t.Error("the zero EffectObservation must fail validation")
	}
	if zero.SafeForReuse() {
		t.Error("the zero EffectObservation must not be safe for reuse")
	}
	if zero.Status().Valid() {
		t.Error("the zero EffectStatus must not be a valid status")
	}
}

func TestUnavailableEffectNotSafe(t *testing.T) {
	o, err := runcache.UnavailableEffect(3)
	if err != nil {
		t.Fatal(err)
	}
	if o.SafeForReuse() {
		t.Error("unavailable effect must not be safe for reuse")
	}
	if !o.Signature().IsZero() {
		t.Error("unavailable effect must carry no signature")
	}
}

func TestNewEffectObservationRejectsContradictions(t *testing.T) {
	// The zero status is not a state a decode can land on.
	if _, err := runcache.NewEffectObservation(runcache.EffectStatus(0), runcache.EffectHash{}, 0); err == nil {
		t.Error("the zero effect status must be rejected")
	}
	// Unavailable with a signature is impossible.
	if _, err := runcache.NewEffectObservation(runcache.EffectUnavailable, effectSig(t), 1); err == nil {
		t.Error("unavailable status with a signature must be rejected")
	}
	// Observed round-trips.
	got, err := runcache.NewEffectObservation(runcache.EffectObserved, effectSig(t), 2)
	if err != nil {
		t.Fatalf("observed round-trip: %v", err)
	}
	if got.Status() != runcache.EffectObserved {
		t.Errorf("status = %v, want observed", got.Status())
	}
}

func TestParseEffectStatusClosedSet(t *testing.T) {
	// The complete current vocabulary round-trips through its own persisted token, and
	// everything else is rejected generically — no token needs naming to be refused.
	for _, s := range []runcache.EffectStatus{runcache.EffectObserved, runcache.EffectUnavailable} {
		got, err := runcache.ParseEffectStatus(s.String())
		if err != nil {
			t.Errorf("ParseEffectStatus(%q): %v", s, err)
			continue
		}
		if got != s {
			t.Errorf("ParseEffectStatus(%q) = %v, want %v", s, got, s)
		}
	}
	if _, err := runcache.ParseEffectStatus("bogus"); err == nil {
		t.Error("an unknown effect status token must be rejected")
	}
	if _, err := runcache.ParseEffectStatus(""); err == nil {
		t.Error("an empty effect status token must be rejected")
	}
}

func TestEffectGuardReasons(t *testing.T) {
	cases := []struct {
		outcome    runcache.EffectOutcome
		cacheable  bool
		wantReason runcache.MismatchReason
	}{
		{runcache.EffectGuardUnchanged, true, ""},
		{runcache.EffectGuardChanged, false, runcache.ReasonEffectStateDiffers},
		{runcache.EffectGuardUnavailable, false, runcache.ReasonEffectStateUnavailable},
	}
	for _, c := range cases {
		g, err := runcache.NewEffectGuardStatus(c.outcome)
		if err != nil {
			t.Fatalf("NewEffectGuardStatus(%v): %v", c.outcome, err)
		}
		if g.CacheableUnderEffect() != c.cacheable {
			t.Errorf("%v cacheable = %v, want %v", c.outcome, g.CacheableUnderEffect(), c.cacheable)
		}
		if g.Reason() != c.wantReason {
			t.Errorf("%v reason = %q, want %q", c.outcome, g.Reason(), c.wantReason)
		}
		// Each valid outcome round-trips through its own persisted token; anything
		// outside the current vocabulary is rejected generically below.
		got, err := runcache.ParseEffectOutcome(c.outcome.String())
		if err != nil {
			t.Errorf("ParseEffectOutcome(%q): %v", c.outcome, err)
		} else if got != c.outcome {
			t.Errorf("ParseEffectOutcome(%q) = %v, want %v", c.outcome, got, c.outcome)
		}
	}
	if _, err := runcache.ParseEffectOutcome("bogus"); err == nil {
		t.Error("an unknown effect guard token must be rejected")
	}
	// The unset zero value is invalid: a persisted run must have run the guard.
	if _, err := runcache.NewEffectGuardStatus(runcache.EffectGuardUnset); err == nil {
		t.Error("EffectGuardUnset must be rejected by NewEffectGuardStatus")
	}
	if _, err := runcache.ParseEffectOutcome("unset"); err == nil {
		t.Error("ParseEffectOutcome must reject the unset token")
	}
}

func TestEffectReasonsAreDisqualifying(t *testing.T) {
	// effect-state-differs and effect-state-unavailable must be constructible as
	// non-reusable disqualification reasons and distinct from input-tree-differs.
	for _, r := range []runcache.MismatchReason{runcache.ReasonEffectStateDiffers, runcache.ReasonEffectStateUnavailable} {
		if _, err := runcache.NonReusableRun(r); err != nil {
			t.Errorf("NonReusableRun(%q): %v", r, err)
		}
	}
	if runcache.ReasonEffectStateDiffers == runcache.ReasonInputTreeDiffers {
		t.Error("effect-state-differs must be distinct from input-tree-differs")
	}
	// fast-trust-mode is a record-only policy reason, not a disqualification.
	if _, err := runcache.RecordOnly(runcache.ReasonFastTrustMode); err != nil {
		t.Errorf("RecordOnly(fast-trust-mode): %v", err)
	}
}

func TestEffectComparePreservesUnavailable(t *testing.T) {
	h := newHasher(t)
	observed, err := runcache.ObservedEffect(effectSig(t), 1)
	if err != nil {
		t.Fatal(err)
	}
	unavailable, err := runcache.UnavailableEffect(1)
	if err != nil {
		t.Fatal(err)
	}
	stored := baseline(t, h)
	stored.Effect = observed
	current := baseline(t, h)
	current.Effect = unavailable

	cmp := runcache.CompareKeyInputs(stored, current)
	// The current effect could not be observed safely: the honest reason is
	// unavailability, not "the output differs".
	if got := runcache.ReasonFromDiffCode(cmp.PrimaryReason()); got != runcache.ReasonEffectStateUnavailable {
		t.Errorf("reason = %q, want effect-state-unavailable when current effect is unavailable", got)
	}

	// The symmetric change (observed -> observed with a different signature) stays
	// effect-state-differs.
	other, err := runcache.ObservedEffect(runcache.EffectHashFromTree(h.HashBytes([]byte("other"))), 1)
	if err != nil {
		t.Fatal(err)
	}
	current2 := baseline(t, h)
	current2.Effect = other
	cmp2 := runcache.CompareKeyInputs(stored, current2)
	if got := runcache.ReasonFromDiffCode(cmp2.PrimaryReason()); got != runcache.ReasonEffectStateDiffers {
		t.Errorf("reason = %q, want effect-state-differs for a changed observed effect", got)
	}
}

func TestEffectParticipatesInKey(t *testing.T) {
	h := newHasher(t)
	base := baseline(t, h)
	changed := base
	changed.Effect = observedEffect(t, h, "other-effect")
	if base.Compute(h).String() == changed.Compute(h).String() {
		t.Error("the effect observation must participate in the run key")
	}
	if err := base.Validate(); err != nil {
		t.Errorf("a key input with an observed effect must validate: %v", err)
	}
	// A key input that never observed its effect state is not a state any execution
	// reaches, so it must not validate its way into a key.
	missing := base
	missing.Effect = runcache.EffectObservation{}
	if err := missing.Validate(); err == nil {
		t.Error("a key input with an unset effect observation must be rejected")
	}
}
