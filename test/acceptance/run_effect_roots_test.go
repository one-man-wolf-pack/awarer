package acceptance

import (
	"strings"
	"testing"

	"awarer/internal/domain/runcache"
)

// The fixture is what makes each outcome attributable: artifacts/ and other/ are
// excluded from the run input scan, and "bin" is not a built-in watched root, so the
// configured selector is the only thing that can observe any of these directories.
func TestRunExactEffectRootSelectsOneLocation(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	write(t, root, "awa.toml", "[run]\n"+
		"extra_excludes = [\"artifacts/\", \"other/\"]\n"+
		"extra_effect_roots = [\"artifacts/bin\"]\n")
	write(t, root, "src.txt", "source")
	write(t, root, "artifacts/bin/tool", "v1")
	write(t, root, "other/bin/tool", "v1")
	write(t, root, "other/artifacts/bin/tool", "v1")

	run := func(t *testing.T) string {
		t.Helper()
		code, _, stderr := awa(t, root, "run", "--", h, "-out", "ok")
		if code != 0 {
			t.Fatalf("run exit = %d, stderr = %q", code, stderr)
		}
		return stderr
	}

	if stderr := run(t); isHit(stderr) {
		t.Fatalf("first run should be a miss, stderr = %q", stderr)
	}
	if stderr := run(t); !isHit(stderr) {
		t.Fatalf("second run should be a hit, stderr = %q", stderr)
	}

	// The replacement differs in length, so the stat signature changes whatever the
	// filesystem's timestamp resolution is.
	write(t, root, "artifacts/bin/tool", "v2-rebuilt")
	missed := run(t)
	if isHit(missed) {
		t.Fatalf("changing the exactly selected effect root must miss, stderr = %q", missed)
	}
	if !strings.Contains(missed, string(runcache.ReasonEffectStateDiffers)) {
		t.Fatalf("the miss must be attributed to effect observation, stderr = %q", missed)
	}
	if stderr := run(t); !isHit(stderr) {
		t.Fatalf("the rerun should re-establish a hit, stderr = %q", stderr)
	}

	write(t, root, "other/bin/tool", "v2-rebuilt")
	if stderr := run(t); !isHit(stderr) {
		t.Fatalf("an exact selector must not watch other/bin by basename, stderr = %q", stderr)
	}

	write(t, root, "other/artifacts/bin/tool", "v2-rebuilt")
	if stderr := run(t); !isHit(stderr) {
		t.Fatalf("an exact selector must not watch other/artifacts/bin by suffix, stderr = %q", stderr)
	}
}
