// Package acceptance builds the awa binary once and exercises the acceptance
// scenarios end to end in real temporary directories, the way a user would.
package acceptance

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// buildDir holds every binary the suite builds — awa and the fixture commands — so
// TestMain removes them all in one place when the run ends.
var buildDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "awa-acceptance-*")
	if err != nil {
		panic(err)
	}
	buildDir = dir
	// Not a defer: os.Exit skips deferred calls, so a deferred removal would leave the
	// built binaries behind after every run.
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

var (
	awaOnce sync.Once
	awaBin  string
	awaErr  error
)

// awaBinary builds the awa binary on first use and returns its path. The build runs
// inside a test rather than in TestMain because go test records the files a test
// touches only once the tests start, and recordBuildInputs must be recorded — and must
// precede the build — for the suite's cached result to follow the code it exercises.
// A failed build panics: no scenario can run without the binary.
func awaBinary() string {
	awaOnce.Do(func() {
		if err := recordBuildInputs(); err != nil {
			awaErr = fmt.Errorf("recording build inputs: %w", err)
			return
		}
		bin := filepath.Join(buildDir, "awa")
		build := exec.Command("go", "build", "-o", bin, "../../cmd/awa")
		build.Stderr = os.Stderr
		if err := build.Run(); err != nil {
			awaErr = fmt.Errorf("building awa: %w", err)
			return
		}
		awaBin = bin
	})
	if awaErr != nil {
		panic(awaErr)
	}
	return awaBin
}

