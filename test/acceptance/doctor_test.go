package acceptance

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// doctorEnvelope decodes the doctor --json envelope and its data payload.
type doctorEnvelope struct {
	SchemaVersion int    `json:"schema_version"`
	Command       string `json:"command"`
	Data          struct {
		Health  string `json:"health"`
		Summary struct {
			Passed     int `json:"passed"`
			Warnings   int `json:"warnings"`
			Errors     int `json:"errors"`
			Repairable int `json:"repairable"`
			Repaired   int `json:"repaired"`
		} `json:"summary"`
		Findings []struct {
			Code       string `json:"code"`
			Severity   string `json:"severity"`
			Subsystem  string `json:"subsystem"`
			Path       string `json:"path"`
			Subject    string `json:"subject"`
			Message    string `json:"message"`
			Repairable bool   `json:"repairable"`
			Repaired   bool   `json:"repaired"`
		} `json:"findings"`
		StoreCapacity struct {
			Scope             string   `json:"scope"`
			Metric            string   `json:"metric"`
			Measurement       string   `json:"measurement"`
			ObservedBytes     *int64   `json:"observed_bytes"`
			ThresholdBytes    *int64   `json:"threshold_bytes"`
			IncompleteReasons []string `json:"incomplete_reasons"`
		} `json:"store_capacity"`
	} `json:"data"`
}

func TestDoctorHealthyProject(t *testing.T) {
	root := initProject(t)
	write(t, root, "calc.go", "package calc")
	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}

	code, stdout, _ := awa(t, root, "doctor")
	if code != 0 {
		t.Fatalf("doctor exit = %d, want 0; stdout=%q", code, stdout)
	}
	if !strings.Contains(stdout, "health: ok") {
		t.Fatalf("expected health: ok, got %q", stdout)
	}
}

func TestDoctorJSONEnvelope(t *testing.T) {
	root := initProject(t)
	code, stdout, _ := awa(t, root, "doctor", "--json")
	if code != 0 {
		t.Fatalf("doctor --json exit = %d", code)
	}
	var env doctorEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid doctor JSON: %v\n%s", err, stdout)
	}
	if env.SchemaVersion != 1 || env.Command != "doctor" {
		t.Fatalf("unexpected envelope: %+v", env)
	}
	if env.Data.Health != "ok" {
		t.Fatalf("health = %q, want ok", env.Data.Health)
	}
	sc := env.Data.StoreCapacity
	if sc.Scope != ".awa" || sc.Metric != "logical-bytes" || sc.Measurement != "exact" ||
		sc.ObservedBytes == nil || *sc.ObservedBytes <= 0 || sc.ThresholdBytes == nil || *sc.ThresholdBytes != 1<<30 ||
		sc.IncompleteReasons == nil || len(sc.IncompleteReasons) != 0 {
		t.Fatalf("store_capacity = %+v, want an exact positive size against the 1GiB default", sc)
	}
}

