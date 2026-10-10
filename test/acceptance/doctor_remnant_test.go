package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"awarer/internal/domain/checkpoint"
	"awarer/internal/infra/lockfile"
)

func headerlessCheckpoint(t *testing.T, root, file string) string {
	t.Helper()
	before := map[string]bool{}
	for _, id := range checkpointIDs(t, root) {
		before[id] = true
	}
	write(t, root, file, file)
	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	for _, id := range checkpointIDs(t, root) {
		if before[id] {
			continue
		}
		dir := filepath.Join(root, ".awa", "checkpoints", id)
		if err := os.Remove(filepath.Join(dir, "header.json")); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	t.Fatal("checkpoint published no new id")
	return ""
}

func headerlessDirs(t *testing.T, root string) []string {
	t.Helper()
	dir := filepath.Join(root, ".awa", "checkpoints")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading checkpoints dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if _, err := checkpoint.ParseCheckpointID(e.Name()); err != nil || !e.IsDir() {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, e.Name(), "header.json")); os.IsNotExist(err) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

func dirBytes(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string]string{}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(data)
	}
	return out
}

func assertDirBytes(t *testing.T, dir string, want map[string]string) {
	t.Helper()
	got := dirBytes(t, dir)
	if len(got) != len(want) {
		t.Fatalf("%s holds %d files, want %d", dir, len(got), len(want))
	}
	for name, data := range want {
		if got[name] != data {
			t.Fatalf("%s/%s changed", dir, name)
		}
	}
}

func TestDoctorIncompleteCheckpointDiagnosedThenRepaired(t *testing.T) {
	root := initProject(t)
	write(t, root, "kept.txt", "kept")
	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	keptID := checkpointIDs(t, root)[0]
	keptDir := filepath.Join(root, ".awa", "checkpoints", keptID)
	keptBytes := dirBytes(t, keptDir)

	payload := headerlessCheckpoint(t, root, "second.txt")
	if err := os.WriteFile(filepath.Join(payload, ".tmp-4242-1"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	payloadBytes := dirBytes(t, payload)
	empty := filepath.Join(root, ".awa", "checkpoints", mutateID(filepath.Base(payload)))
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}

	code, human, _ := awa(t, root, "doctor")
	if code != 5 {
		t.Fatalf("doctor exit = %d, want 5; stdout=%q", code, human)
	}
	if !strings.Contains(human, "health: failed") || strings.Contains(human, "corrupt") {
		t.Fatalf("want a failed verdict that claims no corruption, got %q", human)
	}
	for _, dir := range []string{payload, empty} {
		line := "[repairable] incomplete-checkpoint-publication " + filepath.Base(dir)
		if !strings.Contains(human, line) {
			t.Fatalf("human output lacks %q:\n%s", line, human)
		}
	}

	code, stdout, _ := awa(t, root, "doctor", "--json")
	if code != 5 {
		t.Fatalf("doctor --json exit = %d, want 5", code)
	}
	var env doctorEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	if env.Data.Health != "failed" || env.Data.Summary.Errors != 0 || env.Data.Summary.Repairable != 2 {
		t.Fatalf("summary = %+v health %q; want failed, no errors, two repairable", env.Data.Summary, env.Data.Health)
	}
	located := map[string]bool{}
	for _, f := range env.Data.Findings {
		if f.Code != "incomplete-checkpoint-publication" {
			t.Fatalf("unexpected finding %+v", f)
		}
		if f.Severity != "warning" || !f.Repairable || f.Repaired || f.Subsystem != "checkpoints" {
			t.Errorf("finding %+v: want an unrepaired repairable checkpoints warning", f)
		}
		if f.Subject != filepath.Base(f.Path) {
			t.Errorf("finding subject %q does not name its path %q", f.Subject, f.Path)
		}
		located[filepath.Base(f.Path)] = true
	}
	if !located[filepath.Base(payload)] || !located[filepath.Base(empty)] {
		t.Fatalf("JSON locates %v, want both remnants", located)
	}
	assertDirBytes(t, payload, payloadBytes)
	assertDirBytes(t, empty, nil)

	code, human, stderr := awa(t, root, "doctor", "--repair")
	if code != 0 {
		t.Fatalf("doctor --repair exit = %d, want 0; stdout=%q stderr=%q", code, human, stderr)
	}
	if strings.Count(human, "[repaired] incomplete-checkpoint-publication") != 2 {
		t.Fatalf("want two repaired findings, got %q", human)
	}
	for _, dir := range []string{payload, empty} {
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Fatalf("%s survived repair: %v", dir, err)
		}
	}
	assertDirBytes(t, keptDir, keptBytes)
	if code, stdout, stderr := awa(t, root, "log"); code != 0 || !strings.Contains(stdout, keptID[:12]) {
		t.Fatalf("log after repair: exit %d stdout=%q stderr=%q", code, stdout, stderr)
	}

	if code, stdout, _ := awa(t, root, "doctor", "--repair"); code != 0 || !strings.Contains(stdout, "health: ok") {
		t.Fatalf("second repair: exit %d stdout=%q", code, stdout)
	}
}

