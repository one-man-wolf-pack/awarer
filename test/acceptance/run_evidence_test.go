package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// envelopeData unmarshals the "data" field of a --json envelope into a generic value,
// for structural comparison across surfaces.
func envelopeData(t *testing.T, stdout string) map[string]any {
	t.Helper()
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid JSON envelope: %v\n%s", err, stdout)
	}
	return env.Data
}

// stateCompareOutcome resolves the outcome of `state compare <range> --json`.
func stateCompareOutcome(t *testing.T, root, rng string) string {
	t.Helper()
	code, stdout, stderr := awa(t, root, "state", "compare", rng, "--json")
	if code != 0 {
		t.Fatalf("state compare %s exit = %d, stderr = %q", rng, code, stderr)
	}
	data := envelopeData(t, stdout)
	outcome, _ := data["outcome"].(string)
	return outcome
}

// TestAcceptanceRunShowStoredOnlyUnderBrokenConfig proves run show is stored-only:
// a malformed current awa.toml does not block metadata inspection (--meta or --json)
// or explicit output reads of durable run evidence, while a config-loading command
// (changes) still fails loudly. Broken current policy must not bury historical
// evidence that does not depend on it.
func TestAcceptanceRunShowStoredOnlyUnderBrokenConfig(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	const marker = "STORED-ONLY-OK"
	if code, _, stderr := awa(t, root, "run", "--", h, "-out", marker); code != 0 {
		t.Fatalf("run exit = %d, stderr = %q", code, stderr)
	}

	// Corrupt the shared config after the run is recorded.
	write(t, root, "awa.toml", "this is not valid toml {{{\n")

	// Metadata inspection stays available and correct despite the broken config.
	if code, _, stderr := awa(t, root, "run", "show", "--last", "--meta"); code != 0 {
		t.Errorf("run show --meta under a broken config exit = %d, want 0; stderr=%q", code, stderr)
	}
	code, showJSON, stderr := awa(t, root, "run", "show", "--last", "--json")
	if code != 0 {
		t.Fatalf("run show --json under a broken config exit = %d, want 0; stderr=%q", code, stderr)
	}
	if got := envelopeData(t, showJSON)["provider_contract"]; got != "awa-evidence/v1" {
		t.Errorf("provider_contract = %v, want awa-evidence/v1", got)
	}

	// An explicit output read of a healthy payload still succeeds and returns the bytes.
	code, out, stderr := awa(t, root, "run", "show", "--last", "--stdout")
	if code != 0 {
		t.Errorf("run show --stdout under a broken config exit = %d, want 0; stderr=%q", code, stderr)
	}
	if out != marker {
		t.Errorf("run show --stdout = %q, want %q", out, marker)
	}

	// A config-loading command still fails loudly on the same broken config (exit 4).
	if code, _, _ := awa(t, root, "changes"); code != 4 {
		t.Errorf("changes under a broken config exit = %d, want 4 (config error)", code)
	}
}

