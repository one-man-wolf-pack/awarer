package runstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/runcache"
	"awarer/internal/infra/runstore"
)

// storeRecordOnly publishes a record-only run (no key pointer) with full before/
// after observations, the shape a --record or --no-cache execution records.
func storeRecordOnly(t *testing.T, r *runstore.Repo, h hashing.Hasher, disc string) runcache.RunEntry {
	t.Helper()
	reuse, err := runcache.RecordOnly(runcache.ReasonRecordOnly)
	if err != nil {
		t.Fatal(err)
	}
	return storeUncacheable(t, r, h, disc, reuse, mustUnchanged(), mustUnchangedEffect(), unchangedObs())
}

// storeNonReusable publishes a run that was eligible to cache but disqualified after
// the fact by the effect guard: its watched generated-output state changed, so it is
// durable history and never a hit. Its observations are a full before/after pair —
// the input scope was unchanged; only the effect scope moved.
func storeNonReusable(t *testing.T, r *runstore.Repo, h hashing.Hasher, disc string) runcache.RunEntry {
	t.Helper()
	reuse, err := runcache.NonReusableRun(runcache.ReasonEffectStateDiffers)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := runcache.NewEffectGuardStatus(runcache.EffectGuardChanged)
	if err != nil {
		t.Fatal(err)
	}
	return storeUncacheable(t, r, h, disc, reuse, mustUnchanged(), guard, unchangedObs())
}

// storePostScanFailed publishes a run whose post-execution scan failed: it observed
// its pre-run state and nothing after, the one shape in which a missing
// after-observation is a current state rather than corruption.
func storePostScanFailed(t *testing.T, r *runstore.Repo, h hashing.Hasher, disc string) runcache.RunEntry {
	t.Helper()
	mutation, err := runcache.NewMutationStatus(runcache.MutationScanFailed)
	if err != nil {
		t.Fatal(err)
	}
	obs := unchangedObs()
	obs.After = nil
	obs.AfterScanConfigHash = hashing.ConfigHash{}
	return storeUncacheable(t, r, h, disc, runcache.UnknownPostState(), mutation, mustUnchangedEffect(), obs)
}

// storeUncacheable is the single commit path the non-hit fixtures share, so a test
// about one uncacheable kind cannot drift from another on how a run is stored.
func storeUncacheable(t *testing.T, r *runstore.Repo, h hashing.Hasher, disc string, reuse runcache.ReuseState, mutation runcache.MutationStatus, guard runcache.EffectGuardStatus, obs runcache.RunObservations) runcache.RunEntry {
	t.Helper()
	pending, err := r.Begin(runcache.CaptureLimits{MaxStdout: 1 << 20, MaxStderr: 1 << 20})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	_, _ = io.WriteString(pending.Stdout(), "out")
	_, _ = io.WriteString(pending.Stderr(), "")
	so, se, err := pending.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	id, err := runcache.NewRunID(time.Now().UnixNano(), strings.NewReader(strings.Repeat(disc, 32)[:32]))
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	ki := keyInputFor(h, disc)
	start := time.Now()
	entry := runcache.RunEntry{
		ID:          id,
		Key:         ki.Compute(h),
		KeyInput:    ki,
		StartedAt:   start,
		FinishedAt:  start.Add(time.Second),
		Exit:        runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 0},
		Stdout:      so,
		Stderr:      se,
		Decision:    runcache.CacheDecision{Cacheable: false, Reason: reuse.Reason().String()},
		Reuse:       reuse,
		Mutation:    mutation,
		EffectGuard: guard,
	}
	if err := pending.Commit(context.Background(), entry, obs); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return entry
}

// tamperMeta rewrites a stored run's meta.json through a mutation on its decoded
// JSON map, so a hostile-input test can edit a single field without hand-authoring
// the whole document.
func tamperMeta(t *testing.T, r *runstore.Repo, id runcache.RunID, mutate func(m map[string]any)) {
	t.Helper()
	meta := filepath.Join(r.EntryPath(id), "meta.json")
	data, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	mutate(m)
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, append(out, '\n'), 0o444); err != nil {
		t.Fatal(err)
	}
}

// TestObservationCardinalityRoundTrips walks every kind of entry a real execution
// publishes and pins the one current cardinality rule end to end: a before-observation
// always, an after-observation exactly when the post-run scan succeeded. A
// post-scan-failed run is the sole missing-after shape, and resolving its after is an
// honest typed unavailability rather than corruption.
func TestObservationCardinalityRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		name      string
		store     func(*testing.T, *runstore.Repo, hashing.Hasher, string) runcache.RunEntry
		wantAfter bool
	}{
		{"reusable", func(t *testing.T, r *runstore.Repo, h hashing.Hasher, disc string) runcache.RunEntry {
			return storeRun(t, r, h, disc, "out", "", 1<<20)
		}, true},
		{"non-reusable", storeNonReusable, true},
		{"record-only", storeRecordOnly, true},
		{"post-scan-failed", storePostScanFailed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, h := newStore(t)
			entry := tc.store(t, r, h, tc.name)

			got, err := r.Get(entry.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Before == nil {
				t.Error("a stored run must round trip with its before-observation")
			}
			if (got.After != nil) != tc.wantAfter {
				t.Errorf("after-observation present = %v, want %v", got.After != nil, tc.wantAfter)
			}
			if _, err := r.OpenBeforeManifest(entry.ID); err != nil {
				t.Errorf("OpenBeforeManifest: %v", err)
			}
			_, err = r.OpenAfterManifest(entry.ID)
			switch {
			case tc.wantAfter && err != nil:
				t.Errorf("OpenAfterManifest: %v", err)
			case !tc.wantAfter && !errors.Is(err, runcache.ErrObservationUnavailable):
				t.Errorf("OpenAfterManifest on a post-scan-failed run err = %v, want ErrObservationUnavailable", err)
			}
		})
	}
}

