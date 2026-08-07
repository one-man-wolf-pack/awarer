package runcache_test

import (
	"testing"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/runcache"
)

// scanCfg is a fixed, valid BLAKE3 scan_config_hash for test observations. It is
// deterministic, so a run's before and after observations (which must share one scan
// policy identity) can both use it.
func scanCfg(t *testing.T) hashing.ConfigHash {
	t.Helper()
	return hashing.ConfigHashFromTree(hashOf(t, "scan-config"))
}

func mustMutation(t *testing.T, o runcache.MutationOutcome) runcache.MutationStatus {
	t.Helper()
	st, err := runcache.NewMutationStatus(o)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func mustEffectGuard(t testing.TB, o runcache.EffectOutcome) runcache.EffectGuardStatus {
	t.Helper()
	g, err := runcache.NewEffectGuardStatus(o)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestValidateReusableWithObservations(t *testing.T) {
	if err := validEntry(t).Validate(); err != nil {
		t.Fatalf("a reusable entry with matching observations was rejected: %v", err)
	}
}

func TestValidateRejectsReusableMutated(t *testing.T) {
	e := validEntry(t)
	e.Mutation = mustMutation(t, runcache.MutationChanged)
	if err := e.Validate(); err == nil {
		t.Error("a reusable entry whose command mutated state must be rejected")
	}
}

func TestValidateRejectsReuseDecisionMismatch(t *testing.T) {
	e := validEntry(t)
	// Reuse says reusable; decision says non-cacheable. Contradiction.
	e.Decision = runcache.CacheDecision{Cacheable: false, Reason: "no-cache"}
	if err := e.Validate(); err == nil {
		t.Error("a reuse/decision disagreement must be rejected")
	}
}

func TestValidateMutatedReasonRequiresChanged(t *testing.T) {
	e := validEntry(t)
	nr, err := runcache.NonReusableRun(runcache.ReasonMutatedState)
	if err != nil {
		t.Fatal(err)
	}
	e.Reuse = nr
	e.Decision = runcache.CacheDecision{Cacheable: false, Reason: runcache.ReasonMutatedState.String()}
	e.Mutation = mustMutation(t, runcache.MutationChanged)
	e.Before = &runcache.Observation{Manifest: runcache.ManifestRef{File: "before.manifest.jsonl", TreeHash: e.KeyInput.InputTreeHash}, ScanConfigHash: scanCfg(t)}
	after := &runcache.Observation{Manifest: runcache.ManifestRef{File: "after.manifest.jsonl", TreeHash: hashOf(t, "different")}, ScanConfigHash: scanCfg(t)}
	e.After = after
	if err := e.Validate(); err != nil {
		t.Fatalf("a mutating non-reusable entry was rejected: %v", err)
	}
	// Now claim mutated-state while the recorded outcome is unchanged: contradiction.
	e.Mutation = mustMutation(t, runcache.MutationUnchanged)
	e.After = &runcache.Observation{Manifest: runcache.ManifestRef{File: "after.manifest.jsonl", TreeHash: e.KeyInput.InputTreeHash}, ScanConfigHash: scanCfg(t)}
	if err := e.Validate(); err == nil {
		t.Error("a mutated-state reason with an unchanged outcome must be rejected")
	}
}

func TestValidateBeforeTreeHashMustMatchKey(t *testing.T) {
	e := validEntry(t)
	e.Before = &runcache.Observation{Manifest: runcache.ManifestRef{File: "before.manifest.jsonl", TreeHash: hashOf(t, "wrong")}, ScanConfigHash: scanCfg(t)}
	if err := e.Validate(); err == nil {
		t.Error("a before-observation tree hash that does not match the keyed input must be rejected")
	}
}

func TestValidateRequiresBeforeObservation(t *testing.T) {
	// Every recorded run is a real execution the mutation guard observed before the
	// command, so an entry without that observation is not a state anything produces —
	// and the domain, not only the store decoder, is where that is refused.
	e := validEntry(t)
	e.Before = nil
	if err := e.Validate(); err == nil {
		t.Error("a run entry without a before-observation must be rejected")
	}
}

func TestValidateAfterPresentExactlyWhenScanSucceeded(t *testing.T) {
	// A successful post-run scan always left an after-observation.
	e := validEntry(t)
	e.After = nil
	if err := e.Validate(); err == nil {
		t.Error("an entry whose post-run scan succeeded must carry an after-observation")
	}
}

func TestValidateUnknownPostStateForbidsAfter(t *testing.T) {
	e := validEntry(t)
	e.Reuse = runcache.UnknownPostState()
	e.Decision = runcache.CacheDecision{Cacheable: false, Reason: runcache.ReasonPostScanFailed.String()}
	e.Mutation = mustMutation(t, runcache.MutationScanFailed)
	// A failed post-run scan is the one and only run without an after-observation.
	if err := e.Validate(); err == nil {
		t.Error("an unknown-post-state entry carrying an after-observation must be rejected")
	}
	e.After = nil
	if err := e.Validate(); err != nil {
		t.Fatalf("a valid unknown-post-state entry was rejected: %v", err)
	}
}

// recordOnly turns the baseline valid entry into a record-only one carrying the
// given policy reason, with the cache decision kept in agreement so the test
// isolates the reason/entry-fact cross-check rather than tripping the
// reuse/decision invariant first.
func recordOnly(t *testing.T, reason runcache.MismatchReason) runcache.RunEntry {
	t.Helper()
	e := validEntry(t)
	st, err := runcache.RecordOnly(reason)
	if err != nil {
		t.Fatalf("RecordOnly(%q): %v", reason, err)
	}
	e.Reuse = st
	e.Decision = runcache.CacheDecision{Cacheable: false, Reason: reason.String()}
	return e
}

func TestValidateRejectsUnobservedMutation(t *testing.T) {
	// A record-only run that policy kept out of the cache still executed and was
	// observed, so an unobserved outcome is corrupt history even though no kind-
	// specific rule constrains the mutation of a record-only run.
	e := recordOnly(t, runcache.ReasonRecordOnly)
	e.Mutation = mustMutation(t, runcache.MutationUnobserved)
	if err := e.Validate(); err == nil {
		t.Error("a persisted run with an unobserved mutation outcome must be rejected")
	}
}

func TestValidateFailedNotCachedRequiresFailure(t *testing.T) {
	e := validEntry(t)
	nr, err := runcache.NonReusableRun(runcache.ReasonFailedNotCached)
	if err != nil {
		t.Fatal(err)
	}
	e.Reuse = nr
	e.Decision = runcache.CacheDecision{Cacheable: false, Reason: runcache.ReasonFailedNotCached.String()}
	// A successful exit contradicts a failed-not-cached reason.
	if err := e.Validate(); err == nil {
		t.Error("a failed-not-cached run with a successful exit must be rejected")
	}
	e.Exit = runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 1}
	if err := e.Validate(); err != nil {
		t.Errorf("a failed-not-cached run with a failed exit must validate: %v", err)
	}
}

func TestValidateRejectsCaptureDisabledRecord(t *testing.T) {
	// capture-disabled means nothing was captured to record, so a persisted record
	// can never honestly carry it.
	e := recordOnly(t, runcache.ReasonCaptureDisabled)
	if err := e.Validate(); err == nil {
		t.Error("a record-only entry carrying capture-disabled must be rejected")
	}
}

func TestValidateSkippedInputsReasonRequiresSkips(t *testing.T) {
	e := recordOnly(t, runcache.ReasonSkippedInputs)
	// No skipped inputs contradicts the reason.
	if err := e.Validate(); err == nil {
		t.Error("a skipped-inputs record with no skipped inputs must be rejected")
	}
	e.Skipped = runcache.SkippedSummary{
		Count:   1,
		Allowed: false,
		Samples: []runcache.SkippedSample{{Path: "secret", Reason: "unreadable"}},
	}
	if err := e.Validate(); err != nil {
		t.Errorf("a skipped-inputs record with unallowed skips must validate: %v", err)
	}
}

func TestValidateStdinNotKeyedReasonRequiresPipeOrTTY(t *testing.T) {
	e := recordOnly(t, runcache.ReasonStdinNotKeyed)
	// baseline keys StdinNull, which contradicts stdin-not-keyed.
	if err := e.Validate(); err == nil {
		t.Error("a stdin-not-keyed record with a keyed stdin mode must be rejected")
	}
	e.KeyInput.StdinMode = runcache.StdinPipe
	e.Key = e.KeyInput.Compute(newHasher(t))
	if err := e.Validate(); err != nil {
		t.Errorf("a stdin-not-keyed record with a piped stdin must validate: %v", err)
	}
}
