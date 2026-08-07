package acceptance

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// The awa-state/v1 provider acceptance scenarios:
// atomic resolution with full ids, machine-readable degradation as exit-0 assessments,
// and bounded summary-only comparison.

type stateEnvelope struct {
	SchemaVersion int             `json:"schema_version"`
	Command       string          `json:"command"`
	Data          json.RawMessage `json:"data"`
}

type stateProjection struct {
	Kind         string `json:"kind"`
	CanonicalRef string `json:"canonical_ref"`
	CheckpointID string `json:"checkpoint_id"`
	RunID        string `json:"run_id"`
	TreeHash     string `json:"tree_hash"`
	ScanIdentity struct {
		ScanConfigHash string `json:"scan_config_hash"`
	} `json:"scan_identity"`
	Comparable       bool `json:"comparable"`
	Reconstructable  bool `json:"reconstructable"`
	EvidenceBoundary struct {
		SkippedInputs int `json:"skipped_inputs"`
	} `json:"evidence_boundary"`
}

type resolveData struct {
	ProviderContract string           `json:"provider_contract"`
	InputRef         string           `json:"input_ref"`
	Outcome          string           `json:"outcome"`
	State            *stateProjection `json:"state"`
	Reason           string           `json:"reason"`
}

type compareData struct {
	ProviderContract string           `json:"provider_contract"`
	Outcome          string           `json:"outcome"`
	Left             *stateProjection `json:"left"`
	Right            *stateProjection `json:"right"`
	Counts           *struct {
		Added       int `json:"added"`
		Modified    int `json:"modified"`
		Deleted     int `json:"deleted"`
		TypeChanged int `json:"type_changed"`
		Skipped     int `json:"skipped"`
	} `json:"counts"`
	Completeness *struct {
		Complete bool `json:"complete"`
	} `json:"completeness"`
	Reason  string `json:"reason"`
	rawData map[string]json.RawMessage
}

// assertComparisonCompleteness pins the awa-state/v1 comparison document as a
// discriminated union. A successful comparison (equal/different) carries the only
// meaningful completeness state, {complete:true}; incomparable/unavailable make
// completeness inapplicable, so the member must be absent rather than false or null.
//
// Mutation proof: removing omitempty from compareDoc.Completeness, always projecting a
// completeness object, or omitting it for equal/different makes the corresponding
// branch below fail against the raw JSON member set.
func assertComparisonCompleteness(t *testing.T, d compareData, applicable bool) {
	t.Helper()
	raw, present := d.rawData["completeness"]
	if !applicable {
		if present {
			t.Errorf("%s comparison must omit inapplicable completeness, got %s", d.Outcome, raw)
		}
		return
	}
	if !present || d.Completeness == nil || !d.Completeness.Complete {
		t.Errorf("%s comparison must carry completeness={complete:true}, got present=%v value=%s", d.Outcome, present, raw)
	}
}

func resolveJSON(t *testing.T, root string, ref string) (int, resolveData) {
	t.Helper()
	code, stdout, stderr := awa(t, root, "state", "resolve", ref, "--json")
	if code != 0 {
		return code, resolveData{}
	}
	var env stateEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("resolve %q: envelope decode: %v\nstdout=%q stderr=%q", ref, err, stdout, stderr)
	}
	if env.SchemaVersion != 1 || env.Command != "state.resolve" {
		t.Fatalf("resolve %q: envelope = {%d,%q}, want {1,state.resolve}", ref, env.SchemaVersion, env.Command)
	}
	var d resolveData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		t.Fatalf("resolve %q: data decode: %v", ref, err)
	}
	if d.ProviderContract != "awa-state/v1" {
		t.Errorf("resolve %q: provider_contract = %q, want awa-state/v1", ref, d.ProviderContract)
	}
	return code, d
}

