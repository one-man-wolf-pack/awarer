package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shAvailable reports whether /bin/sh exists; the display tests drive a real
// multi-line command through it.
func shAvailable(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires /bin/sh")
	}
}

// fiveLines prints line1..line5 to stdout and exits 0.
var fiveLines = []string{"/bin/sh", "-c", "for i in 1 2 3 4 5; do echo line$i; done"}

func runShow(root string, args ...string) (int, string, string) {
	return run(append([]string{"run", "show", "--root", root}, args...)...)
}

// TestRunDisplayTailMiss verifies tail mode prints only the bounded tail on a miss
// while the full payload is stored and inspectable.
func TestRunDisplayTailMiss(t *testing.T) {
	shAvailable(t)
	root := initProject(t)

	code, stdout, stderr := run(append([]string{"run", "--root", root, "--cwd", root, "--display", "tail:2", "--"}, fiveLines...)...)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if stdout != "line4\nline5\n" {
		t.Errorf("tail stdout = %q, want last two lines only", stdout)
	}
	if !strings.Contains(stderr, "last 2 line(s) of stdout") {
		t.Errorf("stderr missing tail header: %q", stderr)
	}
	if !strings.Contains(stderr, "inspect:  awa run show") {
		t.Errorf("stderr missing inspect footer: %q", stderr)
	}

	// The full payload was stored despite the bounded display.
	_, full, _ := runShow(root, "--last", "--stdout")
	if full != "line1\nline2\nline3\nline4\nline5\n" {
		t.Errorf("stored stdout = %q, want all five lines", full)
	}
}

// TestRunDisplayNoneMiss verifies none mode suppresses child output but stores it.
func TestRunDisplayNoneMiss(t *testing.T) {
	shAvailable(t)
	root := initProject(t)

	code, stdout, stderr := run(append([]string{"run", "--root", root, "--cwd", root, "--display", "none", "--"}, fiveLines...)...)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if stdout != "" {
		t.Errorf("none stdout = %q, want empty", stdout)
	}
	_, full, _ := runShow(root, "--last", "--stdout")
	if full != "line1\nline2\nline3\nline4\nline5\n" {
		t.Errorf("stored stdout = %q, want all five lines", full)
	}
}

// TestRunDisplayFullDefault verifies the default mode prints all output to stdout.
func TestRunDisplayFullDefault(t *testing.T) {
	shAvailable(t)
	root := initProject(t)

	code, stdout, _ := run(append([]string{"run", "--root", root, "--cwd", root, "--"}, fiveLines...)...)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if stdout != "line1\nline2\nline3\nline4\nline5\n" {
		t.Errorf("full stdout = %q, want all five lines", stdout)
	}
}

// TestRunDisplayExitCodePreserved verifies every display mode returns the wrapped
// command's exit code, including a non-zero one.
func TestRunDisplayExitCodePreserved(t *testing.T) {
	shAvailable(t)
	root := initProject(t)
	for _, mode := range []string{"full", "summary", "tail:3", "none"} {
		// Vary the command per mode so each is a fresh miss (distinct cache key).
		cmd := []string{"/bin/sh", "-c", "echo " + mode + "; exit 7"}
		code, _, stderr := run(append([]string{"run", "--root", root, "--cwd", root, "--display", mode, "--"}, cmd...)...)
		if code != 7 {
			t.Errorf("mode %s: exit = %d, want 7\nstderr: %q", mode, code, stderr)
		}
	}
}

// TestRunDisplayHitObeysMode verifies a cache hit honors the requested display mode
// without altering stored payloads.
func TestRunDisplayHitObeysMode(t *testing.T) {
	shAvailable(t)
	root := initProject(t)

	// Miss with full display.
	if code, _, stderr := run(append([]string{"run", "--root", root, "--cwd", root, "--"}, fiveLines...)...); code != 0 {
		t.Fatalf("miss exit = %d, stderr = %q", code, stderr)
	}
	// Hit with tail:1 must replay only the last line from the store.
	code, stdout, stderr := run(append([]string{"run", "--root", root, "--cwd", root, "--display", "tail:1", "--"}, fiveLines...)...)
	if code != 0 {
		t.Fatalf("hit exit = %d", code)
	}
	if !strings.Contains(stderr, "awa run: hit,") {
		t.Fatalf("expected a hit, stderr = %q", stderr)
	}
	if stdout != "line5\n" {
		t.Errorf("hit tail stdout = %q, want line5 only", stdout)
	}
}

