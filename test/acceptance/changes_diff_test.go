package acceptance

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setMaxFileSize writes an override config with the given max_file_size so
// large-file policy can be exercised without writing a 50 MiB file.
func setMaxFileSize(t *testing.T, root, size string) {
	t.Helper()
	writeProjectConfig(t, root, "[hashing]\nmax_file_size = \""+size+"\"\n")
}

// corruptOneBlob overwrites the bytes of a single stored blob with content that
// no longer hashes to its address, simulating on-disk blob corruption.
func corruptOneBlob(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, ".awa", "store", "blobs")
	var target string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && target == "" {
			target = p
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking blobs: %v", err)
	}
	if target == "" {
		t.Fatal("no blob to corrupt")
	}
	// Blobs are stored read-only; make it writable to tamper with it.
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatalf("chmod blob: %v", err)
	}
	if err := os.WriteFile(target, []byte("corrupted bytes that do not match the hash\n"), 0o644); err != nil {
		t.Fatalf("overwrite blob: %v", err)
	}
}

// removeAllBlobs deletes every stored blob, simulating store corruption.
func removeAllBlobs(t *testing.T, root string) int {
	t.Helper()
	dir := filepath.Join(root, ".awa", "store", "blobs")
	removed := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			if err := os.Remove(p); err != nil {
				return err
			}
			removed++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("removing blobs: %v", err)
	}
	return removed
}

// TestAcceptanceCheckpointModifyChangesDiff covers spec scenario 2.
func TestAcceptanceCheckpointModifyChangesDiff(t *testing.T) {
	root := initProject(t)
	write(t, root, "calc/calc.go", "package calc\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n")
	write(t, root, "data/input.txt", "hello\n")
	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}

	// No edits yet: changes is empty.
	if code, stdout, _ := awa(t, root, "changes"); code != 0 || !strings.Contains(stdout, "no changes") {
		t.Fatalf("changes (clean) exit=%d stdout=%q", code, stdout)
	}

	// Modify calc.go; changes defaults to latest..now and reports M.
	write(t, root, "calc/calc.go", "package calc\n\nfunc Add(a, b int) int {\n\treturn a + b + 1\n}\n")
	code, stdout, _ := awa(t, root, "changes")
	if code != 0 || !strings.Contains(stdout, "M calc/calc.go") {
		t.Fatalf("changes exit=%d stdout=%q, want M calc/calc.go", code, stdout)
	}

	// diff shows the unified content diff.
	code, stdout, _ = awa(t, root, "diff")
	if code != 0 || !strings.Contains(stdout, "@@") || !strings.Contains(stdout, "+\treturn a + b + 1") {
		t.Fatalf("diff exit=%d stdout=%q", code, stdout)
	}

	// Path-limited changes include only matching paths.
	code, stdout, _ = awa(t, root, "changes", "calc")
	if code != 0 || !strings.Contains(stdout, "calc/calc.go") || strings.Contains(stdout, "data/") {
		t.Fatalf("path-limited changes wrong: %q", stdout)
	}

	// File-limited diff shows only that file (and a non-matching file shows none).
	if code, stdout, _ := awa(t, root, "diff", "data/input.txt"); code != 0 || !strings.Contains(stdout, "no changes") {
		t.Fatalf("diff data/input.txt should show no changes, got exit=%d %q", code, stdout)
	}

	// The "now" side is ephemeral: comparing against it must not publish a new
	// checkpoint or materialize new blobs.
	blobsBefore := blobCount(t, root)
	awa(t, root, "changes")
	awa(t, root, "diff")
	if ids := checkpointIDs(t, root); len(ids) != 1 {
		t.Fatalf("now scan must not create checkpoints: have %d", len(ids))
	}
	if after := blobCount(t, root); after != blobsBefore {
		t.Fatalf("now scan must not materialize blobs: %d -> %d", blobsBefore, after)
	}
}

// TestAcceptanceMultipleCheckpointsAndRanges covers spec scenario 3.
func TestAcceptanceMultipleCheckpointsAndRanges(t *testing.T) {
	root := initProject(t)
	write(t, root, "calc/calc.go", "package calc\nfunc Add() {}\n")
	write(t, root, "data/input.txt", "hello\n")
	first := checkpointID(t, root)
	write(t, root, "data/input.txt", "hello\nworld\n")
	second := checkpointID(t, root)
	write(t, root, "calc/calc.go", "package calc\nfunc Add() int { return 0 }\n")

	// Ids resolve; first..second changed only data/input.txt.
	if code, stdout, _ := awa(t, root, "changes", first+".."+second); code != 0 ||
		!strings.Contains(stdout, "M data/input.txt") || strings.Contains(stdout, "calc/calc.go") {
		t.Fatalf("first..second wrong: exit=%d %q", code, stdout)
	}
	// An unambiguous prefix of an id addresses the same checkpoint as the full id.
	if code, stdout, _ := awa(t, root, "changes", first[:12]+".."+second[:12]); code != 0 ||
		!strings.Contains(stdout, "M data/input.txt") || strings.Contains(stdout, "calc/calc.go") {
		t.Fatalf("prefix range wrong: exit=%d %q", code, stdout)
	}
	// second..now changed only calc.go (now = current workspace).
	if code, stdout, _ := awa(t, root, "changes", second+"..now"); code != 0 ||
		!strings.Contains(stdout, "M calc/calc.go") || strings.Contains(stdout, "data/input.txt") {
		t.Fatalf("second..now wrong: exit=%d %q", code, stdout)
	}
	// @-2..@-1 equals first..second.
	if code, stdout, _ := awa(t, root, "changes", "@-2..@-1"); code != 0 || !strings.Contains(stdout, "M data/input.txt") {
		t.Fatalf("@-2..@-1 wrong: exit=%d %q", code, stdout)
	}
}

