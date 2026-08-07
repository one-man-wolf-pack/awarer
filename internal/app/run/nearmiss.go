package run

import (
	"context"
	"errors"
	"slices"
	"sort"
	"time"

	"awarer/internal/domain/compare"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/worktree"
)

// changedPathSampleLimit bounds how many changed input paths a near-miss sample
// lists, so explaining an input-tree-differs miss never prints an unbounded diff.
const changedPathSampleLimit = 8

// maxArtifactHints bounds how many distinct generated-artifact hints a near miss
// raises, so a tree full of generated directories cannot grow the advisory set
// without limit while streaming the drift.
const maxArtifactHints = 4

// CandidateScore is the deterministic, typed score of a candidate against the
// current key input: which keyed categories matched and which differed, derived
// from the KeyComparison rather than filled by hand.
type CandidateScore struct {
	Matched   []string
	Different []string
}

// RunReuseCandidate is one stored run considered as a near match against a target
// invocation. It is the single candidate type shared by run explain and run ls
// --near, so both rank candidates the same way and name the same reason. Comparison
// is the typed key difference; Reason is the shared mismatch-reason token derived
// from it; HealthChecked reports whether Healthy was verified (the returned bounded
// set is verified, a wider scan is not); Changed is a bounded sample of the input
// paths that differ (only for an input-tree-differs miss); Hints are advisory,
// non-authoritative notes about likely generated artifacts among those paths; Effect
// is the same application EffectDiagnosis a just-executed run reports, present only
// for an effect-state miss (see attachEffect).
type RunReuseCandidate struct {
	Entry         runcache.RunEntry
	Comparison    runcache.KeyComparison
	Score         CandidateScore
	Reason        runcache.MismatchReason
	Healthy       bool
	HealthChecked bool
	Changed       *runcache.ChangedPathSample
	Hints         []runcache.AdvisoryHint
	Effect        *EffectDiagnosis
}

// reasonOrigin names where a candidate's mismatch reason came from. It is what decides
// whether the current observation's root is evidence about that candidate at all, so it
// travels with the report rather than being guessed from the entry: run ls --near and
// the inline footer short-circuit a stored non-reusable run to its recorded verdict,
// while run explain re-keys the very same entry and reaches a comparison-derived reason
// for it. The entry alone cannot tell those apart; the caller can.
type reasonOrigin int

// The conservative origin is the zero value on purpose: an unset or forgotten one then
// withholds the root rather than publishing today's observation as evidence about a run
// it says nothing about, so the honesty rule survives a future caller that ever leaves
// the value unfilled.
const (
	// reasonFromStoredVerdict: the reason is the stored run's own recorded non-reusable
	// verdict, reached without re-keying. It describes that past execution, so nothing
	// today's observation names is evidence about it.
	reasonFromStoredVerdict reasonOrigin = iota
	// reasonFromComparison: the reason was derived by comparing this candidate's stored
	// key input against the current one, so the observation folded into that current key
	// input is the same thing the reason is about.
	reasonFromComparison
)

// attachEffect fills the candidate's optional effect diagnosis from an effect report
// the caller already holds, and is the only way a candidate acquires one. It performs
// no observation, no scan, and no store read: the report is a value the current
// observation pass already returned, so enriching any number of candidates is free.
//
// Call it only once the candidate's Reason is final — effectDiagnosisFor gates on that
// reason, so attaching against a provisional one would decide by a reason the candidate
// does not end up carrying. A non-effect reason leaves Effect nil.
//
// report must be the diagnostic side channel of the current-state observation the
// calling surface is reporting against — the one whose identity went into the current
// key input. Under reasonFromComparison the reason and the root then come from one walk,
// so the root is kept. Under reasonFromStoredVerdict the reason is historical while the
// report describes today, so the root is dropped: naming today's dominant root next to a
// past run's verdict would read as causal evidence about that run. The reason, the
// sample fact, and the typed actions still apply, so the diagnosis itself survives. This
// mirrors the existing rule that a candidate which was never re-keyed carries no
// comparison score.
func (c *RunReuseCandidate) attachEffect(report runcache.EffectReport, origin reasonOrigin) {
	if origin == reasonFromStoredVerdict {
		// Drop the root by handing the rule an empty report rather than clearing the field
		// afterwards: "the observation named no root" is already the honest shape for an
		// absent one, so this cannot fall out of step with effectDiagnosisFor.
		report = runcache.EffectReport{}
	}
	c.Effect = effectDiagnosisFor(c.Reason, report)
}

