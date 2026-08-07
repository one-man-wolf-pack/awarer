// Package gitfresh classifies a selected checkpoint baseline against the current
// git HEAD, and carries the review-coverage honesty note that an awa delta is not a
// whole-repo git review.
//
// It exists so review surfaces (status, changes, diff) can say honestly whether the
// baseline they compared against is the current commit, an older commit, a diverged
// commit, or something that can no longer be related to HEAD — without ever
// pretending an empty checkpoint delta means the whole uncommitted worktree was
// reviewed.
//
// The package is deliberately pure: it performs no git I/O and imports nothing from
// the checkpoint or infra layers (only stdlib time). The caller runs git, hands the
// facts in as an Input, and gets a stable Freshness token back. Every freshness
// outcome is a calm note, never a warning: a baseline predating HEAD is context for a
// long review, not a failure.
package gitfresh

import "time"

// GitHead is the current repository HEAD, as much as git could report. It is nil at
// the call site (see Input.Head) when HEAD could not be read at all — an unborn
// branch, a missing git binary, or a project that is not a git worktree.
type GitHead struct {
	Commit      string // full hash
	ShortCommit string
	Subject     string // single-line commit subject
	Committed   time.Time
	Branch      string // empty when detached or unknown
}

// Ancestry is the tri-state result of asking "is the baseline commit an ancestor of
// HEAD?". The middle state (AncestryUnknown) is the reason this is not a bool: after
// a history rewrite the baseline commit may no longer exist, so "not an ancestor" and
// "cannot determine" must stay distinct — collapsing them would report a garbage
// collected baseline as "differs from HEAD", a confident claim the evidence does not
// support.
type Ancestry int

const (
	// AncestryUnknown means the ancestry probe was not run, could not determine an
	// answer, or the baseline commit is not present in the current history.
	AncestryUnknown Ancestry = iota
	// AncestryYes means the baseline commit is an ancestor of HEAD.
	AncestryYes
	// AncestryNo means the baseline commit is definitively not an ancestor of HEAD.
	AncestryNo
)

// Freshness is the stable classification of a baseline against current HEAD. The set
// is closed; Machine returns the wire token an agent keys off. Every value is a note,
// not a warning: see the package doc.
type Freshness int

const (
	// FreshnessNoBaseline: no checkpoint was selected as the baseline (e.g. a now..now
	// comparison), so there is nothing to relate to HEAD.
	FreshnessNoBaseline Freshness = iota
	// FreshnessNoGitMetadata: the baseline checkpoint captured no git commit (it was
	// taken in a non-git project, or before any commit), so it cannot be located in git
	// history.
	FreshnessNoGitMetadata
	// FreshnessAtHead: the baseline commit is exactly the current HEAD.
	FreshnessAtHead
	// FreshnessPredatesHead: the baseline commit is an ancestor of HEAD — HEAD has
	// moved forward since the checkpoint. Useful context for a long review, not a fault.
	FreshnessPredatesHead
	// FreshnessDiffersFromHead: the baseline commit is neither HEAD nor an ancestor of
	// it — history diverged (a rebase or amend), so the checkpoint delta may not map
	// cleanly onto the current HEAD.
	FreshnessDiffersFromHead
	// FreshnessUnknown: HEAD could not be read, or the baseline commit could not be
	// located in the current history (a rewrite removed it), so the relationship is
	// undetermined. Never a false "differs".
	FreshnessUnknown
)

// Machine returns the stable wire token for the freshness value.
func (f Freshness) Machine() string {
	switch f {
	case FreshnessNoBaseline:
		return "no-baseline"
	case FreshnessNoGitMetadata:
		return "baseline-no-git-metadata"
	case FreshnessAtHead:
		return "at-head"
	case FreshnessPredatesHead:
		return "predates-head"
	case FreshnessDiffersFromHead:
		return "differs-from-head"
	case FreshnessUnknown:
		return "unknown"
	default:
		return "unknown"
	}
}

// Label returns a short human tag for the freshness value, for a note line.
func (f Freshness) Label() string {
	switch f {
	case FreshnessNoBaseline:
		return "no baseline"
	case FreshnessNoGitMetadata:
		return "baseline has no git commit"
	case FreshnessAtHead:
		return "at current HEAD"
	case FreshnessPredatesHead:
		return "predates current HEAD"
	case FreshnessDiffersFromHead:
		return "diverges from current HEAD"
	case FreshnessUnknown:
		return "unrelatable to current HEAD"
	default:
		return "unknown"
	}
}

// Known reports whether the freshness relationship is determined. It is false only for
// FreshnessUnknown, so a renderer can pick between a confident context note and an
// "undetermined" note without re-deriving the classification.
func (f Freshness) Known() bool { return f != FreshnessUnknown }

// Input is the pure classifier input. BaselineCommit is the checkpoint's stored full
// git commit hash, empty when the checkpoint carried no git metadata or when there is
// no baseline at all (HasBaseline distinguishes the two). Head is nil when HEAD could
// not be read. Ancestry is the caller's ancestor-probe result; it need only be
// meaningful when both a baseline commit and a HEAD exist and they differ.
type Input struct {
	HasBaseline    bool
	BaselineCommit string
	Head           *GitHead
	Ancestry       Ancestry
}

// Classify maps the observed git facts to a stable freshness token. It is total and
// deterministic: every Input yields exactly one Freshness, and an undetermined
// relationship yields FreshnessUnknown rather than a confident guess.
func Classify(in Input) Freshness {
	if !in.HasBaseline {
		return FreshnessNoBaseline
	}
	if in.BaselineCommit == "" {
		return FreshnessNoGitMetadata
	}
	if in.Head == nil || in.Head.Commit == "" {
		return FreshnessUnknown
	}
	if in.BaselineCommit == in.Head.Commit {
		return FreshnessAtHead
	}
	switch in.Ancestry {
	case AncestryYes:
		return FreshnessPredatesHead
	case AncestryNo:
		return FreshnessDiffersFromHead
	default:
		return FreshnessUnknown
	}
}

// ReviewCoverage is the honesty note that awa's evidence is a checkpoint delta, not
// the authoritative git review. It is a single closed concept — a type rather than a
// bare string — so its token and wording live in one place and can be asserted in
// tests and reused by every review surface.
type ReviewCoverage struct{}

// Token is the stable wire token for the coverage note.
func (ReviewCoverage) Token() string { return "checkpoint-delta-only" }

// Message is the human sentence the coverage note renders.
func (ReviewCoverage) Message() string {
	return "awa shows changes vs the checkpoint baseline; review the full git diff before acceptance"
}
