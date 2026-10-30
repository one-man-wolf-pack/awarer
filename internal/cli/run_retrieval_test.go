package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	apprun "awarer/internal/app/run"
	domainconfig "awarer/internal/domain/config"
	"awarer/internal/domain/runcache"
	"awarer/internal/output"
)

const historyHintLine = "history: awa run log  # recorded executions, any reuse state"

// TestRecordedHistoryFollowUpUsesTheStoredVerdict confirms only a deliberately
// recorded run (record-only kind with reason record-only or no-cache) is routed to history, by its full id and
// with an explicit output selector; every other reason keeps its existing routing.
func TestRecordedHistoryFollowUpUsesTheStoredVerdict(t *testing.T) {
	id, err := runcache.ParseRunID("18e32e3d74d80be05b5a6ee1e5a03d80")
	if err != nil {
		t.Fatal(err)
	}
	recordOnly, _ := runcache.RecordOnly(runcache.ReasonRecordOnly)
	noCache, _ := runcache.RecordOnly(runcache.ReasonNoCache)
	stdin, _ := runcache.RecordOnly(runcache.ReasonStdinNotKeyed)
	captureOff, _ := runcache.RecordOnly(runcache.ReasonCaptureDisabled)
	unauthorized, _ := runcache.RecordOnly(runcache.ReasonExternalFactsUnavailable)
	mutated, _ := runcache.NonReusableRun(runcache.ReasonMutatedState)
	for _, tc := range []struct {
		name  string
		reuse runcache.ReuseState
		// reason is the candidate's displayed reason; it must not drive the decision.
		reason runcache.MismatchReason
		want   string
	}{
		{"record-only", recordOnly, runcache.ReasonRecordOnly, "awa run show 18e32e3d74d80be05b5a6ee1e5a03d80 --tail 50"},
		{"no-cache", noCache, runcache.ReasonNoCache, "awa run show 18e32e3d74d80be05b5a6ee1e5a03d80 --tail 50"},
		{"capture-disabled", captureOff, runcache.ReasonCaptureDisabled, ""},
		{"external-facts-unavailable", unauthorized, runcache.ReasonExternalFactsUnavailable, ""},
		{"stdin-not-keyed", stdin, runcache.ReasonStdinNotKeyed, ""},
		{"mutated", mutated, runcache.ReasonMutatedState, ""},
		{"reusable drifted", runcache.ReusableCacheEntry(), runcache.ReasonInputTreeDiffers, ""},
		{"reason text alone", runcache.ReusableCacheEntry(), runcache.ReasonRecordOnly, ""},
	} {
		c := apprun.RunReuseCandidate{Entry: runcache.RunEntry{ID: id, Reuse: tc.reuse}, Reason: tc.reason}
		got, ok := recordedHistoryFollowUp(c)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("%s: got (%q, %v), want %q", tc.name, got, ok, tc.want)
		}
	}
}

func TestRunHistoryHintQuotesTheCommandFilter(t *testing.T) {
	for in, want := range map[string]string{
		"":              "history: awa run log  # recorded executions, any reuse state",
		"go":            "history: awa run log --command go  # recorded executions, any reuse state",
		"go test ./...": "history: awa run log --command 'go test ./...'  # recorded executions, any reuse state",
		"it's $HOME":    `history: awa run log --command 'it'\''s $HOME'  # recorded executions, any reuse state`,
	} {
		if got := runHistoryHint(in); got != want {
			t.Errorf("runHistoryHint(%q) = %q, want %q", in, got, want)
		}
	}
}

// lsFixtureEntry builds one readable listing row for the human run ls renderer.
func lsFixtureEntry(t *testing.T, id string, cmd string, v runcache.RunReusability) apprun.LsEntry {
	t.Helper()
	rid, err := runcache.ParseRunID(id)
	if err != nil {
		t.Fatal(err)
	}
	e := runcache.RunEntry{ID: rid, StartedAt: time.Unix(1_800_000_000, 0), FinishedAt: time.Unix(1_800_000_001, 0)}
	e.KeyInput.Command.Argv = []string{cmd}
	return apprun.LsEntry{ID: rid, Entry: e, Reusability: v}
}