// TestRunShowLastTailAndGrep verifies run show --last with --tail and --grep.
func TestRunShowLastTailAndGrep(t *testing.T) {
	shAvailable(t)
	root := initProject(t)
	if code, _, stderr := run(append([]string{"run", "--root", root, "--cwd", root, "--"}, fiveLines...)...); code != 0 {
		t.Fatalf("seed run exit = %d, stderr = %q", code, stderr)
	}

	// --last --tail over both streams labels the stdout section.
	code, stdout, _ := runShow(root, "--last", "--tail", "2")
	if code != 0 {
		t.Fatalf("show tail exit = %d", code)
	}
	if !strings.Contains(stdout, "== stdout ==") || !strings.Contains(stdout, "line4") || strings.Contains(stdout, "line1") {
		t.Errorf("show --last --tail 2 = %q", stdout)
	}

	// --last --stdout --tail keeps only the selected stream, raw.
	_, raw, _ := runShow(root, "--last", "--stdout", "--tail", "2")
	if raw != "line4\nline5\n" {
		t.Errorf("show --stdout --tail 2 = %q", raw)
	}

	// --grep filters matching lines.
	_, grepOut, _ := runShow(root, "--last", "--stdout", "--grep", "line3")
	if grepOut != "line3\n" {
		t.Errorf("show --grep line3 = %q", grepOut)
	}

	// No-match grep is a successful empty inspection.
	code, noMatch, _ := runShow(root, "--last", "--stdout", "--grep", "nope")
	if code != 0 || noMatch != "" {
		t.Errorf("no-match grep: code=%d out=%q, want 0 and empty", code, noMatch)
	}
}

// TestRunShowLastNoRun verifies --last with no stored run is a not-found error.
func TestRunShowLastNoRun(t *testing.T) {
	root := initProject(t)
	code, _, stderr := runShow(root, "--last")
	if code != int(ExitNotFound) {
		t.Errorf("exit = %d, want not-found (%d)\nstderr: %q", code, ExitNotFound, stderr)
	}
}

// TestRunShowLastIsMetadataOnly pins the contract: "run show --last" selects
// the newest run with valid *metadata* — it does not eagerly verify payload bytes. A
// tampered payload on the newest run therefore does not exclude it: the metadata/JSON
// view still succeeds (byte-free), while an explicit output read (--stdout) performs
// the integrity check and fails loud.
func TestRunShowLastIsMetadataOnly(t *testing.T) {
	if _, err := os.Stat("/bin/echo"); err != nil {
		t.Skip("requires /bin/echo")
	}
	root := initProject(t)
	// Two distinct commands → two stored runs; the second is the newest.
	if code, _, stderr := run("run", "--root", root, "--cwd", root, "--", "/bin/echo", "older"); code != 0 {
		t.Fatalf("older run exit = %d, stderr = %q", code, stderr)
	}
	if code, _, stderr := run("run", "--root", root, "--cwd", root, "--", "/bin/echo", "newest"); code != 0 {
		t.Fatalf("newest run exit = %d, stderr = %q", code, stderr)
	}

	// Capture the newest id, then tamper with its stored stdout so an explicit read
	// would fail verification.
	_, js, _ := runShow(root, "--last", "--json")
	var doc struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
	}
	decodeEnvelope(t, js, &doc)
	newestID := doc.Run.ID
	payload := filepath.Join(root, ".awa", "runs", "entries", newestID[:2], newestID, "stdout.log")
	if err := os.Chmod(payload, 0o644); err != nil {
		t.Fatalf("chmod payload: %v", err)
	}
	if err := os.WriteFile(payload, []byte("tampered, wrong size"), 0o644); err != nil {
		t.Fatalf("tamper payload: %v", err)
	}

	// --last --json still resolves the newest run (metadata-only) and does not read the
	// tampered payload, so it stays a clean success.
	code, js2, stderr := runShow(root, "--last", "--json")
	if code != 0 {
		t.Fatalf("--last --json exit = %d, stderr = %q", code, stderr)
	}
	decodeEnvelope(t, js2, &doc)
	if doc.Run.ID != newestID {
		t.Errorf("--last resolved id = %q, want the newest %q (metadata-only, not skipped)", doc.Run.ID, newestID)
	}

	// An explicit output read verifies the same descriptor and fails loud on the tamper.
	code, _, _ = runShow(root, "--last", "--stdout")
	if code == 0 {
		t.Fatalf("--last --stdout over a tampered payload must fail loud, got exit 0")
	}
}

