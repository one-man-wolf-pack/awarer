package run

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"awarer/internal/domain/config"
	"awarer/internal/domain/perfdiag"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/worktree"
)

// Outcome is the primary, machine-readable result of an explain. It is a stable
// token an agent can switch on.
type Outcome string

const (
	// OutcomeHit means a healthy stored entry matches the current key exactly, so a
	// real run would replay it.
	OutcomeHit Outcome = "hit"
	// OutcomeMiss means the run is cacheable but no healthy exact entry exists, so a
	// real run would execute and (policy permitting) store a result.
	OutcomeMiss Outcome = "miss"
	// OutcomeUncached means policy forbids using the cache at all this invocation
	// (capture disabled, unkeyed stdin/tty, skipped inputs without the flag).
	OutcomeUncached Outcome = "uncached"
	// OutcomeDisabled means --no-cache turned the cache off for this invocation.
	OutcomeDisabled Outcome = "disabled"
	// OutcomeCorrupt means the entry that would be the answer is durably corrupt:
	// its metadata or payload failed validation. It is never hidden as a miss.
	OutcomeCorrupt Outcome = "corrupt"
)

// Reason tokens that are not key-input difference codes. Difference-based reasons
// reuse runcache.DiffCode values (e.g. "input-tree-changed").
const (
	reasonExactKey             = "exact-key"
	reasonExactEntryUnhealthy  = "exact-entry-unhealthy"
	reasonNoEntry              = "no-entry-for-key"
	reasonRefreshRequested     = "refresh-requested"
	reasonStoredRunCorrupt     = "stored-run-corrupt"
	reasonCachedFailureIgnored = "cached-failure-ignored"
	reasonTTLExpired           = "ttl-expired"
)

// ExplainMode selects which invocation explain reasons about. Exactly one mode is
// valid per request, so an impossible "command and stored run at once" request is
// unrepresentable.
type ExplainMode int

const (
	// ModeCommand explains a command given on the request, against the current
	// workspace and config.
	ModeCommand ExplainMode = iota
	// ModeLast explains the newest readable stored run as if re-invoked now.
	ModeLast
	// ModeFromRunToNow explains a stored run (by id/prefix) as if re-invoked now.
	ModeFromRunToNow
)

// ExplainRequest is one explain invocation. For ModeCommand, Request is fully
// populated (as for Run). For ModeLast and ModeFromRunToNow, Request carries only
// Project and Config; the command, cwd, stdin/tty, and skipped policy come from
// the stored run, and RunRef names that run (ignored for ModeLast).
type ExplainRequest struct {
	Mode    ExplainMode
	Request Request
	RunRef  string
}

// ExactHit is a stored entry whose key equals the current key. Healthy is the
// result of verifying both payloads; an unhealthy exact hit is reported as corrupt,
// never as a usable hit.
type ExactHit struct {
	RunID   runcache.RunID
	Entry   runcache.RunEntry
	Healthy bool
}

// ExplainSubject is the current invocation explain reasons about: its computed key
// and key input, whether it is cacheable, and the exact stored hit if one exists.
type ExplainSubject struct {
	Key       runcache.RunKey
	KeyInput  runcache.KeyInput
	Cacheable bool
	Skipped   runcache.SkippedSummary
	ExactHit  *ExactHit
}

// ExplainResult is the full, rendered-by-CLI explanation. Candidates are the shared
// RunReuseCandidate type run ls --near also uses, so both surfaces rank the same way
// and report the same mismatch reason.
type ExplainResult struct {
	Outcome     Outcome
	Reason      string
	Subject     ExplainSubject
	Differences runcache.KeyComparison
	Candidates  []RunReuseCandidate
	Warnings    []string
	// Performance carries the same bounded latency diagnostics a real run would report
	// for this invocation. Explain performs the identical input observation the hit path
	// performs — the whole point is that its answer cannot diverge from a run's — so it
	// pays the same cost and owes the same explanation for it.
	Performance []perfdiag.Diagnostic
}

// maxCandidates bounds the nearest matches reported, so explain stays cheap on a
// large run cache. The scan that feeds it is bounded separately, by
// explainCandidateWindow.
const maxCandidates = 5

