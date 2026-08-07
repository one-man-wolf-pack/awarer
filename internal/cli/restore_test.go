package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"awarer/internal/domain/paths"
	"awarer/internal/infra/lockfile"
	"awarer/internal/output"
)

// restoreProject prepares a project with a checkpoint over a generated subtree,
// then dirties it the way a generator would. It returns the root and the recorded
// checkpoint id.
func restoreProject(t *testing.T) (root, checkpointID string) {
	t.Helper()
	root = initProject(t)
	// Fail fast on a held lock instead of waiting out the default timeout: these
	// tests assert the lock-timeout exit, not how patient the default is.
	writeFile(t, root, ".awa/config.toml", "[locks]\ntimeout = \"0s\"\n")
	writeFile(t, root, "generated/client/openapi.json", "{\"v\":1}\n")
	writeFile(t, root, "src/service.go", "package src\n")

	code, stdout, stderr := run("checkpoint", "--root", root, "--json", "-m", "baseline")
	if code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	var env struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil || env.Data.ID == "" {
		t.Fatalf("decode checkpoint json: %v\n%s", err, stdout)
	}

	writeFile(t, root, "generated/client/openapi.json", "{\"v\":2}\n")
	writeFile(t, root, "src/service.go", "package src // dirty\n")
	return root, env.Data.ID
}

func TestRestoreUsageErrorsAreExplicit(t *testing.T) {
	root, id := restoreProject(t)
	abs := filepath.Join(root, "generated", "client")

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no source", []string{"restore", "--root", root}, "requires a source state reference"},
		{"no selection", []string{"restore", "--root", root, id}, "requires one or more paths"},
		{"dry-run with apply", []string{"restore", "--root", root, "--dry-run", "--apply", id, "--", abs}, "cannot be combined"},
		{"all with paths", []string{"restore", "--root", root, "--all", id, "--", abs}, "cannot be combined with path selections"},
		{"now as a source", []string{"restore", "--root", root, "now", "--", abs}, "is not a restore source"},
		{"a range", []string{"restore", "--root", root, id + "..now", "--", abs}, "not a range"},
		{"unknown flag", []string{"restore", "--root", root, "--recursive", id, "--", abs}, "unknown flag"},
		{"restore after", []string{"restore", "--root", root, "restore:abcdef:after", "--", abs}, "only restore:<id>:before exists"},
		{"a second reference token", []string{"restore", "--root", root, id, "latest..now"}, "not a range"},
		{"the project root as a selection", []string{"restore", "--root", root, id, "--", root}, "use --all"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := run(tc.args...)
			if code != int(ExitUsageError) {
				t.Fatalf("exit = %d, want %d; stdout = %q stderr = %q", code, ExitUsageError, stdout, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr = %q, want it to mention %q", stderr, tc.want)
			}
		})
	}
}

func TestRestorePreviewSucceedsAndApplyIsExplicit(t *testing.T) {
	root, id := restoreProject(t)
	abs := filepath.Join(root, "generated", "client")

	code, stdout, _ := run("restore", "--root", root, id, "--", abs)
	if code != int(ExitSuccess) {
		t.Fatalf("preview exit = %d", code)
	}
	if !strings.Contains(stdout, "apply: awa restore --apply "+id) {
		t.Errorf("preview does not name the exact apply command:\n%s", stdout)
	}
	// The file is untouched by the preview.
	if got := readWorktree(t, root, "generated/client/openapi.json"); got != "{\"v\":2}\n" {
		t.Errorf("preview mutated the file: %q", got)
	}

	acode, astdout, astderr := run("restore", "--root", root, "--apply", id, "--", abs)
	if acode != int(ExitSuccess) {
		t.Fatalf("apply exit = %d, stdout = %q stderr = %q", acode, astdout, astderr)
	}
	if got := readWorktree(t, root, "generated/client/openapi.json"); got != "{\"v\":1}\n" {
		t.Errorf("apply did not restore the file: %q", got)
	}
	// The unrelated dirty edit survives.
	if got := readWorktree(t, root, "src/service.go"); got != "package src // dirty\n" {
		t.Errorf("apply touched an unselected path: %q", got)
	}
}