func compareJSON(t *testing.T, root string, rng string) compareData {
	t.Helper()
	code, stdout, stderr := awa(t, root, "state", "compare", rng, "--json")
	if code != 0 {
		t.Fatalf("compare %q exit = %d, stderr = %q", rng, code, stderr)
	}
	var env stateEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("compare %q: envelope decode: %v\n%s", rng, err, stdout)
	}
	if env.Command != "state.compare" {
		t.Fatalf("compare %q: command = %q, want state.compare", rng, env.Command)
	}
	var d compareData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		t.Fatalf("compare %q: data decode: %v", rng, err)
	}
	if err := json.Unmarshal(env.Data, &d.rawData); err != nil {
		t.Fatalf("compare %q: raw data decode: %v", rng, err)
	}
	if d.ProviderContract != "awa-state/v1" {
		t.Errorf("compare %q: provider_contract = %q, want awa-state/v1", rng, d.ProviderContract)
	}
	return d
}

// TestAcceptanceStateResolveAtomic covers scenario 11a.
func TestAcceptanceStateResolveAtomic(t *testing.T) {
	root := initProject(t)
	write(t, root, "a.txt", "hello")
	if code, _, stderr := awa(t, root, "checkpoint", "-m", "provider baseline"); code != 0 {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	ids := checkpointIDs(t, root)
	if len(ids) != 1 {
		t.Fatalf("want 1 checkpoint, got %v", ids)
	}
	fullID := ids[0]
	if len(fullID) != 32 {
		t.Fatalf("checkpoint id %q is not the full 32-char id", fullID)
	}

	// latest resolves to one full checkpoint identity with its tree/policy identity.
	_, latest := resolveJSON(t, root, "latest")
	if latest.Outcome != "resolved" || latest.State == nil {
		t.Fatalf("latest = %+v, want resolved with a state", latest)
	}
	if latest.State.CheckpointID != fullID || latest.State.CanonicalRef != fullID {
		t.Errorf("latest id/ref = %q/%q, want full %q (never shortened)", latest.State.CheckpointID, latest.State.CanonicalRef, fullID)
	}
	if latest.State.TreeHash == "" || latest.State.ScanIdentity.ScanConfigHash == "" {
		t.Errorf("latest must carry a tree hash and a complete scan/policy identity, got %+v", latest.State)
	}

	// now is comparable and explicitly non-reconstructable.
	_, now := resolveJSON(t, root, "now")
	if now.Outcome != "resolved" || now.State == nil {
		t.Fatalf("now = %+v", now)
	}
	if !now.State.Comparable || now.State.Reconstructable {
		t.Errorf("now comparable/reconstructable = %v/%v, want true/false", now.State.Comparable, now.State.Reconstructable)
	}

	// A checkpoint id prefix resolves, but the canonical id in JSON is never the prefix.
	prefix := fullID[:8]
	_, byPrefix := resolveJSON(t, root, prefix)
	if byPrefix.Outcome != "resolved" || byPrefix.State == nil {
		t.Fatalf("prefix resolve = %+v", byPrefix)
	}
	if byPrefix.State.CanonicalRef != fullID || byPrefix.State.CheckpointID != fullID {
		t.Errorf("prefix must resolve to the full id, got ref=%q id=%q", byPrefix.State.CanonicalRef, byPrefix.State.CheckpointID)
	}

	// A run-before reference resolves to a run observation.
	h := helper(t)
	code, runOut, _ := awa(t, root, "run", "--json", "--", h, "-out", "ok")
	if code != 0 {
		t.Fatalf("run exit = %d", code)
	}
	runID := runIDFromJSON(t, runOut)
	_, before := resolveJSON(t, root, "run:"+runID+":before")
	if before.Outcome != "resolved" || before.State == nil || before.State.Kind != "run-before" {
		t.Fatalf("run before = %+v, want resolved run-before", before)
	}
	if before.State.RunID != runID {
		t.Errorf("run id = %q, want full %q", before.State.RunID, runID)
	}

	// A missing reference is a complete unavailable assessment (exit 0), not empty
	// success, corruption, or a stderr-only failure.
	code, missing := resolveJSON(t, root, "zzzzzzzz")
	if code != 0 || missing.Outcome != "unavailable" || missing.Reason != "not-found" {
		t.Errorf("missing ref: code=%d outcome=%q reason=%q, want 0/unavailable/not-found", code, missing.Outcome, missing.Reason)
	}

	// Malformed reference syntax remains a usage error, and a token that cannot be a
	// checkpoint id prefix is malformed rather than a lookup that happens to miss.
	if code, _, _ := awa(t, root, "state", "resolve", "latest..now"); code != 2 {
		t.Errorf("range given to resolve: exit = %d, want 2 (usage)", code)
	}
	if code, _, stderr := awa(t, root, "state", "resolve", "no-such-name"); code != 2 {
		t.Errorf("non-prefix token: exit = %d, want 2 (usage); stderr = %q", code, stderr)
	}
	if code, _, _ := awa(t, root, "state", "resolve"); code != 2 {
		t.Errorf("missing ref: exit = %d, want 2 (usage)", code)
	}
}

// TestAcceptanceStateCompareBounded covers scenario 11b.
func TestAcceptanceStateCompareBounded(t *testing.T) {
	root := initProject(t)
	write(t, root, "a.txt", "v1")
	awa(t, root, "checkpoint", "-m", "c1")

	// Equal: latest against itself.
	if d := compareJSON(t, root, "latest..latest"); d.Outcome != "equal" {
		t.Errorf("latest..latest = %q, want equal", d.Outcome)
	} else {
		assertComparisonCompleteness(t, d, true)
	}

	// Different (source-only change): counts present, but no changed-path list.
	const changedPath = "unique_changed_path_marker_9z.txt"
	write(t, root, changedPath, "new content")
	code, stdout, _ := awa(t, root, "state", "compare", "latest..now", "--json")
	if code != 0 {
		t.Fatalf("compare latest..now exit = %d", code)
	}
	var env stateEnvelope
	_ = json.Unmarshal([]byte(stdout), &env)
	var d compareData
	_ = json.Unmarshal(env.Data, &d)
	_ = json.Unmarshal(env.Data, &d.rawData)
	if d.Outcome != "different" {
		t.Fatalf("latest..now = %q, want different", d.Outcome)
	}
	// skipped is asserted zero here, not merely present. It is the one count another
	// scenario pins non-zero (the skipped-transition test asserts one), so this is where a
	// projection publishing a constant instead of the comparator's value gets caught.
	if d.Counts == nil || d.Counts.Added != 1 || d.Counts.Skipped != 0 {
		t.Errorf("counts = %+v, want added=1 and skipped=0", d.Counts)
	}
	if d.Completeness == nil || !d.Completeness.Complete {
		t.Errorf("different comparison must be complete, got %+v", d.Completeness)
	}
	assertComparisonCompleteness(t, d, true)
	// The changed path name must never appear: compare is summary-only.
	if strings.Contains(stdout, changedPath) {
		t.Errorf("compare output leaked a changed path %q; it must be summary-only:\n%s", changedPath, stdout)
	}
	for _, banned := range []string{"\"paths\"", "\"changed_paths\"", "\"renamed\"", "\"hunks\""} {
		if strings.Contains(stdout, banned) {
			t.Errorf("compare output must not contain %s", banned)
		}
	}

	// Incompatible policy: a checkpoint recorded under a different trust mode has a
	// different scan/policy identity and is incomparable, not falsely equal/different.
	normalRoot := initProject(t)
	write(t, normalRoot, "a.txt", "same")
	awa(t, normalRoot, "checkpoint", "-m", "normal")
	_, normalID := resolveJSON(t, normalRoot, "latest")
	awa(t, normalRoot, "checkpoint", "--strict", "-m", "strict")
	_, strictID := resolveJSON(t, normalRoot, "latest")
	if normalID.State == nil || strictID.State == nil {
		t.Fatal("failed to resolve the two policy checkpoints")
	}
	d2 := compareJSON(t, normalRoot, normalID.State.CheckpointID+".."+strictID.State.CheckpointID)
	if d2.Outcome != "incomparable" || d2.Reason != "incompatible-identity-policy" {
		t.Errorf("mixed-policy compare = %q/%q, want incomparable/incompatible-identity-policy", d2.Outcome, d2.Reason)
	}
	if d2.Counts != nil {
		t.Error("incomparable comparison must carry no counts")
	}
	assertComparisonCompleteness(t, d2, false)
}

// TestAcceptanceStateCompareSkippedTransition proves the built binary publishes the
// skipped transition count of a comparison. It uses only the supported large-file skip
// policy — no private-store mutation, no FIFO, no platform-specific setup — so it runs the
// same way on every host lane.
//
// The scenario turns one oversized (therefore skipped) file into a small readable one. That
// path is present on both sides with no honest status other than skipped, so the whole
// difference is one skipped transition: a build that dropped the count would report this
// comparison as different, complete, and empty.
//
// The second oversized file never changes. It stays skipped on both sides, contributes
// nothing to the delta, and exists to push each operand's evidence_boundary.skipped_inputs
// above the comparison's skipped count — so a projection that substituted the operand's
// observation total for the delta could not satisfy both assertions at once.
func TestAcceptanceStateCompareSkippedTransition(t *testing.T) {
	const (
		transitioning = "unique_skip_marker_7q.txt"
		stableSkipped = "unique_stable_marker_3k.txt"
		oversized     = "larger than four bytes"
		small         = "ok" // <= max_file_size, so it is observed rather than skipped
	)
	root := initProject(t)
	// The shared config is what makes the scenario portable: an ordinary size threshold
	// plus the supported skip policy, both of which every host applies identically.
	write(t, root, "awa.toml", "[hashing]\nmax_file_size = \"4B\"\nlarge_file_policy = \"skip\"\n")
	write(t, root, transitioning, oversized)
	write(t, root, stableSkipped, oversized)
	if code, _, stderr := awa(t, root, "checkpoint", "-m", "oversized baseline"); code != 0 {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	write(t, root, transitioning, small)

	d := compareJSON(t, root, "latest..now")
	if d.Outcome != "different" {
		t.Fatalf("latest..now = %q, want different: a skip-only transition is still a difference", d.Outcome)
	}
	assertComparisonCompleteness(t, d, true)
	if d.Counts == nil {
		t.Fatal("different comparison must carry counts")
	}
	if d.Counts.Skipped != 1 {
		t.Errorf("counts.skipped = %d, want 1", d.Counts.Skipped)
	}
	if d.Counts.Added != 0 || d.Counts.Modified != 0 || d.Counts.Deleted != 0 || d.Counts.TypeChanged != 0 {
		t.Errorf("counts = %+v, want every non-skipped count zero", *d.Counts)
	}
	// The counts object publishes exactly its five required fields, skipped included. A
	// typed decode cannot see a missing or an extra key, so the key set is the assertion.
	assertObjectFields(t, "state.compare counts", d.rawData["counts"],
		[]string{"added", "modified", "deleted", "type_changed", "skipped"}, nil)

	// The delta is not the operands' observation boundaries: the left side skipped both
	// oversized files, the right side still skips the unchanged one, and the comparison
	// counts one transition. The bounds are deliberately lower bounds, not equalities —
	// awa.toml is itself larger than the threshold and is skipped alongside them, so the
	// observed totals are higher, and this test must not depend on whether the shared
	// config file participates in the scan.
	if d.Left == nil || d.Right == nil {
		t.Fatal("a difference must publish both operands")
	}
	if d.Left.EvidenceBoundary.SkippedInputs < 2 {
		t.Errorf("left evidence_boundary.skipped_inputs = %d, want at least 2 (both oversized files)", d.Left.EvidenceBoundary.SkippedInputs)
	}
	if d.Right.EvidenceBoundary.SkippedInputs < 1 {
		t.Errorf("right evidence_boundary.skipped_inputs = %d, want at least 1 (the unchanged oversized file)", d.Right.EvidenceBoundary.SkippedInputs)
	}

	// Human output names the skipped count and stays summary-only.
	code, human, stderr := awa(t, root, "state", "compare", "latest..now")
	if code != 0 {
		t.Fatalf("human compare exit = %d, stderr = %q", code, stderr)
	}
	// The word boundary makes the count exact without pinning the compact layout, which is
	// presentation-owned: a plain substring would also accept "skipped 10", so a human
	// renderer disagreeing with the JSON count could pass.
	if !regexp.MustCompile(`skipped 1\b`).MatchString(human) {
		t.Errorf("human compare must name the skipped count as 1, got:\n%s", human)
	}
	for _, path := range []string{transitioning, stableSkipped} {
		if strings.Contains(human, path) {
			t.Errorf("human compare leaked path %q; it must be summary-only:\n%s", path, human)
		}
	}
}

// TestAcceptanceStateResolveAdversarial proves degraded evidence is a machine-readable
// exit-0 assessment with a stable reason, never a corrupt classification or a
// stderr-only failure.
func TestAcceptanceStateResolveAdversarial(t *testing.T) {
	root := initProject(t)
	write(t, root, "a.txt", "hello")
	awa(t, root, "checkpoint", "-m", "c1")
	ids := checkpointIDs(t, root)
	header := filepath.Join(root, ".awa", "checkpoints", ids[0], "header.json")
	if err := os.Chmod(header, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := os.WriteFile(header, []byte("{corrupt"), 0o644); err != nil {
		t.Fatalf("corrupt header: %v", err)
	}
	code, d := resolveJSON(t, root, "latest")
	if code != 0 || d.Outcome != "unavailable" || d.Reason != "metadata-corrupt" {
		t.Errorf("corrupt store: code=%d outcome=%q reason=%q, want 0/unavailable/metadata-corrupt", code, d.Outcome, d.Reason)
	}
	// The corrupt bytes must not have been echoed as a canonical identity.
	if d.State != nil {
		t.Errorf("unavailable assessment must carry no state identity, got %+v", d.State)
	}
}

// TestAcceptanceStateNotInitialized proves the provider is optional: outside a project
// it emits a complete unavailable/not-initialized assessment and exits 0.
func TestAcceptanceStateNotInitialized(t *testing.T) {
	dir := t.TempDir()
	code, d := resolveJSON(t, dir, "latest")
	if code != 0 || d.Outcome != "unavailable" || d.Reason != "not-initialized" {
		t.Errorf("uninitialized resolve: code=%d outcome=%q reason=%q, want 0/unavailable/not-initialized", code, d.Outcome, d.Reason)
	}
	cd := compareJSON(t, dir, "latest..now")
	if cd.Outcome != "unavailable" || cd.Reason != "not-initialized" {
		t.Errorf("uninitialized compare = %q/%q, want unavailable/not-initialized", cd.Outcome, cd.Reason)
	}
	assertComparisonCompleteness(t, cd, false)
}

// TestAcceptanceStateResolveLatestRace moves latest from a competing process while
// resolving it, asserting each result names one checkpoint consistently: the tree hash
// reported for latest always matches the tree hash the same id resolves to explicitly,
// so no response ever pairs one id with another checkpoint's metadata.
func TestAcceptanceStateResolveLatestRace(t *testing.T) {
	root := initProject(t)
	write(t, root, "a.txt", "seed")
	awa(t, root, "checkpoint", "-m", "seed")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
				i++
				// The competing writer must not call t.Fatal from a non-test goroutine, so
				// it drives the filesystem and binary directly and ignores errors: moving
				// latest is the probe's setup, not its subject.
				_ = os.WriteFile(filepath.Join(root, "a.txt"), []byte("rev"+string(rune('a'+i%26))), 0o644)
				cmd := exec.Command(awaBin, "checkpoint", "-m", "race")
				cmd.Dir = root
				_ = cmd.Run()
			}
		}
	}()

	for iter := 0; iter < 25; iter++ {
		_, latest := resolveJSON(t, root, "latest")
		if latest.Outcome != "resolved" || latest.State == nil {
			continue // a mid-move read may legitimately be a consistent assessment; skip
		}
		id, tree := latest.State.CheckpointID, latest.State.TreeHash
		if len(id) != 32 || tree == "" {
			t.Errorf("race: latest returned an inconsistent id/tree: %q/%q", id, tree)
			continue
		}
		// Independent oracle: resolve the same id explicitly and require the identical
		// tree hash. A response that paired this id with another checkpoint's tree hash
		// would disagree here.
		_, byID := resolveJSON(t, root, id)
		if byID.Outcome == "resolved" && byID.State != nil && byID.State.TreeHash != tree {
			t.Errorf("race: latest paired id %s with tree %s, but the id resolves to tree %s", id, tree, byID.State.TreeHash)
		}
	}
	close(stop)
	wg.Wait()
}

