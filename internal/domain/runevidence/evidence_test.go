package runevidence_test

import (
	"strings"
	"testing"
	"time"

	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/provider"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/runevidence"
	"awarer/internal/infra/blake3hash"
)

// Mutation proofs:
//   - drop the before/after InputRef checks in New -> TestNewRejectsSwappedBeforeAfter
//     and TestNewRejectsMismatchedRun go red.
//   - drop the resolved-identity check in bindsToSlot -> TestNewRejectsResolvedIdentityMismatch
//     goes red (the input ref alone still matches).
//   - drop the persisted tree/scan-config match in bindsToSlot ->
//     TestNewRejectsResolvedDriftFromPersisted goes red.
//   - drop the persisted!=nil check in bindsToSlot ->
//     TestNewRejectsResolvedSlotWithoutPersistedObservation goes red.
//   - drop record.Validate() in New -> TestNewRejectsInvalidRecord goes red. That call
//     also owns observation cardinality: a record missing its before, or carrying the
//     wrong after for its scan outcome, is rejected there rather than by a duplicate
//     guard here (internal/domain/runcache owns those mutation proofs).
//   - accept a zero-value assessment (empty InputRef) -> TestNewRejectsAbsentAssessment
//     goes red (the wantBefore/wantAfter refs never equal "").
//   - drop the stdout/stderr Valid() checks in New -> TestNewRejectsUndefinedInspectability
//     goes red (a zero-value OutputInspectability would slip through as "present").

func mustInspect(t *testing.T, p runevidence.Presence) runevidence.OutputInspectability {
	t.Helper()
	o, err := runevidence.NewOutputInspectability(p)
	if err != nil {
		t.Fatalf("NewOutputInspectability: %v", err)
	}
	return o
}

// validRecord builds a valid reusable run record: an unchanged run that observed both
// its states, with the before/after observations carrying the canonical
// obsTreeSeed/obsCfgSeed identity a matching resolvedFor assessment derives from. The
// state assessments themselves are supplied separately.
func validRecord(t *testing.T) runcache.RunEntry {
	t.Helper()
	h := blake3hash.New()
	stdoutHash, err := h.HashReader(strings.NewReader("abc"))
	if err != nil {
		t.Fatalf("HashReader: %v", err)
	}
	stderrHash, err := h.HashReader(strings.NewReader(""))
	if err != nil {
		t.Fatalf("HashReader: %v", err)
	}
	id, err := runcache.NewRunID(1, strings.NewReader("abcdefghijklmnop"))
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	cwd, err := runcache.NewExecutionCWD(".")
	if err != nil {
		t.Fatalf("NewExecutionCWD: %v", err)
	}
	ki := runcache.KeyInput{
		CacheSchemaVersion: runcache.CacheSchemaVersion,
		AwaVersion:         "test",
		InvocationMode:     runcache.InvocationArgv,
		Command:            runcache.Command{Argv: []string{"go", "test"}, RawExecutable: "go"},
		CWD:                cwd,
		InputTreeHash:      h.HashBytes([]byte(obsTreeSeed)),
		Effect:             observedEffect(t, h),
		IncludeScope:       []string{"."},
		TrustMode:          config.TrustNormal,
		RunConfigHash:      hashing.ConfigHashFromTree(h.HashBytes([]byte("cfg"))),
		Env:                runcache.NewEnvironment(nil),
		Platform:           runcache.Platform{GOOS: "linux", GOARCH: "amd64"},
		StdinMode:          runcache.StdinNull,
	}
	mutation, err := runcache.NewMutationStatus(runcache.MutationUnchanged)
	if err != nil {
		t.Fatalf("NewMutationStatus: %v", err)
	}
	guard, err := runcache.NewEffectGuardStatus(runcache.EffectGuardUnchanged)
	if err != nil {
		t.Fatalf("NewEffectGuardStatus: %v", err)
	}
	before := observationOf(t, "before.manifest.jsonl", obsTreeSeed, obsCfgSeed)
	// Unchanged run: the after tree equals the before tree, matching the record's
	// unchanged mutation outcome and its single immutable scan-config policy.
	after := observationOf(t, "after.manifest.jsonl", obsTreeSeed, obsCfgSeed)
	start := time.Unix(1000, 0)
	e := runcache.RunEntry{
		ID:         id,
		Key:        ki.Compute(h),
		KeyInput:   ki,
		StartedAt:  start,
		FinishedAt: start.Add(time.Second),
		Exit:       runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 0},
		Stdout: runcache.OutputCapture{
			OriginalBytes: 3, StoredBytes: 3, TruncationPolicy: runcache.TruncationNone,
			Hash: stdoutHash, File: "stdout.log",
		},
		Stderr:      runcache.OutputCapture{File: "stderr.log", Hash: stderrHash, TruncationPolicy: runcache.TruncationNone},
		Decision:    runcache.CacheDecision{Cacheable: true},
		Reuse:       runcache.ReusableCacheEntry(),
		Mutation:    mutation,
		EffectGuard: guard,
		Before:      &before,
		After:       &after,
	}
	if err := e.Validate(); err != nil {
		t.Fatalf("validRecord is invalid: %v", err)
	}
	return e
}