func TestDoctorRepairLeavesIncompleteCheckpointUnderAWriter(t *testing.T) {
	root := initProject(t)
	if err := os.WriteFile(filepath.Join(root, ".awa", "config.toml"),
		[]byte("[locks]\ntimeout = \"1s\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	remnant := headerlessCheckpoint(t, root, "a.txt")
	before := dirBytes(t, remnant)

	active := writeActiveLock(t, root, lockfile.OpCheckpoint)
	if code, _, stderr := awa(t, root, "doctor", "--repair"); code != 6 {
		t.Fatalf("repair under an active writer exit = %d, want 6; stderr=%q", code, stderr)
	}
	assertDirBytes(t, remnant, before)
	if err := os.Remove(active); err != nil {
		t.Fatal(err)
	}

	unknown := filepath.Join(root, ".awa", "locks", "future.lock")
	if err := os.WriteFile(unknown, []byte("not a lock record"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := awa(t, root, "doctor", "--repair"); code != 1 {
		t.Fatalf("repair under an unknown writer exit = %d, want 1; stderr=%q", code, stderr)
	}
	assertDirBytes(t, remnant, before)
	if err := os.Remove(unknown); err != nil {
		t.Fatal(err)
	}

	if code, stdout, stderr := awa(t, root, "doctor", "--repair"); code != 0 {
		t.Fatalf("repair with no writer exit = %d; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if _, err := os.Lstat(remnant); !os.IsNotExist(err) {
		t.Fatalf("remnant survived: %v", err)
	}
}

func TestDoctorKeepsIncompleteCheckpointWithUnknownContent(t *testing.T) {
	root := initProject(t)
	remnant := headerlessCheckpoint(t, root, "a.txt")
	if err := os.WriteFile(filepath.Join(remnant, "notes.txt"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := dirBytes(t, remnant)

	code, stdout, stderr := awa(t, root, "doctor", "--repair")
	if code != 0 {
		t.Fatalf("doctor --repair exit = %d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "[warning] incomplete-checkpoint-publication") || !strings.Contains(stdout, "notes.txt") {
		t.Fatalf("want a non-repairable warning naming the unknown entry, got %q", stdout)
	}
	assertDirBytes(t, remnant, before)
}

// TestCheckpointSIGINTLeavesNothingOrARepairableRemnant asserts what the interruption
// actually left rather than one fixed outcome: where the signal lands relative to the
// publication is not controllable from outside the process.
func TestCheckpointSIGINTLeavesNothingOrARepairableRemnant(t *testing.T) {
	root := initProject(t)
	for i := 0; i < 3000; i++ {
		write(t, root, filepath.Join("src", "f"+strconv.Itoa(i)+".txt"), strconv.Itoa(i))
	}
	cmd, _, _ := startAwa(t, root, "checkpoint")
	time.Sleep(60 * time.Millisecond)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal: %v", err)
	}
	waitCode(t, cmd)

	remnants := headerlessDirs(t, root)
	code, stdout, stderr := awa(t, root, "doctor")
	if len(remnants) == 0 {
		if code != 0 {
			t.Fatalf("no remnant on disk but doctor exit = %d; stdout=%q stderr=%q", code, stdout, stderr)
		}
		return
	}
	if code != 5 {
		t.Fatalf("%d remnants on disk but doctor exit = %d; stdout=%q", len(remnants), code, stdout)
	}
	for _, dir := range remnants {
		if !strings.Contains(stdout, "[repairable] incomplete-checkpoint-publication "+filepath.Base(dir)) {
			t.Fatalf("doctor does not report remnant %s as repairable:\n%s", dir, stdout)
		}
	}
	if code, stdout, stderr := awa(t, root, "doctor", "--repair"); code != 0 {
		t.Fatalf("doctor --repair exit = %d; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if left := headerlessDirs(t, root); len(left) != 0 {
		t.Fatalf("repair left %v", left)
	}
	if code, stdout, _ := awa(t, root, "doctor"); code != 0 {
		t.Fatalf("doctor after repair exit = %d; stdout=%q", code, stdout)
	}
}