// TestAcceptanceStateLazyConfig pins the lazy-config matrix end to end: a stored-only
// resolve or compare must succeed while the effective config is malformed (a stored
// reference never reads config), and any operation with a "now" side must surface the
// malformed required config as a config error (exit 4). It covers a malformed shared,
// local, and explicit --config layer alike.
func TestAcceptanceStateLazyConfig(t *testing.T) {
	const badTOML = "this is = = not valid toml"

	layers := []struct {
		name  string
		place func(t *testing.T, root string) []string // writes the bad layer, returns extra args
	}{
		{"shared", func(t *testing.T, root string) []string {
			write(t, root, "awa.toml", badTOML)
			return nil
		}},
		{"local", func(t *testing.T, root string) []string {
			write(t, root, ".awa/config.toml", badTOML)
			return nil
		}},
		{"explicit", func(t *testing.T, root string) []string {
			write(t, root, "bad.toml", badTOML)
			return []string{"--config", filepath.Join(root, "bad.toml")}
		}},
	}

	for _, l := range layers {
		t.Run(l.name, func(t *testing.T) {
			root := initProject(t)
			write(t, root, "a.txt", "hello")
			if code, _, stderr := awa(t, root, "checkpoint", "-m", "baseline"); code != 0 {
				t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
			}
			fullID := checkpointIDs(t, root)[0]
			// Break the config layer only after setup, which legitimately reads config.
			extra := l.place(t, root)

			storedResolve := append([]string{"state", "resolve", "latest", "--json"}, extra...)
			if code, _, stderr := awa(t, root, storedResolve...); code != 0 {
				t.Errorf("stored-only resolve with malformed %s config: exit = %d, want 0\nstderr=%q", l.name, code, stderr)
			}
			storedCompare := append([]string{"state", "compare", fullID + "..latest", "--json"}, extra...)
			if code, _, stderr := awa(t, root, storedCompare...); code != 0 {
				t.Errorf("stored-only compare with malformed %s config: exit = %d, want 0\nstderr=%q", l.name, code, stderr)
			}

			nowResolve := append([]string{"state", "resolve", "now", "--json"}, extra...)
			if code, _, _ := awa(t, root, nowResolve...); code != 4 {
				t.Errorf("now resolve with malformed %s config: exit = %d, want 4 (config)", l.name, code)
			}
			nowCompare := append([]string{"state", "compare", "latest..now", "--json"}, extra...)
			if code, _, _ := awa(t, root, nowCompare...); code != 4 {
				t.Errorf("now compare with malformed %s config: exit = %d, want 4 (config)", l.name, code)
			}
		})
	}
}

