package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// initProject runs "awa init --root <tmp>" and returns the temp root.
func initProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	code, _, stderr := run("init", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("init exit = %d, stderr = %q", code, stderr)
	}
	return root
}

func TestBooleanFlagsRejectInlineValue(t *testing.T) {
	root := initProject(t)
	// Boolean flags must reject an inline value rather than silently reading it as
	// true — both global flags and command-local ones.
	cases := [][]string{
		{"--json=false", "version"},
		{"version", "--json=false"},
		{"--help=false"},
		{"status", "--root", root, "--strict=false"},
		{"diff", "--root", root, "--stat=1"},
	}
	for _, args := range cases {
		if code, _, _ := run(args...); code != int(ExitUsageError) {
			t.Errorf("%v exit = %d, want %d", args, code, ExitUsageError)
		}
	}
}

func TestInitCreatesProject(t *testing.T) {
	root := t.TempDir()
	code, stdout, stderr := run("init", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "initialized awa project") {
		t.Errorf("stdout = %q, want success message", stdout)
	}
	// The default profile writes no config file: the project runs on built-in
	// defaults, so config.toml must be absent and the output must say so.
	if _, err := os.Stat(filepath.Join(root, ".awa", "config.toml")); !os.IsNotExist(err) {
		t.Errorf("default init should not create config.toml, stat err = %v", err)
	}
	if info, err := os.Stat(filepath.Join(root, ".awa")); err != nil || !info.IsDir() {
		t.Errorf(".awa marker dir missing: %v", err)
	}
	if !strings.Contains(stdout, "built-in defaults") {
		t.Errorf("stdout = %q, want a built-in-defaults note", stdout)
	}
	// init must not create the SQLite file.
	if _, err := os.Stat(filepath.Join(root, ".awa", "indexes", "worktree.sqlite")); !os.IsNotExist(err) {
		t.Errorf("worktree.sqlite should not exist")
	}
}

func TestInitStrictProfile(t *testing.T) {
	root := t.TempDir()
	if code, _, stderr := run("init", "--profile", "strict", "--root", root); code != int(ExitSuccess) {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	data, err := os.ReadFile(filepath.Join(root, ".awa", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `trust_mode = "strict"`) {
		t.Errorf("strict config missing strict trust_mode:\n%s", data)
	}
}

func TestInitRefusesOverwrite(t *testing.T) {
	root := initProject(t)
	code, _, stderr := run("init", "--root", root)
	if code != int(ExitGenericError) {
		t.Errorf("exit = %d, want %d", code, ExitGenericError)
	}
	if !strings.Contains(stderr, "already initialized") {
		t.Errorf("stderr = %q, want already-initialized error", stderr)
	}
}

func TestInitDoesNotTouchGitignore(t *testing.T) {
	root := t.TempDir()
	if code, _, _ := run("init", "--root", root); code != int(ExitSuccess) {
		t.Fatalf("init failed")
	}
	if _, err := os.Stat(filepath.Join(root, ".gitignore")); !os.IsNotExist(err) {
		t.Errorf(".gitignore should not be created")
	}
}

func TestInitUnknownFlag(t *testing.T) {
	root := t.TempDir()
	code, _, stderr := run("init", "--bogus", "--root", root)
	if code != int(ExitUsageError) {
		t.Errorf("exit = %d, want %d", code, ExitUsageError)
	}
	if !strings.Contains(stderr, "unknown flag") {
		t.Errorf("stderr = %q, want unknown flag", stderr)
	}
}

func TestInitInvalidProfile(t *testing.T) {
	root := t.TempDir()
	code, _, stderr := run("init", "--profile", "paranoid", "--root", root)
	if code != int(ExitUsageError) {
		t.Errorf("exit = %d, want %d", code, ExitUsageError)
	}
	if !strings.Contains(stderr, "invalid profile") {
		t.Errorf("stderr = %q, want invalid profile", stderr)
	}
}

func TestEmptyFlagValueIsUsageError(t *testing.T) {
	// An empty required value must fail loudly, not fall back to a default.
	cases := [][]string{
		{"status", "--root="},
		{"status", "--root", ""},
		{"config", "path", "--config="},
		{"config", "path", "--config", ""},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			code, stdout, stderr := run(args...)
			if code != int(ExitUsageError) {
				t.Errorf("exit = %d, want %d", code, ExitUsageError)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, "non-empty value") {
				t.Errorf("stderr = %q, want non-empty-value error", stderr)
			}
		})
	}
}