// decodeEnvelope unmarshals a --json envelope's data field into v.
func decodeEnvelope(t *testing.T, stdout string, v any) {
	t.Helper()
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decoding envelope: %v\nstdout: %s", err, stdout)
	}
	if err := json.Unmarshal(env.Data, v); err != nil {
		t.Fatalf("decoding data: %v\ndata: %s", err, env.Data)
	}
}

// TestRunJSONDisplayBlock verifies the run JSON envelope reports the requested display
// mode, and that hidden stays true even when that mode is full: --json writes the
// machine document and no child bytes, so a full request never means output was shown.
// Decoding the whole of stdout as one envelope is what proves no child bytes escaped.
func TestRunJSONDisplayBlock(t *testing.T) {
	shAvailable(t)
	root := initProject(t)
	var doc struct {
		Display struct {
			Mode      string `json:"mode"`
			TailLines int    `json:"tail_lines"`
			Hidden    bool   `json:"hidden"`
		} `json:"display"`
	}

	_, stdout, _ := run(append([]string{"run", "--root", root, "--cwd", root, "--json", "--display", "tail:3", "--"}, fiveLines...)...)
	decodeEnvelope(t, stdout, &doc)
	if doc.Display.Mode != "tail" || doc.Display.TailLines != 3 || !doc.Display.Hidden {
		t.Errorf("display block = %+v, want tail/3/hidden", doc.Display)
	}

	doc.Display.Mode, doc.Display.TailLines, doc.Display.Hidden = "", 0, false
	_, stdout, _ = run(append([]string{"run", "--root", root, "--cwd", root, "--json", "--display", "full", "--"}, fiveLines...)...)
	decodeEnvelope(t, stdout, &doc)
	if doc.Display.Mode != "full" || doc.Display.TailLines != 0 || !doc.Display.Hidden {
		t.Errorf("display block = %+v, want full/0/hidden", doc.Display)
	}
}

// TestRunShowJSONSamples verifies run show --json --tail emits bounded samples.
func TestRunShowJSONSamples(t *testing.T) {
	shAvailable(t)
	root := initProject(t)
	if code, _, stderr := run(append([]string{"run", "--root", root, "--cwd", root, "--"}, fiveLines...)...); code != 0 {
		t.Fatalf("seed exit = %d, stderr = %q", code, stderr)
	}
	_, stdout, _ := runShow(root, "--last", "--json", "--tail", "2")
	var doc struct {
		Samples struct {
			Filter    string   `json:"filter"`
			TailLines int      `json:"tail_lines"`
			Stdout    []string `json:"stdout"`
		} `json:"samples"`
	}
	decodeEnvelope(t, stdout, &doc)
	if doc.Samples.Filter != "tail" || doc.Samples.TailLines != 2 {
		t.Errorf("samples meta = %+v, want tail/2", doc.Samples)
	}
	if len(doc.Samples.Stdout) != 2 || doc.Samples.Stdout[0] != "line4" || doc.Samples.Stdout[1] != "line5" {
		t.Errorf("samples stdout = %v, want [line4 line5]", doc.Samples.Stdout)
	}
}

