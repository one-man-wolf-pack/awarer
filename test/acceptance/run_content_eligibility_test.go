package acceptance

import (
	"encoding/json"
	"strings"
	"testing"
)

// eligibilityElem is the part of a changes/diff JSON element the per-change byte
// rule governs, plus the counterpart fact that must survive rendering unchanged.
type eligibilityElem struct {
	Status               string `json:"status"`
	OldPath              string `json:"old_path"`
	NewPath              string `json:"new_path"`
	ContentDiffAvailable bool   `json:"content_diff_available"`
	Availability         string `json:"availability"`
	Diff                 string `json:"diff"`
	Counterpart          *struct {
		Side          string `json:"side"`
		Qualification string `json:"qualification"`
	} `json:"counterpart"`
}

func (e eligibilityElem) path() string {
	if e.NewPath != "" {
		return e.NewPath
	}
	return e.OldPath
}

// eligibilityByPath runs `<command> --json <range>` and indexes its elements by path.
func eligibilityByPath(t *testing.T, root, command, rng string) map[string]eligibilityElem {
	t.Helper()
	code, out, stderr := awa(t, root, command, "--json", rng)
	if code != 0 {
		t.Fatalf("%s --json %s exit = %d, stderr = %q", command, rng, code, stderr)
	}
	var doc struct {
		Data struct {
			Changes []eligibilityElem `json:"changes"`
			Files   []eligibilityElem `json:"files"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("decode %s --json %s: %v\n%s", command, rng, err, out)
	}
	got := map[string]eligibilityElem{}
	for _, e := range append(doc.Data.Changes, doc.Data.Files...) {
		got[e.path()] = e
	}
	return got
}

// recordRun records `true` and returns the new run's short id from run log.
func recordRun(t *testing.T, root string) string {
	t.Helper()
	if code, _, stderr := awa(t, root, "run", "--record", "--", "true"); code != 0 {
		t.Fatalf("run --record exit = %d, stderr = %q", code, stderr)
	}
	code, out, _ := awa(t, root, "run", "log")
	if code != 0 || strings.TrimSpace(out) == "" {
		t.Fatalf("run log = %d / %q", code, out)
	}
	return strings.Fields(strings.SplitN(out, "\n", 2)[0])[0]
}

// createCheckpoint creates a checkpoint and returns its id.
func createCheckpoint(t *testing.T, root string) string {
	t.Helper()
	before := map[string]bool{}
	for _, id := range checkpointIDs(t, root) {
		before[id] = true
	}
	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint exit = %d, %s", code, stderr)
	}
	for _, id := range checkpointIDs(t, root) {
		if !before[id] {
			return id
		}
	}
	t.Fatal("checkpoint created no new id")
	return ""
}

// assertEligibility checks that changes and diff agree on one path's byte
// eligibility and that diff renders it with the wanted availability.
func assertEligibility(t *testing.T, root, rng, path, status string, eligible bool, availability string) eligibilityElem {
	t.Helper()
	ch, ok := eligibilityByPath(t, root, "changes", rng)[path]
	if !ok {
		t.Fatalf("changes %s lacks %s", rng, path)
	}
	fd, ok := eligibilityByPath(t, root, "diff", rng)[path]
	if !ok {
		t.Fatalf("diff %s lacks %s", rng, path)
	}
	if ch.Status != status || fd.Status != status {
		t.Errorf("%s %s status = changes %q / diff %q, want %q", rng, path, ch.Status, fd.Status, status)
	}
	if ch.ContentDiffAvailable != eligible || fd.ContentDiffAvailable != eligible {
		t.Errorf("%s %s content_diff_available = changes %v / diff %v, want %v",
			rng, path, ch.ContentDiffAvailable, fd.ContentDiffAvailable, eligible)
	}
	if fd.Availability != availability {
		t.Errorf("%s %s availability = %q, want %q", rng, path, fd.Availability, availability)
	}
	if (ch.Counterpart == nil) != (fd.Counterpart == nil) ||
		(ch.Counterpart != nil && *ch.Counterpart != *fd.Counterpart) {
		t.Errorf("%s %s counterpart = changes %+v / diff %+v, want the same fact", rng, path, ch.Counterpart, fd.Counterpart)
	}
	return fd
}

// TestRunOnUnreadSideDoesNotBlockOwnedContent: content-diff eligibility is per
// change. An added file needs only its right entry's bytes and a deleted file only
// its left entry's, so a checkpoint-owned file renders as text even when the other
// operand is a run observation. A change that needs a run's bytes stays hash-only,
// even for an empty file or when a checkpoint in the same store owns a matching
// blob, and so does a checkpoint's own hash-only entry. changes and diff report the
// same eligibility for every path.
func TestRunOnUnreadSideDoesNotBlockOwnedContent(t *testing.T) {
	root := initProject(t)
	write(t, root, "keep.txt", "shared bytes\n")
	write(t, root, "mod.txt", "before\n")
	base := createCheckpoint(t, root)

	// The run observes files no checkpoint records: one whose bytes a checkpoint blob
	// already holds, a present empty file, and a content modification.
	write(t, root, "runonly.txt", "shared bytes\n")
	write(t, root, "empty.txt", "")
	write(t, root, "mod.txt", "after\n")
	run := recordRun(t, root)
	runAfter := "run:" + run + ":after"

	// The checkpoint then owns new text and binary files; .env is hash-only under the
	// default retention list although keep.txt's blob holds the same bytes.
	write(t, root, "new.txt", "line one\nline two\n")
	write(t, root, "img.bin", "\x00\x01binary\x00payload")
	write(t, root, ".env", "shared bytes\n")
	cp := createCheckpoint(t, root)

	// Eligible one-sided text with the run on the side the change does not read: the
	// added mirror and the deleted original.
	added := runAfter + ".." + cp
	fd := assertEligibility(t, root, added, "new.txt", "added", true, "text")
	if !strings.Contains(fd.Diff, "--- /dev/null") || !strings.Contains(fd.Diff, "+line one") {
		t.Errorf("%s new.txt diff = %q, want an added text hunk", added, fd.Diff)
	}
	if fd.Counterpart == nil || fd.Counterpart.Side != "left" || fd.Counterpart.Qualification != "absent" {
		t.Errorf("%s new.txt counterpart = %+v, want left absent", added, fd.Counterpart)
	}
	deleted := cp + ".." + runAfter
	fd = assertEligibility(t, root, deleted, "new.txt", "deleted", true, "text")
	if !strings.Contains(fd.Diff, "+++ /dev/null") || !strings.Contains(fd.Diff, "-line one") {
		t.Errorf("%s new.txt diff = %q, want a deleted text hunk", deleted, fd.Diff)
	}
	if fd.Counterpart == nil || fd.Counterpart.Side != "right" || fd.Counterpart.Qualification != "absent" {
		t.Errorf("%s new.txt counterpart = %+v, want right absent", deleted, fd.Counterpart)
	}

	// Eligibility is not a text promise: owned binary bytes classify as binary.
	assertEligibility(t, root, added, "img.bin", "added", true, "binary")
	assertEligibility(t, root, deleted, "img.bin", "deleted", true, "binary")

	// A required checkpoint side recorded hash-only stays unavailable even though the
	// store holds a blob with the same bytes.
	assertEligibility(t, root, added, ".env", "added", false, "hash-only")
	assertEligibility(t, root, deleted, ".env", "deleted", false, "hash-only")

	// Reversed: the run is the side that would have to supply bytes.
	runSide := base + ".." + runAfter
	assertEligibility(t, root, runSide, "runonly.txt", "added", false, "hash-only")
	assertEligibility(t, root, runSide, "empty.txt", "added", false, "hash-only")
	assertEligibility(t, root, runSide, "mod.txt", "modified", false, "hash-only")
	runLeft := runAfter + ".." + base
	assertEligibility(t, root, runLeft, "runonly.txt", "deleted", false, "hash-only")
	assertEligibility(t, root, runLeft, "empty.txt", "deleted", false, "hash-only")
	assertEligibility(t, root, runLeft, "mod.txt", "modified", false, "hash-only")

	// The human diff renders the hunk under the ordinary header and keeps the
	// required-run rows hash-only.
	code, human, stderr := awa(t, root, "diff", added)
	if code != 0 || !strings.Contains(human, "diff new.txt\n") || !strings.Contains(human, "+line two\n") {
		t.Errorf("diff %s exit = %d stdout:\n%s\nstderr: %s", added, code, human, stderr)
	}
	code, human, _ = awa(t, root, "diff", runSide)
	if code != 0 || !strings.Contains(human, "A runonly.txt (hash-only content unavailable)\n") {
		t.Errorf("diff %s exit = %d stdout:\n%s", runSide, code, human)
	}
}

// TestRenderedOneSidedContentKeepsOutsideObservation: rendering an eligible added
// file's bytes does not upgrade a run's outside-observation counterpart into proved
// absence, in either human or JSON output.
func TestRenderedOneSidedContentKeepsOutsideObservation(t *testing.T) {
	root := initProject(t)
	write(t, root, "keep.txt", "x\n")
	writeProjectConfig(t, root, "[scope]\nextra_excludes = [\"gen/\"]\n")
	write(t, root, "gen/x.txt", "generated\n")
	run := recordRun(t, root)
	writeProjectConfig(t, root, "")
	cp := createCheckpoint(t, root)

	rng := "run:" + run + ":after.." + cp
	fd := assertEligibility(t, root, rng, "gen/x.txt", "added", true, "text")
	if fd.Counterpart == nil || fd.Counterpart.Side != "left" || fd.Counterpart.Qualification != "outside-observation" {
		t.Errorf("gen/x.txt counterpart = %+v, want left outside-observation", fd.Counterpart)
	}
	code, human, stderr := awa(t, root, "diff", rng, "--", "gen/x.txt")
	if code != 0 || !strings.Contains(human, "diff gen/x.txt (left: outside observation)\n") ||
		!strings.Contains(human, "+generated\n") {
		t.Errorf("diff exit = %d stdout:\n%s\nstderr: %s", code, human, stderr)
	}
}

// TestRunOnUnreadSideSurfacesPromisedBlobDamage: once a checkpoint's bytes are
// eligible against a run, a missing or corrupt promised blob is storage corruption
// (exit 5) in human and JSON output, never a successful hash-only answer.
func TestRunOnUnreadSideSurfacesPromisedBlobDamage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func(*testing.T, string)
	}{
		{"missing", func(t *testing.T, root string) { removeAllBlobs(t, root) }},
		{"corrupt", corruptOneBlob},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := initProject(t)
			run := recordRun(t, root)
			write(t, root, "new.txt", "line one\n")
			cp := createCheckpoint(t, root)
			// new.txt's blob must be the only one, so damage always hits the promised bytes.
			if n := blobCount(t, root); n != 1 {
				t.Fatalf("blob count = %d, want only new.txt's", n)
			}
			tc.damage(t, root)

			for _, rng := range []string{"run:" + run + ":after.." + cp, cp + "..run:" + run + ":after"} {
				code, out, stderr := awa(t, root, "diff", rng)
				if code != 5 || strings.Contains(out, "hash-only") {
					t.Errorf("diff %s exit = %d stdout = %q stderr = %q, want storage corruption", rng, code, out, stderr)
				}
				code, out, _ = awa(t, root, "diff", "--json", rng)
				if code != 5 || strings.Contains(out, `"availability": "hash-only"`) {
					t.Errorf("diff --json %s exit = %d stdout = %q, want storage corruption", rng, code, out)
				}
				// A mid-stream failure leaves the envelope unterminated, so it never parses
				// as a completed machine document.
				if json.Valid([]byte(out)) {
					t.Errorf("diff --json %s produced a complete document over damaged storage:\n%s", rng, out)
				}
			}
		})
	}
}