func TestInitRespectsTerminator(t *testing.T) {
	// Tokens after "--" are operands, not flags: "init" takes no operands, so
	// they are an error and --profile must NOT be applied.
	root := t.TempDir()
	code, _, stderr := run("init", "--root", root, "--", "--profile", "strict")
	if code != int(ExitUsageError) {
		t.Fatalf("exit = %d, want %d", code, ExitUsageError)
	}
	if !strings.Contains(stderr, "unexpected argument") {
		t.Errorf("stderr = %q, want unexpected-argument error", stderr)
	}
	// The project must not have been created with the (rejected) strict profile.
	if _, err := os.Stat(filepath.Join(root, ".awa", "config.toml")); !os.IsNotExist(err) {
		t.Errorf("config.toml should not exist after a rejected invocation")
	}
}

func TestUnsupportedPresentationFlagsRejected(t *testing.T) {
	// A flag a command does not support is rejected loudly, never silently ignored —
	// whether it was removed entirely (the never-implemented presentation flags, now
	// unknown flags) or is a real global flag outside the command's capability set.
	root := initProject(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		// The never-implemented presentation flags were removed pre-release, so they
		// are now unknown flags — rejected loudly, never silently accepted.
		{"quiet", []string{"status", "--root", root, "--quiet"}, `unknown flag "--quiet" for status`},
		{"verbose", []string{"status", "--root", root, "--verbose"}, `unknown flag "--verbose" for status`},
		{"no-color", []string{"status", "--root", root, "--no-color"}, `unknown flag "--no-color" for status`},
		// A real global flag a command does not accept is still rejected via the
		// capability check with its distinct "not valid for" message.
		{"version root", []string{"version", "--root", root}, "--root is not valid for version"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := run(tt.args...)
			if code != int(ExitUsageError) {
				t.Errorf("exit = %d, want %d", code, ExitUsageError)
			}
			if !strings.Contains(stderr, tt.want) {
				t.Errorf("stderr = %q, want %q", stderr, tt.want)
			}
		})
	}
}

func TestInitEmptyProfileValue(t *testing.T) {
	// --profile shares the global "requires a non-empty value" rule rather than
	// falling through to ParseProfile as an invalid profile.
	for _, args := range [][]string{
		{"init", "--profile=", "--root", t.TempDir()},
		{"init", "--profile", "", "--root", t.TempDir()},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			code, _, stderr := run(args...)
			if code != int(ExitUsageError) {
				t.Errorf("exit = %d, want %d", code, ExitUsageError)
			}
			if !strings.Contains(stderr, "non-empty value") {
				t.Errorf("stderr = %q, want non-empty-value error", stderr)
			}
		})
	}
}