// explainCandidateWindow bounds how many of the newest stored runs explain's
// near-miss ranking considers, mirroring run ls --near and inline diagnostics so all
// three near-miss surfaces select candidates from the same bounded newest window
// rather than decoding every run's metadata. Ranking is inherently a "best few
// recent" surface, so a bounded window is the honest contract here; latest-run
// lookups, which carry a "latest" contract, use the unbounded streaming
// IterRefsNewest instead so an old-but-valid run is still found.
const explainCandidateWindow = defaultLogLimit

// Explain reasons about a run-cache decision without executing the command or
// writing an entry. It computes the same key and cacheability the real run would,
// then reports the outcome, the typed differences, and the nearest stored
// candidates.
func (s *Service) Explain(ctx context.Context, req ExplainRequest) (ExplainResult, error) {
	switch req.Mode {
	case ModeCommand:
		return s.explainCommand(ctx, req.Request)
	case ModeLast:
		id, skipped, err := s.latestRunID(ctx)
		if err != nil {
			return ExplainResult{}, err
		}
		if id.IsZero() {
			return ExplainResult{}, fmt.Errorf("%w: no readable runs recorded", runcache.ErrNotFound)
		}
		return s.explainStored(ctx, req.Request, id, SkippedLatestWarnings(skipped))
	case ModeFromRunToNow:
		id, err := s.deps.Store.Resolve(ctx, req.RunRef)
		if err != nil {
			return ExplainResult{}, err
		}
		return s.explainStored(ctx, req.Request, id, nil)
	default:
		return ExplainResult{}, fmt.Errorf("explain: unknown mode %d", req.Mode)
	}
}

// verdict is what a real run would do with the current key: the outcome and reason,
// plus the entry the key points at (informational, even when the run would not use
// it). A plain miss — cacheable, no entry for the key — leaves Reason empty so the
// caller can explain why through candidates or a specific comparison.
type verdict struct {
	Outcome  Outcome
	Reason   string
	Exact    *ExactHit
	Warnings []string
}

// currentVerdict resolves what a real run would do with the current key, applying
// the same rules as Service.Run: the key-pointer Lookup, then --no-cache-failures,
// TTL expiry, and payload-health checks. It is the single source of explain's
// hit/miss/corrupt verdict, so explain can never claim a hit the run would not
// replay. It does not search for nearest candidates.
func (s *Service) currentVerdict(req Request, kc keyContext) (verdict, error) {
	var v verdict

	// Resolve the key pointer first, regardless of policy: even a run that would not
	// use the cache benefits an agent by reporting that a matching entry exists. The
	// lookup is authoritative only when a real run would read the cache (CanRead). A
	// non-cacheable or --refresh run never reads it, so there the lookup is purely
	// informational: any error becomes a warning rather than failing the diagnostic,
	// and the outcome stays policy-driven — exactly what a real run would do.
	authoritative := kc.CanRead
	exactCorrupt := false
	ce, ok, lookErr := s.deps.Store.Lookup(kc.Key)
	entry := ce.Entry()
	switch {
	case lookErr != nil:
		switch {
		case authoritative && errors.Is(lookErr, runcache.ErrCorruptStore):
			exactCorrupt = true
			v.Warnings = append(v.Warnings, "exact key entry is corrupt: "+lookErr.Error())
		case authoritative:
			return verdict{}, lookErr
		default:
			v.Warnings = append(v.Warnings, "could not read exact key entry: "+lookErr.Error())
		}
	case ok:
		healthy, herr := s.payloadHealth(entry)
		switch {
		case herr != nil && authoritative:
			return verdict{}, herr
		case herr != nil:
			v.Warnings = append(v.Warnings, "could not verify exact key entry payload: "+herr.Error())
		default:
			if !healthy {
				v.Warnings = append(v.Warnings, "exact key entry has an unhealthy payload")
			}
			v.Exact = &ExactHit{RunID: entry.ID, Entry: entry, Healthy: healthy}
		}
	}

	switch {
	case kc.Reason != "":
		// Policy forbids the cache entirely (capture/stdin/tty/skipped/no-cache).
		v.Outcome, v.Reason = policyOutcome(kc.Reason.String(), req.StdinMode)
	case req.Policy.Refresh:
		// Cacheable, but --refresh ignores any hit and re-executes.
		v.Outcome, v.Reason = OutcomeMiss, reasonRefreshRequested
	case exactCorrupt:
		v.Outcome, v.Reason = OutcomeCorrupt, reasonStoredRunCorrupt
	case v.Exact == nil:
		// No entry for the current key: a plain miss the caller explains.
		v.Outcome = OutcomeMiss
	case !v.Exact.Healthy:
		v.Outcome, v.Reason = OutcomeCorrupt, reasonExactEntryUnhealthy
	case req.Policy.NoCacheFailures && v.Exact.Entry.Exit.Failed():
		// Run re-executes a cached failure under --no-cache-failures rather than
		// replaying it, so explain reports a miss, not a hit.
		v.Outcome, v.Reason = OutcomeMiss, reasonCachedFailureIgnored
	case s.expired(v.Exact.Entry, kc.EffCfg.Run.TTL):
		// An entry past the TTL is stale: Run treats it as a miss and re-executes.
		v.Outcome, v.Reason = OutcomeMiss, reasonTTLExpired
	default:
		v.Outcome, v.Reason = OutcomeHit, reasonExactKey
	}
	return v, nil
}

