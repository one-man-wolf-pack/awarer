package cli

import (
	"context"

	"awarer/internal/app/state"
	domainconfig "awarer/internal/domain/config"
	"awarer/internal/domain/gitfresh"
	"awarer/internal/infra/gitmeta"
)

// classifyBaselineFreshness relates a selected checkpoint baseline to the current git
// HEAD. It does the git I/O at the CLI edge (the gitfresh domain stays pure) and hands
// back a stable freshness token plus the current HEAD facts a note can name. Every git
// failure degrades to FreshnessUnknown and never fails the caller — freshness is
// context, not a correctness gate. The head return is meaningful only when it was read
// (freshness at-head/predates/differs); its zero value is fine for the other tokens.
func classifyBaselineFreshness(ctx context.Context, root string, baseline state.StateEndpoint) (gitfresh.Freshness, gitfresh.GitHead) {
	in := gitfresh.Input{
		HasBaseline:    baseline.Kind == state.KindCheckpoint && baseline.HasCheckpoint,
		BaselineCommit: baseline.GitCommit,
	}
	// No baseline, or a baseline that captured no git commit: nothing to relate to HEAD,
	// and no reason to spend a git call.
	if !in.HasBaseline || in.BaselineCommit == "" {
		return gitfresh.Classify(in), gitfresh.GitHead{}
	}

	provider := gitmeta.New(root)
	head, ok, err := provider.CurrentHead(ctx)
	if err != nil || !ok {
		// HEAD unreadable (non-git, unborn, git failure): undetermined, not a failure.
		return gitfresh.Classify(in), gitfresh.GitHead{}
	}
	in.Head = &head

	// The ancestry probe only matters when the baseline is neither absent nor exactly
	// HEAD; Classify short-circuits at-head before consulting Ancestry, so skip the git
	// call in that case.
	if in.BaselineCommit != head.Commit {
		isAncestor, known, aerr := provider.IsAncestor(ctx, in.BaselineCommit, head.Commit)
		switch {
		case aerr != nil || !known:
			in.Ancestry = gitfresh.AncestryUnknown
		case isAncestor:
			in.Ancestry = gitfresh.AncestryYes
		default:
			in.Ancestry = gitfresh.AncestryNo
		}
	}
	return gitfresh.Classify(in), head
}

// baselineGitContext is a checkpoint baseline's freshness against the live git HEAD,
// computed once at the command edge so the presentation layer only formats it and never
// performs git I/O. The zero value (freshness FreshnessNoBaseline, empty head, nil view)
// renders nothing, which is correct for a non-checkpoint baseline or a non-git project.
type baselineGitContext struct {
	freshness gitfresh.Freshness
	head      gitfresh.GitHead
	view      *freshnessView // the JSON projection, nil when there is nothing to report
}

// baselineGitContextOf resolves the baseline endpoint from the compared states and
// classifies it against the current git HEAD. It is the one place the changes/diff
// commands do git I/O for freshness; the resulting value drives both the human header
// and the JSON document, so the two can never disagree and git is queried once.
func baselineGitContextOf(ctx context.Context, root string, left, right *state.ResolvedState) baselineGitContext {
	baseline := state.NewStateRangeSummary(left, right, nil).Left
	fresh, head := classifyBaselineFreshness(ctx, root, baseline)
	return baselineGitContext{
		freshness: fresh,
		head:      head,
		view:      freshnessViewOf(fresh, baseline, head),
	}
}

// freshnessNote renders the human note line for a baseline freshness result, or "" when
// no note is warranted. Only predates/differs/unknown produce a note: at-head is the
// expected case and no-baseline/no-git-metadata have nothing useful to add, so they
// stay silent to keep review output dense. The head short hash and subject are named so
// the reviewer can jump to the right git command. Every freshness is a calm note, never
// a warning.
func freshnessNote(f gitfresh.Freshness, head gitfresh.GitHead) string {
	switch f {
	case gitfresh.FreshnessPredatesHead:
		return "baseline predates git HEAD " + headLabel(head) +
			" — awa shows the checkpoint delta; review the full git diff before acceptance"
	case gitfresh.FreshnessDiffersFromHead:
		return "baseline diverges from git HEAD " + headLabel(head) +
			" (rebased/amended) — the checkpoint delta may not map onto current HEAD"
	case gitfresh.FreshnessUnknown:
		return "baseline cannot be related to git HEAD (rewritten history or git unavailable) — verify against the full git diff"
	default:
		return ""
	}
}

