package acceptance

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func neverMatchingLines(stderr string) []string {
	var out []string
	for _, line := range strings.Split(stderr, "\n") {
		if strings.Contains(line, "never matches") {
			out = append(out, line)
		}
	}
	return out
}

func requireJSONDocument(t *testing.T, what, stdout string) {
	t.Helper()
	var doc struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil || doc.Command == "" {
		t.Fatalf("%s stdout is not one JSON document (%v):\n%s", what, err, stdout)
	}
}

func TestAcceptanceNeverMatchingExclusionsWarnOnceWithoutInvalidatingAnything(t *testing.T) {
	root := initProject(t)
	for i := range 30 {
		write(t, root, fmt.Sprintf("src/f%02d.txt", i), "x\n")
	}
	write(t, root, ".awaignore", "# trailing escape in a comment\\\nsrc/f00.txt\nx[\n")
	writeProjectConfig(t, root, "[scope]\nextra_excludes = [\"ok\", \"[[:nope:]]\"]\n")

	code, stdout, stderr := awa(t, root, "config", "validate", "--json")
	if code != 0 || !strings.Contains(stdout, `"valid": true`) {
		t.Fatalf("config validate exit = %d stdout = %s stderr = %s", code, stdout, stderr)
	}
	requireJSONDocument(t, "config validate", stdout)
	if got := neverMatchingLines(stderr); len(got) != 1 || !strings.Contains(got[0], "warning: scope.extra_excludes[1]: pattern \"[[:nope:]]\"") {
		t.Errorf("config validate warnings = %q, want the one configured element", got)
	}

	for _, args := range [][]string{
		{"checkpoint", "--json"},
		{"status", "--json"},
		{"changes", "latest..now", "--json"},
		{"run", "--json", "--", "true"},
	} {
		code, stdout, stderr := awa(t, root, args...)
		if code != 0 {
			t.Fatalf("%v exit = %d, stderr = %s", args, code, stderr)
		}
		requireJSONDocument(t, strings.Join(args, " "), stdout)
		if strings.Contains(stdout, "never matches") {
			t.Errorf("%v put the warning in its payload:\n%s", args, stdout)
		}
		got := neverMatchingLines(stderr)
		if len(got) != 2 ||
			!strings.Contains(got[0], "warning: scope.extra_excludes[1]:") ||
			!strings.Contains(got[1], "warning: .awaignore:3: pattern \"x[\" never matches: unclosed bracket expression") {
			t.Errorf("%v warnings = %q, want each source element once", args, got)
		}
	}

	code, stdout, stderr = awa(t, root, "doctor", "--json")
	if code != 0 {
		t.Fatalf("doctor exit = %d, stderr = %s", code, stderr)
	}
	var report struct {
		Data struct {
			Findings []struct {
				Code     string `json:"code"`
				Severity string `json:"severity"`
				Path     string `json:"path"`
				Subject  string `json:"subject"`
			} `json:"findings"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decode doctor: %v\n%s", err, stdout)
	}
	subjects := map[string]string{}
	for _, f := range report.Data.Findings {
		if f.Code == "pattern-never-matches" {
			if f.Severity != "warning" {
				t.Errorf("%s severity = %q, want warning", f.Subject, f.Severity)
			}
			subjects[f.Subject] = f.Path
		}
	}
	if len(subjects) != 2 || !strings.HasSuffix(subjects["scope.extra_excludes[1]"], "config.toml") || !strings.HasSuffix(subjects[".awaignore:3"], ".awaignore") {
		t.Errorf("doctor pattern findings = %v, want the config element and the ignore-file line", subjects)
	}
}

func TestAcceptancePatternCarrierAndRetentionRefusals(t *testing.T) {
	root := initProject(t)
	write(t, root, "a.env", dummyEnvContent)

	for _, key := range []string{"scope", "history", "run"} {
		writeProjectConfig(t, root, "["+key+"]\nextra_excludes = [\"ok\", \"a\\nb\"]\n")
		code, stdout, stderr := awa(t, root, "config", "validate")
		if code != 4 || stdout != "" || !strings.Contains(stderr, key+".extra_excludes[1] must not contain a line feed") {
			t.Errorf("[%s] LF element: exit = %d stdout = %q stderr = %q", key, code, stdout, stderr)
		}
	}

	for name, element := range map[string]string{"never-matching": "x[", "carriage return": "a\\r", "line feed": "a\\nb"} {
		writeProjectConfig(t, root, "[checkpoint]\nhash_only_patterns = [\"*.env\", \""+element+"\"]\n")
		for _, args := range [][]string{{"config", "validate"}, {"checkpoint"}} {
			code, _, stderr := awa(t, root, args...)
			if code != 4 || !strings.Contains(stderr, "checkpoint.hash_only_patterns[1]") {
				t.Errorf("%s retention element, %v: exit = %d stderr = %q, want a located refusal", name, args, code, stderr)
			}
		}
	}
	writeProjectConfig(t, root, "")
	if code, stdout, stderr := awa(t, root, "log"); code != 0 || strings.Contains(stdout, "a.env") || !strings.Contains(stdout+stderr, "no checkpoints") {
		t.Errorf("a refused retention list published a checkpoint: exit = %d stdout = %q stderr = %q", code, stdout, stderr)
	}
}

func TestAcceptanceRunExcludeWithALineFeedIsAUsageErrorBeforeTheChildStarts(t *testing.T) {
	root := initProject(t)
	write(t, root, "a.txt", "x\n")

	code, stdout, stderr := awa(t, root, "run", "--exclude", "ok", "--exclude", "a\nb", "--", "touch", "child-ran")
	if code != 2 || stdout != "" || !strings.Contains(stderr, "--exclude[1]") || !strings.Contains(stderr, "line feed") {
		t.Errorf("exit = %d stdout = %q stderr = %q, want a located usage error", code, stdout, stderr)
	}
	if existsAt(root, "child-ran") {
		t.Error("the child ran despite the refused --exclude")
	}
	if code, stdout, _ := awa(t, root, "run", "ls", "--all"); code != 0 || strings.Contains(stdout, "touch") {
		t.Errorf("a refused --exclude stored a run: exit = %d\n%s", code, stdout)
	}
}

func TestAcceptanceRunExcludeIsAPatternNotALiteralPath(t *testing.T) {
	root := initProject(t)
	write(t, root, "deep/gen1", "a\n")
	write(t, root, "dir/name", "a\n")
	write(t, root, "deep/dir/name", "a\n")

	status := func() string {
		t.Helper()
		code, stdout, stderr := awa(t, root, "run", "--json", "--exclude", "gen?", "--exclude", "dir/name", "--", "true")
		if code != 0 {
			t.Fatalf("run exit = %d, %s", code, stderr)
		}
		var env struct {
			Data struct {
				Cache struct {
					Status string `json:"status"`
				} `json:"cache"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(stdout), &env); err != nil {
			t.Fatalf("decode run: %v\n%s", err, stdout)
		}
		return env.Data.Cache.Status
	}

	if got := status(); got != "miss" {
		t.Fatalf("first run = %q, want miss", got)
	}
	write(t, root, "deep/gen1", "b\n")
	write(t, root, "dir/name", "b\n")
	if got := status(); got != "hit" {
		t.Errorf("after changing only inputs matched by `gen?` at depth and root-anchored `dir/name`: %q, want hit", got)
	}
	write(t, root, "deep/dir/name", "b\n")
	if got := status(); got != "miss" {
		t.Errorf("after changing deep/dir/name, which `dir/name` does not match: %q, want miss", got)
	}
}
