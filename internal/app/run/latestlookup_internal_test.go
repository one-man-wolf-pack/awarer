package run

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"awarer/internal/domain/runcache"
)

// iterStore is a fake Store that drives latestMatchingRunID's streaming newest-first
// lookup. It yields a fixed newest-first id sequence through IterRefsNewest and
// otherwise inherits the (nil) Store interface, which the latest-lookup path never
// touches.
type iterStore struct {
	runcache.Store
	newestFirst []runcache.RunID
	yielded     int
}

func (s *iterStore) IterRefsNewest(_ context.Context, yield func(runcache.RunID) (bool, error)) error {
	for _, id := range s.newestFirst {
		s.yielded++
		stop, err := yield(id)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
	}
	return nil
}

func mkRunID(t *testing.T, unixNano int64, disc string) runcache.RunID {
	t.Helper()
	id, err := runcache.NewRunID(unixNano, strings.NewReader(strings.Repeat(disc, 8)))
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	return id
}

// TestLatestMatchingRunIDNotNarrowedToWindow proves the latest-lookup contract is not
// silently narrowed to the ranking window: a valid run far past explainCandidateWindow
// positions must still be found. Only that old run's predicate accepts; everything
// newer is rejected, so the lookup has to stream well beyond any bounded window.
func TestLatestMatchingRunIDNotNarrowedToWindow(t *testing.T) {
	base := int64(1_700_000_000) * int64(time.Second)
	// Build far more ids than the ranking window, newest first. The only acceptable run
	// is the very oldest, sitting beyond explainCandidateWindow.
	total := explainCandidateWindow*3 + 7
	ids := make([]runcache.RunID, total)
	for i := 0; i < total; i++ {
		// Newest first: give the largest timestamp the lowest index.
		ids[i] = mkRunID(t, base+int64(total-i), string(rune('a'+(i%26))))
	}
	want := ids[total-1] // the oldest run

	store := &iterStore{newestFirst: ids}
	svc := &Service{deps: Deps{Store: store}}

	// readable accepts only the oldest run; every newer one is a clean skip (ErrNotFound).
	got, skipped, err := svc.latestMatchingRunID(context.Background(), func(id runcache.RunID) error {
		if id == want {
			return nil
		}
		return runcache.ErrNotFound
	})
	if err != nil {
		t.Fatalf("latestMatchingRunID: %v", err)
	}
	if got != want {
		t.Fatalf("latestMatchingRunID = %s, want the old-but-valid run %s (lookup was narrowed to a window)",
			got.Short(), want.Short())
	}
	// A vanished entry was never passed over — nothing to disclose.
	if skipped.Any() {
		t.Fatalf("unexpected skip report: %+v", skipped)
	}
	if store.yielded < total {
		t.Fatalf("lookup yielded %d ids, want it to stream all %d (semantics narrowed)", store.yielded, total)
	}
}

// TestLatestMatchingRunIDStopsAtFirstMatch proves the lookup stops at the first
// accepted (newest) run and does not keep streaming.
func TestLatestMatchingRunIDStopsAtFirstMatch(t *testing.T) {
	base := int64(1_700_000_000) * int64(time.Second)
	ids := []runcache.RunID{
		mkRunID(t, base+3, "a"),
		mkRunID(t, base+2, "b"),
		mkRunID(t, base+1, "c"),
	}
	store := &iterStore{newestFirst: ids}
	svc := &Service{deps: Deps{Store: store}}

	got, _, err := svc.latestMatchingRunID(context.Background(), func(runcache.RunID) error { return nil })
	if err != nil {
		t.Fatalf("latestMatchingRunID: %v", err)
	}
	if got != ids[0] {
		t.Fatalf("latestMatchingRunID = %s, want newest %s", got.Short(), ids[0].Short())
	}
	if store.yielded != 1 {
		t.Fatalf("lookup yielded %d ids, want it to stop at the first match", store.yielded)
	}
}