// TestDoctorStoreCapacityAdvisory drives the advisory through real config layers: a
// shared threshold the store exceeds warns with exit 0 and no repair, a local "off"
// overrides it and walks nothing, and zero is rejected as invalid config.
func TestDoctorStoreCapacityAdvisory(t *testing.T) {
	root := initProject(t)
	write(t, root, "awa.toml", "[doctor]\nstore_warning_size = \"16B\"\n")

	code, stdout, _ := awa(t, root, "doctor", "--json")
	if code != 0 {
		t.Fatalf("over-threshold doctor exit = %d, want 0\n%s", code, stdout)
	}
	var env doctorEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid doctor JSON: %v\n%s", err, stdout)
	}
	if env.Data.Health != "warning" || len(env.Data.Findings) != 1 {
		t.Fatalf("health %q findings %+v, want one warning", env.Data.Health, env.Data.Findings)
	}
	f := env.Data.Findings[0]
	if f.Code != "store-capacity-threshold" || f.Severity != "warning" || f.Repairable || f.Subsystem != "capacity" {
		t.Fatalf("finding = %+v, want a non-repairable capacity warning", f)
	}
	if th := env.Data.StoreCapacity.ThresholdBytes; th == nil || *th != 16 {
		t.Fatalf("threshold_bytes = %v, want 16", th)
	}
	if code, stdout, _ := awa(t, root, "doctor", "--repair"); code != 0 || !strings.Contains(stdout, "store-capacity-threshold") {
		t.Fatalf("doctor --repair exit = %d, want 0 with the warning still reported\n%s", code, stdout)
	}

	write(t, root, ".awa/config.toml", "[doctor]\nstore_warning_size = \"off\"\n")
	code, stdout, _ = awa(t, root, "doctor", "--json")
	if code != 0 {
		t.Fatalf("disabled doctor exit = %d", code)
	}
	env = doctorEnvelope{}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatal(err)
	}
	sc := env.Data.StoreCapacity
	if sc.Measurement != "disabled" || sc.ObservedBytes != nil || sc.ThresholdBytes != nil || strings.Contains(stdout, "store-capacity-threshold") {
		t.Fatalf("disabled store_capacity = %+v findings %+v, want disabled with null figures", sc, env.Data.Findings)
	}
	if code, stdout, _ := awa(t, root, "doctor"); code != 0 || !strings.Contains(stdout, "store: .awa/ size advisory off") {
		t.Fatalf("disabled human output (exit %d):\n%s", code, stdout)
	}

	write(t, root, ".awa/config.toml", "[doctor]\nstore_warning_size = \"0\"\n")
	if code, _, stderr := awa(t, root, "doctor"); code != 4 || !strings.Contains(stderr, "doctor.store_warning_size") {
		t.Fatalf("zero threshold: exit %d stderr %q, want config error 4 naming the key", code, stderr)
	}
}

func TestDoctorDeletedBlobReportsCorruption(t *testing.T) {
	root := initProject(t)
	write(t, root, "calc.go", "package calc")
	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	if blobCount(t, root) == 0 {
		t.Fatal("expected at least one blob")
	}
	// Delete every stored blob: the checkpoint now references missing content.
	blobsDir := filepath.Join(root, ".awa", "store", "blobs")
	if err := filepath.WalkDir(blobsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return os.Remove(p)
		}
		return nil
	}); err != nil {
		t.Fatalf("removing blobs: %v", err)
	}

	code, stdout, _ := awa(t, root, "doctor", "--json")
	if code != 5 {
		t.Fatalf("doctor exit = %d, want 5 (storage corruption); stdout=%q", code, stdout)
	}
	var env doctorEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid doctor JSON: %v\n%s", err, stdout)
	}
	if env.Data.Health != "failed" {
		t.Fatalf("health = %q, want failed", env.Data.Health)
	}
	found := false
	for _, f := range env.Data.Findings {
		if f.Code == "checkpoint-missing-blob" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a checkpoint-missing-blob finding, got %+v", env.Data.Findings)
	}
}

func TestDoctorMalformedCheckpointMetadata(t *testing.T) {
	root := initProject(t)
	write(t, root, "calc.go", "package calc")
	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	meta := checkpointMetaFile(t, root)
	// Published metadata is read-only; remove then rewrite it as garbage.
	if err := os.Remove(meta); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, _ := awa(t, root, "doctor")
	if code != 5 {
		t.Fatalf("doctor exit = %d, want 5; stdout=%q", code, stdout)
	}
	if !strings.Contains(stdout, "checkpoint-corrupt") {
		t.Fatalf("expected checkpoint-corrupt in report, got %q", stdout)
	}
}

func TestDoctorMalformedRunMetadata(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	if code, _, stderr := awa(t, root, "run", "--", h, "-out", "hello"); code != 0 {
		t.Fatalf("run exit = %d, stderr = %q", code, stderr)
	}

	// Corrupt the single run's metadata document.
	runMeta := singleRunMetaFile(t, root)
	if err := os.Remove(runMeta); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runMeta, []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, _ := awa(t, root, "doctor")
	if code != 5 {
		t.Fatalf("doctor exit = %d, want 5; stdout=%q", code, stdout)
	}
	if !strings.Contains(stdout, "run-corrupt") {
		t.Fatalf("expected run-corrupt in report, got %q", stdout)
	}
}