// newCandidate builds a candidate from a stored entry and the current key input,
// deriving the typed comparison, score, and shared mismatch reason in one place so
// explain and ls --near cannot drift. Health and the changed-path sample are filled
// later, only for the bounded set actually returned.
func newCandidate(entry runcache.RunEntry, current runcache.KeyInput) RunReuseCandidate {
	cmp := runcache.CompareKeyInputs(entry.KeyInput, current)
	return RunReuseCandidate{
		Entry:      entry,
		Comparison: cmp,
		Score:      scoreFrom(cmp),
		Reason:     runcache.ReasonFromDiffCode(cmp.PrimaryReason()),
	}
}

// scoreCategory pairs a stable category name with the difference code that, when
// present, means the category differs.
type scoreCategory struct {
	name string
	code runcache.DiffCode
}

// scoreCategories is the ordered set of categories reported in a candidate score.
var scoreCategories = []scoreCategory{
	{"invocation", runcache.DiffInvocationChanged},
	{"command", runcache.DiffCommandChanged},
	{"executable", runcache.DiffExecutableChanged},
	{"cwd", runcache.DiffCWDChanged},
	{"config", runcache.DiffRunConfigChanged},
	{"scope", runcache.DiffScopeChanged},
	{"env", runcache.DiffEnvChanged},
	{"platform", runcache.DiffPlatformChanged},
	{"input_tree", runcache.DiffInputTreeChanged},
	{"trust", runcache.DiffTrustChanged},
	{"stdin", runcache.DiffStdinChanged},
	{"tty", runcache.DiffTTYChanged},
	{"skipped", runcache.DiffSkippedChanged},
}

// rankCategories is the priority order for ranking candidates: command, then
// executable, cwd, config, scope, env, platform, with the newest run as the final
// tie-breaker.
var rankCategories = []runcache.DiffCode{
	runcache.DiffCommandChanged,
	runcache.DiffExecutableChanged,
	runcache.DiffCWDChanged,
	runcache.DiffRunConfigChanged,
	runcache.DiffScopeChanged,
	runcache.DiffEnvChanged,
	runcache.DiffPlatformChanged,
}

// scoreFrom derives the typed matched/different category sets from a comparison.
func scoreFrom(cmp runcache.KeyComparison) CandidateScore {
	codes := cmp.Codes()
	var score CandidateScore
	for _, cat := range scoreCategories {
		if slices.Contains(codes, cat.code) {
			score.Different = append(score.Different, cat.name)
		} else {
			score.Matched = append(score.Matched, cat.name)
		}
	}
	return score
}

// sortCandidates orders candidates most-useful first. The primary key is the rank
// bucket: a stale-but-reusable-origin run (a real cache entry whose key drifted) is
// a causally useful near miss — it would replay if its inputs matched again — so it
// outranks a run policy or history disqualified from the start (record-only,
// mutating, unknown-post-state, failed). Only within a bucket does the category
// ranking apply — a candidate matching a higher-priority category outranks one that
// differs there — with the newer run as the final tie-break. Without the bucket, a
// fresh --record run (empty comparison, "all matched") would sort ahead of a useful
// input-tree-differs check purely by recency.
func sortCandidates(cands []RunReuseCandidate) {
	sort.SliceStable(cands, func(i, j int) bool {
		if bi, bj := rankBucket(cands[i]), rankBucket(cands[j]); bi != bj {
			return bi < bj
		}
		ci := cands[i].Comparison.Codes()
		cj := cands[j].Comparison.Codes()
		for _, code := range rankCategories {
			mi := !slices.Contains(ci, code)
			mj := !slices.Contains(cj, code)
			if mi != mj {
				return mi // matched (true) ranks before different (false)
			}
		}
		// Tie-break: the newer run first (run ids are time-prefixed and sortable).
		return cands[i].Entry.ID.String() > cands[j].Entry.ID.String()
	})
}