// explainCommand explains a command-mode request against the current workspace.
func (s *Service) explainCommand(ctx context.Context, req Request) (ExplainResult, error) {
	if len(req.Argv) == 0 {
		return ExplainResult{}, fmt.Errorf("explain: empty command")
	}
	kc, err := s.buildKeyContext(ctx, req, false)
	if err != nil {
		return ExplainResult{}, err
	}
	defer func() { _ = kc.Close() }()
	v, err := s.currentVerdict(req, kc)
	if err != nil {
		return ExplainResult{}, err
	}

	res := ExplainResult{
		Subject: ExplainSubject{
			Key:       kc.Key,
			KeyInput:  kc.KeyInput,
			Cacheable: kc.Reason == "",
			Skipped:   kc.Skipped,
			ExactHit:  v.Exact,
		},
		Outcome:     v.Outcome,
		Reason:      v.Reason,
		Warnings:    v.Warnings,
		Performance: runPerformance([]inputReport{kc.InputReport}, kc.EffectReport),
	}

	// Only a plain "no entry for key" miss needs candidate discovery to explain why.
	if v.Outcome != OutcomeMiss || v.Reason != "" {
		return res, nil
	}
	cands, corrupt, err := s.nearestCandidates(ctx, kc.KeyInput, kc.BeforeManifest(), kc.EffectReport)
	if err != nil {
		return ExplainResult{}, err
	}
	res.Candidates = cands
	if corrupt > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d stored run(s) skipped due to corrupt metadata", corrupt))
	}
	if len(cands) == 0 {
		res.Reason = reasonNoEntry
	} else {
		res.Differences = cands[0].Comparison
		// Report the shared mismatch-reason token so explain and run ls --near name the
		// same reason for the same near miss.
		if cands[0].Reason != "" && !cands[0].Comparison.Equal() {
			res.Reason = cands[0].Reason.String()
		} else {
			res.Reason = reasonNoEntry
		}
	}
	return res, nil
}

