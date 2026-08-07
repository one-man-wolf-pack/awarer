package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	domainconfig "awarer/internal/domain/config"
)

// TestConfigEffectiveShowsEveryKey closes the one projection of the config key
// namespace that nothing else guards. The reference, the template, the wire tags,
// and the origin map are all checked against config.Keys() by
// configfile.TestReferenceCoversEveryImplementedKey and its siblings, and the JSON
// view is held by the compiler because renderEffective reads every view field. The
// human renderer is a hand-maintained list of kv() calls: dropping one leaves the
// view field merely unread, which Go does not report, so "awa config effective"
// could silently stop showing a key while every gate stayed green.
//
// Keys are matched as section.leaf rather than by leaf name alone, because
// extra_excludes and use_gitignore each appear under more than one section and a
// leaf-only check would accept a dropped line as covered by its namesake. Derived
// lines the renderer adds on purpose (effective_excludes, injected_env, and the
// other effective_* lists) carry no config key and are simply not required here.
func TestConfigEffectiveShowsEveryKey(t *testing.T) {
	root := initProject(t)
	code, stdout, stderr := run("config", "effective", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}

	shown := map[string]bool{}
	section := ""
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "["):
			section = strings.Trim(line, "[]")
		case line == "" || strings.HasPrefix(line, "#"):
			// blank line or the legend
		default:
			leaf, _, ok := strings.Cut(line, " = ")
			if ok && section != "" {
				shown[section+"."+leaf] = true
			}
		}
	}

	for _, key := range domainconfig.Keys() {
		if !shown[key] {
			t.Errorf("awa config effective does not show %q:\n%s", key, stdout)
		}
	}
}

