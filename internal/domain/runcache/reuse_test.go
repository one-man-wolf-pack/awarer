package runcache

import "testing"

func TestReuseStateConstructors(t *testing.T) {
	if !ReusableCacheEntry().IsReusable() {
		t.Error("ReusableCacheEntry must be reusable")
	}
	if ReusableCacheEntry().Reason() != "" {
		t.Error("a reusable state must carry no reason")
	}
	if _, err := NonReusableRun(""); err == nil {
		t.Error("NonReusableRun must reject an empty reason")
	}
	if _, err := RecordOnly(""); err == nil {
		t.Error("RecordOnly must reject an empty reason")
	}
	st, err := NonReusableRun(ReasonMutatedState)
	if err != nil || st.Kind() != ReuseNonReusable || st.Reason() != ReasonMutatedState {
		t.Errorf("NonReusableRun = %+v, %v", st, err)
	}
	rec, err := RecordOnly(ReasonRecordOnly)
	if err != nil || rec.Kind() != ReuseRecordOnly || rec.Reason() != ReasonRecordOnly {
		t.Errorf("RecordOnly = %+v, %v", rec, err)
	}
	ups := UnknownPostState()
	if ups.Kind() != ReuseUnknownPostState || ups.Reason() != ReasonPostScanFailed {
		t.Errorf("UnknownPostState = %+v", ups)
	}
}

// TestReuseConstructorsRejectMispairedReasons proves a record-only run cannot carry
// a disqualification reason and a non-reusable run cannot carry a policy reason: the
// two kinds model distinct domain facts, enforced by the constructors rather than by
// caller discipline.
func TestReuseConstructorsRejectMispairedReasons(t *testing.T) {
	for _, r := range []MismatchReason{ReasonMutatedState, ReasonFailedNotCached, ReasonPostScanFailed, ReasonExpired} {
		if _, err := RecordOnly(r); err == nil {
			t.Errorf("RecordOnly(%q) must be rejected (not a policy reason)", r)
		}
	}
	for _, r := range []MismatchReason{ReasonRecordOnly, ReasonNoCache, ReasonStdinNotKeyed, ReasonSkippedInputs, ReasonCaptureDisabled} {
		if _, err := NonReusableRun(r); err == nil {
			t.Errorf("NonReusableRun(%q) must be rejected (not a disqualification reason)", r)
		}
	}
}

func TestNewReuseStateRejectsContradictions(t *testing.T) {
	if _, err := NewReuseState(ReuseReusable, ReasonMutatedState); err == nil {
		t.Error("a reusable state with a reason must be rejected")
	}
	if _, err := NewReuseState(ReuseNonReusable, ""); err == nil {
		t.Error("a non-reusable state without a reason must be rejected")
	}
	if _, err := NewReuseState(ReuseNonReusable, ReasonRecordOnly); err == nil {
		t.Error("a non-reusable state with a policy reason must be rejected")
	}
	if _, err := NewReuseState(ReuseRecordOnly, ReasonMutatedState); err == nil {
		t.Error("a record-only state with a disqualification reason must be rejected")
	}
	if _, err := NewReuseState(ReuseUnknownPostState, ReasonMutatedState); err == nil {
		t.Error("unknown-post-state must require the post-scan-failed reason")
	}
	if _, err := NewReuseState(ReuseKind(99), ""); err == nil {
		t.Error("an out-of-range kind must be rejected")
	}
}

func TestParseReuseKindRoundTrip(t *testing.T) {
	for _, k := range []ReuseKind{ReuseReusable, ReuseNonReusable, ReuseRecordOnly, ReuseUnknownPostState} {
		got, err := ParseReuseKind(k.String())
		if err != nil || got != k {
			t.Errorf("round trip %s = %v, %v", k, got, err)
		}
	}
	if _, err := ParseReuseKind("bogus"); err == nil {
		t.Error("ParseReuseKind must reject an unknown token")
	}
}