// rankBucket classifies a near-miss candidate by how useful it is to a reviewer:
// bucket 0 is a run that was a valid cache entry and merely drifted out of reuse
// (inputs/env/config/platform changed) — it would replay again if that drift
// reverted; bucket 1 is a run policy or an after-the-fact disqualification kept out
// of the cache (record-only, mutating, unknown-post-state, failed), which no input
// change can make reusable. Bucket 0 always ranks ahead of bucket 1.
func rankBucket(c RunReuseCandidate) int {
	if c.Entry.Reuse.Kind() == runcache.ReuseReusable {
		return 0
	}
	return 1
}

// changedPathSample builds a bounded sample of the input paths that differ between a
// stored run's recorded pre-run manifest and the current input manifest, plus any
// advisory hints those paths raise. It is meaningful only for an input-tree-differs
// miss; the caller passes the current scan's re-openable manifest. Every stored run
// carries a pre-run observation, so a failure to open it is real trouble: reporting
// it as an error keeps a corrupt or unreadable store visible, where degrading to an
// explicit unavailable sample would dress it as an outcome no current entry can
// legitimately reach.
func (s *Service) changedPathSample(ctx context.Context, id runcache.RunID, current worktree.ManifestStream) (runcache.ChangedPathSample, []runcache.AdvisoryHint, error) {
	before, err := s.deps.Store.OpenBeforeManifest(id)
	if err != nil {
		return runcache.ChangedPathSample{}, nil, err
	}
	leftCursor, err := before.Open(ctx)
	if err != nil {
		return runcache.ChangedPathSample{}, nil, err
	}
	rightCursor, err := current.Open(ctx)
	if err != nil {
		_ = leftCursor.Close()
		return runcache.ChangedPathSample{}, nil, err
	}
	// Stream the comparison rather than materialize a full ChangeSet: without rename
	// detection CompareStream is an O(1)-memory k=2 merge, so a huge input-tree drift
	// never holds millions of changes in memory just to sample a handful. CompareStream
	// owns and closes both cursors.
	cur, err := compare.CompareStream(leftCursor, rightCursor, compare.Options{})
	if err != nil {
		return runcache.ChangedPathSample{}, nil, err
	}
	defer func() { _ = cur.Close() }()

	total := 0
	paths := make([]runcache.ChangedPath, 0, changedPathSampleLimit)
	var hints []runcache.AdvisoryHint
	seenHint := map[string]bool{}
	for cur.Next() {
		ch := cur.Change()
		total++
		p := ch.PrimaryPath().String()
		if len(paths) < changedPathSampleLimit {
			paths = append(paths, runcache.ChangedPath{Path: p, Status: ch.Status.Code()})
		}
		// Classify for hints beyond the sampled prefix so a generated artifact past the
		// bound is still surfaced, but keep the hint set itself bounded (deduped by
		// suggestion and capped) so a pathological tree cannot grow it without limit.
		if len(hints) < maxArtifactHints {
			if hint, ok := runcache.ClassifyGeneratedArtifact(p); ok && !seenHint[hint.Suggestion] {
				seenHint[hint.Suggestion] = true
				hints = append(hints, hint)
			}
		}
	}
	if err := cur.Err(); err != nil {
		return runcache.ChangedPathSample{}, nil, err
	}
	sample, err := runcache.NewChangedPathSample(paths, total)
	if err != nil {
		return runcache.ChangedPathSample{}, nil, err
	}
	return sample, hints, nil
}

// RunStorageState is the compact, machine-stable summary of what one run invocation
// did with durable storage: replayed a reusable entry, executed and published a
// reusable entry, executed and recorded non-reusable history, or executed without
// recording anything. It is what a review footer leads with.
type RunStorageState int

const (
	// RunStateHit: a stored reusable entry was replayed; the command did not execute.
	RunStateHit RunStorageState = iota
	// RunStateStored: the command executed and a reusable cache entry was published.
	RunStateStored
	// RunStateRecordOnly: the command executed and was recorded as durable history but
	// is not reusable (record-only, mutating, unknown-post-state, or failed-not-cached).
	RunStateRecordOnly
	// RunStateUncached: the command executed but nothing durable was recorded (capture
	// disabled), so there is no run to inspect.
	RunStateUncached
)