// TestConfigTemplateListsEverySection confirms the annotated template renders from
// the reference model and covers every section, so a user can discover the schema.
func TestConfigTemplateListsEverySection(t *testing.T) {
	code, stdout, stderr := run("config", "template")
	if code != int(ExitSuccess) {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, section := range []string{"[scope]", "[history]", "[hashing]", "[checkpoint]", "[run]", "[gc]", "[diff]", "[locks]", "[ui]"} {
		if !strings.Contains(stdout, section) {
			t.Errorf("template missing section %s", section)
		}
	}
	// Keys are shown commented out at their defaults so uncommenting is how you override.
	if !strings.Contains(stdout, "# extra_effect_roots = []") {
		t.Errorf("template should show extra_effect_roots commented at its default:\n%s", stdout)
	}
}

// TestConfigInitSharedWritesCommittableRoot proves the shared scaffold lands at the
// project root (outside .awa/), prints committable guidance, and never overwrites
// without --force.
func TestConfigInitSharedWritesCommittableRoot(t *testing.T) {
	root := initProject(t)
	code, stdout, stderr := run("config", "init", "--shared", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	sharedPath := filepath.Join(root, "awa.toml")
	if _, err := os.Stat(sharedPath); err != nil {
		t.Fatalf("shared awa.toml not written: %v", err)
	}
	if strings.Contains(sharedPath, string(os.PathSeparator)+".awa"+string(os.PathSeparator)) {
		t.Errorf("shared config must live outside .awa/, got %s", sharedPath)
	}
	if !strings.Contains(stdout, "commit") || !strings.Contains(stdout, "awa config effective") {
		t.Errorf("shared init should print committable + verify guidance:\n%s", stdout)
	}

	// A second init without --force must not overwrite.
	if code, _, _ := run("config", "init", "--shared", "--root", root); code == int(ExitSuccess) {
		t.Error("second config init --shared without --force should fail")
	}
	// --force overwrites.
	if code, _, stderr := run("config", "init", "--shared", "--force", "--root", root); code != int(ExitSuccess) {
		t.Errorf("config init --shared --force exit = %d, stderr = %q", code, stderr)
	}
}

// TestConfigInitJSONReportsLayer proves the JSON confirmation is reachable (init
// accepts --json) and typed, so an agent can scaffold and confirm without parsing
// prose.
func TestConfigInitJSONReportsLayer(t *testing.T) {
	root := initProject(t)
	code, stdout, stderr := run("config", "init", "--shared", "--json", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	var env struct {
		Command string `json:"command"`
		Data    struct {
			Layer       string `json:"layer"`
			Committable bool   `json:"committable"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	if env.Command != "config.init" || env.Data.Layer != "shared" || !env.Data.Committable {
		t.Errorf("unexpected init JSON: %+v", env)
	}
}

// TestConfigInitLocalIsPrivate proves the local scaffold lands under .awa/ and is
// described as private.
func TestConfigInitLocalIsPrivate(t *testing.T) {
	root := initProject(t)
	code, stdout, stderr := run("config", "init", "--local", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, ".awa", "config.toml")); err != nil {
		t.Fatalf("local config not written: %v", err)
	}
	if !strings.Contains(stdout, "private") {
		t.Errorf("local init should say the override stays private:\n%s", stdout)
	}
}

func TestConfigInitRequiresExactlyOneTarget(t *testing.T) {
	root := initProject(t)
	if code, _, _ := run("config", "init", "--root", root); code != int(ExitUsageError) {
		t.Error("config init with neither --shared nor --local should be a usage error")
	}
	if code, _, _ := run("config", "init", "--shared", "--local", "--root", root); code != int(ExitUsageError) {
		t.Error("config init with both --shared and --local should be a usage error")
	}
}

// TestSharedConfigCommittableWhileAwaIgnored proves awa.toml sits at the root while
// .awa/.gitignore keeps only .awa/ state private — so a team can commit shared
// policy without exposing local state.
func TestSharedConfigCommittableWhileAwaIgnored(t *testing.T) {
	root := initProject(t)
	if code, _, stderr := run("config", "init", "--shared", "--root", root); code != int(ExitSuccess) {
		t.Fatalf("init --shared exit, stderr = %q", stderr)
	}
	// awa.toml is a sibling of .awa/, not inside it.
	if _, err := os.Stat(filepath.Join(root, "awa.toml")); err != nil {
		t.Fatalf("awa.toml missing at root: %v", err)
	}
	guard, err := os.ReadFile(filepath.Join(root, ".awa", ".gitignore"))
	if err != nil {
		t.Fatalf("reading .awa/.gitignore: %v", err)
	}
	// The guard ignores everything under .awa/, and being inside .awa/ it cannot
	// reach a root-level awa.toml.
	if !strings.Contains(string(guard), "*") {
		t.Errorf(".awa/.gitignore should ignore .awa contents, got %q", guard)
	}
}

// TestConfigEffectiveMergesLayersWithOrigins drives a real shared+local stack and
// checks the composed values and their origin annotations.
func TestConfigEffectiveMergesLayersWithOrigins(t *testing.T) {
	root := initProject(t)
	writeFile(t, root, "awa.toml", "[hashing]\ntrust_mode = \"fast\"\n[run]\nttl = \"3d\"\n")
	writeFile(t, root, filepath.Join(".awa", "config.toml"), "[run]\nttl = \"1d\"\n")

	// Human view: shared sets trust_mode, local overrides ttl, each annotated.
	code, stdout, stderr := run("config", "effective", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, `trust_mode = "fast"  # shared`) {
		t.Errorf("effective should annotate trust_mode origin shared:\n%s", stdout)
	}
	if !strings.Contains(stdout, `ttl = "1d"  # local`) {
		t.Errorf("effective should show local overriding shared ttl:\n%s", stdout)
	}

	// JSON origins.
	code, stdout, _ = run("config", "effective", "--json", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("json exit = %d", code)
	}
	var env struct {
		Data struct {
			Origins map[string]string `json:"origins"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	if env.Data.Origins["hashing.trust_mode"] != "shared" || env.Data.Origins["run.ttl"] != "local" {
		t.Errorf("origins = %v, want trust_mode shared / ttl local", env.Data.Origins)
	}
}

// TestConfigShowSelectsLayer proves show reports absent defaults, disambiguates
// when both layers exist, and reproduces a selected layer's bytes.
func TestConfigShowSelectsLayer(t *testing.T) {
	root := initProject(t)
	// No config anywhere -> point at discovery, do not fail.
	if code, stdout, _ := run("config", "show", "--root", root); code != int(ExitSuccess) || !strings.Contains(stdout, "built-in defaults") {
		t.Errorf("config show with no config should report defaults, got %q", stdout)
	}
	writeFile(t, root, "awa.toml", "[run]\nttl = \"3d\"\n")
	writeFile(t, root, filepath.Join(".awa", "config.toml"), "[run]\nttl = \"1d\"\n")
	// Ambiguous with both present.
	if code, _, _ := run("config", "show", "--root", root); code != int(ExitUsageError) {
		t.Error("config show with both layers should ask which one")
	}
	// Explicit selection reproduces the chosen file.
	code, stdout, _ := run("config", "show", "shared", "--root", root)
	if code != int(ExitSuccess) || !strings.Contains(stdout, `ttl = "3d"`) {
		t.Errorf("config show shared should print the shared file, got %q", stdout)
	}
}

// TestSharedLayerChangesRunCacheIdentity proves an effective config layer that
// changes a keyed field changes the run cache identity: a run that hit becomes a
// miss once a shared layer flips a keyed setting.
func TestSharedLayerChangesRunCacheIdentity(t *testing.T) {
	root := initProject(t)
	// First run records and becomes reusable; second run hits.
	if code, _, stderr := run("run", "--root", root, "--cwd", root, "--", "true"); code != int(ExitSuccess) {
		t.Fatalf("first run exit = %d, stderr = %q", code, stderr)
	}
	code, stdout, _ := run("run", "--json", "--root", root, "--cwd", root, "--", "true")
	if code != int(ExitSuccess) {
		t.Fatalf("second run exit = %d", code)
	}
	if status := cacheStatus(t, stdout); status != "hit" {
		t.Fatalf("second identical run status = %q, want hit", status)
	}
	// A shared layer that flips a keyed field (use_gitignore feeds the run key) must
	// change identity, so the next run is a miss, not a hit.
	writeFile(t, root, "awa.toml", "[run]\nuse_gitignore = true\n")
	code, stdout, _ = run("run", "--json", "--root", root, "--cwd", root, "--", "true")
	if code != int(ExitSuccess) {
		t.Fatalf("post-layer run exit = %d", code)
	}
	if status := cacheStatus(t, stdout); status == "hit" {
		t.Errorf("run after a keyed shared-layer change should not hit, got %q", status)
	}
}

// TestRunEffectStateDiagnosis proves a command that writes a watched, input-scan-
// blind effect root (target/) becomes non-reusable for effect-state-differs and the
// footer names the root, is honest about the missing sample, and teaches the
// decision; JSON carries stable tokens.
func TestRunEffectStateDiagnosis(t *testing.T) {
	root := initProject(t)
	// target/ is a baseline exclude (hidden from the input scan) and a watched effect
	// root, so creating it is an effect change, not an input change.
	code, _, stderr := run("run", "--root", root, "--cwd", root, "--", "sh", "-c", "mkdir -p target && date > target/x")
	if code != int(ExitSuccess) {
		t.Fatalf("run exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "effect-state-differs") {
		t.Fatalf("footer should report effect-state-differs:\n%s", stderr)
	}
	if !strings.Contains(stderr, "watched generated-output state changed") {
		t.Errorf("footer should name the effect-state change:\n%s", stderr)
	}
	if !strings.Contains(stderr, "sample unavailable") {
		t.Errorf("footer should be honest that no changed-path sample exists:\n%s", stderr)
	}
	if !strings.Contains(stderr, "awa run --record") {
		t.Errorf("footer should teach the record option:\n%s", stderr)
	}

	// JSON: stable tokens under data.effect.
	os.RemoveAll(filepath.Join(root, "target"))
	code, stdout, _ := run("run", "--json", "--root", root, "--cwd", root, "--", "sh", "-c", "mkdir -p target && date > target/x")
	if code != int(ExitSuccess) {
		t.Fatalf("json run exit = %d", code)
	}
	var env struct {
		Data struct {
			Effect *struct {
				Reason  string `json:"reason"`
				Root    string `json:"root"`
				Sample  string `json:"sample"`
				Actions []struct {
					Condition string   `json:"condition"`
					Action    string   `json:"action"`
					Argv      []string `json:"argv"`
				} `json:"actions"`
			} `json:"effect"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	if env.Data.Effect == nil {
		t.Fatal("data.effect missing for an effect-state-differs run")
	}
	if env.Data.Effect.Reason != "effect-state-differs" {
		t.Errorf("effect.reason = %q, want effect-state-differs", env.Data.Effect.Reason)
	}
	if env.Data.Effect.Sample != "unavailable" {
		t.Errorf("effect.sample = %q, want unavailable", env.Data.Effect.Sample)
	}
	if len(env.Data.Effect.Actions) != 3 {
		t.Errorf("effect.actions = %v, want three typed actions", env.Data.Effect.Actions)
	}
}

// TestStatusInvalidConfigExitsConfigError proves config validation stays enforced
// at the CLI boundary after status stopped loading config itself: an invalid layer
// makes status fail with the config-error code, naming the layer, before the
// dashboard is built.
func TestStatusInvalidConfigExitsConfigError(t *testing.T) {
	root := initProject(t)
	writeFile(t, root, "awa.toml", "[hashing]\nalgorithm = \"md5\"\n")
	code, _, stderr := run("status", "--root", root)
	if code != int(ExitConfigError) {
		t.Errorf("status with invalid shared config exit = %d, want %d", code, ExitConfigError)
	}
	if !strings.Contains(stderr, "shared config") {
		t.Errorf("status error should name the shared layer:\n%s", stderr)
	}
}

// TestStatusReportsSharedLayerHonestly proves a shared-only project reports the
// shared awa.toml that exists (not the absent local path), fixing the contradictory
// config_present/config_path facts.
func TestStatusReportsSharedLayerHonestly(t *testing.T) {
	root := initProject(t)
	writeFile(t, root, "awa.toml", "[hashing]\ntrust_mode = \"fast\"\n")
	code, stdout, _ := run("status", "--json", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("status --json exit = %d", code)
	}
	var env struct {
		Data struct {
			ConfigPresent bool   `json:"config_present"`
			ConfigPath    string `json:"config_path"`
			ConfigLayers  []struct {
				Layer  string `json:"layer"`
				Exists bool   `json:"exists"`
			} `json:"config_layers"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	if !env.Data.ConfigPresent || !strings.HasSuffix(env.Data.ConfigPath, "awa.toml") {
		t.Errorf("config_path should name the present shared awa.toml, got present=%v path=%q", env.Data.ConfigPresent, env.Data.ConfigPath)
	}
	var shared, local bool
	for _, l := range env.Data.ConfigLayers {
		if l.Layer == "shared" {
			shared = l.Exists
		}
		if l.Layer == "local" {
			local = l.Exists
		}
	}
	if !shared || local {
		t.Errorf("config_layers should show shared present, local absent, got %+v", env.Data.ConfigLayers)
	}
}

// cacheStatus extracts data.cache.status from a run --json document.
func cacheStatus(t *testing.T, stdout string) string {
	t.Helper()
	var env struct {
		Data struct {
			Cache struct {
				Status string `json:"status"`
			} `json:"cache"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid run JSON: %v\n%s", err, stdout)
	}
	return env.Data.Cache.Status
}
