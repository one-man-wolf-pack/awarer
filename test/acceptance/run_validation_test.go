package acceptance

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// build/ is both a run input and a watched effect root by default, so without the
// policy this command is never reusable; every waived outcome below is the policy's.
// gen/ is waived too because, unlike build/, history scans see it: restore can only
// show the waiver withholding a deletion where the current scope reaches.
const validationBuild = "mkdir -p build/obj && echo $$ > build/obj/out && echo checked"

const validationPolicyTOML = `
[[run.validation]]
name = "%s"
argv = ["sh", "-c", "` + validationBuild + `"]
cwd = "."
unobserved_dirs = ["build", "gen"]
`

func validationPolicy(name string) string {
	return strings.Replace(validationPolicyTOML, "%s", name, 1)
}

func TestNamedValidationReplaysOnlyTheResult(t *testing.T) {
	root := initProject(t)
	write(t, root, "src.txt", "v1")
	runIt := func(t *testing.T, args ...string) (string, string) {
		t.Helper()
		code, stdout, stderr := awa(t, root, append(append([]string{"run"}, args...), "--", "sh", "-c", validationBuild)...)
		if code != 0 {
			t.Fatalf("run exit = %d, stderr = %q", code, stderr)
		}
		return stdout, stderr
	}

	runIt(t)
	if _, stderr := runIt(t); isHit(stderr) {
		t.Fatalf("without a policy the build directory keeps the command unreusable, stderr = %q", stderr)
	}

	write(t, root, "awa.toml", validationPolicy("check"))
	stdout, stderr := runIt(t, "--json")
	var first struct {
		Data struct {
			Run struct {
				ID string `json:"id"`
			} `json:"run"`
			Inputs struct {
				Validation *struct {
					Policy string `json:"policy"`
				} `json:"validation"`
			} `json:"inputs"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &first); err != nil || first.Data.Inputs.Validation == nil || first.Data.Run.ID == "" {
		t.Fatalf("decode: %v\n%s\n%s", err, stdout, stderr)
	}
	runID := first.Data.Run.ID
	built := readAt(t, root, "build/obj/out")

	stdout, stderr = runIt(t)
	if !isHit(stderr) || stdout != "checked\n" {
		t.Fatalf("repeat: stdout %q stderr %q", stdout, stderr)
	}
	if !strings.Contains(stderr, "replay:   result only (policy check); not observed or recreated: build, gen") {
		t.Errorf("the hit is not disclosed as result-only: %q", stderr)
	}
	if got := readAt(t, root, "build/obj/out"); got != built {
		t.Fatal("a replay executed the command")
	}

	if err := os.RemoveAll(filepath.Join(root, "build")); err != nil {
		t.Fatal(err)
	}
	if _, stderr := runIt(t); !isHit(stderr) {
		t.Fatalf("removing the waived directory lost the hit: %q", stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "build")); !os.IsNotExist(err) {
		t.Fatal("a replay recreated the waived directory")
	}

	write(t, root, "src.txt", "v2")
	if _, stderr := runIt(t); isHit(stderr) {
		t.Fatalf("an input edit replayed: %q", stderr)
	}
	runIt(t)
	write(t, root, "node_modules/pkg/index.js", "x")
	if _, stderr := runIt(t); isHit(stderr) {
		t.Fatalf("an unwaived effect root change replayed: %q", stderr)
	}
	if _, stderr := runIt(t); !isHit(stderr) {
		t.Fatalf("the rerun did not re-establish a hit: %q", stderr)
	}

	write(t, root, ".awa/config.toml", validationPolicy("renamed"))
	// A hit replays a stored run, so it discloses the policy that run recorded.
	if _, stderr := runIt(t); !isHit(stderr) || !strings.Contains(stderr, "(policy check)") {
		t.Fatalf("a local rename lost the hit: %q", stderr)
	}
	if code, stdout, _ := awa(t, root, "run", "explain", "--", "sh", "-c", validationBuild); code != 0 || !strings.Contains(stdout, "(policy renamed)") {
		t.Errorf("command-mode explain does not show the current label: %s", stdout)
	}
	if err := os.Remove(filepath.Join(root, ".awa", "config.toml")); err != nil {
		t.Fatal(err)
	}
	write(t, root, "awa.toml", "# shared\n"+validationPolicy("check"))
	if _, stderr := runIt(t); isHit(stderr) {
		t.Fatalf("an awa.toml edit is an input change and must miss: %q", stderr)
	}

	for _, args := range [][]string{
		{"run", "show", "--meta", "--json", runID},
		{"run", "ls", "--json"},
		{"run", "explain", "--json", "--", "sh", "-c", validationBuild},
	} {
		code, stdout, stderr := awa(t, root, args...)
		var compact bytes.Buffer
		_ = json.Compact(&compact, []byte(stdout))
		if code != 0 || !strings.Contains(compact.String(), `"validation":{"policy":"check","replay":"result-only","unobserved_dirs":["build","gen"]}`) {
			t.Errorf("%v: exit %d\n%s\n%s", args, code, stdout, stderr)
		}
	}
	if code, stdout, _ := awa(t, root, "run", "explain", "--", "sh", "-c", validationBuild); code != 0 || !strings.Contains(stdout, "replay: result only (policy check); not observed or recreated: build, gen") {
		t.Errorf("explain human: exit %d\n%s", code, stdout)
	}

	write(t, root, "gen/later/x", "made after the run")
	_, doc := restoreJSON(t, root, "--all", "run:"+runID+":before")
	if doc.Data.Counts.DeleteFile != 0 || doc.Data.Counts.DeleteDirectory != 0 {
		t.Fatalf("restore planned a deletion below a waived directory: %+v", doc.Data.Counts)
	}
	// A run observation's scan policy already differs from today's history scan, so
	// policy-incompatible can withhold the deletion before the waiver's boundary does.
	if got := failureReasons(doc, "gen/later/x"); !slices.Contains(got, "ignored-boundary") && !slices.Contains(got, "policy-incompatible") {
		t.Errorf("gen/later/x reasons = %v, want the deletion withheld", got)
	}

	code, changes, stderr := awa(t, root, "changes", "run:"+runID+":before..run:"+runID+":after")
	if code != 0 || strings.Contains(changes, "build") {
		t.Errorf("changes reported the waived directory: exit %d\n%s\n%s", code, changes, stderr)
	}
}

func TestNamedValidationConflictIsRefusedBeforeExecution(t *testing.T) {
	root := initProject(t)
	write(t, root, "awa.toml", validationPolicy("check"))
	code, _, stderr := awa(t, root, "run", "--include", "build/obj", "--", "sh", "-c", validationBuild)
	if code != 2 || !strings.Contains(stderr, `validation policy "check" leaves "build" unobserved`) {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "build")); !os.IsNotExist(err) {
		t.Fatal("a refused run executed its command")
	}
}