// TestLatestMatchingRunIDReportsEverySkippedRecord proves that a newer record awa
// could not read is skipped AND disclosed, whichever way it was unreadable. Silence
// would present an older run as "the latest" while newer evidence sits unread — and
// an incompatible record is exactly the state a reset is supposed to resolve, so
// hiding it would hide the reason to act.
func TestLatestMatchingRunIDReportsEverySkippedRecord(t *testing.T) {
	base := int64(1_700_000_000) * int64(time.Second)
	for _, tc := range []struct {
		name string
		err  error
		want func(SkippedLatest) SkippedRuns
		word string
	}{
		{"corrupt", runcache.ErrCorruptStore, func(s SkippedLatest) SkippedRuns { return s.Corrupt }, "corrupt"},
		{"incompatible", runcache.ErrIncompatibleEntry, func(s SkippedLatest) SkippedRuns { return s.Incompatible }, "incompatible"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newest := mkRunID(t, base+2, "a")
			older := mkRunID(t, base+1, "b")
			store := &iterStore{newestFirst: []runcache.RunID{newest, older}}
			svc := &Service{deps: Deps{Store: store}}

			got, skipped, err := svc.latestMatchingRunID(context.Background(), func(id runcache.RunID) error {
				if id == newest {
					return tc.err
				}
				return nil
			})
			if err != nil {
				t.Fatalf("latestMatchingRunID: %v", err)
			}
			if got != older {
				t.Fatalf("latestMatchingRunID = %s, want the readable older run %s", got.Short(), older.Short())
			}
			bucket := tc.want(skipped)
			if bucket.Count != 1 || len(bucket.Sample) != 1 || bucket.Sample[0] != newest {
				t.Fatalf("skip report = %+v, want exactly the newest id", bucket)
			}
			warnings := SkippedLatestWarnings(skipped)
			if len(warnings) != 1 || !strings.Contains(warnings[0], tc.word) {
				t.Fatalf("warnings = %v, want one %s-skip warning", warnings, tc.word)
			}
			if !strings.Contains(warnings[0], newest.Short()) {
				t.Errorf("warning %q does not name the skipped run", warnings[0])
			}
		})
	}
}

// TestSkippedLatestSampleIsBounded proves the disclosure stays a diagnostic rather
// than becoming an unbounded id dump when every newer record is unreadable.
func TestSkippedLatestSampleIsBounded(t *testing.T) {
	base := int64(1_700_000_000) * int64(time.Second)
	const total = maxSkippedLatestSample + 4
	ids := make([]runcache.RunID, 0, total+1)
	for i := 0; i < total; i++ {
		ids = append(ids, mkRunID(t, base+int64(total-i)+1, string(rune('a'+i))))
	}
	readable := mkRunID(t, base+1, "z")
	ids = append(ids, readable)
	svc := &Service{deps: Deps{Store: &iterStore{newestFirst: ids}}}

	got, skipped, err := svc.latestMatchingRunID(context.Background(), func(id runcache.RunID) error {
		if id == readable {
			return nil
		}
		return runcache.ErrIncompatibleEntry
	})
	if err != nil {
		t.Fatalf("latestMatchingRunID: %v", err)
	}
	if got != readable {
		t.Fatalf("latestMatchingRunID = %s, want %s", got.Short(), readable.Short())
	}
	if skipped.Incompatible.Count != total {
		t.Errorf("count = %d, want the exact %d skipped", skipped.Incompatible.Count, total)
	}
	if len(skipped.Incompatible.Sample) != maxSkippedLatestSample {
		t.Errorf("sample = %d ids, want it bounded at %d", len(skipped.Incompatible.Sample), maxSkippedLatestSample)
	}
	if w := SkippedLatestWarnings(skipped); len(w) != 1 || !strings.Contains(w[0], "...") {
		t.Errorf("warning %v does not disclose that the id list is a sample", w)
	}
}

// TestLatestMatchingRunIDPropagatesError proves a non-skippable readable error aborts.
func TestLatestMatchingRunIDPropagatesError(t *testing.T) {
	base := int64(1_700_000_000) * int64(time.Second)
	store := &iterStore{newestFirst: []runcache.RunID{mkRunID(t, base+1, "a")}}
	svc := &Service{deps: Deps{Store: store}}

	sentinel := errors.New("disk on fire")
	_, _, err := svc.latestMatchingRunID(context.Background(), func(runcache.RunID) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("latestMatchingRunID err = %v, want the propagated readable error", err)
	}
}
