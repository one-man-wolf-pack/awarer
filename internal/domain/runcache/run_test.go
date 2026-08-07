package runcache_test

import (
	"strings"
	"testing"
	"time"

	"awarer/internal/domain/runcache"
)

func TestRunIDTimeSortable(t *testing.T) {
	r := strings.NewReader("0123456789abcdef0123456789abcdef")
	a, err := runcache.NewRunID(100, r)
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	b, err := runcache.NewRunID(200, r)
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	if a.String() >= b.String() {
		t.Errorf("earlier id %s should sort before later id %s", a, b)
	}
	if a.IsZero() {
		t.Error("id should not be zero")
	}
}

func TestParseRunIDAndPrefix(t *testing.T) {
	r := strings.NewReader("abcdefghijklmnop") // 16 bytes
	id, err := runcache.NewRunID(1, r)
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	if _, err := runcache.ParseRunID(id.String()); err != nil {
		t.Errorf("ParseRunID round trip: %v", err)
	}
	if _, err := runcache.ParseRunID("short"); err == nil {
		t.Error("short id should be rejected")
	}
	if _, err := runcache.ParseRunID(strings.Repeat("z", 32)); err == nil {
		t.Error("non-hex id should be rejected")
	}
	if err := runcache.ValidateRunIDPrefix(""); err == nil {
		t.Error("empty prefix should be rejected")
	}
	if err := runcache.ValidateRunIDPrefix("zz"); err == nil {
		t.Error("non-hex prefix should be rejected")
	}
	if err := runcache.ValidateRunIDPrefix(id.String()[:6]); err != nil {
		t.Errorf("valid prefix rejected: %v", err)
	}
}

func TestExitStatusValidate(t *testing.T) {
	ok := []runcache.ExitStatus{
		{Kind: runcache.ExitNormal, Code: 0},
		{Kind: runcache.ExitNormal, Code: 3},
		{Kind: runcache.ExitSignaled, Code: 137, Signal: "killed"},
	}
	for _, s := range ok {
		if err := s.Validate(); err != nil {
			t.Errorf("valid exit %+v rejected: %v", s, err)
		}
	}
	bad := []runcache.ExitStatus{
		{Kind: runcache.ExitSignaled, Code: 137},                  // no signal name
		{Kind: runcache.ExitNormal, Code: 0, Signal: "killed"},    // signal on normal
		{Kind: runcache.ExitKind(9), Code: 0},                     // unknown kind
		{Kind: runcache.ExitNormal, Code: -1},                     // negative code
		{Kind: runcache.ExitSignaled, Code: 0, Signal: "killed"},  // signaled below 128+signal floor
		{Kind: runcache.ExitSignaled, Code: 64, Signal: "killed"}, // signaled below floor
	}
	for _, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("invalid exit %+v accepted", s)
		}
	}
}

func validEntry(t *testing.T) runcache.RunEntry {
	t.Helper()
	h := newHasher(t)
	stdoutHash, err := h.HashReader(strings.NewReader("abc"))
	if err != nil {
		t.Fatalf("HashReader: %v", err)
	}
	stderrHash, err := h.HashReader(strings.NewReader(""))
	if err != nil {
		t.Fatalf("HashReader: %v", err)
	}
	id, _ := runcache.NewRunID(1, strings.NewReader("abcdefghijklmnop"))
	ki := baseline(t, h)
	start := time.Unix(1000, 0)
	before := runcache.ManifestRef{File: "before.manifest.jsonl", TreeHash: ki.InputTreeHash, RecordCount: 0}
	after := before
	after.File = "after.manifest.jsonl"
	return runcache.RunEntry{
		ID:         id,
		Key:        ki.Compute(h),
		KeyInput:   ki,
		StartedAt:  start,
		FinishedAt: start.Add(time.Second),
		Exit:       runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 0},
		Stdout: runcache.OutputCapture{
			OriginalBytes:    3,
			StoredBytes:      3,
			TruncationPolicy: runcache.TruncationNone,
			Hash:             stdoutHash,
			File:             "stdout.log",
		},
		Stderr: runcache.OutputCapture{
			File:             "stderr.log",
			Hash:             stderrHash,
			TruncationPolicy: runcache.TruncationNone,
		},
		Decision: runcache.CacheDecision{Cacheable: true},
		Reuse:    runcache.ReusableCacheEntry(),
		// A reusable entry's command must have left observed state explicitly
		// unchanged, on both the input and the effect side, and a real execution
		// observed both states — so it carries a matching before/after pair.
		Mutation:    mustMutation(t, runcache.MutationUnchanged),
		EffectGuard: mustEffectGuard(t, runcache.EffectGuardUnchanged),
		Before:      &runcache.Observation{Manifest: before, ScanConfigHash: scanCfg(t)},
		After:       &runcache.Observation{Manifest: after, ScanConfigHash: scanCfg(t)},
	}
}

