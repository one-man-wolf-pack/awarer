package gitfresh

import "testing"

func head(commit string) *GitHead {
	return &GitHead{Commit: commit, ShortCommit: commit[:min(len(commit), 7)], Subject: "s"}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		in   Input
		want Freshness
	}{
		{
			name: "no baseline",
			in:   Input{HasBaseline: false},
			want: FreshnessNoBaseline,
		},
		{
			name: "baseline without git metadata",
			in:   Input{HasBaseline: true, BaselineCommit: ""},
			want: FreshnessNoGitMetadata,
		},
		{
			name: "head unreadable",
			in:   Input{HasBaseline: true, BaselineCommit: "abc123", Head: nil},
			want: FreshnessUnknown,
		},
		{
			name: "head with empty commit is unreadable",
			in:   Input{HasBaseline: true, BaselineCommit: "abc123", Head: &GitHead{Commit: ""}},
			want: FreshnessUnknown,
		},
		{
			name: "baseline at head",
			in:   Input{HasBaseline: true, BaselineCommit: "abc123", Head: head("abc123")},
			want: FreshnessAtHead,
		},
		{
			name: "baseline predates head (ancestor)",
			in:   Input{HasBaseline: true, BaselineCommit: "old", Head: head("newcommit"), Ancestry: AncestryYes},
			want: FreshnessPredatesHead,
		},
		{
			name: "baseline differs (not ancestor)",
			in:   Input{HasBaseline: true, BaselineCommit: "old", Head: head("newcommit"), Ancestry: AncestryNo},
			want: FreshnessDiffersFromHead,
		},
		{
			name: "baseline ancestry undetermined stays unknown, never differs",
			in:   Input{HasBaseline: true, BaselineCommit: "gone", Head: head("newcommit"), Ancestry: AncestryUnknown},
			want: FreshnessUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.in); got != tc.want {
				t.Errorf("Classify() = %v (%s), want %v (%s)", got, got.Machine(), tc.want, tc.want.Machine())
			}
		})
	}
}

func TestFreshnessMachineTokensAreStableAndDistinct(t *testing.T) {
	all := []Freshness{
		FreshnessNoBaseline, FreshnessNoGitMetadata, FreshnessAtHead,
		FreshnessPredatesHead, FreshnessDiffersFromHead, FreshnessUnknown,
	}
	want := map[Freshness]string{
		FreshnessNoBaseline:      "no-baseline",
		FreshnessNoGitMetadata:   "baseline-no-git-metadata",
		FreshnessAtHead:          "at-head",
		FreshnessPredatesHead:    "predates-head",
		FreshnessDiffersFromHead: "differs-from-head",
		FreshnessUnknown:         "unknown",
	}
	seen := map[string]bool{}
	for _, f := range all {
		tok := f.Machine()
		if tok != want[f] {
			t.Errorf("Machine(%v) = %q, want %q", f, tok, want[f])
		}
		if seen[tok] {
			t.Errorf("duplicate machine token %q", tok)
		}
		seen[tok] = true
		if f.Label() == "" {
			t.Errorf("Label(%v) is empty", f)
		}
	}
}

func TestKnown(t *testing.T) {
	if FreshnessUnknown.Known() {
		t.Error("FreshnessUnknown.Known() should be false")
	}
	for _, f := range []Freshness{FreshnessNoBaseline, FreshnessNoGitMetadata, FreshnessAtHead, FreshnessPredatesHead, FreshnessDiffersFromHead} {
		if !f.Known() {
			t.Errorf("%v.Known() should be true", f)
		}
	}
}

func TestReviewCoverageStable(t *testing.T) {
	var rc ReviewCoverage
	if rc.Token() != "checkpoint-delta-only" {
		t.Errorf("ReviewCoverage.Token() = %q", rc.Token())
	}
	if rc.Message() == "" {
		t.Error("ReviewCoverage.Message() is empty")
	}
}
