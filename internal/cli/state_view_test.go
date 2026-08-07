package cli

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

// TestAssessmentProjectionMatchesSentinels is the independent parity oracle for the
// awa-state/v1 projection. Rather than compare two projections of one projector (which
// would pass even if the projector mangled every field consistently), it fixes known
// sentinel values — run id, slot, tree hash, scan-config hash — builds a resolved
// assessment carrying exactly them, and asserts the projected document reproduces each
// sentinel. It then projects the same assessment through the run-evidence surface
// (evidenceDocOf's states.before) and asserts it against the same sentinels, so both the
// standalone `state resolve` shape and the nested evidence shape are pinned to independent
// truth, not merely to each other.
func TestAssessmentProjectionMatchesSentinels(t *testing.T) {
	const (
		treeSentinel = "blake3:" + "1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd"
		cfgSentinel  = "blake3:" + "9876fedc9876fedc9876fedc9876fedc9876fedc9876fedc9876fedc9876fedc"
	)
	runID, err := runcache.NewRunID(1, strings.NewReader("abcdefghijklmnop"))
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	treeHash, err := hashing.ParseTreeHash(treeSentinel)
	if err != nil {
		t.Fatalf("ParseTreeHash: %v", err)
	}
	scanConfig, err := hashing.ParseConfigHash(cfgSentinel)
	if err != nil {
		t.Fatalf("ParseConfigHash: %v", err)
	}
	scan, err := provider.NewScanIdentity(scanConfig)
	if err != nil {
		t.Fatalf("NewScanIdentity: %v", err)
	}
	bound, err := provider.NewEvidenceBoundary(0)
	if err != nil {
		t.Fatalf("NewEvidenceBoundary: %v", err)
	}
	id, err := provider.NewRunObservationIdentity(true, runID, treeHash, scan, time.Time{}, false, bound)
	if err != nil {
		t.Fatalf("NewRunObservationIdentity: %v", err)
	}
	wantRef := "run:" + runID.String() + ":before"
	assessment, err := provider.Resolved(wantRef, id)
	if err != nil {
		t.Fatalf("Resolved: %v", err)
	}

	// assertAgainstSentinels checks one projected state document against the known values.
	assertAgainstSentinels := func(t *testing.T, surface string, doc *stateProjectionDoc) {
		t.Helper()
		if doc == nil {
			t.Fatalf("%s: projected state is nil", surface)
		}
		if doc.RunID != runID.String() {
			t.Errorf("%s: run_id = %q, want sentinel %q", surface, doc.RunID, runID.String())
		}
		if doc.CanonicalRef != wantRef {
			t.Errorf("%s: canonical_ref = %q, want sentinel %q", surface, doc.CanonicalRef, wantRef)
		}
		if doc.Kind != "run-before" {
			t.Errorf("%s: kind = %q, want run-before", surface, doc.Kind)
		}
		if doc.TreeHash != treeSentinel {
			t.Errorf("%s: tree_hash = %q, want sentinel %q", surface, doc.TreeHash, treeSentinel)
		}
		if doc.ScanIdentity.ScanConfigHash != cfgSentinel {
			t.Errorf("%s: scan_config_hash = %q, want sentinel %q", surface, doc.ScanIdentity.ScanConfigHash, cfgSentinel)
		}
	}

	// Standalone `state resolve` surface.
	assertAgainstSentinels(t, "state resolve", assessmentDoc(assessment).State)

	// Nested run-evidence surface: states.before must pin to the same sentinels. Build a
	// minimal evidence aggregate whose before assessment is the sentinel one.
	ev := sentinelEvidence(t, runID, treeHash, scanConfig, assessment)
	doc := evidenceDocOf(ev, runevidence.IntegrityUnverified, runevidence.IntegrityUnverified)
	assertAgainstSentinels(t, "run evidence states.before", doc.States.Before.State)
}

