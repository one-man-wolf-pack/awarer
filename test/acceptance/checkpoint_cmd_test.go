package acceptance

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"awarer/internal/domain/checkpoint"
)

// initProject creates an initialized awa project in a fresh temp dir.
func initProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if code, _, stderr := awa(t, root, "init"); code != 0 {
		t.Fatalf("init exit = %d, stderr = %q", code, stderr)
	}
	return root
}

func write(t *testing.T, root, name, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// backdate moves a path's timestamps two seconds into the past so its stat
// signature differs from the one a previous scan indexed.
func backdate(t *testing.T, abs string) {
	t.Helper()
	past := time.Now().Add(-2 * time.Second)
	if err := os.Chtimes(abs, past, past); err != nil {
		t.Fatalf("chtimes %s: %v", abs, err)
	}
}

// checkpointIDs returns the ids of every committed checkpoint. The checkpoints
// directory reserves the id namespace and a checkpoint's only address is its
// directory, so an entry counts only when its own name parses as a checkpoint id and
// that directory holds a regular header.json — the commit point. A directory without
// the header is a crashed, uncommitted publish; names/ and anything else do not parse
// as an id; and a symlink standing in for the header is a foreign node, which the
// product itself refuses rather than reads.
//
// The oracle is deliberately at least as strict as the store: an acceptance count
// that admitted a node awa would reject could make a corrupt store look healthy.
func checkpointIDs(t *testing.T, root string) []string {
	t.Helper()
	dir := filepath.Join(root, ".awa", "checkpoints")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading checkpoints dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := checkpoint.ParseCheckpointID(e.Name()); err != nil {
			continue
		}
		// Lstat, not Stat: following a symlink here would let a node outside the
		// store make a directory look committed.
		info, err := os.Lstat(filepath.Join(dir, e.Name(), "header.json"))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		out = append(out, e.Name())
	}
	return out
}

// checkpointMetaFile returns the path of the single checkpoint's metadata document,
// "<id>/header.json", failing if there is not exactly one checkpoint. It is the
// durable record a corruption test tampers with.
func checkpointMetaFile(t *testing.T, root string) string {
	t.Helper()
	ids := checkpointIDs(t, root)
	if len(ids) != 1 {
		t.Fatalf("want exactly 1 checkpoint, got %d", len(ids))
	}
	return filepath.Join(root, ".awa", "checkpoints", ids[0], "header.json")
}

// readOnlyCheckpoint returns the single checkpoint's combined document: the
// header.json metadata plus the entries and skipped inputs its manifest.jsonl
// records. A checkpoint is always those two files together, so assertions read one
// map rather than reopening the pair themselves.
func readOnlyCheckpoint(t *testing.T, root string) map[string]any {
	t.Helper()
	meta := checkpointMetaFile(t, root)
	data, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decoding checkpoint header: %v", err)
	}
	// Fold the JSONL manifest records into entries/skipped arrays.
	mdata, err := os.ReadFile(filepath.Join(filepath.Dir(meta), "manifest.jsonl"))
	if err != nil {
		t.Fatalf("reading manifest: %v", err)
	}
	entries := []any{}
	skipped := []any{}
	for _, line := range strings.Split(strings.TrimRight(string(mdata), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decoding manifest line: %v", err)
		}
		if raw, ok := rec["entry"]; ok {
			var m any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			entries = append(entries, m)
		} else if raw, ok := rec["skipped"]; ok {
			var m any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			skipped = append(skipped, m)
		}
	}
	doc["entries"] = entries
	doc["skipped"] = skipped
	return doc
}

// logCheckpoint is the part of an `awa log --json` record these tests assert on.
type logCheckpoint struct {
	ID string `json:"id"`
}

