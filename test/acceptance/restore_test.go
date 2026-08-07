package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Acceptance scenario 24: evidence-backed worktree restore.
//
// The scenario is the situation the command was built for and the one that
// produced its requirements: a reviewed checkpoint, then a generator that rewrote
// a generated subtree while the developer also holds unrelated dirty edits and a
// local ignored file. Everything here drives the built binary, because the
// properties that matter — that a preview writes nothing, that an apply touches
// only the selection, that the undo reference it prints actually works — are
// properties of the shipped command, not of an internal API.

// restoreEnv is one prepared project plus the ids the scenario refers back to.
type restoreEnv struct {
	root       string
	checkpoint string
}

func writeAt(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func readAt(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func existsAt(root, rel string) bool {
	_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil
}

// checkpointID extracts the recorded checkpoint id from `awa checkpoint --json`.
func checkpointID(t *testing.T, root string) string {
	t.Helper()
	code, stdout, stderr := awa(t, root, "checkpoint", "--json", "-m", "reviewed baseline")
	if code != 0 {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	var env struct {
		Data struct {
			Checkpoint struct {
				ID string `json:"id"`
			} `json:"checkpoint"`
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decode checkpoint json: %v\n%s", err, stdout)
	}
	if env.Data.Checkpoint.ID != "" {
		return env.Data.Checkpoint.ID
	}
	if env.Data.ID == "" {
		t.Fatalf("checkpoint json carries no id:\n%s", stdout)
	}
	return env.Data.ID
}

// newRestoreEnv builds the generator-rewrite fixture: a reviewed checkpoint over a
// generated subtree and a source file, an executable, and a symlink; then a
// generator rewrite, an unrelated dirty edit, and an ignored local file.
func newRestoreEnv(t *testing.T) restoreEnv {
	t.Helper()
	root := t.TempDir()
	if code, _, stderr := awa(t, root, "init"); code != 0 {
		t.Fatalf("init exit = %d, stderr = %q", code, stderr)
	}
	// Ignore a local scratch file: it must survive every restore untouched, because
	// ignored paths are outside awa's evidence entirely.
	writeAt(t, root, ".awaignore", "local-notes.txt\n")

	writeAt(t, root, "generated/client/openapi.json", "{\"paths\":{\"/v1\":{}}}\n")
	writeAt(t, root, "generated/client/model.go", "package client\n\ntype Model struct{}\n")
	writeAt(t, root, "generated/client/tool.sh", "#!/bin/sh\necho v1\n")
	if err := os.Chmod(filepath.Join(root, "generated", "client", "tool.sh"), 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := os.Symlink("model.go", filepath.Join(root, "generated", "client", "current.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	writeAt(t, root, "src/service.go", "package src\n\nfunc A() int { return 1 }\n")

	id := checkpointID(t, root)

	// The generator rewrites the subtree: content changes, a new file appears, the
	// executable loses its bit, and the symlink is repointed.
	writeAt(t, root, "generated/client/openapi.json", "{\"paths\":{\"/v2\":{}}}\n")
	writeAt(t, root, "generated/client/model.go", "package client\n\ntype Model struct{ Broken bool }\n")
	writeAt(t, root, "generated/client/extra.go", "package client\n\n// accidental\n")
	writeAt(t, root, "generated/client/tool.sh", "#!/bin/sh\necho v2\n")
	if err := os.Chmod(filepath.Join(root, "generated", "client", "tool.sh"), 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "generated", "client", "current.go")); err != nil {
		t.Fatalf("remove symlink: %v", err)
	}
	if err := os.Symlink("extra.go", filepath.Join(root, "generated", "client", "current.go")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// Unrelated dirty work in hand, and a local file awa never observes.
	writeAt(t, root, "src/service.go", "package src\n\nfunc A() int { return 2 }\n")
	writeAt(t, root, "local-notes.txt", "local scratch\n")

	return restoreEnv{root: root, checkpoint: id}
}

// snapshot renders the worktree (excluding .awa) as path/mode/content lines. It is
// the oracle for "this command changed nothing": independent of any awa output.
func snapshot(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		if rel == ".awa" {
			return filepath.SkipDir
		}
		b.WriteString(filepath.ToSlash(rel) + " " + info.Mode().String())
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, lerr := os.Readlink(p)
			if lerr != nil {
				return lerr
			}
			b.WriteString(" -> " + target)
		case info.Mode().IsRegular():
			data, rerr := os.ReadFile(p) //nolint:gosec // test fixture path
			if rerr != nil {
				return rerr
			}
			b.WriteString(" " + string(data))
		}
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return b.String()
}

// restoreDoc is the awa-restore/v1 payload, decoded from the standard envelope.
type restoreDoc struct {
	SchemaVersion int    `json:"schema_version"`
	Command       string `json:"command"`
	Data          struct {
		RestoreContract string `json:"restore_contract"`
		Mode            string `json:"mode"`
		Outcome         string `json:"outcome"`
		Source          struct {
			Kind         string `json:"kind"`
			ID           string `json:"id"`
			CanonicalRef string `json:"canonical_ref"`
			Requested    string `json:"requested"`
			Message      string `json:"message"`
			ObservedAt   string `json:"observed_at"`
		} `json:"source"`
		Selection struct {
			All   bool     `json:"all"`
			Paths []string `json:"paths"`
		} `json:"selection"`
		Counts struct {
			Create          int `json:"create"`
			Replace         int `json:"replace"`
			TypeChange      int `json:"type_change"`
			Symlink         int `json:"symlink"`
			DeleteFile      int `json:"delete_file"`
			DeleteDirectory int `json:"delete_directory"`
			Equal           int `json:"equal"`
			Blocked         int `json:"blocked"`
		} `json:"counts"`
		Boundary struct {
			SourceSkippedInputs         int  `json:"source_skipped_inputs"`
			CurrentSkippedInputs        int  `json:"current_skipped_inputs"`
			PolicyCompatible            bool `json:"policy_compatible"`
			ScopeBounded                bool `json:"scope_bounded"`
			IgnoredPathsOutsideEvidence bool `json:"ignored_paths_outside_evidence"`
		} `json:"evidence_boundary"`
		Complete  bool     `json:"complete"`
		Completed int      `json:"completed"`
		Remaining int      `json:"remaining"`
		Reasons   []string `json:"reasons"`
		Failures  []struct {
			Path    string   `json:"path"`
			Kind    string   `json:"kind"`
			Reasons []string `json:"reasons"`
		} `json:"failures"`
		Recovery *struct {
			OperationID        string `json:"operation_id"`
			Ref                string `json:"ref"`
			RetentionPolicyKey string `json:"retention_policy_key"`
		} `json:"recovery"`
	} `json:"data"`
}

func decodeRestore(t *testing.T, stdout string) restoreDoc {
	t.Helper()
	var doc restoreDoc
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("decode restore json: %v\n%s", err, stdout)
	}
	return doc
}

// --- the scenario ---------------------------------------------------------

func TestScenario24PreviewIsSideEffectFreeAndNamesTheApply(t *testing.T) {
	env := newRestoreEnv(t)
	before := snapshot(t, env.root)

	// The default form and the explicit --dry-run form must be the same mode.
	code, plain, stderr := awa(t, env.root, "restore", env.checkpoint, "--", "generated/client")
	if code != 0 {
		t.Fatalf("preview exit = %d, stderr = %q", code, stderr)
	}
	dryCode, dry, _ := awa(t, env.root, "restore", "--dry-run", env.checkpoint, "--", "generated/client")
	if dryCode != 0 {
		t.Fatalf("--dry-run exit = %d", dryCode)
	}
	if plain != dry {
		t.Errorf("default preview and --dry-run differ:\ndefault:\n%s\n--dry-run:\n%s", plain, dry)
	}

	// It names the resolved immutable id, the selection, the counts, and the exact
	// apply command — and that command uses the id, not the reference typed.
	for _, want := range []string{
		"source: " + env.checkpoint,
		"selection: generated/client",
		"apply: awa restore --apply " + env.checkpoint + " -- generated/client",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("preview does not contain %q:\n%s", want, plain)
		}
	}
	// The unconditional evidence-boundary caveat is a stderr note.
	if !strings.Contains(stderr, "ignored paths are outside awa evidence") {
		t.Errorf("preview omits the ignored-paths caveat: %q", stderr)
	}

	if after := snapshot(t, env.root); after != before {
		t.Errorf("preview mutated the worktree:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestScenario24ApplyRestoresOnlyTheSelectedSubtree(t *testing.T) {
	env := newRestoreEnv(t)
	dirtyBefore := readAt(t, env.root, "src/service.go")
	ignoredBefore := readAt(t, env.root, "local-notes.txt")

	code, stdout, stderr := awa(t, env.root, "restore", "--apply", env.checkpoint, "--", "generated/client")
	if code != 0 {
		t.Fatalf("apply exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "outcome: applied") {
		t.Fatalf("apply outcome missing:\n%s", stdout)
	}

	// Content, the executable bit, the symlink target, and the accidental file.
	if got := readAt(t, env.root, "generated/client/openapi.json"); got != "{\"paths\":{\"/v1\":{}}}\n" {
		t.Errorf("openapi.json = %q, want the checkpointed bytes", got)
	}
	info, err := os.Lstat(filepath.Join(env.root, "generated", "client", "tool.sh"))
	if err != nil {
		t.Fatalf("lstat tool.sh: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("tool.sh mode = %04o, want 0755 (the executable bit is part of the restored state)", info.Mode().Perm())
	}
	target, err := os.Readlink(filepath.Join(env.root, "generated", "client", "current.go"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != "model.go" {
		t.Errorf("symlink target = %q, want model.go", target)
	}
	if existsAt(env.root, "generated/client/extra.go") {
		t.Error("the accidentally generated file survived the restore")
	}

	// Byte-identical: the unrelated dirty edit and the ignored local file.
	if got := readAt(t, env.root, "src/service.go"); got != dirtyBefore {
		t.Errorf("an unrelated dirty edit changed: %q, want %q", got, dirtyBefore)
	}
	if got := readAt(t, env.root, "local-notes.txt"); got != ignoredBefore {
		t.Errorf("an ignored local file changed: %q, want %q", got, ignoredBefore)
	}

	// The delta the preview promised is now empty for the selection.
	ccode, changes, _ := awa(t, env.root, "changes", env.checkpoint+"..now", "--", "generated/client")
	if ccode != 0 {
		t.Fatalf("changes exit = %d", ccode)
	}
	if !strings.Contains(changes, "no changes") {
		t.Errorf("the selection still differs from the source after apply:\n%s", changes)
	}
}

func TestScenario24RecoveryObservationUndoesTheRestore(t *testing.T) {
	env := newRestoreEnv(t)
	overwritten := readAt(t, env.root, "generated/client/openapi.json")
	deleted := readAt(t, env.root, "generated/client/extra.go")

	code, stdout, stderr := awa(t, env.root, "restore", "--json", "--apply", env.checkpoint, "--", "generated/client")
	if code != 0 {
		t.Fatalf("apply exit = %d, stderr = %q", code, stderr)
	}
	doc := decodeRestore(t, stdout)
	if doc.Data.Recovery == nil {
		t.Fatal("an applied restore published no recovery observation")
	}
	ref := doc.Data.Recovery.Ref
	if !strings.HasPrefix(ref, "restore:") || !strings.HasSuffix(ref, ":before") {
		t.Fatalf("recovery ref = %q", ref)
	}
	if doc.Data.Recovery.RetentionPolicyKey != "gc.keep_restores_for" {
		t.Errorf("retention policy key = %q", doc.Data.Recovery.RetentionPolicyKey)
	}

	// The reference restores the pre-restore state: the overwritten bytes come back
	// AND the file the restore deleted is recreated.
	ucode, ustdout, ustderr := awa(t, env.root, "restore", "--apply", ref, "--", "generated/client")
	if ucode != 0 {
		t.Fatalf("undo exit = %d, stdout = %q, stderr = %q", ucode, ustdout, ustderr)
	}
	if got := readAt(t, env.root, "generated/client/openapi.json"); got != overwritten {
		t.Errorf("undo did not restore the overwritten bytes: %q, want %q", got, overwritten)
	}
	if !existsAt(env.root, "generated/client/extra.go") {
		t.Fatal("undo did not recreate the file the restore deleted")
	}
	if got := readAt(t, env.root, "generated/client/extra.go"); got != deleted {
		t.Errorf("undo restored %q, want %q", got, deleted)
	}
}

func TestScenario24RestoreNeverCreatesACheckpointOrMovesLatest(t *testing.T) {
	env := newRestoreEnv(t)
	_, logBefore, _ := awa(t, env.root, "log", "--oneline")

	if code, _, stderr := awa(t, env.root, "restore", "--apply", env.checkpoint, "--", "generated/client"); code != 0 {
		t.Fatalf("apply exit = %d, stderr = %q", code, stderr)
	}

	_, logAfter, _ := awa(t, env.root, "log", "--oneline")
	if logAfter != logBefore {
		t.Errorf("restore changed the explicit checkpoint log:\nbefore:\n%s\nafter:\n%s", logBefore, logAfter)
	}
	// It IS visible in the full timeline, as its own record kind.
	code, all, _ := awa(t, env.root, "log", "--all")
	if code != 0 {
		t.Fatalf("log --all exit = %d", code)
	}
	if !strings.Contains(all, "restore:before") {
		t.Errorf("log --all does not show the recovery observation:\n%s", all)
	}
}

func TestScenario24JSONMatchesTheHumanReport(t *testing.T) {
	env := newRestoreEnv(t)

	code, stdout, _ := awa(t, env.root, "restore", "--json", env.checkpoint, "--", "generated/client")
	if code != 0 {
		t.Fatalf("preview --json exit = %d", code)
	}
	doc := decodeRestore(t, stdout)
	if doc.SchemaVersion != 1 || doc.Command != "restore" {
		t.Errorf("envelope = {schema_version:%d command:%q}", doc.SchemaVersion, doc.Command)
	}
	if doc.Data.RestoreContract != "awa-restore/v1" {
		t.Errorf("restore_contract = %q, want awa-restore/v1", doc.Data.RestoreContract)
	}
	if doc.Data.Mode != "preview" || doc.Data.Outcome != "preview" {
		t.Errorf("mode/outcome = %q/%q, want preview/preview", doc.Data.Mode, doc.Data.Outcome)
	}
	if doc.Data.Source.ID != env.checkpoint {
		t.Errorf("source id = %q, want %q", doc.Data.Source.ID, env.checkpoint)
	}
	if doc.Data.Source.Message != "reviewed baseline" {
		t.Errorf("source message = %q", doc.Data.Source.Message)
	}
	if doc.Data.Selection.All || len(doc.Data.Selection.Paths) != 1 || doc.Data.Selection.Paths[0] != "generated/client" {
		t.Errorf("selection = %+v", doc.Data.Selection)
	}
	if !doc.Data.Boundary.IgnoredPathsOutsideEvidence || !doc.Data.Boundary.PolicyCompatible {
		t.Errorf("evidence boundary = %+v", doc.Data.Boundary)
	}
	if !doc.Data.Complete {
		t.Errorf("a plan with no blocked operations reports incomplete: %+v", doc.Data.Counts)
	}
	if doc.Data.Recovery != nil {
		t.Error("a preview published a recovery observation")
	}
	// A document that leaked bytes or internal paths would be a contract violation,
	// so assert on the raw text rather than on the decoded struct.
	for _, forbidden := range []string{"paths\\\":{\\\"/v1", ".awa/store", "blake3:"} {
		if strings.Contains(stdout, forbidden) {
			t.Errorf("restore JSON leaked %q:\n%s", forbidden, stdout)
		}
	}
	// The counts the human report shows are the same counts.
	_, human, _ := awa(t, env.root, "restore", env.checkpoint, "--", "generated/client")
	if doc.Data.Counts.Replace > 0 && !strings.Contains(human, "replace") {
		t.Errorf("human report omits the replace count the JSON carries:\n%s", human)
	}
}

func TestScenario24AllSelectsTheWholeProvenScope(t *testing.T) {
	env := newRestoreEnv(t)

	code, stdout, stderr := awa(t, env.root, "restore", "--apply", "--all", env.checkpoint)
	if code != 0 {
		t.Fatalf("--all apply exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	// Everything awa observed is back, including the unrelated source edit this time.
	if got := readAt(t, env.root, "src/service.go"); !strings.Contains(got, "return 1") {
		t.Errorf("--all did not restore the source file: %q", got)
	}
	// The ignored local file is still outside evidence, so --all never touched it.
	if !existsAt(env.root, "local-notes.txt") {
		t.Error("--all deleted an ignored local file; ignored paths are outside awa evidence")
	}
}

func TestScenario24UsageBoundaries(t *testing.T) {
	env := newRestoreEnv(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no source", []string{"restore"}, "requires a source state reference"},
		{"no selection", []string{"restore", env.checkpoint}, "requires one or more paths"},
		{"dry-run with apply", []string{"restore", "--dry-run", "--apply", env.checkpoint, "--", "src"}, "cannot be combined"},
		{"all with paths", []string{"restore", "--all", env.checkpoint, "--", "src"}, "cannot be combined with path selections"},
		{"now as a source", []string{"restore", "now", "--", "src"}, "is not a restore source"},
		{"a range as a source", []string{"restore", env.checkpoint + "..now", "--", "src"}, "not a range"},
		{"an unsupported flag", []string{"restore", "--recursive", env.checkpoint, "--", "src"}, "unknown flag"},
		{"restore after", []string{"restore", "restore:abcdef:after", "--", "src"}, "only restore:<id>:before exists"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := awa(t, env.root, tc.args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (usage error); stderr = %q", code, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr = %q, want it to mention %q", stderr, tc.want)
			}
		})
	}
}

func TestScenario24RefusesWhenTheSourceCannotProduceBytes(t *testing.T) {
	root := t.TempDir()
	if code, _, stderr := awa(t, root, "init"); code != 0 {
		t.Fatalf("init exit = %d, stderr = %q", code, stderr)
	}
	// A project that records identity but not content: the checkpoint proves what
	// the file was, and cannot produce it.
	writeAt(t, root, ".awa/config.toml", "[checkpoint]\nstore_file_contents = false\n")
	writeAt(t, root, "data.txt", "original\n")
	id := checkpointID(t, root)
	writeAt(t, root, "data.txt", "rewritten\n")

	code, stdout, _ := awa(t, root, "restore", "--json", id, "--", "data.txt")
	if code != 0 {
		t.Fatalf("preview exit = %d", code)
	}
	doc := decodeRestore(t, stdout)
	if doc.Data.Counts.Blocked != 1 || doc.Data.Complete {
		t.Fatalf("preview counts = %+v, complete = %v; want one blocked operation", doc.Data.Counts, doc.Data.Complete)
	}

	acode, astdout, astderr := awa(t, root, "restore", "--json", "--apply", id, "--", "data.txt")
	if acode != 1 {
		t.Fatalf("apply exit = %d, want 1 (generic failure); stderr = %q", acode, astderr)
	}
	adoc := decodeRestore(t, astdout)
	if adoc.Data.Outcome != "refused" {
		t.Errorf("outcome = %q, want refused", adoc.Data.Outcome)
	}
	if len(adoc.Data.Reasons) == 0 || adoc.Data.Reasons[0] != "hash-only-content" {
		t.Errorf("reasons = %v, want hash-only-content", adoc.Data.Reasons)
	}
	if adoc.Data.Recovery != nil {
		t.Error("a refused apply published a recovery observation")
	}
	if got := readAt(t, root, "data.txt"); got != "rewritten\n" {
		t.Errorf("a refused apply mutated the worktree: %q", got)
	}
}

// TestScenario24InterruptedApplyIsPartialAndRerunConverges is the shipped-binary
// proof of the honest-degradation rule: a commit that stops after writing one path
// must report `partial`, never success, and a rerun must re-plan from what the
// worktree is now and finish only the remaining work.
//
// The interruption is a real external fault, not a test seam: the destination
// directory of a later path is made unwritable, so the atomic replacement cannot
// create its same-directory temp there. The earlier path, in a writable directory,
// has already been committed by then — which is exactly the non-transactional
// window multi-path filesystem mutation always has.
func TestScenario24InterruptedApplyIsPartialAndRerunConverges(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory write permission is not modeled the same way here")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unwritable directory is still writable")
	}
	root := t.TempDir()
	if code, _, stderr := awa(t, root, "init"); code != 0 {
		t.Fatalf("init exit = %d, stderr = %q", code, stderr)
	}
	// Two generated subtrees. "early" sorts before "late", so the commit reaches the
	// writable one first and the blocked one second.
	writeAt(t, root, "gen/early/a.txt", "reviewed early\n")
	writeAt(t, root, "gen/late/z.txt", "reviewed late\n")
	id := checkpointID(t, root)
	writeAt(t, root, "gen/early/a.txt", "generated early\n")
	writeAt(t, root, "gen/late/z.txt", "generated late\n")

	lateDir := filepath.Join(root, "gen", "late")
	if err := os.Chmod(lateDir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	restored := false
	defer func() {
		if !restored {
			_ = os.Chmod(lateDir, 0o700)
		}
	}()

	code, stdout, stderr := awa(t, root, "restore", "--json", "--apply", id, "--", "gen")
	if code == 0 {
		t.Fatalf("an apply that could not finish exited 0\nstdout: %s", stdout)
	}
	doc := decodeRestore(t, stdout)
	if doc.Data.Outcome != "partial" {
		t.Fatalf("outcome = %q, want partial (reasons %v, failures %+v); stderr = %q",
			doc.Data.Outcome, doc.Data.Reasons, doc.Data.Failures, stderr)
	}
	if doc.Data.Completed != 1 || doc.Data.Remaining != 1 {
		t.Errorf("completed = %d remaining = %d, want 1 and 1", doc.Data.Completed, doc.Data.Remaining)
	}
	if !hasString(doc.Data.Reasons, "partial-apply") {
		t.Errorf("reasons = %v, want partial-apply", doc.Data.Reasons)
	}
	if doc.Data.Recovery == nil || doc.Data.Recovery.Ref == "" {
		t.Fatal("a partial apply carries no recovery observation to undo it from")
	}
	// The prefix that landed really landed, and the blocked path is untouched.
	if got := readAt(t, root, "gen/early/a.txt"); got != "reviewed early\n" {
		t.Errorf("gen/early/a.txt = %q, want the completed prefix", got)
	}
	if got := readAt(t, root, "gen/late/z.txt"); got != "generated late\n" {
		t.Errorf("gen/late/z.txt = %q, want it untouched by the stopped commit", got)
	}

	// Clear the fault and rerun: it re-plans from current reality, so only the one
	// remaining path is work.
	if err := os.Chmod(lateDir, 0o700); err != nil {
		t.Fatalf("chmod back: %v", err)
	}
	restored = true

	rcode, rstdout, rstderr := awa(t, root, "restore", "--json", "--apply", id, "--", "gen")
	if rcode != 0 {
		t.Fatalf("rerun exit = %d, stderr = %q\nstdout: %s", rcode, rstderr, rstdout)
	}
	rdoc := decodeRestore(t, rstdout)
	if rdoc.Data.Outcome != "applied" {
		t.Fatalf("rerun outcome = %q, want applied (reasons %v)", rdoc.Data.Outcome, rdoc.Data.Reasons)
	}
	if rdoc.Data.Completed != 1 {
		t.Errorf("rerun completed %d operation(s), want only the 1 that remained", rdoc.Data.Completed)
	}
	// Convergence, stated directly: the rerun plans the one path that remained and
	// does not re-plan the one that already landed as work.
	if rdoc.Data.Counts.Replace != 1 || rdoc.Data.Counts.Equal < 1 {
		t.Errorf("rerun counts = %+v, want exactly 1 replacement with the already-restored path proved equal", rdoc.Data.Counts)
	}
	if got := readAt(t, root, "gen/late/z.txt"); got != "reviewed late\n" {
		t.Errorf("gen/late/z.txt = %q, want it restored by the rerun", got)
	}
}

func hasString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestScenario24RerunAfterANarrowedSelectionConverges(t *testing.T) {
	env := newRestoreEnv(t)

	// Restore one file, then the rest: the second invocation re-plans from what the
	// worktree is now and does only the remaining work.
	if code, _, stderr := awa(t, env.root, "restore", "--apply", env.checkpoint, "--", "generated/client/openapi.json"); code != 0 {
		t.Fatalf("first apply exit = %d, stderr = %q", code, stderr)
	}
	code, stdout, stderr := awa(t, env.root, "restore", "--json", "--apply", env.checkpoint, "--", "generated/client")
	if code != 0 {
		t.Fatalf("second apply exit = %d, stderr = %q", code, stderr)
	}
	doc := decodeRestore(t, stdout)
	if doc.Data.Outcome != "applied" {
		t.Fatalf("second apply outcome = %q", doc.Data.Outcome)
	}
	if doc.Data.Counts.Replace != 2 {
		// openapi.json is already correct, so only model.go and tool.sh remain to
		// replace; the already-restored file counts as equal.
		t.Errorf("second apply replaced %d file(s), want 2 (the already-restored one is equal): %+v", doc.Data.Counts.Replace, doc.Data.Counts)
	}

	// A third invocation has nothing left to do.
	tcode, tstdout, _ := awa(t, env.root, "restore", "--json", "--apply", env.checkpoint, "--", "generated/client")
	if tcode != 0 {
		t.Fatalf("third apply exit = %d", tcode)
	}
	if got := decodeRestore(t, tstdout).Data.Outcome; got != "no-op" {
		t.Errorf("third apply outcome = %q, want no-op", got)
	}
}

// restoreGCReasons returns the gc reasons planned for restore recovery
// observations, read from the machine surface: the human report lists retained and
// blocked reasons but summarizes deletions by count, so JSON is the honest place to
// assert which decision was made.
func restoreGCReasons(t *testing.T, root string, args ...string) []string {
	t.Helper()
	code, stdout, stderr := awa(t, root, append([]string{"gc", "--dry-run", "--json"}, args...)...)
	if code != 0 {
		t.Fatalf("gc --dry-run --json %v exit = %d, stderr = %q", args, code, stderr)
	}
	var doc struct {
		Data struct {
			Candidates []struct {
				Kind   string `json:"kind"`
				Action string `json:"action"`
				Reason string `json:"reason"`
			} `json:"candidates"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("decode gc json: %v\n%s", err, stdout)
	}
	var out []string
	for _, c := range doc.Data.Candidates {
		if c.Kind == "restore" {
			out = append(out, c.Action+"/"+c.Reason)
		}
	}
	return out
}

// TestScenario24RunObservationIsAnHonestSource pins what a recorded run can and
// cannot do as a restore source through the shipped command. A run observation
// records identity but stores no bytes, so it names the state a command started
// from and then refuses to manufacture content for it — with the closed reason a
// consumer can branch on, not a failure.
func TestScenario24RunObservationIsAnHonestSource(t *testing.T) {
	env := newRestoreEnv(t)

	code, stdout, stderr := awa(t, env.root, "run", "--record", "--json", "--", "true")
	if code != 0 {
		t.Fatalf("run --record exit = %d, stderr = %q", code, stderr)
	}
	var run struct {
		Data struct {
			Run struct {
				ID string `json:"id"`
			} `json:"run"`
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &run); err != nil {
		t.Fatalf("decode run json: %v\n%s", err, stdout)
	}
	runID := run.Data.Run.ID
	if runID == "" {
		runID = run.Data.ID
	}
	if runID == "" {
		t.Fatalf("run json carries no id:\n%s", stdout)
	}

	// The run observed the worktree as it then was, so restoring straight away would
	// find everything equal and prove nothing. Change an observed file first: now the
	// source names an identity it holds no bytes for, which is the whole point of a
	// metadata-only source.
	writeAt(t, env.root, "generated/client/model.go", "package client\n\ntype Model struct{ Later bool }\n")

	pcode, pstdout, _ := awa(t, env.root, "restore", "--json", "run:"+runID+":before", "--", "generated/client")
	if pcode != 0 {
		t.Fatalf("preview from a run observation exit = %d\n%s", pcode, pstdout)
	}
	doc := decodeRestore(t, pstdout)
	if doc.Data.Source.Kind != "run-observation" {
		t.Errorf("source kind = %q, want run-observation", doc.Data.Source.Kind)
	}
	if doc.Data.Source.CanonicalRef != "run:"+runID+":before" {
		t.Errorf("canonical ref = %q, want the run observation reference", doc.Data.Source.CanonicalRef)
	}
	if doc.Data.Counts.Blocked != 1 {
		t.Fatalf("counts = %+v, want the one content write blocked; a metadata-only source must never plan one", doc.Data.Counts)
	}
	if doc.Data.Complete {
		t.Error("a plan that cannot produce the bytes it needs reported itself complete")
	}
	if len(doc.Data.Failures) != 1 || doc.Data.Failures[0].Path != "generated/client/model.go" {
		t.Fatalf("failures = %+v, want the changed file named", doc.Data.Failures)
	}
	// hash-only-content, and specifically not blob-missing: a run observation records
	// identity and publishes no content, so runstore persists every regular entry as
	// hash-only whatever the project's storage preference is. The reason therefore says
	// "this source never held these bytes", which is true, rather than "a blob that was
	// promised is gone", which would describe reclaimed content and send a reader
	// looking for a gc window that was never involved.
	if !hasString(doc.Data.Failures[0].Reasons, "hash-only-content") {
		t.Errorf("reasons = %v, want hash-only-content", doc.Data.Failures[0].Reasons)
	}

	// And apply refuses rather than writing something it cannot prove.
	acode, astdout, _ := awa(t, env.root, "restore", "--json", "--apply", "run:"+runID+":before", "--", "generated/client")
	if acode == 0 {
		t.Fatalf("apply from a metadata-only source exited 0\n%s", astdout)
	}
	if got := decodeRestore(t, astdout).Data.Outcome; got != "refused" {
		t.Errorf("apply outcome = %q, want refused", got)
	}
	if got := readAt(t, env.root, "generated/client/model.go"); got != "package client\n\ntype Model struct{ Later bool }\n" {
		t.Errorf("a refused apply wrote anyway: %q", got)
	}
}

// TestScenario24PolicyMismatchBlocksDeletions proves the compatibility rule end to
// end: once the current scan boundary differs from the one the source was observed
// under, absence in the source is no longer proof of absence in awa scope, so no
// deletion may be planned — while positive evidence stays usable.
func TestScenario24PolicyMismatchBlocksDeletions(t *testing.T) {
	env := newRestoreEnv(t)

	// Baseline: the accidental extra file is a proved deletion.
	code, stdout, _ := awa(t, env.root, "restore", "--json", env.checkpoint, "--", "generated/client")
	if code != 0 {
		t.Fatalf("baseline preview exit = %d\n%s", code, stdout)
	}
	base := decodeRestore(t, stdout)
	if !base.Data.Boundary.PolicyCompatible {
		t.Fatal("the fixture already has a policy mismatch; the contrast below would prove nothing")
	}
	if base.Data.Counts.DeleteFile == 0 {
		t.Fatalf("baseline planned no deletion, so the mismatch case cannot show one being withheld: %+v", base.Data.Counts)
	}

	// Shift the scan boundary. The excluded path matches nothing, so the observation's
	// content is identical and only its policy identity changes.
	writeAt(t, env.root, ".awa/config.toml", "[history]\nextra_excludes = [\"nothing-matches-this\"]\n")

	mcode, mstdout, _ := awa(t, env.root, "restore", "--json", env.checkpoint, "--", "generated/client")
	if mcode != 0 {
		t.Fatalf("mismatch preview exit = %d\n%s", mcode, mstdout)
	}
	doc := decodeRestore(t, mstdout)
	if doc.Data.Boundary.PolicyCompatible {
		t.Fatal("the scan policy identity did not actually change")
	}
	if doc.Data.Counts.DeleteFile != 0 || doc.Data.Counts.DeleteDirectory != 0 {
		t.Errorf("a deletion was planned from a policy-incompatible source: %+v", doc.Data.Counts)
	}
	if doc.Data.Counts.Replace == 0 {
		t.Errorf("positive evidence stopped being usable under a policy mismatch: %+v", doc.Data.Counts)
	}
	if doc.Data.Complete {
		t.Error("a plan that withheld a proved deletion reported itself complete")
	}
	found := false
	for _, f := range doc.Data.Failures {
		if hasString(f.Reasons, "policy-incompatible") {
			found = true
		}
	}
	if !found {
		t.Errorf("failures = %+v, want policy-incompatible named", doc.Data.Failures)
	}
}

func TestScenario24GCRetainsAndThenReclaimsTheRecoveryObservation(t *testing.T) {
	env := newRestoreEnv(t)
	code, stdout, stderr := awa(t, env.root, "restore", "--json", "--apply", env.checkpoint, "--", "generated/client")
	if code != 0 {
		t.Fatalf("apply exit = %d, stderr = %q", code, stderr)
	}
	ref := decodeRestore(t, stdout).Data.Recovery.Ref

	// Inside the default window, ordinary gc keeps it and says why.
	if got := restoreGCReasons(t, env.root); len(got) != 1 || got[0] != "retain/restore-too-recent" {
		t.Fatalf("gc decisions = %v, want one retain/restore-too-recent", got)
	}
	// And it is still resolvable.
	if rcode, _, rstderr := awa(t, env.root, "restore", ref, "--", "generated/client"); rcode != 0 {
		t.Fatalf("the retained observation is not resolvable: exit=%d stderr=%q", rcode, rstderr)
	}

	// Shrink the configured window to nothing: the record is now past it. This is the
	// deterministic way to cross the boundary — the alternative would be sleeping past
	// a real duration, since the grammar's smallest unit is a second.
	writeAt(t, env.root, ".awa/config.toml", "[gc]\nkeep_restores_for = \"0s\"\n")
	if got := restoreGCReasons(t, env.root); len(got) != 1 || got[0] != "delete/restore-expired" {
		t.Fatalf("gc decisions with a zero window = %v, want one delete/restore-expired", got)
	}

	// An explicit --older-than overrides the configured window for this invocation —
	// in either direction, which is why it is tested against a config saying the
	// opposite.
	if got := restoreGCReasons(t, env.root, "--older-than", "365d"); len(got) != 1 || got[0] != "retain/restore-too-recent" {
		t.Fatalf("gc decisions with --older-than 365d = %v, want the override to retain", got)
	}

	// A subsystem filter excludes recovery observations entirely, even when the
	// configured window says they are expired.
	if got := restoreGCReasons(t, env.root, "--runs-only"); len(got) != 1 || got[0] != "skipped/subsystem-filtered" {
		t.Fatalf("gc decisions under --runs-only = %v, want skipped/subsystem-filtered", got)
	}

	// And a real collection reclaims it, after which the reference stops resolving —
	// the honest consequence retention has, and why output never promises a date.
	if ccode, _, cstderr := awa(t, env.root, "gc"); ccode != 0 {
		t.Fatalf("gc exit = %d, stderr = %q", ccode, cstderr)
	}
	if rcode, _, _ := awa(t, env.root, "restore", ref, "--", "generated/client"); rcode == 0 {
		t.Error("a reclaimed recovery observation still resolves")
	}
}

func TestScenario24DoctorReportsDamagedRecoveryEvidence(t *testing.T) {
	env := newRestoreEnv(t)
	code, stdout, _ := awa(t, env.root, "restore", "--json", "--apply", env.checkpoint, "--", "generated/client")
	if code != 0 {
		t.Fatalf("apply exit = %d", code)
	}
	id := decodeRestore(t, stdout).Data.Recovery.OperationID

	// A healthy store reports nothing about restores.
	if dcode, dout, derr := awa(t, env.root, "doctor"); dcode != 0 {
		t.Fatalf("doctor on a healthy project exit = %d\nstdout:\n%s\nstderr:\n%s", dcode, dout, derr)
	}

	// Damage the record: doctor must name it rather than leaving the user to find
	// out at the moment they try to undo.
	meta := filepath.Join(env.root, ".awa", "restores", id, "meta.json")
	if err := os.Chmod(meta, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := os.WriteFile(meta, []byte("{ not a record }"), 0o600); err != nil {
		t.Fatalf("corrupt meta: %v", err)
	}
	dcode, dout, _ := awa(t, env.root, "doctor")
	if dcode != 5 {
		t.Fatalf("doctor exit = %d, want 5 (state action required)", dcode)
	}
	if !strings.Contains(dout, "restore-recovery-corrupt") {
		t.Errorf("doctor does not name the damaged recovery observation:\n%s", dout)
	}
}