func runIDFromJSON(t *testing.T, stdout string) string {
	t.Helper()
	var env stateEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("run envelope decode: %v\n%s", err, stdout)
	}
	var d struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil {
		t.Fatalf("run data decode: %v", err)
	}
	if d.Run.ID == "" {
		t.Fatalf("no run id in %s", stdout)
	}
	return d.Run.ID
}

// objectFields decodes one JSON object and returns its key set. It works on the raw
// bytes rather than a typed struct on purpose: a typed decode is permissive, so a
// field that should not exist decodes into nothing and asserts nothing.
func objectFields(t *testing.T, label string, raw []byte) map[string]bool {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: decode object: %v\n%s", label, err, raw)
	}
	got := make(map[string]bool, len(m))
	for k := range m {
		got[k] = true
	}
	return got
}

// assertObjectFields proves a JSON object carries exactly the documented fields:
// every required key is present, and no key outside required∪optional appears.
//
// The "no extra key" half is the load-bearing one. Every other JSON assertion in
// this package reads through a typed struct, and encoding/json ignores fields the
// struct does not name — so an undocumented field would decode into nothing and
// leave the whole suite green. Only a check against the exact key set can see it.
// Optional keys exist because the wire shape omits ids and timestamps that do not
// apply to a state's kind.
func assertObjectFields(t *testing.T, label string, raw []byte, required, optional []string) {
	t.Helper()
	got := objectFields(t, label, raw)
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, k := range append(append([]string(nil), required...), optional...) {
		allowed[k] = true
	}
	for k := range got {
		if !allowed[k] {
			t.Errorf("%s: unexpected field %q in the published shape", label, k)
		}
	}
	for _, k := range required {
		if !got[k] {
			t.Errorf("%s: required field %q is missing", label, k)
		}
	}
}

