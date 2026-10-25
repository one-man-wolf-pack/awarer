package acceptance

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const probeSentinel = "SENTINEL-identity-report-41d9"

// probeLauncher is a stable launcher outside the project: the bytes awa hashes for
// the project never change, but the toolchain it selects does.
type probeLauncher struct {
	script, selection, calls string
}

func newProbeLauncher(t *testing.T) probeLauncher {
	t.Helper()
	dir := t.TempDir()
	l := probeLauncher{
		script:    filepath.Join(dir, "launcher.sh"),
		selection: filepath.Join(dir, "selected"),
		calls:     filepath.Join(dir, "calls"),
	}
	body := "echo x >> '" + l.calls + "'\nprintf 'tool %s " + probeSentinel + "\\n' \"$(cat '" + l.selection + "')\"\n"
	if err := os.WriteFile(l.script, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	l.selectVersion(t, "1.0")
	return l
}

func (l probeLauncher) selectVersion(t *testing.T, v string) {
	t.Helper()
	if err := os.WriteFile(l.selection, []byte(v), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (l probeLauncher) callCount(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(l.calls)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "x")
}

func (l probeLauncher) policy() string {
	return validationPolicy("check") + `
[[run.validation.identity_probes]]
name = "tool"
argv = ["sh", "` + l.script + `"]
`
}

func TestIdentityProbesKeyTheRunOnTheSelectedTool(t *testing.T) {
	root := initProject(t)
	l := newProbeLauncher(t)
	write(t, root, "src.txt", "v1")
	write(t, root, "awa.toml", l.policy())
	runIt := func(t *testing.T, args ...string) (int, string, string) {
		t.Helper()
		return awa(t, root, append(append([]string{"run"}, args...), "--", "sh", "-c", validationBuild)...)
	}

	code, stdout, stderr := runIt(t, "--json")
	if code != 0 || l.callCount(t) != 0 {
		t.Fatalf("ungranted run: exit %d probes %d stderr %q", code, l.callCount(t), stderr)
	}
	var denied struct {
		Data struct {
			Cache struct {
				Reason string `json:"reason"`
				Key    string `json:"key"`
			} `json:"cache"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &denied); err != nil || denied.Data.Cache.Reason != "external-facts-unavailable" {
		t.Fatalf("ungranted run reason = %q (%v)\n%s", denied.Data.Cache.Reason, err, stdout)
	}

	_, stdout, stderr = awa(t, root, "run", "explain", "--json", "--", "sh", "-c", validationBuild)
	var explained struct {
		Data struct {
			Outcome string `json:"outcome"`
			Current struct {
				Key       string `json:"key"`
				Cacheable bool   `json:"cacheable"`
			} `json:"current"`
			ExactMatch struct {
				Assessment string `json:"assessment"`
				Exists     *bool  `json:"exists"`
			} `json:"exact_match"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &explained); err != nil {
		t.Fatalf("explain: %v\n%s\n%s", err, stdout, stderr)
	}
	if e := explained.Data; e.Outcome != "uncached" || e.Current.Cacheable || e.Current.Key != denied.Data.Cache.Key || e.ExactMatch.Assessment != "complete" || e.ExactMatch.Exists == nil || *e.ExactMatch.Exists {
		t.Fatalf("denied explain = %+v, want uncached under the run's key %s", e, denied.Data.Cache.Key)
	}

	write(t, root, ".awa/config.toml", "[run]\nallow_identity_probes = true\n")
	if code, _, stderr := runIt(t); code != 0 || isHit(stderr) || l.callCount(t) != 2 || !strings.Contains(stderr, "facts:    1 identity probe reported the same bytes") {
		t.Fatalf("granted miss: exit %d probes %d stderr %q", code, l.callCount(t), stderr)
	}
	if code, stdout, stderr := runIt(t); code != 0 || !isHit(stderr) || stdout != "checked\n" || l.callCount(t) != 3 {
		t.Fatalf("hit with a fresh probe: exit %d probes %d stdout %q stderr %q", code, l.callCount(t), stdout, stderr)
	}

	_, stdout, _ = awa(t, root, "run", "show", "--last", "--json")
	for _, want := range []string{`"identity_probes"`, `"before"`, `"after"`, `"status": "acquired"`, `"fingerprint": "blake3:`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("run show --json lacks %s:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, probeSentinel) {
		t.Error("run show discloses probe output")
	}

	if code, stdout, stderr := awa(t, root, "status"); code != 0 || strings.Contains(stdout, "none for the current state") || !strings.Contains(stdout, "reusable:    unknown") {
		t.Errorf("status without fresh facts claims a verdict: exit %d\n%s\n%s", code, stdout, stderr)
	}
	for _, args := range [][]string{
		{"doctor"}, {"config", "effective"}, {"run", "log"}, {"run", "audit"},
		{"run", "show", "--last"},
	} {
		awa(t, root, args...)
	}
	if l.callCount(t) != 3 {
		t.Fatalf("a diagnostic executed %d probes", l.callCount(t)-3)
	}
	for _, args := range [][]string{
		{"run", "explain", "--", "sh", "-c", validationBuild},
		{"run", "ls"},
		{"run", "ls", "--near"},
	} {
		if code, _, stderr := awa(t, root, args...); code != 1 || !strings.Contains(stderr, "cannot assess current reuse") {
			t.Errorf("%v answered without fresh facts: exit %d stderr %q", args, code, stderr)
		}
	}
	if l.callCount(t) != 3 {
		t.Fatalf("explain or ls executed %d probes", l.callCount(t)-3)
	}

	l.selectVersion(t, "2.0")
	if code, _, stderr := runIt(t); code != 0 || isHit(stderr) {
		t.Fatalf("a different selected tool replayed the old result: exit %d stderr %q", code, stderr)
	}

	calls := l.callCount(t)
	write(t, root, ".awa/config.toml", "[run]\nallow_identity_probes = false\n")
	for _, args := range [][]string{
		{"run", "explain", "--json", "--", "sh", "-c", validationBuild},
		{"run", "ls", "--all", "--json"},
		{"run", "ls", "--near", "--json"},
		{"status", "--json"},
	} {
		code, stdout, stderr := awa(t, root, args...)
		if code != 0 || !strings.Contains(stdout, "external-facts-unavailable") || strings.Contains(stdout, "<denied>") {
			t.Errorf("%v under a known denial: exit %d\n%s\n%s", args, code, stdout, stderr)
		}
	}
	_, stdout, _ = awa(t, root, "run", "ls", "--near", "--json")
	if !strings.Contains(stdout, `"matched": []`) || !strings.Contains(stdout, `"differences": []`) {
		t.Errorf("a denied candidate claims a computed comparison:\n%s", stdout)
	}
	code, stdout, stderr = runIt(t, "--json")
	if code != 0 || !strings.Contains(stdout, `"reason": "external-facts-unavailable"`) || strings.Contains(stdout, "<denied>") || !strings.Contains(stdout, `"matched": []`) {
		t.Errorf("denied run inline diagnosis: exit %d\n%s\n%s", code, stdout, stderr)
	}
	if l.callCount(t) != calls {
		t.Fatalf("a diagnostic under a known denial executed %d probes", l.callCount(t)-calls)
	}

	err := filepath.WalkDir(filepath.Join(root, ".awa"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p) //nolint:gosec // test fixture path
		if err == nil && strings.Contains(string(b), probeSentinel) {
			t.Errorf("%s stores probe output", p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIdentityProbeGrantOnlyFromThePrivateConfig(t *testing.T) {
	root := initProject(t)
	write(t, root, "awa.toml", "[run]\nallow_identity_probes = true\n")
	code, _, stderr := awa(t, root, "run", "--", "true")
	if code != 4 || !strings.Contains(stderr, ".awa/config.toml") {
		t.Fatalf("shared grant: exit %d stderr %q", code, stderr)
	}
}