// TestAcceptanceRootDiscoveryFromNested covers spec scenario 5 for changes/diff.
func TestAcceptanceRootDiscoveryFromNested(t *testing.T) {
	root := initProject(t)
	write(t, root, "packages/api/api.go", "package api\nfunc H() {}\n")
	write(t, root, "calc/calc.go", "package calc\n")
	if code, _, _ := awa(t, root, "checkpoint"); code != 0 {
		t.Fatal("checkpoint")
	}
	write(t, root, "packages/api/api.go", "package api\nfunc H() int { return 1 }\n")

	nested := filepath.Join(root, "packages", "api")
	// api.go path argument is resolved relative to packages/api, and output paths
	// are normalized relative to the project root.
	code, stdout, stderr := awa(t, nested, "diff", "api.go")
	if code != 0 {
		t.Fatalf("nested diff exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "packages/api/api.go") {
		t.Fatalf("nested diff should show root-relative path, got %q", stdout)
	}
	// A non-matching path from the nested cwd narrows to nothing.
	if code, stdout, _ := awa(t, nested, "changes", "."); code != 0 || !strings.Contains(stdout, "api/api.go") {
		t.Fatalf("nested changes . wrong: exit=%d %q", code, stdout)
	}
}

// TestAcceptanceJSONContract covers spec scenario 11 for changes/diff.
func TestAcceptanceChangesDiffJSONContract(t *testing.T) {
	root := initProject(t)
	write(t, root, "calc/calc.go", "line1\nline2\n")
	if code, _, _ := awa(t, root, "checkpoint"); code != 0 {
		t.Fatal("checkpoint")
	}
	write(t, root, "calc/calc.go", "line1\nCHANGED\n")

	for _, cmd := range []string{"changes", "diff"} {
		code, stdout, _ := awa(t, root, cmd, "--json")
		if code != 0 {
			t.Fatalf("%s --json exit = %d", cmd, code)
		}
		var env struct {
			SchemaVersion int    `json:"schema_version"`
			Command       string `json:"command"`
			Data          json.RawMessage
		}
		if err := json.Unmarshal([]byte(stdout), &env); err != nil {
			t.Fatalf("%s --json invalid: %v\n%s", cmd, err, stdout)
		}
		if env.SchemaVersion != 1 || env.Command != cmd {
			t.Errorf("%s envelope = %+v", cmd, env)
		}
	}
}

// TestAcceptanceLargeFileHashOnlyDiff covers spec scenario 14 (hash-only).
func TestAcceptanceLargeFileHashOnlyDiff(t *testing.T) {
	root := initProject(t)
	setMaxFileSize(t, root, "16B")
	write(t, root, "big.bin", strings.Repeat("A", 64)+"\n")
	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint exit=%d stderr=%q", code, stderr)
	}
	// Change the large file's content; it is hash-only, so changes detects it but
	// diff reports it as unavailable for a content diff.
	write(t, root, "big.bin", strings.Repeat("B", 64)+"\n")
	if code, stdout, _ := awa(t, root, "changes"); code != 0 || !strings.Contains(stdout, "big.bin") {
		t.Fatalf("changes should detect hash-only change: exit=%d %q", code, stdout)
	}
	code, stdout, _ := awa(t, root, "diff")
	if code != 0 || !strings.Contains(stdout, "big.bin") || !strings.Contains(stdout, "hash-only") {
		t.Fatalf("diff should report hash-only unavailable: exit=%d %q", code, stdout)
	}
}

// TestAcceptanceSymlinkTargetChange covers spec scenario 15 with follow_symlinks=false.
func TestAcceptanceSymlinkTargetChange(t *testing.T) {
	root := initProject(t)
	write(t, root, "real-a.txt", "a\n")
	write(t, root, "real-b.txt", "b\n")
	link := filepath.Join(root, "link")
	if err := os.Symlink("real-a.txt", link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint exit=%d stderr=%q", code, stderr)
	}
	// Re-point the symlink; the link entry changes (reported as M), without
	// following it.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real-b.txt", link); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ := awa(t, root, "changes")
	if code != 0 || !strings.Contains(stdout, "link") {
		t.Fatalf("symlink target change not reported: exit=%d %q", code, stdout)
	}
}