func TestDecodeRejectsMissingBefore(t *testing.T) {
	r, _, h := newStore(t)
	entry := storeRun(t, r, h, "nobefore", "out", "", 1<<20)
	// Drop the before-observation: every real execution observed pre-run state, so a
	// record claiming the current schema without one is corrupt.
	tamperMeta(t, r, entry.ID, func(m map[string]any) {
		m["observations"].(map[string]any)["before"] = nil
	})
	if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("Get on an entry missing its before-observation err = %v, want ErrCorruptStore", err)
	}
}

func TestDecodeRejectsReusableWithoutObservations(t *testing.T) {
	r, _, h := newStore(t)
	entry := storeRun(t, r, h, "noobs", "out", "", 1<<20)
	tamperMeta(t, r, entry.ID, func(m map[string]any) {
		obs := m["observations"].(map[string]any)
		obs["before"] = nil
		obs["after"] = nil
	})
	if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("Get on a reusable entry without observations err = %v, want ErrCorruptStore", err)
	}
}

func TestDecodeRejectsReusableUnobserved(t *testing.T) {
	r, _, h := newStore(t)
	entry := storeRun(t, r, h, "unobserved", "out", "", 1<<20)
	// A reusable run must have left observed state explicitly unchanged; an unobserved
	// outcome never proved that, so it must not decode as a reusable hit candidate.
	tamperMeta(t, r, entry.ID, func(m map[string]any) {
		m["mutation"] = "unobserved"
	})
	if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("Get on a reusable+unobserved entry err = %v, want ErrCorruptStore", err)
	}
}

// TestDecodeRejectsWrongAfterCardinality tampers each direction of the cardinality
// rule on a record whose other invariants stay satisfied, so the hostile-boundary
// check is what rejects the document rather than a reuse or mutation cross-check
// tripping first.
func TestDecodeRejectsWrongAfterCardinality(t *testing.T) {
	t.Run("missing after when the scan succeeded", func(t *testing.T) {
		r, _, h := newStore(t)
		entry := storeRecordOnly(t, r, h, "noafter")
		tamperMeta(t, r, entry.ID, func(m map[string]any) {
			m["observations"].(map[string]any)["after"] = nil
		})
		if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
			t.Errorf("Get on an entry missing its after-observation err = %v, want ErrCorruptStore", err)
		}
	})
	t.Run("after present when the scan failed", func(t *testing.T) {
		r, _, h := newStore(t)
		entry := storePostScanFailed(t, r, h, "afterscanfail")
		// A scan-failed run has no knowable post-state, so any after-observation on it is
		// contradictory — here one echoing the before it does legitimately carry.
		tamperMeta(t, r, entry.ID, func(m map[string]any) {
			obs := m["observations"].(map[string]any)
			after := map[string]any{}
			for k, v := range obs["before"].(map[string]any) {
				after[k] = v
			}
			after["file"] = "after.manifest.jsonl"
			obs["after"] = after
		})
		if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
			t.Errorf("Get on a scan-failed entry that carries an after-observation err = %v, want ErrCorruptStore", err)
		}
	})
}

func TestRecordOnlyHasNoKeyPointer(t *testing.T) {
	r, _, h := newStore(t)
	entry := storeRecordOnly(t, r, h, "rec")

	// The record-only run is durable history (Get succeeds)...
	if _, err := r.Get(entry.ID); err != nil {
		t.Fatalf("Get record-only run: %v", err)
	}
	// ...but it has no key pointer, so a Lookup of its key is a clean miss.
	if _, ok, err := r.Lookup(entry.Key); err != nil || ok {
		t.Errorf("Lookup of a record-only run's key = (ok=%v, err=%v), want a clean miss", ok, err)
	}
}

func TestLookupRejectsPointerToNonReusableRun(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRecordOnly(t, r, h, "rec")

	// Hand-plant a key pointer that targets the non-reusable record-only run, the
	// corruption the read-boundary guard must reject rather than replay as a hit.
	key := entry.Key
	shard := filepath.Join(layout.RunsDir(), "keys", hashing.Namespace, key.Hex()[:2])
	if err := os.MkdirAll(shard, 0o755); err != nil {
		t.Fatal(err)
	}
	ptr := `{
  "schema_version": 1,
  "run_id": "` + entry.ID.String() + `"
}
`
	if err := os.WriteFile(filepath.Join(shard, key.Hex()+".json"), []byte(ptr), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := r.Lookup(key); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("Lookup of a pointer to a non-reusable run err = %v, want ErrCorruptStore", err)
	}
}