// TestRestoreSerializesAgainstAnotherRestore proves the exclusive restore lock is
// really taken: a restore that cannot get it reports the lock-timeout exit rather
// than planning against a worktree another restore is changing.
func TestRestoreSerializesAgainstAnotherRestore(t *testing.T) {
	root, id := restoreProject(t)
	layout := paths.New(root)

	lease, err := lockfile.AcquireExclusive(context.Background(), layout.Root(), layout.LocksDir(),
		lockfile.RestoreLockName, lockfile.OpRestore, lockfile.Owner{})
	if err != nil {
		t.Fatalf("acquire restore lock: %v", err)
	}
	defer func() { _ = lease.Release() }()

	code, _, stderr := run("restore", "--root", root, "--apply", id, "--", filepath.Join(root, "generated", "client"))
	if code != int(ExitLockTimeout) {
		t.Fatalf("exit = %d, want %d (lock timeout); stderr = %q", code, ExitLockTimeout, stderr)
	}
	// And it did not mutate on its way to giving up.
	if got := readWorktree(t, root, "generated/client/openapi.json"); got != "{\"v\":2}\n" {
		t.Errorf("a restore that could not take its lock still wrote: %q", got)
	}
}

// TestRestoreStandsDownForAnActiveCollector proves the writer presence lock is
// taken: while a collector holds the exclusive collector lease, a restore does not
// read blobs the collector may be about to reclaim.
func TestRestoreStandsDownForAnActiveCollector(t *testing.T) {
	root, id := restoreProject(t)
	layout := paths.New(root)

	lease, err := lockfile.AcquireExclusive(context.Background(), layout.Root(), layout.LocksDir(),
		lockfile.CollectorLockName, lockfile.OpGC, lockfile.Owner{})
	if err != nil {
		t.Fatalf("acquire collector lock: %v", err)
	}
	defer func() { _ = lease.Release() }()

	code, _, stderr := run("restore", "--root", root, "--apply", id, "--", filepath.Join(root, "generated", "client"))
	if code != int(ExitLockTimeout) {
		t.Fatalf("exit = %d, want %d (lock timeout); stderr = %q", code, ExitLockTimeout, stderr)
	}
	if got := readWorktree(t, root, "generated/client/openapi.json"); got != "{\"v\":2}\n" {
		t.Errorf("a restore that stood down for a collector still wrote: %q", got)
	}
}

// TestRestorePreviewTakesNoLock proves preview really is read-only: it must work
// while another restore holds the exclusive lock, because it changes nothing.
func TestRestorePreviewTakesNoLock(t *testing.T) {
	root, id := restoreProject(t)
	layout := paths.New(root)

	lease, err := lockfile.AcquireExclusive(context.Background(), layout.Root(), layout.LocksDir(),
		lockfile.RestoreLockName, lockfile.OpRestore, lockfile.Owner{})
	if err != nil {
		t.Fatalf("acquire restore lock: %v", err)
	}
	defer func() { _ = lease.Release() }()

	code, _, stderr := run("restore", "--root", root, id, "--", filepath.Join(root, "generated", "client"))
	if code != int(ExitSuccess) {
		t.Fatalf("preview exit = %d under a held restore lock, want success; stderr = %q", code, stderr)
	}
}