func TestStatusInProject(t *testing.T) {
	root := initProject(t)
	code, stdout, stderr := run("status", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"root:", "config:", "initialized: yes", "checkpoints:", "run cache:"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestStatusJSON(t *testing.T) {
	root := initProject(t)
	code, stdout, stderr := run("status", "--json", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	var env struct {
		SchemaVersion int    `json:"schema_version"`
		Command       string `json:"command"`
		Data          struct {
			Initialized bool `json:"initialized"`
			Checkpoints struct {
				Recorded  int  `json:"recorded"`
				Populated bool `json:"populated"`
			} `json:"checkpoints"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	if env.SchemaVersion != 1 || env.Command != "status" {
		t.Errorf("envelope = %+v, want schema 1 / status", env)
	}
	if !env.Data.Initialized || env.Data.Checkpoints.Populated {
		t.Errorf("data = %+v, want initialized and unpopulated checkpoints", env.Data)
	}
}

// TestStatusReviewJSONEmptyState pins the review dashboard's empty-state JSON: a
// fresh project has no checkpoint, an uncomputed dirty summary, no runs, and a
// structured next-command list that still suggests creating a checkpoint. A clean
// empty state must not be reported as partial.
func TestStatusReviewJSONEmptyState(t *testing.T) {
	root := initProject(t)
	code, stdout, stderr := run("status", "--json", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("status --json exit = %d, stderr = %q", code, stderr)
	}
	var env struct {
		Data struct {
			Review struct {
				Checkpoint struct {
					Present bool `json:"present"`
				} `json:"checkpoint"`
				Dirty struct {
					Computed bool `json:"computed"`
					Dirty    bool `json:"dirty"`
				} `json:"dirty"`
				Runs struct {
					Reusable int              `json:"reusable"`
					Near     int              `json:"near"`
					Nearest  *json.RawMessage `json:"nearest"`
				} `json:"runs"`
				Next []struct {
					Kind   string   `json:"kind"`
					Reason string   `json:"reason"`
					Argv   []string `json:"argv"`
				} `json:"next"`
				Partial  bool `json:"partial"`
				Warnings []struct {
					Section string `json:"section"`
					Code    string `json:"code"`
				} `json:"warnings"`
			} `json:"review"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	rv := env.Data.Review
	if rv.Checkpoint.Present {
		t.Errorf("checkpoint present in a fresh project: %+v", rv.Checkpoint)
	}
	if rv.Dirty.Computed || rv.Dirty.Dirty {
		t.Errorf("dirty = %+v, want uncomputed for a project with no checkpoint", rv.Dirty)
	}
	if rv.Runs.Reusable != 0 || rv.Runs.Near != 0 || rv.Runs.Nearest != nil {
		t.Errorf("runs = %+v, want all empty", rv.Runs)
	}
	// A clean fresh project degrades no section, so it is not partial.
	if rv.Partial || len(rv.Warnings) != 0 {
		t.Errorf("empty state should not be partial: partial=%v warnings=%+v", rv.Partial, rv.Warnings)
	}
	// With no checkpoint, the dashboard still suggests creating one — as a typed,
	// machine-actionable entry (kind/reason/argv), not a human string.
	var sawCreate bool
	for _, n := range rv.Next {
		if n.Kind == "create-checkpoint" {
			sawCreate = true
			if n.Reason != "no-checkpoint" || len(n.Argv) == 0 || n.Argv[0] != "awa" {
				t.Errorf("create-checkpoint suggestion malformed: %+v", n)
			}
		}
	}
	if !sawCreate {
		t.Errorf("next = %+v, want a create-checkpoint suggestion", rv.Next)
	}
}

// The baseline discovery aid: once a checkpoint exists, the dashboard offers a bounded
// look at the recent checkpoint timeline — as a typed, machine-actionable suggestion in
// JSON and the same line in the human dashboard — so a reviewer surprised by the
// shared, movable latest can find the right explicit id. The human checkpoint line
// also names the checkpoint time.
func TestStatusReviewSuggestsRecentCheckpointInspection(t *testing.T) {
	root := checkpointProject(t)

	// Human dashboard: the suggestion and the checkpoint time are both visible.
	code, stdout, stderr := run("status", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("status exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "awa log -n 5") {
		t.Errorf("status must suggest 'awa log -n 5' when a checkpoint exists:\n%s", stdout)
	}
	// The checkpoint line carries a parenthesized time/age after "checkpoint:".
	cpLine := ""
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "checkpoint:") {
			cpLine = line
		}
	}
	if cpLine == "" || !strings.Contains(cpLine, "(") {
		t.Errorf("checkpoint line missing a time: %q\n%s", cpLine, stdout)
	}

	// JSON: the same suggestion is a typed entry (kind/reason/argv), not a string.
	code, jsonOut, stderr := run("status", "--json", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("status --json exit = %d, stderr = %q", code, stderr)
	}
	var env struct {
		Data struct {
			Review struct {
				Checkpoint struct {
					Present   bool   `json:"present"`
					CreatedAt string `json:"created_at"`
				} `json:"checkpoint"`
				Next []struct {
					Kind   string   `json:"kind"`
					Reason string   `json:"reason"`
					Argv   []string `json:"argv"`
				} `json:"next"`
			} `json:"review"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &env); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, jsonOut)
	}
	rv := env.Data.Review
	if !rv.Checkpoint.Present || rv.Checkpoint.CreatedAt == "" {
		t.Errorf("checkpoint = %+v, want present with a created_at", rv.Checkpoint)
	}
	var saw bool
	for _, n := range rv.Next {
		if n.Kind == "log-recent-checkpoints" {
			saw = true
			wantArgv := []string{"awa", "log", "-n", "5"}
			if n.Reason != "baseline-discovery" || !slices.Equal(n.Argv, wantArgv) {
				t.Errorf("log-recent-checkpoints suggestion malformed: %+v", n)
			}
		}
	}
	if !saw {
		t.Errorf("next = %+v, want a log-recent-checkpoints suggestion", rv.Next)
	}
}

// TestStatusReviewJSONDegradedSectionWarns pins the machine-readable degraded path
// (the A-6 contract): when the runs section cannot be fully computed — here a run
// with corrupt metadata — status --json stays a single valid JSON document whose
// review.partial is true and whose review.warnings names the degraded section and a
// stable code, and the same warning is mirrored to stderr. Without this an agent
// would again depend on parsing stderr to learn a section degraded.
func TestStatusReviewJSONDegradedSectionWarns(t *testing.T) {
	if _, err := os.Stat("/bin/echo"); err != nil {
		t.Skip("requires /bin/echo")
	}
	root := initProject(t)
	if code, _, stderr := run("run", "--root", root, "--cwd", root, "--", "/bin/echo", "hi"); code != 0 {
		t.Fatalf("seed run exit = %d, stderr = %q", code, stderr)
	}

	// Corrupt the stored run's metadata so the run cache counts it as corrupt: the
	// reusable list is then empty and the runs section degrades.
	_, js, _ := runShow(root, "--last", "--json")
	var doc struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
	}
	decodeEnvelope(t, js, &doc)
	meta := filepath.Join(root, ".awa", "runs", "entries", doc.Run.ID[:2], doc.Run.ID, "meta.json")
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatalf("chmod meta: %v", err)
	}
	if err := os.WriteFile(meta, []byte("{ not valid json"), 0o644); err != nil {
		t.Fatalf("corrupt meta: %v", err)
	}

	code, stdout, stderr := run("status", "--json", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("status --json exit = %d, stderr = %q", code, stderr)
	}
	// stdout must remain a single valid JSON document despite the degraded section.
	var env struct {
		Data struct {
			Review struct {
				Partial  bool `json:"partial"`
				Warnings []struct {
					Section string `json:"section"`
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"warnings"`
			} `json:"review"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("status --json is not a single valid JSON document: %v\n%s", err, stdout)
	}
	rv := env.Data.Review
	if !rv.Partial {
		t.Errorf("review.partial = false, want true for a degraded runs section:\n%s", stdout)
	}
	var sawCorrupt bool
	for _, wn := range rv.Warnings {
		if wn.Section == "runs" && wn.Code == "corrupt" && wn.Message != "" {
			sawCorrupt = true
		}
	}
	if !sawCorrupt {
		t.Errorf("review.warnings = %+v, want a {runs, corrupt} warning", rv.Warnings)
	}
	// The same degradation is mirrored to stderr for humans.
	if !strings.Contains(stderr, "corrupt metadata") {
		t.Errorf("stderr = %q, want the corrupt-metadata warning", stderr)
	}
}

func TestBareRoutesToStatus(t *testing.T) {
	// Bare "awa" routes to status, not usage. In a fresh project it succeeds.
	root := initProject(t)
	code, stdout, _ := run("--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("bare exit = %d", code)
	}
	if strings.Contains(stdout, "usage:") {
		t.Errorf("bare invocation should no longer print usage:\n%s", stdout)
	}
	if !strings.Contains(stdout, "initialized: yes") {
		t.Errorf("bare invocation should print status:\n%s", stdout)
	}
}

func TestStatusOutsideProjectExits3(t *testing.T) {
	// An empty temp dir as --root has no config; status must exit 3.
	root := t.TempDir()
	code, stdout, stderr := run("status", "--root", root)
	if code != int(ExitNotFound) {
		t.Errorf("exit = %d, want %d", code, ExitNotFound)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "awa:") {
		t.Errorf("stderr = %q, want an awa error", stderr)
	}
}

func TestCommandsRejectInapplicableFlags(t *testing.T) {
	// A global flag a command does not act on must be a usage error, not a
	// silently ignored option. status never reads --config and has no trust
	// mode; init configures via --profile, not --config/--strict.
	root := initProject(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"status config", []string{"status", "--root", root, "--config", "x.toml"}, "--config is not valid for status"},
		{"status strict", []string{"status", "--root", root, "--strict"}, "--trust-mode/--strict is not valid for status"},
		{"init config", []string{"init", "--root", t.TempDir(), "--config", "x.toml"}, "--config is not valid for init"},
		{"init strict", []string{"init", "--root", t.TempDir(), "--strict"}, "--trust-mode/--strict is not valid for init"},
		{"config path strict", []string{"config", "path", "--root", root, "--strict"}, "--trust-mode/--strict is not valid for config path"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := run(tt.args...)
			if code != int(ExitUsageError) {
				t.Errorf("exit = %d, want %d", code, ExitUsageError)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, tt.want) {
				t.Errorf("stderr = %q, want %q", stderr, tt.want)
			}
		})
	}
}

func TestConfigPathFromNestedDir(t *testing.T) {
	root := initProject(t)
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	// Drive discovery by changing into the nested dir for this test.
	restore := chdir(t, nested)
	defer restore()

	code, stdout, stderr := run("config", "path")
	if code != int(ExitSuccess) {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	// config path lists both layers so a user knows where shared vs local live.
	if !strings.Contains(stdout, filepath.Join(".awa", "config.toml")) {
		t.Errorf("config path missing local layer:\n%s", stdout)
	}
	if !strings.Contains(stdout, "awa.toml") || !strings.Contains(stdout, "shared") || !strings.Contains(stdout, "local") {
		t.Errorf("config path should list shared and local layers:\n%s", stdout)
	}
}

func TestConfigShowAbsent(t *testing.T) {
	// A default project has no config file; show must report that clearly rather
	// than failing.
	root := initProject(t)
	code, stdout, stderr := run("config", "show", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "built-in defaults") || !strings.Contains(stdout, "awa config template") {
		t.Errorf("config show should report the absent default config and point at discovery:\n%s", stdout)
	}
}

func TestConfigShowExisting(t *testing.T) {
	// A strict-profile project has a real config file; show reproduces it.
	root := t.TempDir()
	if code, _, stderr := run("init", "--profile", "strict", "--root", root); code != int(ExitSuccess) {
		t.Fatalf("init exit, stderr = %q", stderr)
	}
	code, stdout, stderr := run("config", "show", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "[hashing]") || !strings.Contains(stdout, `trust_mode = "strict"`) {
		t.Errorf("config show missing expected content:\n%s", stdout)
	}
}

func TestConfigEffectiveJSON(t *testing.T) {
	root := initProject(t)
	code, stdout, stderr := run("config", "effective", "--json", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	// The section views stay at the top level of data (unchanged contract);
	// origins is additive.
	var env struct {
		Command string `json:"command"`
		Data    struct {
			Hashing struct {
				TrustMode   string `json:"trust_mode"`
				MaxFileSize string `json:"max_file_size"`
			} `json:"hashing"`
			Run struct {
				EffectiveEnvAllowlist []string `json:"effective_env_allowlist"`
			} `json:"run"`
			Diff struct {
				Algorithm string `json:"algorithm"`
			} `json:"diff"`
			Origins map[string]string `json:"origins"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	if env.Command != "config.effective" {
		t.Errorf("command = %q, want config.effective", env.Command)
	}
	if env.Data.Hashing.TrustMode != "normal" || env.Data.Hashing.MaxFileSize != "50MiB" {
		t.Errorf("hashing = %+v, want normal/50MiB", env.Data.Hashing)
	}
	if env.Data.Diff.Algorithm != "histogram" {
		t.Errorf("diff = %+v, want algorithm histogram", env.Data.Diff)
	}
	// The effective env allowlist exposes the built-in baseline alongside the
	// configured names, so PATH is present even though it is not in the config.
	hasPATH := false
	for _, n := range env.Data.Run.EffectiveEnvAllowlist {
		if n == "PATH" {
			hasPATH = true
		}
	}
	if !hasPATH {
		t.Errorf("effective_env_allowlist = %v, want it to include PATH", env.Data.Run.EffectiveEnvAllowlist)
	}
}

func TestConfigEffectiveTrustModeOverride(t *testing.T) {
	root := initProject(t)
	code, stdout, _ := run("config", "effective", "--strict", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, `trust_mode = "strict"`) {
		t.Errorf("--strict override not applied:\n%s", stdout)
	}
}

func TestConfigValidate(t *testing.T) {
	root := initProject(t)
	code, stdout, stderr := run("config", "validate", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "valid") {
		t.Errorf("stdout = %q, want valid", stdout)
	}
}

func TestConfigValidateInvalidExits4(t *testing.T) {
	root := t.TempDir()
	cfgDir := filepath.Join(root, ".awa")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte("[hashing]\nalgorithm = \"md5\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := run("config", "validate", "--root", root)
	if code != int(ExitConfigError) {
		t.Errorf("exit = %d, want %d", code, ExitConfigError)
	}
	if !strings.Contains(stderr, "awa:") {
		t.Errorf("stderr = %q, want awa error", stderr)
	}
}

func TestConfigUnknownSubcommand(t *testing.T) {
	code, _, stderr := run("config", "bogus")
	if code != int(ExitUsageError) {
		t.Errorf("exit = %d, want %d", code, ExitUsageError)
	}
	if !strings.Contains(stderr, "unknown config subcommand") {
		t.Errorf("stderr = %q, want unknown subcommand", stderr)
	}
}

func TestConfigOverrideWithConfigFlag(t *testing.T) {
	// --config points directly at a file outside any project.
	dir := t.TempDir()
	cfg := filepath.Join(dir, "custom.toml")
	if err := os.WriteFile(cfg, []byte("[hashing]\ntrust_mode = \"fast\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := run("config", "effective", "--config", cfg)
	if code != int(ExitSuccess) {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, `trust_mode = "fast"`) {
		t.Errorf("--config file not honored:\n%s", stdout)
	}
}

// chdir changes the working directory for a test and returns a restore func.
func chdir(t *testing.T, dir string) func() {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	return func() { _ = os.Chdir(prev) }
}