// explainStored explains a stored run re-invoked now: it rebuilds the current key
// for the stored command/cwd/policy under the current config and compares the
// stored and current key inputs.
func (s *Service) explainStored(ctx context.Context, base Request, id runcache.RunID, warnings []string) (ExplainResult, error) {
	stored, err := s.deps.Store.Get(id)
	if err != nil {
		if errors.Is(err, runcache.ErrCorruptStore) {
			return ExplainResult{
				Outcome:  OutcomeCorrupt,
				Reason:   reasonStoredRunCorrupt,
				Warnings: append(warnings, err.Error()),
			}, nil
		}
		return ExplainResult{}, err
	}

	req, err := s.requestFromEntry(base, stored)
	if err != nil {
		return ExplainResult{}, err
	}
	kc, err := s.buildKeyContext(ctx, req, false)
	if err != nil {
		return ExplainResult{}, err
	}
	defer func() { _ = kc.Close() }()

	// The outcome reflects what a real run would do with the current key — the same
	// key-pointer lookup, TTL, and failure-policy rules as Run — never merely whether
	// the requested stored entry happens to match. An orphaned, expired, or repointed
	// entry must not be reported as a replayable hit.
	v, err := s.currentVerdict(req, kc)
	if err != nil {
		return ExplainResult{}, err
	}

	// The requested stored run is the subject of comparison, shown as the candidate
	// regardless of whether it is the entry the current key resolves to.
	cand := newCandidate(stored, kc.KeyInput)
	cmp := cand.Comparison
	healthy, herr := s.payloadHealth(stored)
	if herr != nil {
		return ExplainResult{}, herr
	}
	cand.Healthy = healthy
	cand.HealthChecked = true
	// The effect diagnosis is a projection of the observation already folded into
	// kc.KeyInput — the very key input this candidate was compared against — so an
	// effect-state miss names its root without a second walk of the watched roots.
	// Explain always re-keys, even a stored run recorded as non-reusable history, so its
	// reason is comparison-derived here where run ls --near would short-circuit.
	cand.attachEffect(kc.EffectReport, reasonFromComparison)
	// When the input tree is what differs, attach the same bounded changed-path sample
	// and advisory hints run ls --near shows, so the two surfaces read consistently.
	// Gate on the derived reason (not the raw diff code) so every near-miss surface
	// shares one classifier and none pays for a comparison a reason can't use.
	if runcache.ReasonCanSampleChangedPaths(cand.Reason) {
		sample, hints, serr := s.changedPathSample(ctx, stored.ID, kc.BeforeManifest())
		if serr != nil {
			return ExplainResult{}, serr
		}
		cand.Changed = &sample
		cand.Hints = hints
	}

	res := ExplainResult{
		Subject: ExplainSubject{
			Key:       kc.Key,
			KeyInput:  kc.KeyInput,
			Cacheable: kc.Reason == "",
			Skipped:   kc.Skipped,
			ExactHit:  v.Exact,
		},
		Outcome:     v.Outcome,
		Reason:      v.Reason,
		Differences: cmp,
		Candidates:  []RunReuseCandidate{cand},
		Warnings:    append(warnings, v.Warnings...),
		Performance: runPerformance([]inputReport{kc.InputReport}, kc.EffectReport),
	}

	// A plain miss (no entry for the current key) is explained by the requested run's
	// own differences rather than a separate candidate search, named with the shared
	// mismatch-reason token so it matches run ls --near.
	if v.Outcome == OutcomeMiss && v.Reason == "" {
		if cand.Reason != "" && !cmp.Equal() {
			res.Reason = cand.Reason.String()
		} else {
			res.Reason = reasonNoEntry
		}
	}
	return res, nil
}

// requestFromEntry rebuilds a run request from a stored entry's command, cwd,
// effective scope, and stdin/tty/skipped policy, under the caller's current project
// and config. The stored effective scope (include, exclude, ignore flags) is
// reproduced so a run recorded with --scope is explained against the same input set;
// the symlink policy and env are taken from the current config, so a genuine config
// change still surfaces in the comparison.
func (s *Service) requestFromEntry(base Request, entry runcache.RunEntry) (Request, error) {
	layout, err := base.Project.Paths()
	if err != nil {
		return Request{}, err
	}
	ki := entry.KeyInput
	cwd := ki.CWD
	absDir := filepath.Join(layout.Root(), filepath.FromSlash(cwd.String()))
	scope := config.ScanScope{
		Include:                append([]string(nil), ki.IncludeScope...),
		Exclude:                append([]string(nil), ki.ExcludeScope...),
		UseGitignore:           ki.UseGitignore,
		UseAwaignore:           ki.UseAwaignore,
		FollowSymlinks:         base.Config.Scope.FollowSymlinks,
		SymlinkMaxDepth:        base.Config.Scope.SymlinkMaxDepth,
		AllowSymlinkRootEscape: base.Config.Scope.AllowSymlinkRootEscape,
	}
	return Request{
		Project:        base.Project,
		Config:         base.Config,
		Argv:           append([]string(nil), ki.Command.Argv...),
		CWD:            cwd,
		AbsDir:         absDir,
		EffectiveScope: &scope,
		StdinMode:      ki.StdinMode,
		TTYAllowed:     ki.TTYAllowed,
		Policy:         Policy{AllowSkippedInputs: ki.AllowSkippedInputs},
	}, nil
}