// TestAcceptanceContentAndModeChange checks that a permission change stays
// visible even when it happens together with a content edit.
func TestAcceptanceContentAndModeChange(t *testing.T) {
	root := initProject(t)
	write(t, root, "run.sh", "#!/bin/sh\necho one\n")
	if err := os.Chmod(filepath.Join(root, "run.sh"), 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint exit=%d stderr=%q", code, stderr)
	}
	// Edit content and flip the executable bit in the same change.
	write(t, root, "run.sh", "#!/bin/sh\necho two\n")
	if err := os.Chmod(filepath.Join(root, "run.sh"), 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	// changes surfaces the mode change alongside the modification.
	if code, stdout, _ := awa(t, root, "changes"); code != 0 || !strings.Contains(stdout, "mode 0644 -> 0755") {
		t.Fatalf("changes should report the mode change: exit=%d %q", code, stdout)
	}
	// diff shows both the mode change (header) and the content diff (body).
	code, stdout, _ := awa(t, root, "diff")
	if code != 0 {
		t.Fatalf("diff exit=%d", code)
	}
	if !strings.Contains(stdout, "mode 0644 -> 0755") {
		t.Errorf("diff header should report the mode change:\n%s", stdout)
	}
	if !strings.Contains(stdout, "-echo one") || !strings.Contains(stdout, "+echo two") {
		t.Errorf("diff body should show the content change:\n%s", stdout)
	}
}

// TestAcceptanceLiteralPathFilterAfterDoubleDash covers path filters whose names
// collide with range or flag syntax: they are expressible only after "--".
func TestAcceptanceLiteralPathFilterAfterDoubleDash(t *testing.T) {
	root := initProject(t)
	// A file whose name looks like a range, and one whose name looks like a flag.
	write(t, root, "a..b.txt", "one\n")
	write(t, root, "-dash.go", "package x\n")
	write(t, root, "other.txt", "keep\n")
	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint exit=%d stderr=%q", code, stderr)
	}
	write(t, root, "a..b.txt", "two\n")
	backdate(t, filepath.Join(root, "a..b.txt"))
	write(t, root, "-dash.go", "package y\n")
	backdate(t, filepath.Join(root, "-dash.go"))
	write(t, root, "other.txt", "changed\n")

	// "--" makes the range-looking name a literal path filter.
	code, stdout, stderr := awa(t, root, "changes", "--", "a..b.txt")
	if code != 0 {
		t.Fatalf("changes -- a..b.txt exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "a..b.txt") || strings.Contains(stdout, "other.txt") {
		t.Fatalf("literal range-like path filter wrong: %q", stdout)
	}

	// "--" makes the flag-looking name a literal path filter.
	code, stdout, stderr = awa(t, root, "diff", "--", "-dash.go")
	if code != 0 {
		t.Fatalf("diff -- -dash.go exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "-dash.go") || strings.Contains(stdout, "other.txt") {
		t.Fatalf("literal flag-like path filter wrong: %q", stdout)
	}

	// A range before "--" still applies, with the operand narrowing it.
	if code, stdout, _ := awa(t, root, "changes", "latest..now", "--", "other.txt"); code != 0 ||
		!strings.Contains(stdout, "other.txt") || strings.Contains(stdout, "a..b.txt") {
		t.Fatalf("range + operand filter wrong: exit=%d %q", code, stdout)
	}
}

// TestAcceptanceMissingBlobIsStorageCorruption covers spec scenario 18 for diff.
func TestAcceptanceMissingBlobIsStorageCorruption(t *testing.T) {
	root := initProject(t)
	write(t, root, "calc/calc.go", "line1\nline2\n")
	if code, _, _ := awa(t, root, "checkpoint"); code != 0 {
		t.Fatal("checkpoint")
	}
	// Modify the file so a diff must read the checkpoint's (old) blob, then delete
	// every blob to simulate corruption.
	write(t, root, "calc/calc.go", "line1\nCHANGED\n")
	if removed := removeAllBlobs(t, root); removed == 0 {
		t.Fatal("expected at least one blob to remove")
	}
	code, _, stderr := awa(t, root, "diff")
	if code != 5 {
		t.Fatalf("diff with missing blob exit = %d, want 5 (storage corruption); stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "missing") && !strings.Contains(stderr, "corrupt") {
		t.Errorf("stderr should explain corruption, got %q", stderr)
	}
}

// TestAcceptanceCorruptBlobIsStorageCorruption checks that a present-but-tampered
// blob (hash mismatch), not just a missing one, is reported as storage corruption.
func TestAcceptanceCorruptBlobIsStorageCorruption(t *testing.T) {
	root := initProject(t)
	write(t, root, "calc/calc.go", "line1\nline2\n")
	if code, _, _ := awa(t, root, "checkpoint"); code != 0 {
		t.Fatal("checkpoint")
	}
	// Modify the file so a diff must read the checkpoint's old blob, then corrupt
	// that blob's bytes so it no longer hashes to its address.
	write(t, root, "calc/calc.go", "line1\nCHANGED\n")
	corruptOneBlob(t, root)

	code, _, stderr := awa(t, root, "diff")
	if code != 5 {
		t.Fatalf("diff with corrupt blob exit = %d, want 5 (storage corruption); stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "corrupt") {
		t.Errorf("stderr should explain corruption, got %q", stderr)
	}
}