// TestRunShowJSONSamplesSingleStream verifies --json --stderr --grep samples only
// the selected stream, leaving the other empty.
func TestRunShowJSONSamplesSingleStream(t *testing.T) {
	shAvailable(t)
	root := initProject(t)
	cmd := []string{"/bin/sh", "-c", "echo out1; echo err1 1>&2"}
	if code, _, stderr := run(append([]string{"run", "--root", root, "--cwd", root, "--"}, cmd...)...); code != 0 {
		t.Fatalf("seed exit = %d, stderr = %q", code, stderr)
	}
	_, stdout, _ := runShow(root, "--last", "--json", "--stderr", "--grep", "err")
	var doc struct {
		Samples struct {
			Filter string   `json:"filter"`
			Stdout []string `json:"stdout"`
			Stderr []string `json:"stderr"`
		} `json:"samples"`
	}
	decodeEnvelope(t, stdout, &doc)
	if doc.Samples.Filter != "grep" {
		t.Errorf("filter = %q, want grep", doc.Samples.Filter)
	}
	if len(doc.Samples.Stdout) != 0 {
		t.Errorf("stdout samples = %v, want empty (stderr selected)", doc.Samples.Stdout)
	}
	if len(doc.Samples.Stderr) != 1 || doc.Samples.Stderr[0] != "err1" {
		t.Errorf("stderr samples = %v, want [err1]", doc.Samples.Stderr)
	}
}

// TestRunLsJSONOneObjectFamily pins the run.ls JSON shape: plain "run ls --json" and
// "run ls --near --json" are one object family distinguished only by the near marker,
// each carrying reusable/near_misses/corrupt and an always-present diagnostics block. A
// consumer branches on the near field, never on the document's top-level type, and
// machine-readable performance facts always live under data.diagnostics.performance.
func TestRunLsJSONOneObjectFamily(t *testing.T) {
	shAvailable(t)
	root := initProject(t)
	if code, _, stderr := run(append([]string{"run", "--root", root, "--cwd", root, "--"}, fiveLines...)...); code != 0 {
		t.Fatalf("seed run exit = %d, stderr = %q", code, stderr)
	}

	type lsData struct {
		Near        bool              `json:"near"`
		Reusable    []json.RawMessage `json:"reusable"`
		NearMisses  []json.RawMessage `json:"near_misses"`
		Corrupt     int               `json:"corrupt"`
		Diagnostics struct {
			Performance []json.RawMessage `json:"performance"`
		} `json:"diagnostics"`
	}
	decode := func(t *testing.T, out string) lsData {
		t.Helper()
		var env struct {
			Command string `json:"command"`
			Data    lsData `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("invalid run ls JSON: %v\n%s", err, out)
		}
		if env.Command != "run.ls" {
			t.Errorf("command = %q, want run.ls", env.Command)
		}
		// The diagnostics block and its performance array are always present (stable
		// shape), even when nothing crossed the interactive latency threshold.
		if env.Data.Diagnostics.Performance == nil {
			t.Errorf("diagnostics.performance must be a present array, got null:\n%s", out)
		}
		if env.Data.NearMisses == nil {
			t.Errorf("near_misses must be a present array, got null:\n%s", out)
		}
		return env.Data
	}

	// Plain: near=false, the seeded run is reusable now, no near misses.
	_, plainOut, plainErr := run("run", "ls", "--root", root, "--json")
	plain := decode(t, plainOut)
	if plain.Near {
		t.Errorf("plain near = true, want false\n%s", plainOut)
	}
	if len(plain.Reusable) != 1 {
		t.Errorf("plain reusable = %d, want 1\n%s", len(plain.Reusable), plainOut)
	}
	if len(plain.NearMisses) != 0 {
		t.Errorf("plain near_misses = %d, want 0\n%s", len(plain.NearMisses), plainOut)
	}
	if strings.Contains(plainErr, "{") {
		t.Errorf("plain stderr contains JSON, want a single stdout document: %q", plainErr)
	}

	// --near: near=true, same reusable-now set plus the near-miss section.
	_, nearOut, nearErr := run("run", "ls", "--near", "--root", root, "--json")
	near := decode(t, nearOut)
	if !near.Near {
		t.Errorf("near = false, want true\n%s", nearOut)
	}
	if len(near.Reusable) != 1 {
		t.Errorf("near reusable = %d, want 1\n%s", len(near.Reusable), nearOut)
	}
	if strings.Contains(nearErr, "{") {
		t.Errorf("near stderr contains JSON, want a single stdout document: %q", nearErr)
	}
}
