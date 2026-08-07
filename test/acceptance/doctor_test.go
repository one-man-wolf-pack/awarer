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