// TestAcceptanceRunEvidenceAndNeighboringLedger is acceptance scenario 11c: run
// evidence binding (awa-evidence/v1 with nested before/after state identities, byte-
// free metadata, parity with state resolve) and the neighboring-tool feedback loop
// (an excluded .rezonator/ ledger write does not move state; a real source edit does;
// a command that reads the excluded state uses --record; GC-removed evidence resolves
// honestly unavailable; .awa/ stays private).
func TestAcceptanceRunEvidenceAndNeighboringLedger(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	// A neighboring tool keeps churny private state beside .awa/. Exclude it explicitly
	// via a committable .awaignore; a real source file is part of evidence identity.
	// Crucially, .rezonator/ does NOT exist yet at checkpoint time: the primary oracle is
	// that a directory-only .awaignore pattern excludes the directory even when it first
	// appears after the baseline.
	write(t, root, ".awaignore", ".rezonator/\n")
	write(t, root, "src/app.go", "package app\n\nfunc A() {}\n")

	// Checkpoint the clean worktree (baseline for latest..now).
	if code, _, stderr := awa(t, root, "checkpoint", "-m", "clean"); code != 0 {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}

	// Execute a clean recorded validation with a distinctive stdout marker.
	const marker = "VALIDATION-OK-MARKER"
	if code, _, stderr := awa(t, root, "run", "--record", "--", h, "-out", marker); code != 0 {
		t.Fatalf("run --record exit = %d, stderr = %q", code, stderr)
	}

	// --- Run evidence: awa-evidence/v1 with nested before/after state assessments. ---
	code, showJSON, stderr := awa(t, root, "run", "show", "--last", "--json")
	if code != 0 {
		t.Fatalf("run show --json exit = %d, stderr = %q", code, stderr)
	}
	show := envelopeData(t, showJSON)
	if got := show["provider_contract"]; got != "awa-evidence/v1" {
		t.Errorf("provider_contract = %v, want awa-evidence/v1", got)
	}
	runObj, _ := show["run"].(map[string]any)
	id, _ := runObj["id"].(string)
	if id == "" {
		t.Fatal("run.id missing from evidence document")
	}
	// A --record run is durable history but never reusable evidence: it is precisely
	// record-only.
	reuseObj, _ := show["reuse"].(map[string]any)
	if state, _ := reuseObj["state"].(string); state != "record-only" {
		t.Errorf("a --record run must be record-only, got reuse.state = %q", state)
	}
	// The recorded exit carries its protocol provenance: a stored exit is always the
	// wrapped child's, so a consumer can classify wrapper-vs-child without guessing.
	execObj, _ := show["execution"].(map[string]any)
	exitObj, _ := execObj["exit"].(map[string]any)
	if got := exitObj["origin"]; got != "child" {
		t.Errorf("evidence execution.exit.origin = %v, want child", got)
	}
	// Default metadata JSON is byte-free: output inspectability is present/unverified,
	// there is no captured-output "samples" block, and the output section carries only
	// byte/hash/truncation facts plus inspectability — never the captured text itself.
	// (The marker appears only in run.command argv, which is legitimate evidence.)
	output, _ := show["output"].(map[string]any)
	stdoutObj, _ := output["stdout"].(map[string]any)
	insp, _ := stdoutObj["inspectability"].(map[string]any)
	if insp["presence"] != "present" || insp["integrity"] != "unverified" {
		t.Errorf("stdout inspectability = %v, want present/unverified", insp)
	}
	if _, hasSamples := show["samples"]; hasSamples {
		t.Errorf("default metadata JSON must not carry a captured-output samples block:\n%s", showJSON)
	}
	for key := range stdoutObj {
		switch key {
		case "original_bytes", "stored_bytes", "truncated", "omitted_bytes", "truncation_policy", "hash", "inspectability":
		default:
			t.Errorf("output.stdout carries an unexpected key %q (possible captured text leak)", key)
		}
	}

	// --- Integrity: an explicit --tail read verifies the selected stream only. ---
	// --stdout --tail selects stdout: it is opened and hash-verified, so its integrity
	// becomes "verified"; the unselected stderr stays "unverified".
	code, verifiedJSON, stderr := awa(t, root, "run", "show", "--last", "--json", "--stdout", "--tail", "50")
	if code != 0 {
		t.Fatalf("run show --json --stdout --tail exit = %d, stderr = %q", code, stderr)
	}
	vOut, _ := envelopeData(t, verifiedJSON)["output"].(map[string]any)
	vStdout, _ := vOut["stdout"].(map[string]any)
	vStdoutInsp, _ := vStdout["inspectability"].(map[string]any)
	if vStdoutInsp["integrity"] != "verified" {
		t.Errorf("explicitly-read stdout integrity = %v, want verified", vStdoutInsp["integrity"])
	}
	vStderr, _ := vOut["stderr"].(map[string]any)
	vStderrInsp, _ := vStderr["inspectability"].(map[string]any)
	if vStderrInsp["integrity"] != "unverified" {
		t.Errorf("unselected stderr integrity = %v, want unverified", vStderrInsp["integrity"])
	}

	// --- Parity: nested states.before/after == standalone state resolve. ---
	for _, sel := range []string{"before", "after"} {
		code, resolveJSON, stderr := awa(t, root, "state", "resolve", "run:"+id+":"+sel, "--json")
		if code != 0 {
			t.Fatalf("state resolve run:%s:%s exit = %d, stderr = %q", id, sel, code, stderr)
		}
		standalone := envelopeData(t, resolveJSON)
		nested, _ := show["states"].(map[string]any)
		nestedSel, _ := nested[sel].(map[string]any)
		if !reflect.DeepEqual(standalone, nestedSel) {
			t.Errorf("states.%s in run evidence differs from state resolve run:%s:%s:\nnested=%#v\nstandalone=%#v", sel, id, sel, nestedSel, standalone)
		}
		if standalone["provider_contract"] != "awa-state/v1" {
			t.Errorf("nested %s provider_contract = %v, want awa-state/v1", sel, standalone["provider_contract"])
		}
	}

	// --- Primary oracle: a directory-only .awaignore excludes .rezonator/ even though
	// it did not exist at checkpoint time. Creating the directory and its ledger after
	// the baseline must not move state, and changes must stay empty. ---
	if got := stateCompareOutcome(t, root, "latest..now"); got != "equal" {
		t.Fatalf("baseline latest..now = %q, want equal", got)
	}
	write(t, root, ".rezonator/ledger.sqlite", "BINARY-LEDGER-DATA-v1")
	if got := stateCompareOutcome(t, root, "latest..now"); got != "equal" {
		t.Errorf("after first-time .rezonator/ creation, latest..now = %q, want equal (no feedback loop)", got)
	}
	code, changesOut, changesErr := awa(t, root, "changes")
	if code != 0 {
		t.Fatalf("changes exit = %d, stderr = %q", code, changesErr)
	}
	if !strings.Contains(changesOut, "no changes") {
		t.Errorf("changes after excluded .rezonator/ creation should be empty (no changes):\n%s", changesOut)
	}

	// Additional oracle: churn of the existing ledger also does not move state.
	write(t, root, ".rezonator/ledger.sqlite", "BINARY-LEDGER-DATA-v2-heavily-churned-and-longer")
	if got := stateCompareOutcome(t, root, "latest..now"); got != "equal" {
		t.Errorf("after excluded .rezonator ledger churn, latest..now = %q, want equal", got)
	}

	// A real source edit still moves state.
	write(t, root, "src/app.go", "package app\n\nfunc A() { _ = 1 }\n")
	if got := stateCompareOutcome(t, root, "latest..now"); got != "different" {
		t.Errorf("after a real source edit, latest..now = %q, want different", got)
	}

	// --- A command that reads the excluded state is run under --record, not cache. ---
	// Its behavior depends on .rezonator/, which is outside the keyed inputs, so a
	// reusable hit would be unsound; --record records history without publishing a hit.
	code, _, stderr = awa(t, root, "run", "--record", "--", h, "-cat", ".rezonator/ledger.sqlite")
	if code != 0 {
		t.Fatalf("run --record reading excluded state exit = %d, stderr = %q", code, stderr)
	}
	_, ledgerShow, _ := awa(t, root, "run", "show", "--last", "--json")
	ledgerReuse, _ := envelopeData(t, ledgerShow)["reuse"].(map[string]any)
	if state, _ := ledgerReuse["state"].(string); state != "record-only" {
		t.Errorf("a --record run reading excluded state must be record-only, got %q", state)
	}

	// --- GC-removed evidence resolves honestly unavailable, non-fatally. ---
	if code, _, stderr := awa(t, root, "run", "rm", id); code != 0 {
		t.Fatalf("run rm exit = %d, stderr = %q", code, stderr)
	}
	code, goneJSON, stderr := awa(t, root, "state", "resolve", "run:"+id+":before", "--json")
	if code != 0 {
		t.Fatalf("state resolve of a removed run must exit 0 (a complete unavailable assessment), got %d, stderr = %q", code, stderr)
	}
	if got := envelopeData(t, goneJSON)["outcome"]; got != "unavailable" {
		t.Errorf("resolve of a removed run outcome = %v, want unavailable", got)
	}

	// --- Privacy: awa keeps .awa/ private via its own gitignore guard. ---
	if _, err := os.Stat(filepath.Join(root, ".awa", ".gitignore")); err != nil {
		t.Errorf(".awa/.gitignore guard missing: %v", err)
	}
}

