package runstore_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/runcache"
	"awarer/internal/infra/runstore"
)

const probeSentinel = "SENTINEL-probe-output-7f3a"

func probedKeyInput(t *testing.T, h hashing.Hasher, facts []runcache.ExternalFact) runcache.KeyInput {
	t.Helper()
	v, err := runcache.NewValidationScope("check", []string{"target"}, []runcache.ProbeDefinition{
		{Label: "tool", Argv: []string{"tool", "--version"}},
		{Label: "linker", Argv: []string{"linker", "-v"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	v, err = v.WithFacts(facts)
	if err != nil {
		t.Fatal(err)
	}
	ki := keyInputFor(h, "probed")
	ki.Validation = &v
	return ki
}

func acquiredPair(h hashing.Hasher) []runcache.ExternalFact {
	return []runcache.ExternalFact{
		runcache.AcquiredFact(h.HashBytes([]byte("linker 7 " + probeSentinel))),
		runcache.AcquiredFact(h.HashBytes([]byte("tool 1.0 " + probeSentinel))),
	}
}

func storeProbed(t *testing.T, r *runstore.Repo, h hashing.Hasher) runcache.RunEntry {
	t.Helper()
	facts := acquiredPair(h)
	return storeRunWith(t, r, h, probedKeyInput(t, h, facts), "out", "", 1<<20, unchangedObs(), func(e *runcache.RunEntry) {
		e.PostRunFacts = slices.Clone(facts)
	})
}

func TestProbedRunRoundTripsWithoutProbeOutput(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeProbed(t, r, h)
	got, err := r.Get(entry.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !slices.Equal(got.KeyInput.Validation.Facts, entry.KeyInput.Validation.Facts) || !slices.Equal(got.PostRunFacts, entry.PostRunFacts) {
		t.Fatalf("facts did not round-trip: %+v / %+v", got.KeyInput.Validation.Facts, got.PostRunFacts)
	}
	if ce, ok, err := r.Lookup(entry.Key); err != nil || !ok || ce.Entry().ID != entry.ID {
		t.Fatalf("Lookup = %v %v", ok, err)
	}
	meta := readFile(t, metaPathOf(t, layout.RunsDir(), entry.ID))
	if strings.Contains(meta, probeSentinel) || !strings.Contains(meta, `"schema_version": 4`) {
		t.Fatalf("metadata leaks probe output or is not schema 4:\n%s", meta)
	}
}

func TestProbedRecordsRejectTampering(t *testing.T) {
	cases := map[string]func(string) string{
		"a predecessor generation cannot carry probes": func(d string) string {
			return strings.Replace(d, `"schema_version": 4`, `"schema_version": 3`, 1)
		},
		"an unknown fact status": func(d string) string {
			return strings.Replace(d, `"status": "acquired"`, `"status": "maybe"`, 1)
		},
		"a diagnostic placeholder is not evidence": func(d string) string {
			return strings.Replace(d, `"status": "acquired"`, `"status": "unassessed"`, 1)
		},
		"an unavailable fact with a fingerprint": func(d string) string {
			return strings.Replace(d, `"status": "acquired"`, `"status": "timed-out"`, 1)
		},
		"another interpretation": func(d string) string {
			return strings.Replace(d, `"fact_interpretation": "stdout-exact-bytes"`, `"fact_interpretation": "trimmed"`, 1)
		},
		"a reusable record without its post-run phase": func(d string) string {
			i := strings.Index(d, `"post_run_external_facts"`)
			return d[:i-4] + "\n}\n"
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			r, layout, h := newStore(t)
			entry := storeProbed(t, r, h)
			meta := metaPathOf(t, layout.RunsDir(), entry.ID)
			before := readFile(t, meta)
			editMeta(t, meta, edit)
			if readFile(t, meta) == before {
				t.Fatal("the edit did not apply")
			}
			if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
				t.Fatalf("Get = %v, want corruption", err)
			}
		})
	}
}

func TestFactReasonsRoundTripAndStayCoherent(t *testing.T) {
	r, _, h := newStore(t)
	acquired := acquiredPair(h)
	denied := []runcache.ExternalFact{runcache.UnavailableFact(runcache.FactDenied), runcache.UnavailableFact(runcache.FactDenied)}
	changed := []runcache.ExternalFact{acquired[0], runcache.AcquiredFact(h.HashBytes([]byte("tool 2.0")))}
	lost := []runcache.ExternalFact{acquired[0], runcache.UnavailableFact(runcache.FactTimedOut)}
	cases := []struct {
		name   string
		pre    []runcache.ExternalFact
		post   []runcache.ExternalFact
		kind   runcache.ReuseKind
		reason runcache.MismatchReason
		ok     bool
	}{
		{"pre-run unavailable", denied, nil, runcache.ReuseRecordOnly, runcache.ReasonExternalFactsUnavailable, true},
		{"post-run changed", acquired, changed, runcache.ReuseNonReusable, runcache.ReasonExternalFactsChanged, true},
		{"post-run unavailable", acquired, lost, runcache.ReuseNonReusable, runcache.ReasonExternalFactsUnavailablePostRun, true},
		{"unavailable reason with complete facts", acquired, nil, runcache.ReuseRecordOnly, runcache.ReasonExternalFactsUnavailable, false},
		{"changed reason with agreeing facts", acquired, acquired, runcache.ReuseNonReusable, runcache.ReasonExternalFactsChanged, false},
		{"reacquired after an unavailable prestate", denied, acquired, runcache.ReuseRecordOnly, runcache.ReasonExternalFactsUnavailable, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reuse, err := runcache.NewReuseState(tc.kind, tc.reason)
			if err != nil {
				t.Fatal(err)
			}
			ki := probedKeyInput(t, h, tc.pre)
			adjust := func(e *runcache.RunEntry) {
				e.Reuse = reuse
				e.Decision = runcache.CacheDecision{Reason: tc.reason.String()}
				e.PostRunFacts = tc.post
			}
			stored, err := commitRunWith(t, r, h, ki, "out", "", 1<<20, unchangedObs(), adjust)
			if !tc.ok {
				if err == nil {
					t.Fatal("an incoherent fact record was committed")
				}
				return
			}
			if err != nil {
				t.Fatalf("Commit: %v", err)
			}
			got, err := r.Get(stored.ID)
			if err != nil || got.Reuse != reuse || !slices.Equal(got.PostRunFacts, tc.post) {
				t.Fatalf("Get = %v reuse %v post %v, want %v %v", err, got.Reuse, got.PostRunFacts, reuse, tc.post)
			}
		})
	}
}