func TestRunEntryValidate(t *testing.T) {
	e := validEntry(t)
	if err := e.Validate(); err != nil {
		t.Fatalf("valid entry rejected: %v", err)
	}
	if e.DurationMs() != 1000 {
		t.Errorf("duration = %d, want 1000", e.DurationMs())
	}

	bad := e
	bad.FinishedAt = e.StartedAt.Add(-time.Second)
	if err := bad.Validate(); err == nil {
		t.Error("entry finishing before start should be rejected")
	}

	noID := e
	noID.ID = runcache.RunID{}
	if err := noID.Validate(); err == nil {
		t.Error("entry without id should be rejected")
	}

	cacheableWithReason := e
	cacheableWithReason.Decision = runcache.CacheDecision{Cacheable: true, Reason: "no-cache"}
	if err := cacheableWithReason.Validate(); err == nil {
		t.Error("cacheable entry carrying a reason should be rejected")
	}

	uncachedNoReason := e
	uncachedNoReason.Decision = runcache.CacheDecision{Cacheable: false, Reason: ""}
	if err := uncachedNoReason.Validate(); err == nil {
		t.Error("non-cacheable entry without a reason should be rejected")
	}

	// The baseline platform is linux, so an exit code above the 8-bit Unix range is
	// impossible and must be rejected.
	unixCodeTooBig := e
	unixCodeTooBig.Exit = runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 999}
	if err := unixCodeTooBig.Validate(); err == nil {
		t.Error("exit code above 255 on a Unix platform should be rejected")
	}
	// The same code is within range on Windows, whose exit codes are 32-bit.
	windowsCodeOK := e
	windowsCodeOK.KeyInput.Platform.GOOS = "windows"
	windowsCodeOK.Key = windowsCodeOK.KeyInput.Compute(newHasher(t))
	windowsCodeOK.Exit = runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 999}
	if err := windowsCodeOK.Validate(); err != nil {
		t.Errorf("exit code 999 on Windows rejected: %v", err)
	}

	// Self-consistent but impossible for a stored entry: the write path only ever
	// persists a cacheable result, so a non-cacheable (even well-formed) decision
	// must be rejected.
	storedNonCacheable := e
	storedNonCacheable.Decision = runcache.CacheDecision{Cacheable: false, Reason: "no-cache"}
	if err := storedNonCacheable.Validate(); err == nil {
		t.Error("stored entry marked non-cacheable should be rejected")
	}

	// Skipped.Allowed and KeyInput.AllowSkippedInputs are filled from one flag, so a
	// document where they disagree is impossible.
	skippedPolicyMismatch := e
	skippedPolicyMismatch.Skipped = runcache.SkippedSummary{Allowed: true}
	if err := skippedPolicyMismatch.Validate(); err == nil {
		t.Error("skipped-allowed disagreeing with key input allow-skipped-inputs should be rejected")
	}

	// Skipped inputs are only stored when allowed, so a stored entry with skipped
	// inputs that disallows them is impossible. (Set both policy flags false but a
	// positive count.)
	skippedButDisallowed := e
	skippedButDisallowed.Skipped = runcache.SkippedSummary{
		Count:   1,
		Samples: []runcache.SkippedSample{{Path: "a", Reason: "r"}},
	}
	if err := skippedButDisallowed.Validate(); err == nil {
		t.Error("stored entry with disallowed skipped inputs should be rejected")
	}

	moreSamplesThanCount := e
	moreSamplesThanCount.Skipped = runcache.SkippedSummary{
		Count:   1,
		Samples: []runcache.SkippedSample{{Path: "a", Reason: "r"}, {Path: "b", Reason: "r"}},
	}
	if err := moreSamplesThanCount.Validate(); err == nil {
		t.Error("skipped summary with more samples than count should be rejected")
	}

	blankSample := e
	blankSample.Skipped = runcache.SkippedSummary{
		Count:   1,
		Samples: []runcache.SkippedSample{{Path: "", Reason: "r"}},
	}
	if err := blankSample.Validate(); err == nil {
		t.Error("skipped sample without a path should be rejected")
	}
}