// recordBuildInputs stats every file the awa binary can be built from. go test caches
// this package's result by its own test binary and the files the test touches, and the
// awa binary is built by a child go process it cannot see; without these stats, a change
// confined to the CLI or application code would replay a stale cached pass. Unit-test
// files are skipped because they never reach the binary.
func recordBuildInputs() error {
	for _, f := range []string{"../../go.mod", "../../go.sum"} {
		if _, err := os.Stat(f); err != nil {
			return err
		}
	}
	for _, root := range []string{"../../cmd", "../../internal"} {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.HasSuffix(p, "_test.go") {
				return err
			}
			_, err = os.Stat(p)
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// awa runs the built binary in dir and returns exit code, stdout, and stderr.
func awa(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(awaBinary(), args...)
	cmd.Dir = dir
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running awa %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return code, out.String(), errBuf.String()
}

func TestAcceptanceInitStatusConfig(t *testing.T) {
	root := t.TempDir()

	// awa init
	if code, _, stderr := awa(t, root, "init"); code != 0 {
		t.Fatalf("init exit = %d, stderr = %q", code, stderr)
	}
	// The default profile writes no config file: the project marker is the .awa
	// directory and its state subdirectories.
	for _, sub := range []string{
		filepath.Join("store", "blobs"),
		filepath.Join("store", "tmp"),
		"checkpoints", "runs", "indexes", "locks", "logs",
	} {
		if _, err := os.Stat(filepath.Join(root, ".awa", sub)); err != nil {
			t.Errorf("missing .awa/%s: %v", sub, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".awa", "config.toml")); !os.IsNotExist(err) {
		t.Errorf("default init should not write config.toml, stat err = %v", err)
	}

	// awa status
	if code, stdout, _ := awa(t, root, "status"); code != 0 || !strings.Contains(stdout, "initialized: yes") {
		t.Errorf("status exit = %d, stdout = %q", code, stdout)
	}

	// awa status --json
	code, stdout, _ := awa(t, root, "status", "--json")
	if code != 0 {
		t.Fatalf("status --json exit = %d", code)
	}
	var env struct {
		SchemaVersion int    `json:"schema_version"`
		Command       string `json:"command"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil || env.SchemaVersion != 1 {
		t.Errorf("status --json envelope invalid: %v\n%s", err, stdout)
	}

	// Nested directory: config path and status discover the root.
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	// config path lists both layers (shared awa.toml + local .awa/config.toml) and
	// discovers the root from a nested directory.
	if code, stdout, _ := awa(t, nested, "config", "path"); code != 0 ||
		!strings.Contains(stdout, filepath.Join(root, "awa.toml")) ||
		!strings.Contains(stdout, filepath.Join(root, ".awa", "config.toml")) {
		t.Errorf("config path from nested = %d / %q", code, stdout)
	}
	if code, _, _ := awa(t, nested, "status"); code != 0 {
		t.Errorf("status from nested exit = %d", code)
	}

	// awa (bare) routes to status.
	if code, stdout, _ := awa(t, root); code != 0 || !strings.Contains(stdout, "initialized: yes") {
		t.Errorf("bare awa exit = %d, stdout = %q", code, stdout)
	}

	// awa config effective --json and validate.
	if code, _, _ := awa(t, root, "config", "effective", "--json"); code != 0 {
		t.Errorf("config effective --json exit = %d", code)
	}
	if code, _, _ := awa(t, root, "config", "validate"); code != 0 {
		t.Errorf("config validate exit = %d", code)
	}
}

func TestAcceptanceStateGitignoreGuard(t *testing.T) {
	root := t.TempDir()

	// init always writes the awa-owned guard and never creates the root .gitignore.
	// There is no flag that changes either half.
	if code, _, stderr := awa(t, root, "init"); code != 0 {
		t.Fatalf("init exit = %d, stderr = %q", code, stderr)
	}
	guard := filepath.Join(root, ".awa", ".gitignore")
	got, err := os.ReadFile(guard)
	if err != nil {
		t.Fatalf("guard not created: %v", err)
	}
	if !strings.Contains(string(got), "*") {
		t.Errorf("guard = %q, want it to ignore all of .awa", got)
	}
	if _, err := os.Stat(filepath.Join(root, ".gitignore")); !os.IsNotExist(err) {
		t.Errorf("init must not create root .gitignore, stat err = %v", err)
	}

	// doctor reports a missing guard and --repair restores it.
	if err := os.Remove(guard); err != nil {
		t.Fatal(err)
	}
	// A missing guard is a repairable warning, which needs attention (exit 5) and
	// is named by its code in the report.
	if code, stdout, _ := awa(t, root, "doctor"); code != 5 || !strings.Contains(stdout, "state-gitignore-missing") {
		t.Errorf("doctor should report state-gitignore-missing (exit 5), exit=%d stdout=%q", code, stdout)
	}
	if code, _, stderr := awa(t, root, "doctor", "--repair"); code != 0 {
		t.Fatalf("doctor --repair exit = %d, stderr = %q", code, stderr)
	}
	if _, err := os.Stat(guard); err != nil {
		t.Fatalf("guard not restored by --repair: %v", err)
	}

	// In a real git repo, .awa is ignored (not untracked) after init.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed; file-creation checks above already passed")
	}
	gitRoot := t.TempDir()
	runGit(t, gitRoot, "init")
	if code, _, stderr := awa(t, gitRoot, "init"); code != 0 {
		t.Fatalf("init in git repo exit = %d, stderr = %q", code, stderr)
	}
	out := gitStatusIgnored(t, gitRoot)
	if !strings.Contains(out, "!! .awa/") {
		t.Errorf("want .awa ignored (\"!! .awa/\") in git status, got:\n%s", out)
	}
	if strings.Contains(out, "?? .awa/") {
		t.Errorf(".awa should be ignored, not untracked, got:\n%s", out)
	}
}

// gitStatusIgnored returns `git status --porcelain --ignored` output for dir.
func gitStatusIgnored(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "status", "--porcelain", "--ignored")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, out)
	}
	return string(out)
}

func TestAcceptanceOutsideProject(t *testing.T) {
	dir := t.TempDir()
	code, stdout, stderr := awa(t, dir, "status")
	if code != 3 {
		t.Errorf("status outside project exit = %d, want 3", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "awa init") {
		t.Errorf("stderr = %q, want suggestion to run 'awa init'", stderr)
	}
}

// writeProjectConfig writes a .awa/config.toml carrying only the given override
// body. Since the config file is override-based — defaults fill everything else —
// a minimal body is enough to exercise a single setting end-to-end. It replaces
// any existing file.
func writeProjectConfig(t *testing.T, root, body string) {
	t.Helper()
	path := filepath.Join(root, ".awa", "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}