// logCheckpoints runs `awa log --json` and returns the store's own count and its
// records. It is the second oracle for enumeration: an assertion about how many
// checkpoints exist reads the count from awa here and from the filesystem in
// checkpointIDs, so neither side can confirm the other's mistake.
func logCheckpoints(t *testing.T, root string) (int, []logCheckpoint) {
	t.Helper()
	code, stdout, stderr := awa(t, root, "log", "--json")
	if code != 0 {
		t.Fatalf("log --json exit = %d, stderr = %q", code, stderr)
	}
	var env struct {
		Data struct {
			Total       int             `json:"total"`
			Checkpoints []logCheckpoint `json:"checkpoints"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("log --json decode: %v\n%s", err, stdout)
	}
	return env.Data.Total, env.Data.Checkpoints
}

func blobCount(t *testing.T, root string) int {
	t.Helper()
	dir := filepath.Join(root, ".awa", "store", "blobs")
	count := 0
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			count++
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walking blobs: %v", err)
	}
	return count
}

func TestAcceptanceCheckpointCreatesCheckpoint(t *testing.T) {
	root := initProject(t)
	write(t, root, "calc.go", "package calc")

	code, _, stderr := awa(t, root, "checkpoint")
	if code != 0 {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	if got := len(checkpointIDs(t, root)); got != 1 {
		t.Fatalf("want 1 checkpoint file, got %d", got)
	}
	if blobCount(t, root) != 1 {
		t.Fatalf("want 1 blob, got %d", blobCount(t, root))
	}

	doc := readOnlyCheckpoint(t, root)
	if doc["schema_version"].(float64) != 1 {
		t.Errorf("schema_version = %v, want 1", doc["schema_version"])
	}
	if doc["git"] != nil {
		t.Errorf("non-git project should have git: null, got %v", doc["git"])
	}
}

// TestAcceptanceCheckpointEnumerationIgnoresForeignNodes proves the acceptance
// enumeration oracle is at least as strict as the store it observes. Two nodes are
// planted at addresses the store does not own: a directory whose name is not a
// checkpoint id, and an id-shaped directory whose header.json is a symlink rather
// than the regular file that marks a commit. Neither is a committed checkpoint, so
// neither may raise the count — an oracle that admitted one could let a corrupt store
// read back as healthy. awa's own verdict is the independent check: it ignores the
// unowned name and refuses the symlink at a reserved address outright.
func TestAcceptanceCheckpointEnumerationIgnoresForeignNodes(t *testing.T) {
	root := initProject(t)
	write(t, root, "a.txt", "x")
	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	ids := checkpointIDs(t, root)
	if len(ids) != 1 {
		t.Fatalf("setup: want exactly 1 checkpoint, got %v", ids)
	}
	dir := filepath.Join(root, ".awa", "checkpoints")
	realHeader := filepath.Join(dir, ids[0], "header.json")
	header, err := os.ReadFile(realHeader)
	if err != nil {
		t.Fatal(err)
	}

	// A directory outside the id namespace carrying a copy of a real header: the
	// address disqualifies it, not its contents.
	foreign := filepath.Join(dir, "not-a-checkpoint-id")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, "header.json"), header, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := checkpointIDs(t, root); len(got) != 1 || got[0] != ids[0] {
		t.Errorf("a directory outside the id namespace was counted as a checkpoint: %v", got)
	}
	// awa reads the store without complaint and still counts one checkpoint: a clean
	// exit alone would not distinguish ignoring the directory from adopting it.
	if total, _ := logCheckpoints(t, root); total != 1 {
		t.Errorf("awa counted %d checkpoints beside an unowned directory name, want 1", total)
	}

	// An id-shaped directory whose commit point is a symlink. Following it would let
	// a node the store never wrote present itself as a committed checkpoint.
	symlinked := filepath.Join(dir, mutateID(ids[0]))
	if err := os.MkdirAll(symlinked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realHeader, filepath.Join(symlinked, "header.json")); err != nil {
		t.Fatal(err)
	}
	if got := checkpointIDs(t, root); len(got) != 1 || got[0] != ids[0] {
		t.Errorf("a symlinked header was counted as a committed checkpoint: %v", got)
	}
	// awa does not merely skip it: a foreign node at a reserved address is structural
	// corruption, which exits 5 rather than reporting a readable store.
	if code, _, stderr := awa(t, root, "log"); code != 5 {
		t.Errorf("log over a symlink at a reserved checkpoint address exit = %d, want 5; stderr = %q", code, stderr)
	}
}

func TestAcceptanceFpMessageInLog(t *testing.T) {
	root := initProject(t)
	write(t, root, "a.txt", "x")
	if code, _, stderr := awa(t, root, "checkpoint", "-m", "first cut"); code != 0 {
		t.Fatalf("checkpoint -m exit = %d, %q", code, stderr)
	}

	code, stdout, _ := awa(t, root, "log")
	if code != 0 || !strings.Contains(stdout, "first cut") {
		t.Errorf("log missing message:\n%s", stdout)
	}

	// log --oneline, -1, --json
	if code, out, _ := awa(t, root, "log", "--oneline"); code != 0 || !strings.Contains(out, "first cut") {
		t.Errorf("log --oneline = %d / %q", code, out)
	}
	if code, out, _ := awa(t, root, "log", "-1"); code != 0 || !strings.Contains(out, "first cut") {
		t.Errorf("log -1 = %d / %q", code, out)
	}
	total, cps := logCheckpoints(t, root)
	if total != 1 || len(cps) != 1 {
		t.Fatalf("log --json reported total=%d, %d record(s); want exactly one checkpoint", total, len(cps))
	}
	if len(cps[0].ID) != 32 {
		t.Errorf("log --json id should be full 32 chars, got %q", cps[0].ID)
	}
}

func TestAcceptanceLogInvalidFlags(t *testing.T) {
	root := initProject(t)
	for _, args := range [][]string{
		{"log", "-n", "0"},                // 0 is ambiguous with the default, rejected
		{"log", "-n", "-1"},               // not a valid limit
		{"log", "--all", "--limit", "5"},  // mutually exclusive
		{"log", "--config", "other.toml"}, // log does not read config
		{"log", "--all=false"},            // boolean flag takes no value
		{"log", "-1", "--oneline"},        // conflicting render modes
		{"log", "--bogus"},
	} {
		if code, _, _ := awa(t, root, args...); code != 2 {
			t.Errorf("%v exit = %d, want 2", args, code)
		}
	}
}

func TestAcceptanceFpHonorsConfigOverride(t *testing.T) {
	root := initProject(t)
	write(t, root, "a.txt", "content worth storing")

	// An override config that disables content storage, written outside .awa so it
	// is unambiguously a separate file.
	override := filepath.Join(root, "override.toml")
	if err := os.WriteFile(override, []byte("[checkpoint]\nstore_file_contents = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code, _, stderr := awa(t, root, "checkpoint", "--config", override); code != 0 {
		t.Fatalf("checkpoint --config exit = %d, %q", code, stderr)
	}
	// store_file_contents=false from the override must take effect: no blobs, and
	// the entry recorded hash-only.
	if bc := blobCount(t, root); bc != 0 {
		t.Errorf("blob count = %d, want 0 (override disables content storage)", bc)
	}
	doc := readOnlyCheckpoint(t, root)
	for _, e := range doc["entries"].([]any) {
		m := e.(map[string]any)
		if m["path"] == "a.txt" && m["content_storage"] != "hash-only" {
			t.Errorf("a.txt content_storage = %v, want hash-only", m["content_storage"])
		}
	}
}

func TestAcceptanceTreeHashStableAcrossStorageToggle(t *testing.T) {
	root := initProject(t)
	write(t, root, "a.txt", "stable content")

	type checkpointData struct {
		TreeHash             string `json:"tree_hash"`
		ScanConfigHash       string `json:"scan_config_hash"`
		CheckpointPolicyHash string `json:"checkpoint_policy_hash"`
	}
	decodeFp := func(out string) checkpointData {
		t.Helper()
		var env struct {
			Data checkpointData `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("decode checkpoint json: %v\n%s", err, out)
		}
		return env.Data
	}

	code, out1, _ := awa(t, root, "checkpoint", "--json")
	if code != 0 {
		t.Fatalf("checkpoint exit = %d", code)
	}
	d1 := decodeFp(out1)
	if d1.TreeHash == "" || d1.ScanConfigHash == "" || d1.CheckpointPolicyHash == "" {
		t.Fatalf("checkpoint --json missing canonical hashes: %+v", d1)
	}

	// An override config (outside the project, so it is not itself scanned) that
	// disables content storage.
	override := filepath.Join(t.TempDir(), "override.toml")
	if err := os.WriteFile(override, []byte("[checkpoint]\nstore_file_contents = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out2, _ := awa(t, root, "checkpoint", "--config", override, "--json")
	if code != 0 {
		t.Fatalf("checkpoint --config exit = %d", code)
	}
	d2 := decodeFp(out2)

	if d1.TreeHash != d2.TreeHash {
		t.Errorf("tree_hash changed across store_file_contents toggle: %q vs %q", d1.TreeHash, d2.TreeHash)
	}
	if d1.ScanConfigHash != d2.ScanConfigHash {
		t.Errorf("scan_config_hash changed across store toggle (should be stable): %q vs %q", d1.ScanConfigHash, d2.ScanConfigHash)
	}
	if d1.CheckpointPolicyHash == d2.CheckpointPolicyHash {
		t.Errorf("checkpoint_policy_hash unchanged across store toggle: %q", d1.CheckpointPolicyHash)
	}

	// log --json exposes the same canonical names.
	code, lout, _ := awa(t, root, "log", "--json")
	if code != 0 {
		t.Fatalf("log --json exit = %d", code)
	}
	for _, f := range []string{"tree_hash", "scan_config_hash", "checkpoint_policy_hash"} {
		if !strings.Contains(lout, f) {
			t.Errorf("log --json missing canonical field %q", f)
		}
	}
}

func TestAcceptanceCorruptCheckpointExitsStorageCorruption(t *testing.T) {
	root := initProject(t)
	write(t, root, "a.txt", "x")
	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint exit = %d, %q", code, stderr)
	}
	// Corrupt the durable checkpoint metadata record (written read-only): header.json,
	// which log reads as a header.
	meta := checkpointMetaFile(t, root)
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte("{ not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Reading corrupt durable state must signal storage corruption (exit 5), not a
	// generic error (1) that scripts would treat as retryable.
	code, _, stderr := awa(t, root, "log")
	if code != 5 {
		t.Errorf("awa log over corrupt store exit = %d, want 5; stderr = %q", code, stderr)
	}
}

// TestAcceptanceIncompatibleCheckpointLogDegrades is the counterpart to the corrupt
// case: a record declaring a schema this build cannot read is intact evidence, not
// damage, so log degrades to a warning-bearing success (exit 0) rather than exit 5.
// This pins the store policy's incompatible-vs-corrupt distinction at the black-box
// boundary.
func TestAcceptanceIncompatibleCheckpointLogDegrades(t *testing.T) {
	root := initProject(t)
	write(t, root, "a.txt", "x")
	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint exit = %d, %q", code, stderr)
	}
	// Restamp the header one version past the current schema, so it declares a
	// generation this binary has no reader for. Reading the number out of the record
	// keeps this fixture free of any version literal.
	meta := checkpointMetaFile(t, root)
	data, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	current, err := strconv.Atoi(string(m["schema_version"]))
	if err != nil {
		t.Fatalf("header has no numeric schema_version: %v", err)
	}
	m["schema_version"] = json.RawMessage(strconv.Itoa(current + 1))
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, out, 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := awa(t, root, "log")
	if code != 0 {
		t.Errorf("awa log over incompatible store exit = %d, want 0 (degraded); stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "incompatible") {
		t.Errorf("awa log over incompatible store missing incompatible warning; stderr = %q", stderr)
	}
}

func TestAcceptanceFpRejectsPathArgument(t *testing.T) {
	root := initProject(t)
	write(t, root, "calc.go", "package calc")
	code, stdout, stderr := awa(t, root, "checkpoint", "./calc.go")
	if code != 2 {
		t.Errorf("checkpoint ./calc.go exit = %d, want 2", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "no path arguments") {
		t.Errorf("stderr = %q, want path-argument rejection", stderr)
	}
}

func TestAcceptanceFpInvalidFlags(t *testing.T) {
	root := initProject(t)
	for _, args := range [][]string{
		{"checkpoint", "--unknown"},
		{"checkpoint", "--message"}, // a value-taking flag with no value
		{"checkpoint", "--trust-mode", "nonsense"},
	} {
		if code, _, _ := awa(t, root, args...); code != 2 {
			t.Errorf("%v exit = %d, want 2", args, code)
		}
	}
}

func TestAcceptanceNestedCwdRecorded(t *testing.T) {
	root := initProject(t)
	write(t, root, "a/b/c.txt", "deep")
	nested := filepath.Join(root, "a", "b")
	if code, _, stderr := awa(t, nested, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint from nested exit = %d, %q", code, stderr)
	}
	doc := readOnlyCheckpoint(t, root)
	if doc["command_cwd"] != "a/b" {
		t.Errorf("command_cwd = %v, want a/b", doc["command_cwd"])
	}
}

func TestAcceptanceGitMetadata(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := initProject(t)
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "t@example.com")
	runGit(t, root, "config", "user.name", "T")
	runGit(t, root, "config", "commit.gpgsign", "false")
	write(t, root, "tracked.txt", "v1")
	runGit(t, root, "add", "tracked.txt")
	runGit(t, root, "commit", "-m", "init")
	write(t, root, "untracked.txt", "new")

	code, out, _ := awa(t, root, "checkpoint", "--json")
	if code != 0 {
		t.Fatalf("checkpoint --json exit = %d", code)
	}
	var env struct {
		Data struct {
			Git *struct {
				Branch      string `json:"branch"`
				ShortCommit string `json:"short_commit"`
				Clean       bool   `json:"clean"`
			} `json:"git"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.Git == nil {
		t.Fatal("git metadata absent in a git project")
	}
	if env.Data.Git.Branch == "" || env.Data.Git.ShortCommit == "" {
		t.Errorf("git branch/commit missing: %+v", env.Data.Git)
	}
	if env.Data.Git.Clean {
		t.Error("worktree with an untracked file should be dirty")
	}
}

func TestAcceptanceLargeFilePolicies(t *testing.T) {
	root := initProject(t)
	// Tighten the large-file policy to skip via an override config.
	writeProjectConfig(t, root, "[hashing]\nmax_file_size = \"4B\"\nlarge_file_policy = \"skip\"\n")

	write(t, root, "small.txt", "ok")
	write(t, root, "big.bin", "way over four bytes")
	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint exit = %d, %q", code, stderr)
	}
	doc := readOnlyCheckpoint(t, root)
	skipped, _ := doc["skipped"].([]any)
	var found bool
	for _, s := range skipped {
		m := s.(map[string]any)
		if m["path"] == "big.bin" && m["reason"] == "large-file-skip" {
			found = true
		}
	}
	if !found {
		t.Errorf("big.bin not skipped under skip policy: %v", skipped)
	}
}

func TestAcceptanceSymlinkAndHardlink(t *testing.T) {
	root := initProject(t)
	write(t, root, "real.txt", "shared content")
	// Hardlink: same content, must dedupe to one blob.
	if err := os.Link(filepath.Join(root, "real.txt"), filepath.Join(root, "hard.txt")); err != nil {
		t.Skipf("hardlink unsupported: %v", err)
	}
	// Symlink: recorded inline, no blob.
	if err := os.Symlink("real.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint exit = %d, %q", code, stderr)
	}
	doc := readOnlyCheckpoint(t, root)
	entries := doc["entries"].([]any)
	var symlinkInline bool
	for _, e := range entries {
		m := e.(map[string]any)
		if m["path"] == "link.txt" {
			if m["kind"] != "symlink" || m["content_storage"] != "inline-symlink-target" {
				t.Errorf("symlink entry wrong: %v", m)
			}
			if m["symlink_target"] != "real.txt" {
				t.Errorf("symlink target = %v", m["symlink_target"])
			}
			symlinkInline = true
		}
	}
	if !symlinkInline {
		t.Error("symlink entry missing")
	}
	// real.txt and hard.txt share content -> exactly one blob.
	if blobCount(t, root) != 1 {
		t.Errorf("hard-linked identical content should dedupe to 1 blob, got %d", blobCount(t, root))
	}
}

func TestAcceptanceStatusAfterCheckpoint(t *testing.T) {
	root := initProject(t)
	write(t, root, "a.txt", "x")
	if code, _, _ := awa(t, root, "checkpoint", "-m", "baseline"); code != 0 {
		t.Fatal("checkpoint failed")
	}
	code, stdout, _ := awa(t, root, "status")
	if code != 0 {
		t.Fatalf("status exit = %d", code)
	}
	if !strings.Contains(stdout, "checkpoints: 1") || !strings.Contains(stdout, "latest:") {
		t.Errorf("status missing checkpoint counts:\n%s", stdout)
	}

	code, jsonOut, _ := awa(t, root, "status", "--json")
	if code != 0 {
		t.Fatalf("status --json exit = %d", code)
	}
	var env struct {
		Data struct {
			Checkpoints struct {
				Recorded  int  `json:"recorded"`
				Populated bool `json:"populated"`
				Latest    *struct {
					ID string `json:"id"`
				} `json:"latest"`
			} `json:"checkpoints"`
			Store struct {
				Footprint struct {
					Available bool   `json:"available"`
					Reason    string `json:"reason"`
				} `json:"footprint"`
			} `json:"store"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &env); err != nil {
		t.Fatalf("status --json decode: %v", err)
	}
	if env.Data.Checkpoints.Recorded != 1 || !env.Data.Checkpoints.Populated {
		t.Errorf("checkpoints subsystem = %+v", env.Data.Checkpoints)
	}
	if env.Data.Checkpoints.Latest == nil || len(env.Data.Checkpoints.Latest.ID) != 32 {
		t.Errorf("latest = %+v", env.Data.Checkpoints.Latest)
	}
	// Ordinary status must not walk the blob store: the exact footprint is intentionally
	// unavailable, with a bounded reason. The deep count lives in gc --dry-run --json.
	if env.Data.Store.Footprint.Available || env.Data.Store.Footprint.Reason != "bounded" {
		t.Errorf("store footprint = %+v, want unavailable/bounded", env.Data.Store.Footprint)
	}

	code, gcOut, _ := awa(t, root, "gc", "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("gc --dry-run --json exit = %d", code)
	}
	var gcEnv struct {
		Data struct {
			StoreFootprint *struct {
				BlobCount int64 `json:"blob_count"`
				Complete  bool  `json:"complete"`
			} `json:"store_footprint"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(gcOut), &gcEnv); err != nil {
		t.Fatalf("gc --json decode: %v", err)
	}
	if gcEnv.Data.StoreFootprint == nil {
		t.Fatalf("gc --dry-run --json missing store_footprint")
	}
	if gcEnv.Data.StoreFootprint.BlobCount != 1 || !gcEnv.Data.StoreFootprint.Complete {
		t.Errorf("gc store_footprint = %+v, want blob_count 1 complete", *gcEnv.Data.StoreFootprint)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
