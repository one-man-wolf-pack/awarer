package runstore_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"awarer/internal/domain/runcache"
	"awarer/internal/domain/runevidence"
	"awarer/internal/infra/runstore"
)

// metaPathOf returns the on-disk meta.json path for a stored run.
func metaPathOf(t *testing.T, layoutDir string, id runcache.RunID) string {
	t.Helper()
	s := id.String()
	return filepath.Join(layoutDir, "entries", s[:2], s, "meta.json")
}

// TestRunObservationScanConfigRoundTrips proves the persisted observation identity:
// the scanner's scan_config_hash captured on the before/after observations survives
// store → Get and is returned through the typed observation read the resolver uses.
func TestRunObservationScanConfigRoundTrips(t *testing.T) {
	r, _, h := newStore(t)
	entry := storeRun(t, r, h, "k1", "out", "err", 1<<20)

	got, err := r.Get(entry.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := testScanCfg()
	if got.Before == nil || got.Before.ScanConfigHash != want {
		t.Errorf("before scan config hash = %v, want %v", got.Before, want)
	}
	if got.After == nil || got.After.ScanConfigHash != want {
		t.Errorf("after scan config hash = %v, want %v", got.After, want)
	}

	// The typed observation read the state resolver consumes carries the same identity.
	read, err := r.Observation(entry.ID, false)
	if err != nil {
		t.Fatalf("Observation: %v", err)
	}
	if read.ScanConfigHash() != want {
		t.Errorf("observation read scan config hash = %v, want %v", read.ScanConfigHash(), want)
	}
	if read.TreeHash() != got.Before.Manifest.TreeHash {
		t.Errorf("observation read tree hash = %v, want %v", read.TreeHash(), got.Before.Manifest.TreeHash)
	}
}

// TestNonCurrentSchemaClassifiedIncompatible pins the formative-mode rule: there is
// one current record shape and no reader for any other. A record declaring a
// different schema is refused as incompatible — never decoded through the current
// type, never served as a hit, and never mistaken for corruption.
//
// The probes are a version below and a version above the current one, which is the
// whole rule: the classifier keys on "not the current version", not on a list of
// versions it has met before. Nothing here names a historical generation, because
// this build has no relationship with one.
func TestNonCurrentSchemaClassifiedIncompatible(t *testing.T) {
	for _, other := range []int{runstore.MetaSchemaVersionForTest - 1, runstore.MetaSchemaVersionForTest + 1} {
		t.Run(fmt.Sprintf("schema%d", other), func(t *testing.T) {
			r, layout, h := newStore(t)
			entry := storeRun(t, r, h, fmt.Sprintf("s%d", other), "out", "", 1<<20)
			meta := metaPathOf(t, layout.RunsDir(), entry.ID)

			data, err := os.ReadFile(meta)
			if err != nil {
				t.Fatalf("read meta: %v", err)
			}
			restamped := strings.Replace(string(data),
				fmt.Sprintf("\"schema_version\": %d", runstore.MetaSchemaVersionForTest),
				fmt.Sprintf("\"schema_version\": %d", other), 1)
			if restamped == string(data) {
				t.Fatalf("expected to find the current schema_version %d to restamp", runstore.MetaSchemaVersionForTest)
			}
			if err := os.Chmod(meta, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(meta, []byte(restamped), 0o444); err != nil {
				t.Fatal(err)
			}

			_, got := r.Get(entry.ID)
			if !errors.Is(got, runcache.ErrIncompatibleEntry) {
				t.Fatalf("Get a schema-%d entry err = %v, want ErrIncompatibleEntry", other, got)
			}
			if errors.Is(got, runcache.ErrCorruptStore) {
				t.Errorf("an incompatible schema was classified as corruption: %v", got)
			}
			if _, ok, err := r.Lookup(entry.Key); err != nil || ok {
				t.Errorf("Lookup of a schema-%d entry = (ok=%v, err=%v), want a clean miss", other, ok, err)
			}
		})
	}
}

// TestForgedObservationScanConfigRejected proves an observation whose scan-config
// identity is blanked out (a forged/zeroed policy hash) does not decode into a
// weakened partial identity: it fails loud as corruption.
func TestForgedObservationScanConfigRejected(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "forged", "out", "", 1<<20)
	meta := metaPathOf(t, layout.RunsDir(), entry.ID)

	data, err := os.ReadFile(meta)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	// Blank the persisted scan_config_hash on the observations; an observation must
	// carry a non-zero policy hash, so this is unresolvable.
	forged := strings.ReplaceAll(string(data), testScanCfg().String(), "")
	if forged == string(data) {
		t.Fatal("expected to find the scan_config_hash to blank out")
	}
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte(forged), 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Fatalf("Get forged-scan-config entry err = %v, want ErrCorruptStore", err)
	}
}