// newestRunMeta returns the meta.json path of the newest stored run. Run ids are
// time-prefixed, so the lexically greatest id is the newest record.
func newestRunMeta(t *testing.T, root string) string {
	t.Helper()
	entries := filepath.Join(root, ".awa", "runs", "entries")
	var newest string
	err := filepath.WalkDir(entries, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "meta.json" && path > newest {
			newest = path
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking run entries: %v", err)
	}
	if newest == "" {
		t.Fatal("no stored run metadata found")
	}
	return newest
}

// TestAcceptanceRunShowLastDisclosesSkippedIncompatibleRuns is the black-box guard on
// the skipped_latest contract.
//
// "run show --last" answers with the newest READABLE run. When a newer record is in a
// schema this binary cannot read, that answer is not the newest stored run, and both
// surfaces have to say so — otherwise a partial answer reads as a complete one and
// the very state this disclosure exists to surface stays invisible. The JSON field is the
// machine half of that disclosure: without this test, deleting its projection would
// leave every other test green, because they exercise the app aggregate and the human
// formatter but never the emitted document.
func TestAcceptanceRunShowLastDisclosesSkippedIncompatibleRuns(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	const marker = "OLDER-READABLE"
	if code, _, stderr := awa(t, root, "run", "--", h, "-out", marker); code != 0 {
		t.Fatalf("first run exit = %d, stderr = %q", code, stderr)
	}
	if code, _, stderr := awa(t, root, "run", "--", h, "-out", "NEWER"); code != 0 {
		t.Fatalf("second run exit = %d, stderr = %q", code, stderr)
	}

	// Restamp the newest record with a schema this build does not speak. The number is
	// read back out of the document so the fixture carries no version literal.
	meta := newestRunMeta(t, root)
	raw, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	current, err := strconv.Atoi(string(doc["schema_version"]))
	if err != nil {
		t.Fatalf("run metadata has no numeric schema_version: %v", err)
	}
	doc["schema_version"] = json.RawMessage(strconv.Itoa(current + 1))
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, out, 0o644); err != nil {
		t.Fatal(err)
	}

	// The JSON document carries the skip structurally.
	code, showJSON, stderr := awa(t, root, "run", "show", "--last", "--json")
	if code != 0 {
		t.Fatalf("run show --last --json exit = %d, stderr = %q", code, stderr)
	}
	data := envelopeData(t, showJSON)
	skipped, ok := data["skipped_latest"].(map[string]any)
	if !ok {
		t.Fatalf("skipped_latest absent from run show --last --json; a newer unreadable record was not disclosed\n%s", showJSON)
	}
	if _, isCorrupt := skipped["corrupt"]; isCorrupt {
		t.Errorf("an unreadable schema was reported as corruption: %v", skipped)
	}
	inc, ok := skipped["incompatible"].(map[string]any)
	if !ok {
		t.Fatalf("skipped_latest.incompatible absent: %v", skipped)
	}
	if got, want := inc["count"], float64(1); got != want {
		t.Errorf("skipped_latest.incompatible.count = %v, want %v", got, want)
	}
	sample, ok := inc["sample"].([]any)
	if !ok || len(sample) != 1 {
		t.Fatalf("skipped_latest.incompatible.sample = %v, want exactly the one skipped id", inc["sample"])
	}
	skippedID, _ := sample[0].(string)
	if skippedID == "" {
		t.Fatalf("skipped_latest sample carries no id: %v", sample)
	}

	// The run actually returned is the older readable one, not the skipped record.
	run, _ := data["run"].(map[string]any)
	shownID, _ := run["id"].(string)
	if shownID == skippedID {
		t.Errorf("run show --last returned the record it reported as skipped: %s", shownID)
	}

	// Human parity: the same fact, as a warning, naming the same record.
	code, humanOut, humanErr := awa(t, root, "run", "show", "--last")
	if code != 0 {
		t.Fatalf("run show --last exit = %d, stderr = %q", code, humanErr)
	}
	if !strings.Contains(humanErr, "incompatible schema") {
		t.Errorf("human run show --last does not warn about the skipped record; stderr = %q", humanErr)
	}
	if !strings.Contains(humanErr, skippedID[:12]) {
		t.Errorf("human warning %q does not name the skipped run %s", humanErr, skippedID[:12])
	}
	if !strings.Contains(humanOut, marker) && !strings.Contains(humanOut, shownID[:12]) {
		t.Errorf("human run show --last did not display the older readable run:\n%s", humanOut)
	}
}