// SkippedLatest reports the newer records a "--last" selection passed over before it
// reached a readable one. It exists because silence here is a lie of omission: the
// answer looks like "this is your newest run" while newer evidence sits in the store
// unread, and the two ways that happens need different responses — damage to
// investigate, or a schema this build cannot read, which the reset guidance covers.
//
// Counts are exact; the id samples are bounded, because a store whose newest records
// are all unreadable would otherwise turn one diagnostic into an unbounded dump.
type SkippedLatest struct {
	Corrupt      SkippedRuns
	Incompatible SkippedRuns
}

// SkippedRuns is an exact count with a bounded sample of the ids behind it.
type SkippedRuns struct {
	Count  int
	Sample []runcache.RunID
}

// Any reports whether anything was skipped.
func (s SkippedLatest) Any() bool { return s.Corrupt.Count > 0 || s.Incompatible.Count > 0 }

// maxSkippedLatestSample bounds the ids retained per reason. A handful is enough to
// act on; the exact count carries the scale.
const maxSkippedLatestSample = 5

func (s *SkippedRuns) record(id runcache.RunID) {
	s.Count++
	if len(s.Sample) < maxSkippedLatestSample {
		s.Sample = append(s.Sample, id)
	}
}

// latestMatchingRunID returns the newest stored run id accepted by readable. It has
// a "latest" contract, so it must not be narrowed to a bounded window: it streams run
// ids newest first (IterRefsNewest) and stops at the first accepted run, so an
// old-but-valid run is still found while only the accepted entry (and any unreadable
// ones ahead of it) is decoded and the whole id list is never materialized.
//
// An entry readable rejects because it will not decode — corrupt or in a schema this
// build cannot read — is skipped and REPORTED, so the caller can disclose that the
// returned run is not the newest stored one. A vanished entry (ErrNotFound) is
// skipped silently: nothing was passed over, it simply stopped existing. Any other
// error aborts the scan. A zero id means no accepted run exists.
func (s *Service) latestMatchingRunID(ctx context.Context, readable func(runcache.RunID) error) (runcache.RunID, SkippedLatest, error) {
	var (
		found   runcache.RunID
		skipped SkippedLatest
	)
	err := s.deps.Store.IterRefsNewest(ctx, func(id runcache.RunID) (bool, error) {
		if rerr := readable(id); rerr != nil {
			switch {
			case errors.Is(rerr, runcache.ErrIncompatibleEntry):
				skipped.Incompatible.record(id)
				return false, nil
			case errors.Is(rerr, runcache.ErrCorruptStore):
				skipped.Corrupt.record(id)
				return false, nil
			case errors.Is(rerr, runcache.ErrNotFound):
				return false, nil
			}
			return false, rerr
		}
		found = id
		return true, nil // stop at the first accepted (newest) run
	})
	if err != nil {
		return runcache.RunID{}, SkippedLatest{}, err
	}
	return found, skipped, nil
}

// latestRunID returns the newest run with readable metadata. It does not verify
// output payloads: explain reconstructs its facts from metadata and the key alone,
// so a payload-corrupt entry is still a usable explain target. "run show --last"
// (LatestRunID) is likewise metadata-only; its payload health is projected as typed
// inspectability, and explicit output selection performs the byte verification.
func (s *Service) latestRunID(ctx context.Context) (runcache.RunID, SkippedLatest, error) {
	return s.latestMatchingRunID(ctx, func(id runcache.RunID) error {
		_, err := s.deps.Store.Get(id)
		return err
	})
}