// String returns a stable token for the state.
func (s RunStorageState) String() string {
	switch s {
	case RunStateHit:
		return "hit"
	case RunStateStored:
		return "stored"
	case RunStateRecordOnly:
		return "record-only"
	case RunStateUncached:
		return "uncached"
	default:
		return "unknown"
	}
}

// RunExecutionSummary is the review-facing summary of one run invocation: its
// identity, where and how it ran, and what became of it in the cache. It is built
// once from a Result through NewRunExecutionSummary, whose validation makes the
// contradictory shapes unconstructable — a hit that did not record, a reusable run
// that mutated state, a record-only run that published a reusable pointer — so a
// rendered footer can never claim an impossible combination. The CLI turns these
// facts into the actual `awa ...` inspection commands; the summary itself names no
// command strings, keeping the domain fact separate from its presentation.
type RunExecutionSummary struct {
	State    RunStorageState
	RunID    runcache.RunID
	CWD      string
	Exit     runcache.ExitStatus
	Duration time.Duration
	// Reusable reports whether the resulting (or replayed) entry is a valid cache-hit
	// candidate. Reason names why not when it is not reusable.
	Reusable bool
	Reason   runcache.MismatchReason
	// Recorded reports whether a durable run record exists (inspectable via run show).
	Recorded bool
	// Mutated reports whether this execution changed observed state.
	Mutated bool
	// prePostAvailable reports whether this execution captured both a pre- and a
	// post-run observation, so a before/after compare command is meaningful.
	prePostAvailable bool
}

// NewRunExecutionSummary projects a run Result into the review-facing summary,
// rejecting the impossible combinations so an illegal footer state cannot be
// constructed. It derives the storage state from the result's cache status and
// reuse classification.
func NewRunExecutionSummary(res Result) (RunExecutionSummary, error) {
	reuse := res.Reuse
	mutated := res.Execution.Mutation.Changed()
	observed := res.Execution.Mutation.Outcome() == runcache.MutationChanged ||
		res.Execution.Mutation.Outcome() == runcache.MutationUnchanged

	var state RunStorageState
	switch {
	case res.Status == runcache.CacheHit:
		state = RunStateHit
	case res.Written:
		state = RunStateStored
	case res.Recorded:
		state = RunStateRecordOnly
	default:
		state = RunStateUncached
	}

	sum := RunExecutionSummary{
		State:            state,
		RunID:            res.RunID,
		CWD:              res.Execution.KeyInput.CWD.String(),
		Exit:             res.Execution.Exit,
		Duration:         res.Execution.FinishedAt.Sub(res.Execution.StartedAt),
		Reusable:         reuse.IsReusable(),
		Reason:           reuse.Reason(),
		Recorded:         res.Recorded,
		Mutated:          mutated,
		prePostAvailable: res.Recorded && res.Status != runcache.CacheHit && observed,
	}

	// A cache hit must be a replay of a recorded, reusable entry.
	if state == RunStateHit && (!sum.Recorded || !sum.Reusable) {
		return RunExecutionSummary{}, errors.New("run execution summary: a hit must replay a recorded reusable entry")
	}
	// A published reusable entry cannot have mutated observed state.
	if sum.Reusable && sum.Mutated {
		return RunExecutionSummary{}, errors.New("run execution summary: a reusable run cannot have mutated state")
	}
	// A record-only / non-reusable / uncached execution never published a reusable
	// pointer, so it cannot be a hit, and its entry (if any) is not reusable.
	if state == RunStateRecordOnly && sum.Reusable {
		return RunExecutionSummary{}, errors.New("run execution summary: a record-only run cannot be reusable")
	}
	return sum, nil
}

// PrePostCompareAvailable reports whether this execution recorded both observations,
// so a `awa changes run:<id>:before..run:<id>:after` compare is meaningful.
func (s RunExecutionSummary) PrePostCompareAvailable() bool {
	return s.prePostAvailable && !s.RunID.IsZero()
}