func TestInspectKeyPointersFlagsNonReusableTarget(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRecordOnly(t, r, h, "rec")

	// Plant a pointer at the record-only run's key. doctor's pointer scan must report
	// it: a pointer to a non-reusable run is a corrupt index, exactly what Lookup
	// would reject at run time.
	key := entry.Key
	shard := filepath.Join(layout.RunsDir(), "keys", hashing.Namespace, key.Hex()[:2])
	if err := os.MkdirAll(shard, 0o755); err != nil {
		t.Fatal(err)
	}
	ptr := "{\n  \"schema_version\": 1,\n  \"run_id\": \"" + entry.ID.String() + "\"\n}\n"
	if err := os.WriteFile(filepath.Join(shard, key.Hex()+".json"), []byte(ptr), 0o644); err != nil {
		t.Fatal(err)
	}

	problems, err := r.InspectKeyPointers()
	if err != nil {
		t.Fatalf("InspectKeyPointers: %v", err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0].Detail, "non-reusable") {
		t.Fatalf("want one non-reusable-target problem, got %+v", problems)
	}
}

func TestTamperedObservationManifestDetected(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "obs", "out", "", 1<<20)

	// Tamper the before-observation manifest (append a stray record line). The
	// verifying stream re-derives the tree hash on drain and must reject it.
	manifest := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String(), "before.manifest.jsonl")
	if err := os.Chmod(manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte(`{"entry":{"path":"x","kind":"file","content_hash":"blake3:00","content_storage":"hash-only","size":0,"mode":420,"mtime_ns":0,"omitted_stat_fields":["ctime","dev","ino","nlink"]}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stream, err := r.OpenBeforeManifest(entry.ID)
	if err != nil {
		t.Fatalf("OpenBeforeManifest: %v", err)
	}
	cur, err := stream.Open(context.Background())
	if err != nil {
		// A record-count mismatch may surface at Open or drain; either way it is corrupt.
		if !errors.Is(err, runcache.ErrCorruptStore) {
			t.Fatalf("Open: %v", err)
		}
		return
	}
	defer func() { _ = cur.Close() }()
	for cur.Next() {
	}
	if !errors.Is(cur.Err(), runcache.ErrCorruptStore) {
		t.Errorf("drain of a tampered manifest err = %v, want ErrCorruptStore", cur.Err())
	}
}

func TestGetRejectsTamperedObservationFileName(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "obsfile", "out", "", 1<<20)
	meta := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String(), "meta.json")
	data, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	// Re-point the before-observation at the after-observation file. Get must reject
	// the renamed stream identity, just as it does for stdout/stderr.
	tampered := strings.Replace(string(data), `"file": "before.manifest.jsonl"`, `"file": "after.manifest.jsonl"`, 1)
	if tampered == string(data) {
		t.Fatal("expected to find the before-observation file name to tamper")
	}
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte(tampered), 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("Get on a renamed observation file err = %v, want ErrCorruptStore", err)
	}
}

// TestInspectKeyPointersNamesAForeignNamespace proves a pointer filed under an
// identity namespace this build does not own is reported as exactly that, not as a
// key mismatch. The hex here matches its target run perfectly, so the only thing
// wrong is the namespace directory, and a diagnostic that blamed the key would send
// the reader after a problem that does not exist.
func TestInspectKeyPointersNamesAForeignNamespace(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "foreign-ns", "out", "", 1<<20)

	shard := filepath.Join(layout.RunsDir(), "keys", "sha256", entry.Key.Hex()[:2])
	if err := os.MkdirAll(shard, 0o755); err != nil {
		t.Fatal(err)
	}
	ptr := "{\n  \"schema_version\": 1,\n  \"run_id\": \"" + entry.ID.String() + "\"\n}\n"
	if err := os.WriteFile(filepath.Join(shard, entry.Key.Hex()+".json"), []byte(ptr), 0o644); err != nil {
		t.Fatal(err)
	}

	problems, err := r.InspectKeyPointers()
	if err != nil {
		t.Fatalf("InspectKeyPointers: %v", err)
	}
	var got string
	for _, p := range problems {
		if strings.Contains(p.Path, "sha256") {
			got = p.Detail
		}
	}
	if got == "" {
		t.Fatalf("no problem reported for the foreign-namespace pointer; got %+v", problems)
	}
	if !strings.Contains(got, "identity namespace") || !strings.Contains(got, "sha256") {
		t.Errorf("detail = %q, want it to name the foreign identity namespace", got)
	}
	if strings.Contains(got, "different key") {
		t.Errorf("detail = %q, but the key hex matches its target: the namespace is the only fault", got)
	}
}