// nearestCandidates compares the newest stored runs against the current key input,
// ranks them deterministically, and returns up to maxCandidates with their payload
// health verified and — for an input-tree-differs miss — a bounded changed-path
// sample against the current manifest. Corrupt entries are counted and skipped rather
// than mistaken for "no match". Discovery is bounded to the newest
// explainCandidateWindow runs (ListRefsNewest), the same bounded window run ls --near
// and inline diagnostics use, so explain no longer decodes every run's metadata to
// pick the top few.
//
// effectReport is the diagnostic side channel of the observation already folded into
// current, so an effect-state candidate carries the root that observation named without
// re-walking the watched roots.
func (s *Service) nearestCandidates(ctx context.Context, current runcache.KeyInput, currentManifest worktree.ManifestStream, effectReport runcache.EffectReport) ([]RunReuseCandidate, int, error) {
	refs, err := s.deps.Store.ListRefsNewest(ctx, explainCandidateWindow)
	if err != nil {
		return nil, 0, err
	}
	corrupt := 0
	cands := make([]RunReuseCandidate, 0, len(refs))
	for _, id := range refs {
		entry, err := s.deps.Store.Get(id)
		if err != nil {
			if errors.Is(err, runcache.ErrCorruptStore) {
				corrupt++
				continue
			}
			if errors.Is(err, runcache.ErrNotFound) || errors.Is(err, runcache.ErrIncompatibleEntry) {
				// Vanished, or an incompatible entry: not a near-miss candidate; skip it.
				continue
			}
			return nil, 0, err
		}
		cands = append(cands, newCandidate(entry, current))
	}
	sortCandidates(cands)
	if len(cands) > maxCandidates {
		cands = cands[:maxCandidates]
	}
	for i := range cands {
		healthy, herr := s.payloadHealth(cands[i].Entry)
		if herr != nil {
			return nil, 0, herr
		}
		cands[i].Healthy = healthy
		cands[i].HealthChecked = true
		// Every candidate here came through newCandidate, which re-keys against current,
		// so the reason is comparison-derived and the observed root is evidence about it.
		cands[i].attachEffect(effectReport, reasonFromComparison)
		if currentManifest != nil && runcache.ReasonCanSampleChangedPaths(cands[i].Reason) {
			sample, hints, serr := s.changedPathSample(ctx, cands[i].Entry.ID, currentManifest)
			if serr != nil {
				return nil, 0, serr
			}
			cands[i].Changed = &sample
			cands[i].Hints = hints
		}
	}
	return cands, corrupt, nil
}

// payloadHealth reports whether both of an entry's stored payloads verify. A
// payload that fails verification is corruption, reported as unhealthy (false, nil)
// rather than an error so explain can present a degraded candidate honestly; a
// non-corruption I/O error is returned to the caller.
func (s *Service) payloadHealth(entry runcache.RunEntry) (bool, error) {
	if err := s.verifyPayloads(entry); err != nil {
		if errors.Is(err, runcache.ErrCorruptStore) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// policyOutcome maps a cacheability reason to the explain outcome and reason. An
// unkeyed stdin is reported as tty-not-keyed when the stdin is an inherited TTY, and
// stdin-not-keyed otherwise, so the explanation names the actual cause.
func policyOutcome(reason string, stdin runcache.StdinMode) (Outcome, string) {
	switch reason {
	case "no-cache":
		return OutcomeDisabled, "no-cache"
	case "capture-disabled":
		return OutcomeUncached, "capture-disabled"
	case "skipped-inputs":
		return OutcomeUncached, "skipped-inputs"
	case "stdin-not-keyed":
		if stdin == runcache.StdinTTY {
			return OutcomeUncached, "tty-not-keyed"
		}
		return OutcomeUncached, "stdin-not-keyed"
	default:
		return OutcomeUncached, reason
	}
}

// SkippedLatestWarnings renders the skipped-record facts as the human sentences the
// string warning channels carry. It is the single wording for both, so "run explain
// --last" and "run show --last" cannot describe the same store differently.
func SkippedLatestWarnings(s SkippedLatest) []string {
	var out []string
	if n := s.Corrupt.Count; n > 0 {
		out = append(out, fmt.Sprintf("skipped %d newer run(s) with corrupt metadata (%s)", n, shortIDList(s.Corrupt.Sample, n)))
	}
	if n := s.Incompatible.Count; n > 0 {
		out = append(out, fmt.Sprintf("skipped %d newer run(s) in an incompatible schema (%s); this is not the newest stored run", n, shortIDList(s.Incompatible.Sample, n)))
	}
	return out
}

// shortIDList renders a bounded id sample, saying so when it does not cover the count.
func shortIDList(ids []runcache.RunID, total int) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, id.Short())
	}
	joined := strings.Join(parts, ", ")
	if total > len(ids) {
		return joined + ", ..."
	}
	return joined
}
