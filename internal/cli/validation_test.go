package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"awarer/internal/infra/configfile"
)

const validationScript = "mkdir -p target && date +%N > target/out && echo checked"

func validationProject(t *testing.T, extra string) string {
	t.Helper()
	root := initProject(t)
	toml := extra + `
[[run.validation]]
name = "check"
argv = ["sh", "-c", "` + validationScript + `"]
cwd = "."
unobserved_dirs = ["target"]
`
	if err := os.WriteFile(filepath.Join(root, "awa.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func runValidation(root string, flags ...string) (int, string, string) {
	args := append([]string{"run"}, flags...)
	args = append(args, "--root", root, "--cwd", root, "--", "sh", "-c", validationScript)
	return run(args...)
}

func TestValidationConflictExitClasses(t *testing.T) {
	root := validationProject(t, "")
	code, _, stderr := runValidation(root, "--include", filepath.Join(root, "target", "debug"))
	if code != int(ExitUsageError) || !strings.Contains(stderr, `"check"`) || !strings.Contains(stderr, `--include "target/debug"`) {
		t.Fatalf("CLI conflict: exit %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "target")); !os.IsNotExist(err) {
		t.Fatal("a refused run executed its command")
	}

	root = validationProject(t, "[run]\ndefault_scope = [\"target\"]\n")
	code, _, stderr = runValidation(root)
	if code != int(ExitConfigError) || !strings.Contains(stderr, "run.default_scope") {
		t.Fatalf("config conflict: exit %d, stderr %q", code, stderr)
	}
	code, _, stderr = runValidation(root, "--include", filepath.Join(root, "target", "x"))
	if code != int(ExitUsageError) {
		t.Fatalf("both conflict: exit %d, want the CLI conflict; stderr %q", code, stderr)
	}
	if code, _, stderr = runValidation(root, "--scope", root); code != int(ExitSuccess) {
		t.Fatalf("a replaced default was checked: exit %d, stderr %q", code, stderr)
	}
}

func TestValidationReplayIsDisclosedAsResultOnly(t *testing.T) {
	root := validationProject(t, "")

	code, stdout, stderr := runValidation(root, "--json")
	if code != int(ExitSuccess) {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	var first struct {
		Data struct {
			Inputs struct {
				Validation *validationPolicyDoc `json:"validation"`
			} `json:"inputs"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &first); err != nil {
		t.Fatalf("%v\n%s", err, stdout)
	}
	if v := first.Data.Inputs.Validation; v == nil || v.Policy != "check" || v.Replay != "result-only" || len(v.UnobservedDirs) != 1 || v.UnobservedDirs[0] != "target" {
		t.Fatalf("inputs.validation = %+v", first.Data.Inputs.Validation)
	}

	if err := os.RemoveAll(filepath.Join(root, "target")); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runValidation(root)
	if code != int(ExitSuccess) || !strings.Contains(stderr, "awa run: hit,") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "checked") || !strings.Contains(stderr, "replay:   result only (policy check); not observed or recreated: target") {
		t.Fatalf("hit disclosure missing: stdout %q stderr %q", stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "target")); !os.IsNotExist(err) {
		t.Fatal("a replay recreated the waived directory")
	}

	code, stdout, _ = run("run", "explain", "--json", "--root", root, "--cwd", root, "--", "sh", "-c", validationScript)
	if code != int(ExitSuccess) {
		t.Fatalf("explain exit %d", code)
	}
	var explained struct {
		Data struct {
			Current struct {
				Validation *validationPolicyDoc `json:"validation"`
			} `json:"current"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &explained); err != nil || explained.Data.Current.Validation == nil {
		t.Fatalf("current.validation missing: %v\n%s", err, stdout)
	}

	for _, args := range [][]string{
		{"run", "show", "--last", "--meta", "--json", "--root", root},
		{"run", "ls", "--json", "--root", root},
		{"run", "log", "--json", "--root", root},
	} {
		code, stdout, stderr := run(args...)
		var compact bytes.Buffer
		_ = json.Compact(&compact, []byte(stdout))
		if code != int(ExitSuccess) || !strings.Contains(compact.String(), `"validation":{"policy":"check","replay":"result-only","unobserved_dirs":["target"]}`) {
			t.Errorf("%v: exit %d, stdout %s stderr %s", args, code, stdout, stderr)
		}
	}
	if code, stdout, _ := run("run", "show", "--last", "--meta", "--root", root); code != int(ExitSuccess) || !strings.Contains(stdout, "check (result-only replay); not observed: target") {
		t.Errorf("show --meta: exit %d\n%s", code, stdout)
	}
}

func TestConfigEffectiveListsValidationPolicies(t *testing.T) {
	root := validationProject(t, "")
	code, stdout, stderr := run("config", "effective", "--root", root)
	if code != int(ExitSuccess) || !strings.Contains(stdout, `name = "check"`) || !strings.Contains(stdout, `unobserved_dirs = ["target"]`) {
		t.Fatalf("exit %d stderr %q\n%s", code, stderr, stdout)
	}
}

func TestConfigTemplateValidationExampleDecodes(t *testing.T) {
	code, stdout, _ := run("config", "template")
	if code != int(ExitSuccess) {
		t.Fatal(code)
	}
	_, rest, ok := strings.Cut(stdout, "# Example:\n")
	if !ok {
		t.Fatal("template has no validation example")
	}
	var lines []string
	for _, l := range strings.Split(rest, "\n") {
		if !strings.HasPrefix(l, "# ") {
			break
		}
		lines = append(lines, strings.TrimPrefix(l, "# "))
	}
	cfg, err := configfile.Decode([]byte(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatalf("uncommented example does not load: %v\n%s", err, strings.Join(lines, "\n"))
	}
	if len(cfg.Run.Validation) != 1 || cfg.Run.Validation[0].Name != "cargo-test" {
		t.Fatalf("example decoded to %+v", cfg.Run.Validation)
	}
}
