package run

import (
	"context"
	"errors"

	"awarer/internal/domain/config"
	"awarer/internal/domain/runcache"
)

// inlineDiagnosisWindow bounds how far back DiagnoseRun looks for a nearest
// previous run. It reuses the run-ls candidate window so inline diagnosis stays in
// the same bounded scan as the deeper near-miss surfaces: a run must never trigger
// a full history scan just to annotate its footer.
const inlineDiagnosisWindow = defaultLogLimit

// RunInlineDiagnosis is the bounded, review-facing preview of why a run did not hit
// the cache, meant for the run footer and JSON. It is facts only — it names no
// command strings; the CLI turns Nearest into the actual `awa run explain ...` /
// `awa run ls --near` follow-ups. Nearest is the single most useful previous run
// considered against the current invocation, carrying its own shared mismatch
// reason, score, changed-path sample, and advisory hints. It is deliberately a
// preview, not the whole report: run explain and run ls --near remain the detailed
// tools.
type RunInlineDiagnosis struct {
	Nearest *RunReuseCandidate
}

// Available reports whether a useful nearest candidate was found. Deriving it from
// Nearest rather than storing a separate flag keeps the two from ever disagreeing.
func (d RunInlineDiagnosis) Available() bool { return d.Nearest != nil }

// DiagnoseRun computes the bounded inline cache diagnosis for a just-finished run.
// It answers the first "why did this not hit?" question by finding the nearest
// previous run within the bounded newest-window and, for an input-tree-differs
// miss, attaching the same bounded changed-path sample and advisory hints the
// deeper surfaces show.
//
// It reuses the shared near-miss model (newCandidate, sortCandidates,
// changedPathSample) rather than a parallel diagnosis pipeline, and it stays cheap:
// a cache hit needs no diagnosis and returns early, discovery is bounded to
// inlineDiagnosisWindow, and only the single surviving nearest candidate has its
// payload verified and a changed-path sample computed.
//
// A diagnosis is presentation, not correctness: a non-fatal failure to build it
// must never change what a run stored or its exit code, so the CLI treats an error
// here as "no diagnosis available", not a run failure.
//
// ttl is the run TTL from the invocation's effective config, so the nearest
// candidate's freshness is judged by the same rule the hit path uses.
func (s *Service) DiagnoseRun(ctx context.Context, res Result, ttl config.Duration) (RunInlineDiagnosis, error) {
	// A hit replayed a reusable entry; there is nothing to explain.
	if res.Status == runcache.CacheHit {
		return RunInlineDiagnosis{}, nil
	}

	refs, err := s.deps.Store.ListRefsNewest(ctx, inlineDiagnosisWindow)
	if err != nil {
		return RunInlineDiagnosis{}, err
	}

	current := res.Execution.KeyInput
	cands := make([]RunReuseCandidate, 0, len(refs))
	for _, id := range refs {
		// The current run just published an entry with this exact key; it would sort to
		// the top as an "all matched" self-match and drown the real near miss, so skip it.
		if !res.RunID.IsZero() && id == res.RunID {
			continue
		}
		entry, err := s.deps.Store.Get(id)
		if err != nil {
			if errors.Is(err, runcache.ErrCorruptStore) || errors.Is(err, runcache.ErrNotFound) || errors.Is(err, runcache.ErrIncompatibleEntry) {
				continue
			}
			return RunInlineDiagnosis{}, err
		}
		cands = append(cands, newCandidate(entry, current))
	}
	if len(cands) == 0 {
		return RunInlineDiagnosis{}, nil
	}
	sortCandidates(cands)
	nearest := cands[0]

	healthy, err := s.payloadHealth(nearest.Entry)
	if err != nil {
		return RunInlineDiagnosis{}, err
	}
	nearest.Healthy = healthy
	nearest.HealthChecked = true

	// newCandidate derived the reason and score purely from the key comparison, which
	// is only honest for a run that was actually re-keyed. A run recorded as
	// non-reusable history (record-only, mutating, post-scan-failed) was never a cache
	// candidate, so its comparison would fabricate a reason (e.g. config-mismatch on an
	// identical key) and a score claiming everything matched. Derive the honest reason
	// the same way Service.reusability does, and drop the score a non-reusable-kind
	// entry never earned.
	reason := inlineReason(nearest.Entry, nearest.Comparison, s.expired(nearest.Entry, ttl), healthy)
	if reason == "" {
		// A reusable entry whose key still matches, is unexpired, and verifies is an
		// anomaly here (an orphaned duplicate, or a cached failure skipped by policy) that
		// cannot be named without re-deriving policy. Prefer silence and the run explain
		// pointer to a guessed reason.
		return RunInlineDiagnosis{}, nil
	}
	nearest.Reason = reason
	origin := reasonFromComparison
	if nearest.Entry.Reuse.Kind() != runcache.ReuseReusable {
		nearest.Score = CandidateScore{}
		origin = reasonFromStoredVerdict
	}
	// Attach only now: inlineReason above replaces the comparison-derived reason for a
	// candidate that was never re-keyed, and the diagnosis is gated on the final reason.
	// The report is the pre-run observation's — the one folded into res.Execution.KeyInput,
	// which is exactly what a re-keyed candidate was compared against — so the root and
	// the reason describe one observation and no watched root is walked again here. A
	// stored verdict gets the same diagnosis without that root, exactly as it gets no score.
	nearest.attachEffect(res.effectReport, origin)

	// Attach the bounded changed-path sample only when the input tree is what we blame
	// and the current run recorded a pre-run manifest to compare against. An uncached
	// run (capture disabled) recorded nothing at all, so the sample is omitted rather
	// than invented.
	if runcache.ReasonCanSampleChangedPaths(nearest.Reason) && res.Recorded && !res.RunID.IsZero() {
		// The just-finished run's recorded pre-run manifest is this invocation's input
		// tree. It was published a moment ago and every published run carries that
		// observation, so a failure to open it is real trouble and fails the diagnosis
		// rather than degrading it into a missing sample.
		manifest, err := s.deps.Store.OpenBeforeManifest(res.RunID)
		if err != nil {
			return RunInlineDiagnosis{}, err
		}
		sample, hints, serr := s.changedPathSample(ctx, nearest.Entry.ID, manifest)
		if serr != nil {
			return RunInlineDiagnosis{}, serr
		}
		nearest.Changed = &sample
		nearest.Hints = hints
	}

	return RunInlineDiagnosis{Nearest: &nearest}, nil
}

// inlineReason names why the nearest candidate is not reusable, honestly and without
// a scan, mirroring the gate ordering in Service.reusability so the inline footer and
// run ls --near never name different reasons for the same entry: a run recorded as
// non-reusable history reports its stored verdict reason (it was never re-keyed); a
// reusable entry that drifted reports the key difference; a reusable entry whose key
// still matches reports expiry, then a corrupt payload, in that order. An empty result
// means the entry looks reusable here (an anomaly the caller resolves by staying
// silent) rather than a named miss.
func inlineReason(entry runcache.RunEntry, cmp runcache.KeyComparison, expired, healthy bool) runcache.MismatchReason {
	if entry.Reuse.Kind() != runcache.ReuseReusable {
		return entry.Reuse.Reason()
	}
	if !cmp.Equal() {
		return runcache.ReasonFromDiffCode(cmp.PrimaryReason())
	}
	if expired {
		return runcache.ReasonExpired
	}
	if !healthy {
		return runcache.ReasonPayloadMissing
	}
	return ""
}