func TestDoctorFreshTempNotRepaired(t *testing.T) {
	root := initProject(t)
	// A just-created temp artifact may belong to an active operation, so doctor reports
	// it as a non-repairable warning (exit 0) and --repair must not delete it.
	fresh := filepath.Join(root, ".awa", "store", "tmp", ".active-xyz")
	if err := os.WriteFile(fresh, []byte("in progress"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ := awa(t, root, "doctor", "--repair")
	if code != 0 {
		t.Fatalf("doctor --repair exit = %d, want 0 for a fresh temp warning; stdout=%q", code, stdout)
	}
	if !strings.Contains(stdout, "orphan-temp") {
		t.Fatalf("expected orphan-temp finding, got %q", stdout)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("a fresh temp artifact must not be removed: %v", err)
	}
}

func TestDoctorStaleTempRepairedAndIdempotent(t *testing.T) {
	root := initProject(t)
	orphan := filepath.Join(root, ".awa", "store", "tmp", ".orphan-xyz")
	if err := os.WriteFile(orphan, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Backdate it well past the stale threshold so it is provably a crash leftover,
	// not an active operation, and therefore safely repairable.
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}

	// Reported but not repaired without --repair (exit 5: unrepaired repairable).
	code, stdout, _ := awa(t, root, "doctor")
	if code != 5 {
		t.Fatalf("doctor exit = %d, want 5; stdout=%q", code, stdout)
	}
	if !strings.Contains(stdout, "orphan-temp") {
		t.Fatalf("expected orphan-temp finding, got %q", stdout)
	}

	// --repair removes it and exits 0.
	code, stdout, _ = awa(t, root, "doctor", "--repair")
	if code != 0 {
		t.Fatalf("doctor --repair exit = %d, want 0; stdout=%q", code, stdout)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan temp still present after repair: %v", err)
	}

	// A second doctor run is clean.
	if code, _, _ := awa(t, root, "doctor"); code != 0 {
		t.Fatalf("second doctor exit = %d, want 0", code)
	}
}

func TestDoctorUnknownLockNotRemoved(t *testing.T) {
	root := initProject(t)
	lockPath := filepath.Join(root, ".awa", "locks", "weird.lock")
	if err := os.WriteFile(lockPath, []byte("garbage not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Read-only doctor reports the unrecognized lock as a warning-only finding (exit 0).
	code, stdout, _ := awa(t, root, "doctor")
	if !strings.Contains(stdout, "lock-unknown") {
		t.Fatalf("expected lock-unknown finding, got %q", stdout)
	}
	if code != 0 {
		t.Fatalf("doctor exit = %d, want 0 for warning-only; stdout=%q", code, stdout)
	}

	// doctor --repair fails closed on an unrecognized lock rather than mutating the
	// store next to a writer it cannot interpret. Either way it must not remove the lock.
	if rcode, _, _ := awa(t, root, "doctor", "--repair"); rcode == 0 {
		t.Fatalf("doctor --repair should fail closed on an unrecognized lock, got exit 0")
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("unknown lock must not be removed: %v", err)
	}
}

func TestDoctorAwaTrackedByGitWarning(t *testing.T) {
	root := initProject(t)
	// The default profile writes no config file; write one so there is a real
	// file under .awa for git to track (git does not track empty directories).
	writeProjectConfig(t, root, "[diff]\nalgorithm = \"histogram\"\n")
	// Make root a git repo and stage .awa so it appears tracked.
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "test@example.com")
	runGit(t, root, "config", "user.name", "Test")
	runGit(t, root, "add", "-f", ".awa")

	code, stdout, _ := awa(t, root, "doctor")
	if !strings.Contains(stdout, "awa-tracked-by-git") {
		t.Fatalf("expected awa-tracked-by-git warning, got %q", stdout)
	}
	// A warning alone keeps exit 0.
	if code != 0 {
		t.Fatalf("doctor exit = %d, want 0 for a git-tracking warning; stdout=%q", code, stdout)
	}
}

// singleRunMetaFile returns the entry metadata document of the single run. Runs
// are stored sharded under .awa/runs/entries/<shard>/<id>/meta.json; the keys/
// subtree holds separate pointer documents that this skips, so a corruption test
// tampers with the entry metadata itself.
func singleRunMetaFile(t *testing.T, root string) string {
	t.Helper()
	entriesDir := filepath.Join(root, ".awa", "runs", "entries")
	var candidates []string
	err := filepath.WalkDir(entriesDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "meta.json" {
			candidates = append(candidates, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking run entries: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("want exactly one run entry meta.json, found %d: %v", len(candidates), candidates)
	}
	return candidates[0]
}

// TestDoctorWatchedRootAudit drives the opt-in audit end to end: plain doctor reports
// none, --capacity measures a watched root and warns at the configured threshold with
// exit 0, --repair --capacity removes no output, and path operands are refused.
func TestDoctorWatchedRootAudit(t *testing.T) {
	root := initProject(t)
	write(t, root, "awa.toml", "[doctor]\nwatched_root_warning_size = \"64B\"\n")
	write(t, root, "dist/bundle.js", strings.Repeat("x", 100))

	code, stdout, _ := awa(t, root, "doctor", "--json")
	if code != 0 || !strings.Contains(stdout, `"watched_root_capacity": null`) {
		t.Fatalf("plain doctor (exit %d) must carry a null audit:\n%s", code, stdout)
	}

	code, stdout, _ = awa(t, root, "doctor", "--capacity", "--json")
	if code != 0 {
		t.Fatalf("doctor --capacity exit = %d, want 0\n%s", code, stdout)
	}
	var env struct {
		Data struct {
			Health   string `json:"health"`
			Findings []struct {
				Code       string `json:"code"`
				Subject    string `json:"subject"`
				Repairable bool   `json:"repairable"`
			} `json:"findings"`
			StoreCapacity *struct {
				Measurement string `json:"measurement"`
			} `json:"store_capacity"`
			Watched *struct {
				Search struct {
					Complete bool `json:"complete"`
				} `json:"search"`
				RootsTotal *int `json:"roots_total"`
				Roots      []struct {
					Path          string `json:"path"`
					Measurement   string `json:"measurement"`
					ObservedBytes *int64 `json:"observed_bytes"`
				} `json:"roots"`
			} `json:"watched_root_capacity"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid doctor JSON: %v\n%s", err, stdout)
	}
	w := env.Data.Watched
	if w == nil || !w.Search.Complete || w.RootsTotal == nil || *w.RootsTotal != 1 || len(w.Roots) != 1 ||
		w.Roots[0].Path != "dist" || w.Roots[0].Measurement != "exact" || w.Roots[0].ObservedBytes == nil || *w.Roots[0].ObservedBytes != 100 {
		t.Fatalf("watched_root_capacity = %+v, want dist exact at 100 bytes from a complete search", w)
	}
	if env.Data.StoreCapacity == nil || env.Data.StoreCapacity.Measurement != "exact" {
		t.Errorf("store_capacity = %+v, want the store measurement unchanged beside the audit", env.Data.StoreCapacity)
	}
	if env.Data.Health != "warning" || len(env.Data.Findings) != 1 || env.Data.Findings[0].Code != "watched-root-capacity-threshold" ||
		env.Data.Findings[0].Subject != "dist" || env.Data.Findings[0].Repairable {
		t.Fatalf("health %q findings %+v, want one non-repairable watched-root warning", env.Data.Health, env.Data.Findings)
	}

	code, stdout, _ = awa(t, root, "doctor", "--repair", "--capacity")
	if code != 0 || !strings.Contains(stdout, "watched roots: 1 found (search complete)") {
		t.Fatalf("doctor --repair --capacity exit = %d\n%s", code, stdout)
	}
	if data, err := os.ReadFile(filepath.Join(root, "dist", "bundle.js")); err != nil || len(data) != 100 {
		t.Fatalf("dist/bundle.js after --repair --capacity: %d bytes, %v; want it untouched", len(data), err)
	}

	for _, args := range [][]string{{"doctor", "--capacity", "dist"}, {"doctor", "--capacity=yes"}} {
		if code, _, _ := awa(t, root, args...); code != 2 {
			t.Errorf("awa %v exit = %d, want usage error 2", args, code)
		}
	}
}