// sentinelEvidence builds a run-evidence aggregate whose record persists a before
// observation matching the sentinel identity, so New accepts the sentinel resolved before
// assessment, and whose after is a normal unavailable assessment.
func sentinelEvidence(t *testing.T, runID runcache.RunID, treeHash hashing.TreeHash, scanConfig hashing.ConfigHash, before provider.Assessment) runevidence.Evidence {
	t.Helper()
	rec := sentinelRecord(t, runID, treeHash, scanConfig)
	after, err := provider.Unavailable("run:"+runID.String()+":after", provider.ReasonNotFound)
	if err != nil {
		t.Fatalf("Unavailable: %v", err)
	}
	insp, err := runevidence.NewOutputInspectability(runevidence.PresencePresent)
	if err != nil {
		t.Fatalf("NewOutputInspectability: %v", err)
	}
	ev, err := runevidence.New(rec, before, after, insp, insp)
	if err != nil {
		t.Fatalf("runevidence.New: %v", err)
	}
	return ev
}

// sentinelRecord builds a minimal valid run record whose keyed input tree hash matches
// the sentinel before observation it carries. Every recorded run observed its pre- and
// post-run state, so the observations are part of the record rather than bolted on.
func sentinelRecord(t *testing.T, runID runcache.RunID, treeHash hashing.TreeHash, scanConfig hashing.ConfigHash) runcache.RunEntry {
	t.Helper()
	cwd, err := runcache.NewExecutionCWD(".")
	if err != nil {
		t.Fatalf("NewExecutionCWD: %v", err)
	}
	mutation, err := runcache.NewMutationStatus(runcache.MutationUnchanged)
	if err != nil {
		t.Fatalf("NewMutationStatus: %v", err)
	}
	guard, err := runcache.NewEffectGuardStatus(runcache.EffectGuardUnchanged)
	if err != nil {
		t.Fatalf("NewEffectGuardStatus: %v", err)
	}
	h := blake3hash.New()
	effect, err := runcache.ObservedEffect(runcache.EffectHashFromTree(h.HashBytes([]byte("effect"))), 1)
	if err != nil {
		t.Fatalf("ObservedEffect: %v", err)
	}
	start := time.Unix(1000, 0)
	ki := runcache.KeyInput{
		CacheSchemaVersion: runcache.CacheSchemaVersion,
		AwaVersion:         "test",
		InvocationMode:     runcache.InvocationArgv,
		Command:            runcache.Command{Argv: []string{"go", "test"}, RawExecutable: "go"},
		CWD:                cwd,
		InputTreeHash:      treeHash,
		Effect:             effect,
		IncludeScope:       []string{"."},
		TrustMode:          config.TrustNormal,
		RunConfigHash:      hashing.ConfigHashFromTree(treeHash),
		Env:                runcache.NewEnvironment(nil),
		Platform:           runcache.Platform{GOOS: "linux", GOARCH: "amd64"},
		StdinMode:          runcache.StdinNull,
	}
	emptyHash, err := h.HashReader(strings.NewReader(""))
	if err != nil {
		t.Fatalf("HashReader: %v", err)
	}
	e := runcache.RunEntry{
		ID:          runID,
		Key:         ki.Compute(h),
		KeyInput:    ki,
		StartedAt:   start,
		FinishedAt:  start.Add(time.Second),
		Exit:        runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 0},
		Stdout:      runcache.OutputCapture{File: "stdout.log", Hash: emptyHash, TruncationPolicy: runcache.TruncationNone},
		Stderr:      runcache.OutputCapture{File: "stderr.log", Hash: emptyHash, TruncationPolicy: runcache.TruncationNone},
		Decision:    runcache.CacheDecision{Cacheable: true},
		Reuse:       runcache.ReusableCacheEntry(),
		Mutation:    mutation,
		EffectGuard: guard,
		Before: &runcache.Observation{
			Manifest:       runcache.ManifestRef{File: "before.manifest.jsonl", TreeHash: treeHash, RecordCount: 1},
			ScanConfigHash: scanConfig,
		},
		After: &runcache.Observation{
			Manifest:       runcache.ManifestRef{File: "after.manifest.jsonl", TreeHash: treeHash, RecordCount: 1},
			ScanConfigHash: scanConfig,
		},
	}
	if err := e.Validate(); err != nil {
		t.Fatalf("sentinelRecord invalid: %v", err)
	}
	return e
}