// TestAcceptanceStateJSONPublishesExactlyItsDocumentedFields pins the awa-state/v1
// wire shape by key set, for both a stored checkpoint and a live "now" observation.
// The published document is exactly the documented one: a typed decode cannot detect
// an undocumented extra key, so the key set is the only thing that can hold the
// public shape to what the contract says it is.
func TestAcceptanceStateJSONPublishesExactlyItsDocumentedFields(t *testing.T) {
	root := initProject(t)
	write(t, root, "a.txt", "hello")
	if code, _, stderr := awa(t, root, "checkpoint", "-m", "wire shape baseline"); code != 0 {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}

	for _, ref := range []string{"latest", "now"} {
		code, stdout, stderr := awa(t, root, "state", "resolve", ref, "--json")
		if code != 0 {
			t.Fatalf("state resolve %s exit = %d, stderr = %q", ref, code, stderr)
		}
		data := assertEnvelope(t, stdout, "state.resolve")
		assertObjectFields(t, "state.resolve "+ref,
			data,
			[]string{"provider_contract", "input_ref", "outcome", "state"},
			[]string{"reason"})

		var d struct {
			State json.RawMessage `json:"state"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			t.Fatalf("state resolve %s: data decode: %v", ref, err)
		}
		assertObjectFields(t, "state.resolve "+ref+" state",
			d.State,
			[]string{"kind", "canonical_ref", "tree_hash", "scan_identity", "comparable", "reconstructable", "evidence_boundary"},
			[]string{"checkpoint_id", "run_id", "observation_time"})

		var s struct {
			ScanIdentity     json.RawMessage `json:"scan_identity"`
			EvidenceBoundary json.RawMessage `json:"evidence_boundary"`
		}
		if err := json.Unmarshal(d.State, &s); err != nil {
			t.Fatalf("state resolve %s: state decode: %v", ref, err)
		}
		// The scan-config hash is the whole observation-policy identity, so anything
		// published beside it would be a second, unaccounted authority.
		assertObjectFields(t, "state.resolve "+ref+" scan_identity",
			s.ScanIdentity, []string{"scan_config_hash"}, nil)
		assertObjectFields(t, "state.resolve "+ref+" evidence_boundary",
			s.EvidenceBoundary, []string{"skipped_inputs", "ignored_paths_outside_evidence"}, nil)
	}
}

// TestAcceptanceConfigEffectiveJSONPublishesExactlyItsDocumentedFields pins the
// [hashing] section of config effective by key set: the section publishes exactly the
// observation-policy settings and nothing else. As above, a typed decode cannot detect
// an undocumented extra key, so only the exact key set holds the published section to
// what is documented.
func TestAcceptanceConfigEffectiveJSONPublishesExactlyItsDocumentedFields(t *testing.T) {
	root := initProject(t)
	code, stdout, stderr := awa(t, root, "config", "effective", "--json")
	if code != 0 {
		t.Fatalf("config effective --json exit = %d, stderr = %q", code, stderr)
	}
	data := assertEnvelope(t, stdout, "config.effective")

	var d struct {
		Hashing json.RawMessage `json:"hashing"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatalf("config effective: data decode: %v", err)
	}
	if len(d.Hashing) == 0 {
		t.Fatalf("config effective published no [hashing] section:\n%s", stdout)
	}
	assertObjectFields(t, "config.effective hashing", d.Hashing,
		[]string{"trust_mode", "max_file_size", "large_file_policy"}, nil)
}
