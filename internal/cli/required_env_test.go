package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

const requireCheckToken = `[[run.required_env]]
argv = ["true"]
cwd = "."
names = ["AWA_TEST_REQ_TOKEN_DIR"]
`

// TestRunRefusesAnUnmetRequirementAsAnAwaFailure proves the CLI boundary of the
// preflight: exit 1 (not usage 2, not config 4, not a child code), an empty JSON
// stdout, a diagnostic that names the declaration, its layer, the invocation and the
// name's state, and never a value or an undeclared ambient name.
func TestRunRefusesAnUnmetRequirementAsAnAwaFailure(t *testing.T) {
	root := initProject(t)
	writeFile(t, root, filepath.Join(".awa", "config.toml"), requireCheckToken)
	t.Setenv("AWA_TEST_UNRELATED_SECRET", "do-not-print")

	for _, state := range []struct {
		name, value, want string
		set               bool
	}{{"unset", "", "is unset", false}, {"empty", "", "is empty", true}} {
		t.Run(state.name, func(t *testing.T) {
			if state.set {
				t.Setenv("AWA_TEST_REQ_TOKEN_DIR", state.value)
			}
			for _, args := range [][]string{
				{"run", "--root", root, "--cwd", root, "--", "true"},
				{"run", "--json", "--record", "--root", root, "--cwd", root, "--", "true"},
			} {
				code, stdout, stderr := run(args...)
				if code != int(ExitGenericError) {
					t.Fatalf("%q exit = %d, want 1; stderr = %q", args, code, stderr)
				}
				if stdout != "" {
					t.Fatalf("%q wrote stdout on refusal: %q", args, stdout)
				}
				for _, want := range []string{"run refused before execution", "run.required_env[0] (local config)", "`true`", "AWA_TEST_REQ_TOKEN_DIR " + state.want, "awa help privacy"} {
					if !strings.Contains(stderr, want) {
						t.Errorf("stderr lacks %q:\n%s", want, stderr)
					}
				}
				if strings.Contains(stderr, "AWA_TEST_UNRELATED_SECRET") || strings.Contains(stderr, "do-not-print") {
					t.Errorf("diagnostic enumerated ambient state:\n%s", stderr)
				}
			}
			if code, stdout, _ := run("run", "log", "--json", "--root", root); code != 0 || strings.Contains(stdout, `"true"`) {
				t.Fatalf("refusal recorded a run: %s", stdout)
			}
		})
	}

	t.Setenv("AWA_TEST_REQ_TOKEN_DIR", "/secret/path/value")
	code, _, stderr := run("run", "--root", root, "--cwd", root, "--", "true")
	if code != int(ExitSuccess) {
		t.Fatalf("satisfied run exit = %d: %s", code, stderr)
	}
	if strings.Contains(stderr, "/secret/path/value") || strings.Contains(stderr, "required_env") {
		t.Fatalf("satisfied run printed requirement noise or a value:\n%s", stderr)
	}
}