// observedEffect is the effect identity every execution keys on: production always
// observes the non-empty built-in watch set.
func observedEffect(t *testing.T, h hashing.Hasher) runcache.EffectObservation {
	t.Helper()
	o, err := runcache.ObservedEffect(runcache.EffectHashFromTree(h.HashBytes([]byte("effect"))), 1)
	if err != nil {
		t.Fatalf("ObservedEffect: %v", err)
	}
	return o
}

// assessmentFor builds a (typed-unavailable) assessment for one of a run's
// observation references. The aggregate only checks the input reference, so an
// unavailable assessment is enough to exercise its guards.
func assessmentFor(t *testing.T, id runcache.RunID, before bool) provider.Assessment {
	t.Helper()
	sel := "after"
	if before {
		sel = "before"
	}
	a, err := provider.Unavailable("run:"+id.String()+":"+sel, provider.ReasonNotFound)
	if err != nil {
		t.Fatalf("Unavailable: %v", err)
	}
	return a
}

// obsTreeSeed and obsCfgSeed seed the tree hash and scan-config hash that both the
// persisted observations (validRecord) and a matching resolved assessment
// (resolvedFor with the same seeds) derive from, so a "good" resolved pair agrees with
// the record while a drifted seed models an assessment that disagrees with the durable
// observation.
const (
	obsTreeSeed = "tree"
	obsCfgSeed  = "cfg"
)

// observationOf builds a persisted observation from a tree/scan-config seed pair. file
// distinguishes the before/after manifests; only the tree hash and scan-config identity
// participate in the aggregate's cross-check.
func observationOf(t *testing.T, file, treeSeed, cfgSeed string) runcache.Observation {
	t.Helper()
	h := blake3hash.New()
	return runcache.Observation{
		Manifest: runcache.ManifestRef{
			File:        file,
			TreeHash:    h.HashBytes([]byte(treeSeed)),
			RecordCount: 1,
		},
		ScanConfigHash: hashing.ConfigHashFromTree(h.HashBytes([]byte(cfgSeed))),
	}
}

// recordUnknownPostState builds a valid record whose post-execution scan failed: a before
// observation but no after, marked unknown-post-state. It is the one shape in which a
// missing after observation is legitimate.
func recordUnknownPostState(t *testing.T) runcache.RunEntry {
	t.Helper()
	e := validRecord(t)
	e.After = nil
	e.Reuse = runcache.UnknownPostState()
	// An unknown-post-state record is internally consistent only when its mutation outcome
	// is scan-failed and it is not a cache-hit candidate.
	mut, err := runcache.NewMutationStatus(runcache.MutationScanFailed)
	if err != nil {
		t.Fatalf("NewMutationStatus: %v", err)
	}
	e.Mutation = mut
	e.Decision = runcache.CacheDecision{Cacheable: false, Reason: runcache.ReasonPostScanFailed.String()}
	if err := e.Validate(); err != nil {
		t.Fatalf("recordUnknownPostState invalid: %v", err)
	}
	return e
}

// resolvedFor builds a resolved observed-state assessment whose echoed input reference is
// slotRef and whose resolved identity names identityID's before/after slot, carrying the
// tree/scan-config identity derived from treeSeed/cfgSeed. Matching seeds and ids agree
// with a validRecord record; a different id, slot, or seed models the exact
// shapes (wrong run, wrong slot, drifted tree, drifted scan-config) the aggregate rejects.
func resolvedFor(t *testing.T, slotRef string, identityID runcache.RunID, before bool, treeSeed, cfgSeed string) provider.Assessment {
	t.Helper()
	h := blake3hash.New()
	scan, err := provider.NewScanIdentity(hashing.ConfigHashFromTree(h.HashBytes([]byte(cfgSeed))))
	if err != nil {
		t.Fatalf("NewScanIdentity: %v", err)
	}
	bound, err := provider.NewEvidenceBoundary(0)
	if err != nil {
		t.Fatalf("NewEvidenceBoundary: %v", err)
	}
	id, err := provider.NewRunObservationIdentity(before, identityID, h.HashBytes([]byte(treeSeed)), scan, time.Time{}, false, bound)
	if err != nil {
		t.Fatalf("NewRunObservationIdentity: %v", err)
	}
	a, err := provider.Resolved(slotRef, id)
	if err != nil {
		t.Fatalf("Resolved: %v", err)
	}
	return a
}