func TestRestoreJSONIsTheVersionedContract(t *testing.T) {
	root, id := restoreProject(t)

	code, stdout, _ := run("restore", "--root", root, "--json", id, "--", filepath.Join(root, "generated", "client"))
	if code != int(ExitSuccess) {
		t.Fatalf("preview --json exit = %d", code)
	}
	var doc struct {
		SchemaVersion int    `json:"schema_version"`
		Command       string `json:"command"`
		Data          struct {
			RestoreContract string `json:"restore_contract"`
			Mode            string `json:"mode"`
			Outcome         string `json:"outcome"`
			Source          struct {
				ID           string `json:"id"`
				CanonicalRef string `json:"canonical_ref"`
			} `json:"source"`
			Recovery *struct{} `json:"recovery"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	if doc.SchemaVersion != output.SchemaVersion || doc.Command != "restore" {
		t.Errorf("envelope = %d/%q", doc.SchemaVersion, doc.Command)
	}
	if doc.Data.RestoreContract != "awa-restore/v1" {
		t.Errorf("restore_contract = %q", doc.Data.RestoreContract)
	}
	if doc.Data.Mode != "preview" {
		t.Errorf("mode = %q, want preview", doc.Data.Mode)
	}
	if doc.Data.Source.ID != id || doc.Data.Source.CanonicalRef != id {
		t.Errorf("source = %+v, want the full checkpoint id", doc.Data.Source)
	}
	if doc.Data.Recovery != nil {
		t.Error("a preview carries a recovery observation")
	}
	// stdout must be the JSON document alone; advisory notes belong on stderr.
	if !strings.HasPrefix(strings.TrimSpace(stdout), "{") || !strings.HasSuffix(strings.TrimSpace(stdout), "}") {
		t.Errorf("--json stdout is not a single document:\n%s", stdout)
	}
}

// TestPreviewNamesEveryBlockedPathAndReason proves the preview's evidence gaps
// reach both projections. A blocked count on its own is the one preview shape that
// is actively unhelpful: it tells a user that something cannot be restored while
// withholding which path and why, and the human report then points at "the reasons
// above" that were never printed.
func TestPreviewNamesEveryBlockedPathAndReason(t *testing.T) {
	root := initProject(t)
	// hash-only evidence: identity is recorded, bytes are not, so a replace is
	// blocked with a reason no amount of reading could resolve.
	writeFile(t, root, ".awa/config.toml", "[checkpoint]\nstore_file_contents = false\n")
	writeFile(t, root, "generated/api.json", "{\"v\":1}\n")
	if code, _, stderr := run("checkpoint", "--root", root, "-m", "baseline"); code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	writeFile(t, root, "generated/api.json", "{\"v\":2}\n")
	abs := filepath.Join(root, "generated")

	code, stdout, _ := run("restore", "--root", root, "latest", "--", abs)
	if code != int(ExitSuccess) {
		t.Fatalf("preview exit = %d", code)
	}
	if !strings.Contains(stdout, "generated/api.json: hash-only-content") {
		t.Errorf("preview does not name the blocked path and its reason:\n%s", stdout)
	}

	jcode, jstdout, _ := run("restore", "--root", root, "--json", "latest", "--", abs)
	if jcode != int(ExitSuccess) {
		t.Fatalf("preview --json exit = %d", jcode)
	}
	var doc struct {
		Data struct {
			Complete bool `json:"complete"`
			Failures []struct {
				Path    string   `json:"path"`
				Reasons []string `json:"reasons"`
			} `json:"failures"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(jstdout), &doc); err != nil {
		t.Fatalf("decode: %v\n%s", err, jstdout)
	}
	if doc.Data.Complete {
		t.Error("a plan with a blocked operation reported complete")
	}
	if len(doc.Data.Failures) != 1 ||
		doc.Data.Failures[0].Path != "generated/api.json" ||
		len(doc.Data.Failures[0].Reasons) != 1 ||
		doc.Data.Failures[0].Reasons[0] != "hash-only-content" {
		t.Errorf("failures = %+v, want the blocked path with its closed reason token", doc.Data.Failures)
	}
}

func TestStateProviderRefusesARecoveryObservationReference(t *testing.T) {
	root, _ := restoreProject(t)
	// The provider's kind vocabulary deliberately does not name recovery
	// observations, so a reference to one is a usage error that says where to look
	// instead — never an "unknown state kind" dressed up as an availability outcome.
	for _, args := range [][]string{
		{"state", "resolve", "--root", root, "restore:abcdef0123456789abcdef0123456789:before"},
		{"state", "compare", "--root", root, "restore:abcdef0123456789abcdef0123456789:before..now"},
	} {
		code, _, stderr := run(args...)
		if code != int(ExitUsageError) {
			t.Errorf("%v exit = %d, want %d; stderr = %q", args, code, ExitUsageError, stderr)
		}
		if !strings.Contains(stderr, "awa log --all") {
			t.Errorf("%v stderr does not point at an inspection surface: %q", args, stderr)
		}
	}
}

// readWorktree reads a project file for an assertion.
func readWorktree(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}