func TestExplainReportsTheRequirementWithoutValues(t *testing.T) {
	root := initProject(t)
	writeFile(t, root, "awa.toml", requireCheckToken)

	code, stdout, stderr := run("run", "explain", "--json", "--root", root, "--cwd", root, "--", "true")
	if code != int(ExitSuccess) {
		t.Fatalf("explain exit = %d, stderr = %q", code, stderr)
	}
	var doc struct {
		Data struct {
			Outcome string `json:"outcome"`
			Reason  string `json:"reason"`
			Current struct {
				Assessment  string  `json:"assessment"`
				Key         *string `json:"key"`
				Cacheable   *bool   `json:"cacheable"`
				Requirement *struct {
					Declaration int      `json:"declaration"`
					Origin      string   `json:"origin"`
					Argv        []string `json:"argv"`
					CWD         string   `json:"cwd"`
					Satisfied   bool     `json:"satisfied"`
					Names       []struct {
						Name  string `json:"name"`
						State string `json:"state"`
					} `json:"names"`
				} `json:"environment_requirement"`
			} `json:"current"`
			Exact struct {
				Assessment string `json:"assessment"`
				Exists     *bool  `json:"exists"`
			} `json:"exact_match"`
			Candidates []json.RawMessage `json:"candidates"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	d := doc.Data
	if d.Outcome != "refused" || d.Reason != "env-requirement-unmet" {
		t.Errorf("outcome = %s/%s", d.Outcome, d.Reason)
	}
	if d.Current.Key != nil || d.Current.Cacheable == nil || *d.Current.Cacheable || d.Current.Assessment != "identity-unavailable" {
		t.Errorf("current = %+v, want null key and known non-cacheable", d.Current)
	}
	if d.Exact.Assessment != "current-identity-unavailable" || d.Exact.Exists != nil || len(d.Candidates) != 0 {
		t.Errorf("exact = %+v candidates = %d", d.Exact, len(d.Candidates))
	}
	r := d.Current.Requirement
	if r == nil || r.Origin != "shared" || r.Satisfied || r.CWD != "." || len(r.Names) != 1 || r.Names[0].State != "unset" {
		t.Fatalf("environment_requirement = %+v", r)
	}

	code, stdout, _ = run("run", "explain", "--root", root, "--cwd", root, "--", "true")
	if code != int(ExitSuccess) || !strings.Contains(stdout, "cache:  refused") ||
		!strings.Contains(stdout, "env requirement: run.required_env[0] (shared config) unmet: AWA_TEST_REQ_TOKEN_DIR is unset") {
		t.Fatalf("human explain = %d\n%s", code, stdout)
	}

	t.Setenv("AWA_TEST_REQ_TOKEN_DIR", "/secret/path/value")
	code, stdout, _ = run("run", "explain", "--json", "--root", root, "--cwd", root, "--", "true")
	if code != 0 || strings.Contains(stdout, "/secret/path/value") || !strings.Contains(stdout, `"satisfied": true`) || !strings.Contains(stdout, `"state": "set"`) {
		t.Fatalf("satisfied explain = %d\n%s", code, stdout)
	}
	// An unmatched invocation carries no requirement facts at all.
	_, stdout, _ = run("run", "explain", "--json", "--root", root, "--cwd", root, "--", "true", "x")
	if strings.Contains(stdout, "environment_requirement") {
		t.Fatalf("unmatched explain carries requirement facts:\n%s", stdout)
	}
}

// TestConfigInspectionShowsDeclarationsWithoutTheirVariables proves config inspection
// describes declarations and their origin whether or not any variable is set, and that
// an invalid declaration is a config error (exit 4) while a missing value is not.
func TestConfigInspectionShowsDeclarationsWithoutTheirVariables(t *testing.T) {
	root := initProject(t)
	writeFile(t, root, "awa.toml", requireCheckToken)
	code, stdout, stderr := run("config", "effective", "--root", root)
	if code != 0 || !strings.Contains(stdout, `required_env = [{ argv = ["true"], cwd = ".", names = ["AWA_TEST_REQ_TOKEN_DIR"] }]  # shared`) {
		t.Fatalf("config effective = %d %s\n%s", code, stderr, stdout)
	}
	if code, _, stderr := run("config", "validate", "--root", root); code != 0 {
		t.Fatalf("config validate with the variable unset = %d: %s", code, stderr)
	}
	code, stdout, _ = run("config", "effective", "--json", "--root", root)
	var eff struct {
		Data struct {
			Run struct {
				RequiredEnv []struct {
					Argv  []string `json:"argv"`
					CWD   string   `json:"cwd"`
					Names []string `json:"names"`
				} `json:"required_env"`
			} `json:"run"`
			Origins map[string]string `json:"origins"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &eff); err != nil || code != 0 {
		t.Fatalf("config effective --json = %d, %v\n%s", code, err, stdout)
	}
	if rs := eff.Data.Run.RequiredEnv; len(rs) != 1 || rs[0].CWD != "." || rs[0].Names[0] != "AWA_TEST_REQ_TOKEN_DIR" || eff.Data.Origins["run.required_env"] != "shared" {
		t.Fatalf("config effective --json required_env = %+v origins = %v", rs, eff.Data.Origins["run.required_env"])
	}

	writeFile(t, root, filepath.Join(".awa", "config.toml"), "[[run.required_env]]\nargv = [\"true\"]\ncwd = \".\"\nnames = [\"AWA_RUN\"]\n")
	code, _, stderr = run("run", "--root", root, "--cwd", root, "--", "true")
	if code != int(ExitConfigError) || !strings.Contains(stderr, "reserved by awa") {
		t.Fatalf("reserved required name = %d: %s", code, stderr)
	}
}

// TestStatusAndListTreatABlockedInvocationAsKnownNotReusable proves the current
// listing and status keep a complete window: the blocked run is assessed and not
// reusable, and an unrelated reusable run still counts.
func TestStatusAndListTreatABlockedInvocationAsKnownNotReusable(t *testing.T) {
	root := initProject(t)
	writeFile(t, root, "awa.toml", requireCheckToken)
	t.Setenv("AWA_TEST_REQ_TOKEN_DIR", "/x")
	for _, argv := range [][]string{{"true"}, {"true", "other"}} {
		if code, _, stderr := run(append([]string{"run", "--root", root, "--cwd", root, "--"}, argv...)...); code != 0 {
			t.Fatalf("seed run %q = %d: %s", argv, code, stderr)
		}
	}
	t.Setenv("AWA_TEST_REQ_TOKEN_DIR", "")

	code, stdout, stderr := run("run", "ls", "--all", "--json", "--root", root)
	var ls struct {
		Data struct {
			Assessment string `json:"assessment"`
			Reusable   []struct {
				Command  []string `json:"command"`
				Reusable bool     `json:"reusable"`
				Reason   string   `json:"reason"`
			} `json:"reusable"`
			Unassessed []json.RawMessage `json:"unassessed"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &ls); err != nil || code != 0 {
		t.Fatalf("run ls = %d %v: %s %s", code, err, stdout, stderr)
	}
	if ls.Data.Assessment != "complete" || len(ls.Data.Unassessed) != 0 || len(ls.Data.Reusable) != 2 {
		t.Fatalf("run ls = %+v", ls.Data)
	}
	for _, r := range ls.Data.Reusable {
		blocked := len(r.Command) == 1
		if blocked && (r.Reusable || r.Reason != "env-requirement-unmet") || !blocked && !r.Reusable {
			t.Errorf("row %q = reusable %t reason %q", r.Command, r.Reusable, r.Reason)
		}
	}

	code, stdout, _ = run("status", "--json", "--root", root)
	var st struct {
		Data struct {
			Review struct {
				Runs struct {
					Assessment string `json:"assessment"`
					Reusable   *int   `json:"reusable"`
				} `json:"runs"`
			} `json:"review"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &st); err != nil || code != 0 {
		t.Fatalf("status = %d %v: %s", code, err, stdout)
	}
	if r := st.Data.Review.Runs; r.Assessment != "complete" || r.Reusable == nil || *r.Reusable != 1 {
		t.Fatalf("status runs = %+v, want a complete window with one reusable run", r)
	}
}

// TestRequirementFromAnExplicitConfigFileIsLabelledAsSuch proves the declaration's
// layer reads naturally for every source, including an explicit --config file, and
// that human explain places the requirement beside the decision, before the exact
// match line.
func TestRequirementFromAnExplicitConfigFileIsLabelledAsSuch(t *testing.T) {
	root := initProject(t)
	cfgPath := filepath.Join(t.TempDir(), "explicit.toml")
	writeFile(t, filepath.Dir(cfgPath), filepath.Base(cfgPath), requireCheckToken)

	code, _, stderr := run("run", "--config", cfgPath, "--root", root, "--cwd", root, "--", "true")
	if code != int(ExitGenericError) || !strings.Contains(stderr, "run.required_env[0] (--config file)") {
		t.Fatalf("refusal = %d: %s", code, stderr)
	}
	code, stdout, _ := run("run", "explain", "--config", cfgPath, "--root", root, "--cwd", root, "--", "true")
	req, exact := strings.Index(stdout, "env requirement: run.required_env[0] (--config file) unmet"), strings.Index(stdout, "exact match:")
	if code != 0 || req < 0 || exact < 0 || req > exact {
		t.Fatalf("explain = %d\n%s", code, stdout)
	}
	_, stdout, _ = run("run", "explain", "--json", "--config", cfgPath, "--root", root, "--cwd", root, "--", "true")
	if !strings.Contains(stdout, `"origin": "config"`) {
		t.Fatalf("JSON origin:\n%s", stdout)
	}
}

// TestRefusalBoundsAndEscapesFailingNames proves the diagnostic lists at most eight
// failing names with an omitted count, and renders a control character in a configured
// name as a visible escape rather than sending it to the terminal.
func TestRefusalBoundsAndEscapesFailingNames(t *testing.T) {
	root := initProject(t)
	names := []string{`"AWA_T_ESC\u001b[2J"`}
	for i := range 10 {
		names = append(names, fmt.Sprintf(`"AWA_T_MISSING_%02d"`, i))
	}
	writeFile(t, root, "awa.toml", "[[run.required_env]]\nargv = [\"true\"]\ncwd = \".\"\nnames = ["+strings.Join(names, ", ")+"]\n")

	code, _, stderr := run("run", "--root", root, "--cwd", root, "--", "true")
	if code != int(ExitGenericError) {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if strings.ContainsRune(stderr, 0x1b) || !strings.Contains(stderr, `AWA_T_ESC\x1b[2J is unset`) {
		t.Errorf("name not escaped:\n%q", stderr)
	}
	if !strings.Contains(stderr, "AWA_T_MISSING_06 is unset, 3 more") || strings.Contains(stderr, "AWA_T_MISSING_07") {
		t.Errorf("failing names not bounded to eight plus a count:\n%s", stderr)
	}
}