func TestNewComposesEvidence(t *testing.T) {
	// A fully-observed record whose before/after assessments are the honest unavailable
	// degradation of persisted observations that were since GC-removed: the record still
	// records that it observed both states, so this is a valid evidence document.
	rec := validRecord(t)
	before := assessmentFor(t, rec.ID, true)
	after := assessmentFor(t, rec.ID, false)
	ev, err := runevidence.New(rec, before, after, mustInspect(t, runevidence.PresencePresent), mustInspect(t, runevidence.PresenceMissing))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if ev.Contract() != runevidence.ContractEvidenceV1 || !ev.Contract().Valid() {
		t.Errorf("Contract() = %q", ev.Contract())
	}
	if ev.Record().ID != rec.ID {
		t.Error("Record() did not round-trip the run id")
	}
	if ev.Before().InputRef() != "run:"+rec.ID.String()+":before" {
		t.Errorf("Before().InputRef() = %q", ev.Before().InputRef())
	}
	if ev.After().InputRef() != "run:"+rec.ID.String()+":after" {
		t.Errorf("After().InputRef() = %q", ev.After().InputRef())
	}
	if ev.Stdout().Presence() != runevidence.PresencePresent {
		t.Error("stdout inspectability not preserved")
	}
	if ev.Stderr().Presence() != runevidence.PresenceMissing {
		t.Error("stderr inspectability not preserved")
	}
}

func TestNewRejectsSwappedBeforeAfter(t *testing.T) {
	rec := validRecord(t)
	before := assessmentFor(t, rec.ID, true)
	after := assessmentFor(t, rec.ID, false)
	// Swap the before/after assessments: their input references no longer match the
	// before/after slots, so the aggregate rejects the composition.
	if _, err := runevidence.New(rec, after, before, mustInspect(t, runevidence.PresencePresent), mustInspect(t, runevidence.PresencePresent)); err == nil {
		t.Error("New must reject before/after assessments that are swapped")
	}
}

func TestNewRejectsMismatchedRun(t *testing.T) {
	rec := validRecord(t)
	otherID, err := runcache.NewRunID(2, strings.NewReader("ponmlkjihgfedcba"))
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	// A before assessment that belongs to a different run must not pair with this record.
	before := assessmentFor(t, otherID, true)
	after := assessmentFor(t, rec.ID, false)
	if _, err := runevidence.New(rec, before, after, mustInspect(t, runevidence.PresencePresent), mustInspect(t, runevidence.PresencePresent)); err == nil {
		t.Error("New must reject a before assessment from a different run")
	}
}

func TestNewRejectsAbsentAssessment(t *testing.T) {
	rec := validRecord(t)
	after := assessmentFor(t, rec.ID, false)
	// A zero-value assessment (no input reference) is an absent observation assessment;
	// neither slot may be absent.
	if _, err := runevidence.New(rec, provider.Assessment{}, after, mustInspect(t, runevidence.PresencePresent), mustInspect(t, runevidence.PresencePresent)); err == nil {
		t.Error("New must reject an absent before assessment")
	}
	before := assessmentFor(t, rec.ID, true)
	if _, err := runevidence.New(rec, before, provider.Assessment{}, mustInspect(t, runevidence.PresencePresent), mustInspect(t, runevidence.PresencePresent)); err == nil {
		t.Error("New must reject an absent after assessment")
	}
}

func TestNewRejectsResolvedIdentityMismatch(t *testing.T) {
	rec := validRecord(t)
	otherID, err := runcache.NewRunID(2, strings.NewReader("ponmlkjihgfedcba"))
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	// A resolved before assessment whose echoed input ref is this run's before slot but
	// whose resolved identity names a different run: the input-ref check alone passes, so
	// only the identity cross-check catches this impossible evidence document.
	before := resolvedFor(t, "run:"+rec.ID.String()+":before", otherID, true, obsTreeSeed, obsCfgSeed)
	after := assessmentFor(t, rec.ID, false)
	if _, err := runevidence.New(rec, before, after, mustInspect(t, runevidence.PresencePresent), mustInspect(t, runevidence.PresencePresent)); err == nil {
		t.Error("New must reject a resolved before assessment whose identity names a different run")
	}

	// A resolved assessment whose identity agrees with its slot and the persisted
	// observation is accepted: the guard rejects only genuine disagreement.
	good := resolvedFor(t, "run:"+rec.ID.String()+":before", rec.ID, true, obsTreeSeed, obsCfgSeed)
	if _, err := runevidence.New(rec, good, after, mustInspect(t, runevidence.PresencePresent), mustInspect(t, runevidence.PresenceMissing)); err != nil {
		t.Errorf("New must accept a resolved before assessment whose identity matches its slot: %v", err)
	}
}