// TestOutputInspectabilityIsByteFree is the poison-payload proof for metadata
// inspection: OutputInspectability stats the stored streams without reading their
// bytes, so a payload whose content is corrupted (wrong bytes) is still reported
// present — presence is readability, never a byte-integrity claim. A removed payload
// is reported missing. Neither is an error.
func TestOutputInspectabilityIsByteFree(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "insp", "hello stdout", "hello stderr", 1<<20)

	// Corrupt the stdout bytes in place (same-descriptor verification would reject this).
	stdoutPath := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String(), "stdout.log")
	if err := os.Chmod(stdoutPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stdoutPath, []byte("tampered bytes, wrong content"), 0o644); err != nil {
		t.Fatalf("tamper stdout: %v", err)
	}
	// Remove the stderr payload entirely.
	stderrPath := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String(), "stderr.log")
	if err := os.Remove(stderrPath); err != nil {
		t.Fatalf("remove stderr: %v", err)
	}

	stdout, stderr, err := r.OutputInspectability(entry.ID)
	if err != nil {
		t.Fatalf("OutputInspectability must not fail on a corrupt/missing payload: %v", err)
	}
	if stdout.Presence() != runevidence.PresencePresent {
		t.Errorf("tampered stdout presence = %s, want present (bytes never read)", stdout.Presence())
	}
	if stdout.Integrity() != runevidence.IntegrityUnverified {
		t.Errorf("stdout integrity = %s, want unverified", stdout.Integrity())
	}
	if stderr.Presence() != runevidence.PresenceMissing {
		t.Errorf("removed stderr presence = %s, want missing", stderr.Presence())
	}

	// The explicit output path still verifies bytes and fails loud on the tamper.
	if _, err := r.OpenStdout(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("OpenStdout over tampered bytes err = %v, want ErrCorruptStore", err)
	}
}

// TestOutputInspectabilityNeverReadsPayloadBytes is the deterministic read-poison proof:
// a payload whose bytes cannot be read (mode 000) but whose entry can still be stat'd is
// reported present. If inspection opened the payload for reading it would fail with a
// permission error and classify unreadable; that it reports present proves the stat-only
// capability never attempts a read. Skipped as root, which bypasses the mode bits.
func TestOutputInspectabilityNeverReadsPayloadBytes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file mode bits, so a 000 payload would still be readable")
	}
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "poison", "secret stdout bytes", "err", 1<<20)

	stdoutPath := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String(), "stdout.log")
	if err := os.Chmod(stdoutPath, 0o000); err != nil {
		t.Fatalf("chmod 000: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(stdoutPath, 0o600) }) // let the temp dir clean up

	// Sanity: an actual read of the poisoned payload does fail, so "present" below can only
	// mean inspection never read it.
	if f, err := os.Open(stdoutPath); err == nil {
		_ = f.Close()
		t.Fatal("precondition: a 000 payload must not be openable for reading")
	}

	stdout, _, err := r.OutputInspectability(entry.ID)
	if err != nil {
		t.Fatalf("OutputInspectability on an unreadable-bytes payload: %v", err)
	}
	if stdout.Presence() != runevidence.PresencePresent {
		t.Errorf("unreadable-bytes stdout presence = %s, want present (stat only, never read)", stdout.Presence())
	}
}

// TestOutputInspectabilitySurvivesConcurrentGC proves inspection stats the canonical
// payload files directly and never reads metadata: a run whose entire entry (metadata
// included) is GC-removed after the caller loaded its record degrades to two missing
// streams, not a hard not-found error. This is the concurrent-GC contract the evidence
// builder relies on.
func TestOutputInspectabilitySurvivesConcurrentGC(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "gc-race", "out", "err", 1<<20)

	// Simulate a concurrent GC that removes the whole entry directory after the caller
	// has already loaded the record (metadata + payloads vanish together).
	entryDir := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String())
	if err := os.RemoveAll(entryDir); err != nil {
		t.Fatalf("remove entry dir: %v", err)
	}

	stdout, stderr, err := r.OutputInspectability(entry.ID)
	if err != nil {
		t.Fatalf("OutputInspectability must not fail when the entry was GC-removed: %v", err)
	}
	if stdout.Presence() != runevidence.PresenceMissing {
		t.Errorf("GC-removed stdout presence = %s, want missing", stdout.Presence())
	}
	if stderr.Presence() != runevidence.PresenceMissing {
		t.Errorf("GC-removed stderr presence = %s, want missing", stderr.Presence())
	}
}

// TestOutputInspectabilityRejectsZeroID proves the id guard: a zero run id is refused
// with an error rather than reaching the shard-path derivation, whose id.String()[:2]
// would panic on the empty hex of a zero id.
func TestOutputInspectabilityRejectsZeroID(t *testing.T) {
	r, _, _ := newStore(t)
	if _, _, err := r.OutputInspectability(runcache.RunID{}); err == nil {
		t.Error("OutputInspectability must reject a zero run id")
	}
}