// headLabel renders a current-HEAD identity for a note line: the short commit and, when
// available, its quoted subject.
func headLabel(head gitfresh.GitHead) string {
	if head.ShortCommit == "" {
		return "HEAD"
	}
	label := head.ShortCommit
	if head.Subject != "" {
		label += " \"" + head.Subject + "\""
	}
	return label
}

// freshnessView is the machine projection of a baseline-freshness result: the stable
// token plus the commits it relates. It is omitted from JSON entirely (see the
// omitempty pointer on the carrying doc) when there is no baseline or the baseline
// carried no git commit, so a pipeline sees a freshness object only when it is
// meaningful.
type freshnessView struct {
	Token          string `json:"token"`
	BaselineCommit string `json:"baseline_commit,omitempty"`
	HeadCommit     string `json:"head_commit,omitempty"`
	HeadSubject    string `json:"head_subject,omitempty"`
}

// freshnessViewOf builds the JSON freshness projection, or nil when there is nothing
// worth reporting (no baseline / baseline captured no git commit), so the field is
// omitted rather than carrying a bare "no-baseline" token in every pipe-clean case.
func freshnessViewOf(f gitfresh.Freshness, baseline state.StateEndpoint, head gitfresh.GitHead) *freshnessView {
	if f == gitfresh.FreshnessNoBaseline || f == gitfresh.FreshnessNoGitMetadata {
		return nil
	}
	return &freshnessView{
		Token:          f.Machine(),
		BaselineCommit: baseline.GitCommit,
		HeadCommit:     head.Commit,
		HeadSubject:    head.Subject,
	}
}

// reviewCoverageView is the machine projection of the review-coverage honesty note: an
// awa delta is a checkpoint delta, not a whole-repo git review. It is always present in
// the surfaces that carry it (a stable contract an agent can key off unconditionally).
type reviewCoverageView struct {
	Token   string `json:"token"`
	Message string `json:"message"`
}

func reviewCoverageViewOf() reviewCoverageView {
	var rc gitfresh.ReviewCoverage
	return reviewCoverageView{Token: rc.Token(), Message: rc.Message()}
}

// ignoredPathsOutsideEvidence is a contract assertion, not a measured value: awa's scan
// prunes ignored/excluded paths before any comparison, so a delta can never prove
// anything about them. It is invariantly true while awa enforces any ignore policy —
// and the product baseline excludes always apply — so it is never computed and never
// set false. It is emitted so an agent reads the caveat from the payload itself rather
// than having to know it out-of-band.
const ignoredPathsOutsideEvidence = true

// scopeView is the machine projection of the comparison's scope honesty. SkippedInScope
// and IgnoreSources are measured facts (how many scoped-in inputs were unhashable, which
// ignore sources are active). IgnoredPathsOutsideEvidence is NOT a measured fact: it is
// a stable contract assertion (see the const) that an awa delta says nothing about
// ignored paths — the machine form of the human caveat that a deleted path may in fact
// be one that became ignored.
type scopeView struct {
	SkippedInScope              int      `json:"skipped_in_scope"`
	IgnoreSources               []string `json:"ignore_sources"`
	IgnoredPathsOutsideEvidence bool     `json:"ignored_paths_outside_evidence"`
}

func scopeViewOf(skipped int, cfg domainconfig.Config) scopeView {
	sources := []string{}
	if cfg.Scope.UseGitignore {
		sources = append(sources, "gitignore")
	}
	if cfg.Scope.UseAwaignore {
		sources = append(sources, "awaignore")
	}
	if len(cfg.Scope.ExtraExcludes) > 0 || len(cfg.History.ExtraExcludes) > 0 {
		sources = append(sources, "config-excludes")
	}
	// The product baseline excludes (node_modules, target, and similar) always apply, so
	// there are always ignored paths outside the compared scope.
	sources = append(sources, "baseline-excludes")
	return scopeView{
		SkippedInScope:              skipped,
		IgnoreSources:               sources,
		IgnoredPathsOutsideEvidence: ignoredPathsOutsideEvidence,
	}
}