func TestNewRejectsResolvedDriftFromPersisted(t *testing.T) {
	rec := validRecord(t)
	after := assessmentFor(t, rec.ID, false)
	slot := "run:" + rec.ID.String() + ":before"

	// A resolved before assessment whose tree hash drifted from the persisted before
	// observation is an impossible mixed-evidence document: right run, right slot, wrong
	// observed state (as if the durable record changed between the two reads).
	treeDrift := resolvedFor(t, slot, rec.ID, true, "drifted-tree", obsCfgSeed)
	if _, err := runevidence.New(rec, treeDrift, after, mustInspect(t, runevidence.PresencePresent), mustInspect(t, runevidence.PresencePresent)); err == nil {
		t.Error("New must reject a resolved before assessment whose tree hash drifted from the persisted observation")
	}

	// Same for a drifted scan-config policy identity.
	cfgDrift := resolvedFor(t, slot, rec.ID, true, obsTreeSeed, "drifted-cfg")
	if _, err := runevidence.New(rec, cfgDrift, after, mustInspect(t, runevidence.PresencePresent), mustInspect(t, runevidence.PresencePresent)); err == nil {
		t.Error("New must reject a resolved before assessment whose scan-config drifted from the persisted observation")
	}
}

func TestNewRejectsResolvedSlotWithoutPersistedObservation(t *testing.T) {
	// A post-scan-failed (unknown-post-state) run legitimately has no after observation.
	// A resolved after assessment claims a slot the record never recorded, so it is
	// rejected; the honest degradation is an unavailable after (TestNewAcceptsUnknownPostState).
	rec := recordUnknownPostState(t)
	before := resolvedFor(t, "run:"+rec.ID.String()+":before", rec.ID, true, obsTreeSeed, obsCfgSeed)
	after := resolvedFor(t, "run:"+rec.ID.String()+":after", rec.ID, false, obsTreeSeed, obsCfgSeed)
	if _, err := runevidence.New(rec, before, after, mustInspect(t, runevidence.PresencePresent), mustInspect(t, runevidence.PresenceMissing)); err == nil {
		t.Error("New must reject a resolved after assessment when the record persisted no after observation")
	}
}

func TestNewAcceptsUnknownPostState(t *testing.T) {
	// A post-scan-failed run has a before observation but legitimately no after; its after
	// assessment is the honest unavailable degradation. This is a valid evidence document.
	rec := recordUnknownPostState(t)
	before := assessmentFor(t, rec.ID, true)
	after := assessmentFor(t, rec.ID, false)
	if _, err := runevidence.New(rec, before, after, mustInspect(t, runevidence.PresencePresent), mustInspect(t, runevidence.PresenceMissing)); err != nil {
		t.Errorf("New must accept an unknown-post-state record with no after observation: %v", err)
	}
}

func TestNewRejectsUndefinedInspectability(t *testing.T) {
	rec := validRecord(t)
	before := assessmentFor(t, rec.ID, true)
	after := assessmentFor(t, rec.ID, false)
	// A zero-value OutputInspectability is undefined (its presence is not present/missing/
	// unreadable), and must never slip through as a false "present".
	if _, err := runevidence.New(rec, before, after, runevidence.OutputInspectability{}, mustInspect(t, runevidence.PresencePresent)); err == nil {
		t.Error("New must reject an undefined stdout inspectability")
	}
	if _, err := runevidence.New(rec, before, after, mustInspect(t, runevidence.PresencePresent), runevidence.OutputInspectability{}); err == nil {
		t.Error("New must reject an undefined stderr inspectability")
	}
}

func TestNewRejectsInvalidRecord(t *testing.T) {
	rec := validRecord(t)
	before := assessmentFor(t, rec.ID, true)
	after := assessmentFor(t, rec.ID, false)
	bad := rec
	bad.Exit = runcache.ExitStatus{Kind: runcache.ExitNormal, Code: -1} // invalid exit code
	if _, err := runevidence.New(bad, before, after, mustInspect(t, runevidence.PresencePresent), mustInspect(t, runevidence.PresencePresent)); err == nil {
		t.Error("New must reject an invalid run record")
	}
}
