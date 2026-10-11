package run_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	apprun "awarer/internal/app/run"
	"awarer/internal/domain/runcache"
)

func (h *harness) recordRuns(t *testing.T, argvs ...[]string) []runcache.RunID {
	t.Helper()
	ids := make([]runcache.RunID, 0, len(argvs))
	for _, argv := range argvs {
		h.clock.now = h.clock.now.Add(time.Second)
		res, err := h.svc.Run(t.Context(), h.request(argv...))
		if err != nil {
			t.Fatalf("run %q: %v", argv, err)
		}
		ids = append(ids, res.RunID)
	}
	return ids
}

func logCommands(t *testing.T, h *harness, req apprun.LogRequest) []string {
	t.Helper()
	entries, err := h.svc.Log(t.Context(), req)
	if err != nil {
		t.Fatalf("log %+v: %v", req, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, strings.Join(e.Entry.KeyInput.Command.Argv, " "))
	}
	return out
}

func TestLogCommandFilterSearchesWholeCommand(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.write(t, "a.txt", "v1")
	h.recordRuns(t,
		[]string{"cargo", "fmt"},
		[]string{"cargo", "xtask", "check", "one"},
		[]string{"go", "test"},
		[]string{"cargo", "xtask", "check", "two"},
		[]string{"go", "vet"},
	)

	for _, tc := range []struct {
		name string
		req  apprun.LogRequest
		want []string
	}{
		{"argument", apprun.LogRequest{Command: "fmt"}, []string{"cargo fmt"}},
		{"across arguments, newest first", apprun.LogRequest{Command: "xtask check"},
			[]string{"cargo xtask check two", "cargo xtask check one"}},
		{"limit counts matching rows", apprun.LogRequest{Command: "cargo", Limit: 2},
			[]string{"cargo xtask check two", "cargo xtask check one"}},
		{"limit reaches past unrelated newer runs", apprun.LogRequest{Command: "fmt", Limit: 1}, []string{"cargo fmt"}},
		{"case-sensitive", apprun.LogRequest{Command: "FMT"}, []string{}},
		{"empty filter is no filter", apprun.LogRequest{Limit: 2}, []string{"go vet", "cargo xtask check two"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := logCommands(t, h, tc.req); !slices.Equal(got, tc.want) {
				t.Errorf("log = %q, want %q", got, tc.want)
			}
		})
	}
}

func removedIDs(res apprun.RmResult) []string {
	out := make([]string, 0, len(res.Removed))
	for _, e := range res.Removed {
		out = append(out, e.ID.String())
	}
	slices.Sort(out)
	return out
}

func sortedIDs(ids ...runcache.RunID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	slices.Sort(out)
	return out
}

func (h *harness) storedIDs(t *testing.T) []string {
	t.Helper()
	refs, err := h.store.ListRefs(t.Context())
	if err != nil {
		t.Fatalf("list refs: %v", err)
	}
	return sortedIDs(refs...)
}

func TestRmCommandFilterSelectsByWholeCommand(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.write(t, "a.txt", "v1")
	ids := h.recordRuns(t,
		[]string{"cargo", "fmt"},
		[]string{"cargo", "xtask", "check", "one"},
		[]string{"go", "test"},
		[]string{"cargo", "xtask", "check", "two"},
	)
	all := sortedIDs(ids...)
	want := sortedIDs(ids[1], ids[3])

	dry, err := h.svc.Rm(t.Context(), apprun.RmRequest{Command: "xtask check", DryRun: true})
	if err != nil {
		t.Fatalf("rm --dry-run: %v", err)
	}
	if got := removedIDs(dry); !slices.Equal(got, want) {
		t.Errorf("dry-run targets = %v, want %v", got, want)
	}
	if got := h.storedIDs(t); !slices.Equal(got, all) {
		t.Fatalf("dry run changed the store: %v, want %v", got, all)
	}

	res, err := h.svc.Rm(t.Context(), apprun.RmRequest{Command: "xtask check"})
	if err != nil {
		t.Fatalf("rm: %v", err)
	}
	if got := removedIDs(res); !slices.Equal(got, want) {
		t.Errorf("removed = %v, want the dry-run targets %v", got, want)
	}
	if got, kept := h.storedIDs(t), sortedIDs(ids[0], ids[2]); !slices.Equal(got, kept) {
		t.Errorf("store after rm = %v, want only the unselected runs %v", got, kept)
	}

	none, err := h.svc.Rm(t.Context(), apprun.RmRequest{Command: "XTASK"})
	if err != nil {
		t.Fatalf("rm with a non-matching filter: %v", err)
	}
	if len(none.Removed)+len(none.RemovedUnreadable) != 0 {
		t.Errorf("a non-matching filter removed %+v", none)
	}
}

func TestRmCommandAndAgeFiltersAreConjunctive(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.write(t, "a.txt", "v1")
	old := h.recordRuns(t, []string{"cargo", "xtask", "check", "old"}, []string{"go", "test"})
	h.clock.now = h.clock.now.Add(48 * time.Hour)
	recent := h.recordRuns(t, []string{"cargo", "xtask", "check", "recent"})

	res, err := h.svc.Rm(t.Context(), apprun.RmRequest{
		Command: "xtask check", OlderThan: 24 * time.Hour, HasOlderThan: true,
	})
	if err != nil {
		t.Fatalf("rm: %v", err)
	}
	if got, want := removedIDs(res), sortedIDs(old[0]); !slices.Equal(got, want) {
		t.Errorf("removed = %v, want only the old matching run %v", got, want)
	}
	if got, want := h.storedIDs(t), sortedIDs(old[1], recent[0]); !slices.Equal(got, want) {
		t.Errorf("store after rm = %v, want %v", got, want)
	}
}

func TestRmRejectsEmptySelector(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.write(t, "a.txt", "v1")
	ids := h.recordRuns(t, []string{"cargo", "fmt"}, []string{"go", "test"})

	for _, req := range []apprun.RmRequest{{}, {DryRun: true}} {
		if _, err := h.svc.Rm(t.Context(), req); err == nil {
			t.Errorf("Rm(%+v) = nil, want a refusal for a request without a selector", req)
		}
	}
	if got, want := h.storedIDs(t), sortedIDs(ids...); !slices.Equal(got, want) {
		t.Errorf("store after refused rm = %v, want %v", got, want)
	}
}

func TestCommandFilterNeverMatchesUnreadableMetadata(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.write(t, "a.txt", "v1")
	ids := h.recordRuns(t, []string{"cargo", "fmt"}, []string{"cargo", "xtask", "check"})

	// The damaged record still holds the filter text in its raw bytes, so only a
	// matcher that searched undecoded metadata could select it.
	meta := filepath.Join(h.store.EntryPath(ids[1]), "meta.json")
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte(`{"schema_version": 1, "argv": ["cargo", "xtask", "check"]`), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := logCommands(t, h, apprun.LogRequest{Command: "xtask check"}); len(got) != 0 {
		t.Errorf("log matched an unreadable run: %q", got)
	}
	if got, want := logCommands(t, h, apprun.LogRequest{Command: "cargo"}), []string{"cargo fmt"}; !slices.Equal(got, want) {
		t.Errorf("log = %q, want only the readable match %q", got, want)
	}
	if _, err := h.svc.Rm(t.Context(), apprun.RmRequest{Command: "cargo"}); err == nil {
		t.Error("filter-mode rm over an unreadable record succeeded, want its existing loud failure")
	}
	if got, want := h.storedIDs(t), sortedIDs(ids...); !slices.Equal(got, want) {
		t.Errorf("store after failed rm = %v, want %v", got, want)
	}
}
