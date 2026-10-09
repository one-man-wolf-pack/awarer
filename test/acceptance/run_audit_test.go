package acceptance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type auditEnvelope struct {
	Command string `json:"command"`
	Data    struct {
		Window struct {
			History   string `json:"history"`
			Requested int    `json:"requested"`
			Selected  int    `json:"selected"`
			Older     string `json:"older"`
			Coverage  string `json:"coverage"`
		} `json:"window"`
		Reads struct {
			Classified   int  `json:"classified"`
			Unavailable  int  `json:"unavailable"`
			Complete     bool `json:"complete"`
			Availability []struct {
				State    string `json:"state"`
				Count    int    `json:"count"`
				Examples struct {
					RunIDs []string `json:"run_ids"`
				} `json:"examples"`
			} `json:"availability"`
		} `json:"reads"`
		Classifications []struct {
			Group    string `json:"group"`
			Kind     string `json:"kind"`
			Reason   string `json:"reason"`
			Count    int    `json:"count"`
			Examples struct {
				RunIDs []string `json:"run_ids"`
			} `json:"examples"`
		} `json:"classifications"`
		Suggestions []struct {
			Kind string   `json:"kind"`
			Argv []string `json:"argv"`
		} `json:"suggestions"`
	} `json:"data"`
}

func runAudit(t *testing.T, root string, args ...string) auditEnvelope {
	t.Helper()
	code, stdout, stderr := awa(t, root, append([]string{"run", "audit", "--json"}, args...)...)
	if code != 0 {
		t.Fatalf("run audit exit = %d, stderr = %q", code, stderr)
	}
	var env auditEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("parse audit JSON: %v\n%s", err, stdout)
	}
	if env.Command != "run.audit" {
		t.Fatalf("command = %q", env.Command)
	}
	return env
}

func auditPairs(env auditEnvelope) string {
	var out []string
	for _, c := range env.Data.Classifications {
		out = append(out, fmt.Sprintf("%s %s/%s=%d", c.Group, c.Kind, c.Reason, c.Count))
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// TestRunAuditSummarizesRetainedExecutions drives real executions of each shape
// through the built binary and audits them: a replayed hit adds no record, a
// --record run over piped input counts once as deliberate recording, and a failed
// child is history, not an audit failure. Corrupting one selected record keeps it
// in the window as unavailable rather than refilling; a removed output payload does
// not change the classification, because the audit never reads payloads.
func TestRunAuditSummarizesRetainedExecutions(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	write(t, root, "data.txt", "v1")

	steps := []struct {
		stdin bool
		args  []string
	}{
		{false, []string{"run", "--", h, "-out", "a"}},                          // reusable
		{false, []string{"run", "--", h, "-out", "a"}},                          // hit: no record
		{true, []string{"run", "--record", "--", h, "-out", "b"}},               // record-only/record-only
		{true, []string{"run", "--", h, "-out", "c"}},                           // record-only/stdin-not-keyed
		{false, []string{"run", "--no-cache-failures", "--", h, "-exit", "3"}},  // failed-not-cached
		{false, []string{"run", "--", h, "-fix", "data.txt", "-content", "v2"}}, // mutated-state
	}
	for _, s := range steps {
		var code int
		var stderr string
		if s.stdin {
			code, _, stderr = awaWithStdin(t, root, false, s.args...)
		} else {
			code, _, stderr = awa(t, root, s.args...)
		}
		if code != 0 && code != 3 {
			t.Fatalf("awa %v exit = %d, stderr = %q", s.args, code, stderr)
		}
	}

	env := runAudit(t, root)
	want := "changed-state non-reusable/mutated-state=1 deliberate-recording record-only/record-only=1 " +
		"failure-cache-policy non-reusable/failed-not-cached=1 policy-blocker record-only/stdin-not-keyed=1 " +
		"reusable-when-stored reusable/=1"
	if got := auditPairs(env); got != want {
		t.Errorf("classifications:\n got %s\nwant %s", got, want)
	}
	w := env.Data.Window
	if w.History != "present" || w.Requested != 200 || w.Selected != 5 || w.Older != "none" || w.Coverage != "all-retained" {
		t.Errorf("window = %+v", w)
	}
	if env.Data.Reads.Classified != 5 || !env.Data.Reads.Complete {
		t.Errorf("reads = %+v", env.Data.Reads)
	}

	var reusableID, mutatedID string
	for _, c := range env.Data.Classifications {
		switch c.Group {
		case "reusable-when-stored":
			reusableID = c.Examples.RunIDs[0]
		case "changed-state":
			mutatedID = c.Examples.RunIDs[0]
		}
	}
	entry := func(id string) string { return filepath.Join(root, ".awa", "runs", "entries", id[:2], id) }

	// A missing payload is invisible to a metadata-only audit.
	if err := os.Remove(filepath.Join(entry(mutatedID), "stdout.log")); err != nil {
		t.Fatal(err)
	}
	// Corrupt the oldest record's metadata: it keeps its place in the window.
	meta := filepath.Join(entry(reusableID), "meta.json")
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	env = runAudit(t, root, "-n", "5")
	if env.Data.Window.Selected != 5 || env.Data.Reads.Classified != 4 || env.Data.Reads.Unavailable != 1 || env.Data.Reads.Complete {
		t.Errorf("after corruption: window %+v reads %+v", env.Data.Window, env.Data.Reads)
	}
	if a := env.Data.Reads.Availability; len(a) != 1 || a[0].State != "corrupt" || a[0].Examples.RunIDs[0] != reusableID {
		t.Errorf("availability = %+v, want the corrupt record %s", a, reusableID)
	}
	if !strings.Contains(auditPairs(env), "changed-state non-reusable/mutated-state=1") {
		t.Errorf("payload removal changed the classification: %s", auditPairs(env))
	}

	// A smaller window discloses that older history exists.
	env = runAudit(t, root, "-n", "2")
	if env.Data.Window.Selected != 2 || env.Data.Window.Older != "present" || env.Data.Window.Coverage != "newest-window" {
		t.Errorf("-n 2 window = %+v", env.Data.Window)
	}

	// Human form: a partial audit still succeeds and warns on stderr.
	code, stdout, stderr := awa(t, root, "run", "audit")
	if code != 0 {
		t.Fatalf("human audit exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "warning:") || !strings.Contains(stdout, "not every invocation") {
		t.Errorf("human audit:\nstdout %s\nstderr %s", stdout, stderr)
	}
	for _, leaked := range []string{h, "-out", "data.txt"} {
		if strings.Contains(stdout, leaked) {
			t.Errorf("human audit discloses argv %q:\n%s", leaked, stdout)
		}
	}
}