// TestRenderLsHumanMixedWindow exercises the real human run ls renderer on a window
// whose rows were partly assessed and partly unassessed. With nothing reusable among
// the assessed rows, every form keeps its qualification, its rows and the unassessed
// section, and adds exactly one history pointer; with a reusable assessed row, the
// rows stay and no empty-reuse pointer appears.
func TestRenderLsHumanMixedWindow(t *testing.T) {
	recorded, _ := runcache.NotReusable(runcache.ReasonRecordOnly)
	reusable := runcache.Reusable()
	stale := lsFixtureEntry(t, "18e32e3d74d80be05b5a6ee1e5a03d80", "stale-check", recorded)
	fresh := lsFixtureEntry(t, "18e32e3d74d80be05b5a6ee1e5a03d81", "fresh-check", reusable)
	pending := lsFixtureEntry(t, "18e32e3d74d80be05b5a6ee1e5a03d82", "probed-check", runcache.RunReusability{})
	unassessed := "unassessed (identity facts only awa run acquires):\n  18e32e3d74d8"

	render := func(res apprun.LsResult, all, near bool) string {
		var out, errb bytes.Buffer
		renderLsHuman(output.New(&out, &errb), res, all, near, "", domainconfig.TimeUTC, time.Unix(1_800_000_100, 0))
		return out.String()
	}

	t.Run("assessed non-reusable plus unassessed", func(t *testing.T) {
		near := apprun.RunReuseCandidate{Entry: stale.Entry, Reason: runcache.ReasonRecordOnly}
		for _, tc := range []struct {
			name      string
			res       apprun.LsResult
			all, near bool
			want      []string
		}{
			{"plain", apprun.LsResult{Unassessed: []apprun.LsEntry{pending}}, false, false,
				[]string{"no reusable runs among the assessed runs\n" + historyHintLine + "\n", unassessed}},
			{"all", apprun.LsResult{Reusable: []apprun.LsEntry{stale}, Unassessed: []apprun.LsEntry{pending}}, true, false,
				[]string{"stale-check  not-reusable(record-only)\n" + historyHintLine + "\n", unassessed}},
			{"near", apprun.LsResult{NearMisses: []apprun.RunReuseCandidate{near}, Unassessed: []apprun.LsEntry{pending}}, false, true,
				[]string{"reusable now:\n  none among the assessed runs\n  " + historyHintLine + "\n", "near misses:\n  18e32e3d74d8", "stale-check  not-reusable(record-only)", unassessed}},
		} {
			got := render(tc.res, tc.all, tc.near)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("%s: missing %q:\n%s", tc.name, want, got)
				}
			}
			if n := strings.Count(got, "history: awa run log"); n != 1 {
				t.Errorf("%s: %d history pointers, want one:\n%s", tc.name, n, got)
			}
			if strings.Contains(got, "probed-check  not-reusable") {
				t.Errorf("%s: the unassessed row was rendered as a miss:\n%s", tc.name, got)
			}
		}
	})

	t.Run("assessed reusable plus unassessed", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			res       apprun.LsResult
			all, near bool
			want      string
		}{
			{"plain", apprun.LsResult{Reusable: []apprun.LsEntry{fresh}, Unassessed: []apprun.LsEntry{pending}}, false, false, "fresh-check  reusable\n"},
			{"all", apprun.LsResult{Reusable: []apprun.LsEntry{fresh, stale}, Unassessed: []apprun.LsEntry{pending}}, true, false, "fresh-check  reusable\n"},
			{"near", apprun.LsResult{Reusable: []apprun.LsEntry{fresh}, Unassessed: []apprun.LsEntry{pending}}, false, true, "reusable now:\n  18e32e3d74d8"},
		} {
			got := render(tc.res, tc.all, tc.near)
			for _, want := range []string{tc.want, unassessed} {
				if !strings.Contains(got, want) {
					t.Errorf("%s: missing %q:\n%s", tc.name, want, got)
				}
			}
			reusableSection, _, _ := strings.Cut(got, "near misses:")
			if strings.Contains(got, "history:") || strings.Contains(reusableSection, "none") {
				t.Errorf("%s: a reusable row still got the empty-reuse wording:\n%s", tc.name, got)
			}
		}
	})
}

// TestRenderLsHumanAllKnownEmptyNeedsAnEmptyWindow confirms --all states an empty
// store only from an empty reference window; zero rows from a non-empty window keep a
// qualified result and the history pointer.
func TestRenderLsHumanAllKnownEmptyNeedsAnEmptyWindow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		res     apprun.LsResult
		command string
		want    string
	}{
		{"empty store", apprun.LsResult{WindowEmpty: true}, "", "no runs recorded\n"},
		{"empty store with filter", apprun.LsResult{WindowEmpty: true}, "go", "no runs recorded\n"},
		{"unlisted window", apprun.LsResult{}, "", "no readable runs among the recent runs\n" + historyHintLine + "\n"},
		{"filtered window", apprun.LsResult{}, "go", "no recent runs match the command filter\nhistory: awa run log --command go  # recorded executions, any reuse state\n"},
	} {
		var out, errb bytes.Buffer
		renderLsHuman(output.New(&out, &errb), tc.res, true, false, tc.command, domainconfig.TimeUTC, time.Unix(1_800_000_000, 0))
		if got := out.String(); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
