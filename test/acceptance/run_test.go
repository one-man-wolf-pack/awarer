package acceptance

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// helperSrc is a tiny, dependency-free command used as the fixture for run-cache
// acceptance tests: it prints deterministic output, exits with a requested code,
// can read a file relative to its cwd, can emit a large blob for truncation, and can
// idempotently "format" a file to a target content (a fixer/formatter fixture).
const helperSrc = `package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

func main() {
	// If -echo-argv is present anywhere in argv, print it verbatim and exit before
	// flag.Parse — so the fixture can report child flags (e.g. --json) that Go's flag
	// package would otherwise reject. This proves awa forwarded them. os.Args is only
	// read here, so no copy is needed.
	raw := os.Args[1:]
	for _, a := range raw {
		if a == "-echo-argv" || a == "--echo-argv" {
			fmt.Println("ARGV\t" + strings.Join(raw, "\t"))
			return
		}
	}
	exit := flag.Int("exit", 0, "exit code")
	out := flag.String("out", "", "stdout text")
	errs := flag.String("err", "", "stderr text")
	cat := flag.String("cat", "", "file to read relative to cwd")
	big := flag.Int("big", 0, "emit N bytes to stdout")
	fix := flag.String("fix", "", "file to format idempotently to -content")
	content := flag.String("content", "", "target content for -fix")
	sleepMs := flag.Int("sleep", 0, "sleep this many milliseconds before exiting (a long-running child for cancellation tests)")
	flag.Parse()
	if *sleepMs > 0 {
		time.Sleep(time.Duration(*sleepMs) * time.Millisecond)
	}
	if *out != "" {
		fmt.Fprint(os.Stdout, *out)
	}
	if *errs != "" {
		fmt.Fprint(os.Stderr, *errs)
	}
	if *cat != "" {
		b, err := os.ReadFile(*cat)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cat error:", err)
			os.Exit(1)
		}
		os.Stdout.Write(b)
	}
	if *fix != "" {
		// Like a real idempotent formatter: only write when the file is not already at
		// the target content, so a no-op run leaves state (and mtime) unchanged.
		cur, _ := os.ReadFile(*fix)
		if string(cur) != *content {
			if err := os.WriteFile(*fix, []byte(*content), 0o644); err != nil {
				fmt.Fprintln(os.Stderr, "fix error:", err)
				os.Exit(1)
			}
		}
	}
	if *big > 0 {
		os.Stdout.Write([]byte(strings.Repeat("A", *big)))
	}
	os.Exit(*exit)
}
`

var (
	helperOnce sync.Once
	helperBin  string
	helperErr  error
)

// helper builds the fixture command once and returns its path.
func helper(t *testing.T) string {
	t.Helper()
	helperOnce.Do(func() {
		dir, err := os.MkdirTemp("", "awa-helper-*")
		if err != nil {
			helperErr = err
			return
		}
		src := filepath.Join(dir, "helper.go")
		if err := os.WriteFile(src, []byte(helperSrc), 0o644); err != nil {
			helperErr = err
			return
		}
		helperBin = filepath.Join(dir, "helper")
		build := exec.Command("go", "build", "-o", helperBin, src)
		build.Stderr = os.Stderr
		helperErr = build.Run()
	})
	if helperErr != nil {
		t.Fatalf("building helper: %v", helperErr)
	}
	return helperBin
}

func isHit(stderr string) bool { return strings.Contains(stderr, "awa run: hit,") }

func TestRunMissThenHit(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	write(t, root, "data.txt", "v1")

	code, stdout, stderr := awa(t, root, "run", "--", h, "-out", "hello")
	if code != 0 || stdout != "hello" {
		t.Fatalf("first run: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if isHit(stderr) {
		t.Errorf("first run should be a miss, stderr = %q", stderr)
	}

	code, stdout, stderr = awa(t, root, "run", "--", h, "-out", "hello")
	if code != 0 || stdout != "hello" {
		t.Fatalf("second run: exit %d stdout %q", code, stdout)
	}
	if !isHit(stderr) {
		t.Errorf("second run should be a hit, stderr = %q", stderr)
	}
}

func TestRunInputEditMisses(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	write(t, root, "data.txt", "one")

	_, stdout, _ := awa(t, root, "run", "--", h, "-cat", "data.txt")
	if stdout != "one" {
		t.Fatalf("stdout = %q, want one", stdout)
	}
	_, _, stderr := awa(t, root, "run", "--", h, "-cat", "data.txt")
	if !isHit(stderr) {
		t.Fatal("identical run should hit")
	}

	write(t, root, "data.txt", "two")
	_, stdout, stderr = awa(t, root, "run", "--", h, "-cat", "data.txt")
	if isHit(stderr) {
		t.Error("run after input edit should miss")
	}
	if stdout != "two" {
		t.Errorf("stdout = %q, want two", stdout)
	}
}

func TestRunCwd(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, root, "data.txt", "root-data")
	write(t, root, "sub/data.txt", "sub-data")

	// --cwd executes in the requested directory: it reads sub/data.txt.
	_, stdout, _ := awa(t, root, "run", "--cwd", "sub", "--", h, "-cat", "data.txt")
	if stdout != "sub-data" {
		t.Errorf("--cwd sub stdout = %q, want sub-data", stdout)
	}

	// The root cwd reads the root file and is a distinct key (a fresh miss).
	_, stdout, stderr := awa(t, root, "run", "--", h, "-cat", "data.txt")
	if stdout != "root-data" {
		t.Errorf("root cwd stdout = %q, want root-data", stdout)
	}
	if isHit(stderr) {
		t.Error("a different cwd must not hit the sub-cwd entry")
	}
}

func TestRunHitReplaysEachStream(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	// A miss produces both streams; the hit must replay each stream's bytes exactly
	// to the matching stream (per-stream content is the contract; cross-stream
	// interleaving is not preserved).
	awa(t, root, "run", "--", h, "-out", "the-output", "-err", "the-errors")
	code, stdout, stderr := awa(t, root, "run", "--", h, "-out", "the-output", "-err", "the-errors")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !isHit(stderr) {
		t.Fatal("second run should hit")
	}
	if stdout != "the-output" {
		t.Errorf("replayed stdout = %q, want the-output", stdout)
	}
	// The hit message goes to stderr too; the cached stderr bytes must be present.
	if !strings.Contains(stderr, "the-errors") {
		t.Errorf("replayed stderr = %q, want it to contain the-errors", stderr)
	}
}

// The agent workflow: run a noisy command with bounded terminal output, then inspect
// the durable full output through "run show" — no /tmp log wrapper required.
func TestRunDisplayBoundedOutput(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	noisy := "alpha\nbeta\ngamma\nPassed!\nDONE\n"

	// tail:2 prints only the last two lines to stdout; the full log is stored.
	code, stdout, stderr := awa(t, root, "run", "--display", "tail:2", "--", h, "-out", noisy)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if stdout != "Passed!\nDONE\n" {
		t.Errorf("tail stdout = %q, want last two lines", stdout)
	}

	// The full output is durable and inspectable.
	_, full, _ := awa(t, root, "run", "show", "--last", "--stdout")
	if full != noisy {
		t.Errorf("stored stdout = %q, want full noisy output", full)
	}

	// Summary lines are greppable from the stored output.
	_, grep, _ := awa(t, root, "run", "show", "--last", "--stdout", "--grep", "Passed!|Failed!")
	if grep != "Passed!\n" {
		t.Errorf("grep = %q, want Passed!", grep)
	}

	// A non-zero exit survives display filtering (none mode, distinct command).
	code, out, _ := awa(t, root, "run", "--display", "none", "--", h, "-out", "noise", "-exit", "5")
	if code != 5 {
		t.Errorf("none-mode exit = %d, want 5", code)
	}
	if out != "" {
		t.Errorf("none-mode stdout = %q, want empty", out)
	}
}

func TestRunCwdSymlinkOutsideRejected(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	// A directory outside the project, reached through a symlink inside it. Running
	// there would read unscanned outside data and cache a result that goes stale when
	// that data changes, so --cwd must reject the escape rather than follow it.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "data.txt"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	code, _, stderr := awa(t, root, "run", "--cwd", "link", "--", h, "-cat", "data.txt")
	if code == 0 {
		t.Errorf("--cwd through a symlink outside root should be rejected, exit = %d", code)
	}
	if !strings.Contains(stderr, "outside the project root") {
		t.Errorf("stderr = %q, want it to mention escaping the project root", stderr)
	}
}

func TestRunRefresh(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	awa(t, root, "run", "--", h, "-out", "x")
	if _, _, stderr := awa(t, root, "run", "--", h, "-out", "x"); !isHit(stderr) {
		t.Fatal("expected a hit before refresh")
	}
	if _, _, stderr := awa(t, root, "run", "--refresh", "--", h, "-out", "x"); isHit(stderr) {
		t.Error("--refresh must execute, not hit")
	}
	// After refresh, a normal run hits the refreshed entry.
	if _, _, stderr := awa(t, root, "run", "--", h, "-out", "x"); !isHit(stderr) {
		t.Error("normal run after refresh should hit")
	}
}

func TestRunNoCache(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	if _, _, stderr := awa(t, root, "run", "--no-cache", "--", h, "-out", "x"); isHit(stderr) {
		t.Error("--no-cache must execute")
	}
	// --no-cache did not write, so a normal run is still a miss.
	if _, _, stderr := awa(t, root, "run", "--", h, "-out", "x"); isHit(stderr) {
		t.Error("--no-cache must not create a hit")
	}
}

func TestRunFailedCachedByDefault(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	code, _, _ := awa(t, root, "run", "--", h, "-out", "boom", "-exit", "3")
	if code != 3 {
		t.Fatalf("exit = %d, want 3", code)
	}
	code, stdout, stderr := awa(t, root, "run", "--", h, "-out", "boom", "-exit", "3")
	if code != 3 {
		t.Errorf("cached failure exit = %d, want 3", code)
	}
	if !isHit(stderr) {
		t.Error("failed run should be cached by default")
	}
	if stdout != "boom" {
		t.Errorf("replayed stdout = %q, want boom", stdout)
	}
}

func TestRunNoCacheFailures(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	if code, _, _ := awa(t, root, "run", "--no-cache-failures", "--", h, "-exit", "4"); code != 4 {
		t.Fatalf("exit = %d, want 4", code)
	}
	if _, _, stderr := awa(t, root, "run", "--", h, "-exit", "4"); isHit(stderr) {
		t.Error("a failure suppressed by --no-cache-failures must not be cached")
	}
}

func TestRunJSON(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	code, stdout, _ := awa(t, root, "run", "--json", "--", h, "-out", "hi")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var env struct {
		SchemaVersion int    `json:"schema_version"`
		Command       string `json:"command"`
		Data          struct {
			Cache struct {
				Status string `json:"status"`
				Key    string `json:"key"`
			} `json:"cache"`
			Run struct {
				ExitCode int      `json:"exit_code"`
				Command  []string `json:"command"`
			} `json:"run"`
			Inputs struct {
				TreeHash string `json:"tree_hash"`
			} `json:"inputs"`
			Outputs struct {
				Stdout struct {
					CapturedBytes int64 `json:"captured_bytes"`
				} `json:"stdout"`
			} `json:"outputs"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	if env.SchemaVersion != 1 || env.Command != "run" {
		t.Errorf("envelope = %+v", env)
	}
	if env.Data.Cache.Status != "miss" {
		t.Errorf("cache status = %q, want miss", env.Data.Cache.Status)
	}
	if !strings.HasPrefix(env.Data.Cache.Key, "blake3:") {
		t.Errorf("key = %q, want blake3 prefix", env.Data.Cache.Key)
	}
	if env.Data.Run.ExitCode != 0 || env.Data.Outputs.Stdout.CapturedBytes != 2 {
		t.Errorf("run/outputs = %+v", env.Data)
	}
}

func TestRunJSONIDOnlyWhenStored(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	type runJSON struct {
		status  string
		id      string
		hitID   string
		written bool
		reason  string
	}
	decode := func(stdout string) runJSON {
		t.Helper()
		var env struct {
			Data struct {
				Cache struct {
					Status   string `json:"status"`
					Written  bool   `json:"written"`
					Reason   string `json:"reason"`
					HitRunID string `json:"hit_run_id"`
				} `json:"cache"`
				Run struct {
					ID string `json:"id"`
				} `json:"run"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(stdout), &env); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, stdout)
		}
		return runJSON{
			status:  env.Data.Cache.Status,
			id:      env.Data.Run.ID,
			hitID:   env.Data.Cache.HitRunID,
			written: env.Data.Cache.Written,
			reason:  env.Data.Cache.Reason,
		}
	}

	// A stored miss reports written=true, no reason, and an id "run show" can resolve.
	_, stdout, _ := awa(t, root, "run", "--json", "--", h, "-out", "x")
	miss := decode(stdout)
	if miss.status != "miss" || !miss.written || miss.id == "" {
		t.Fatalf("stored miss = %+v, want status=miss written=true with a resolvable id", miss)
	}
	if miss.reason != "" {
		t.Errorf("stored miss reason = %q, want empty", miss.reason)
	}
	if code, _, stderr := awa(t, root, "run", "show", miss.id[:12], "--stdout"); code != 0 {
		t.Errorf("run show on the reported id: exit %d, %s", code, stderr)
	}

	// A hit replays the stored entry: written=false (nothing new written), yet the
	// entry is resolvable, so id and hit_run_id are present and equal the miss's id.
	_, hitOut, _ := awa(t, root, "run", "--json", "--", h, "-out", "x")
	hit := decode(hitOut)
	if hit.status != "hit" || hit.written {
		t.Errorf("hit = %+v, want status=hit written=false", hit)
	}
	if hit.id != miss.id || hit.hitID != miss.id {
		t.Errorf("hit id=%q hit_run_id=%q, want both = %q", hit.id, hit.hitID, miss.id)
	}

	// --no-cache publishes no reusable pointer (written=false) but is now recorded as
	// durable history, so it has a resolvable id and a machine-readable reason. (The
	// run is record-only: inspectable, never a reusable hit.)
	_, ncOut, _ := awa(t, root, "run", "--no-cache", "--json", "--", h, "-out", "x")
	nc := decode(ncOut)
	if nc.written {
		t.Errorf("--no-cache run = %+v, want written=false", nc)
	}
	if nc.id == "" {
		t.Errorf("--no-cache run = %+v, want a resolvable recorded id", nc)
	}
	if nc.reason != "no-cache" {
		t.Errorf("--no-cache reason = %q, want no-cache", nc.reason)
	}
	if code, _, stderr := awa(t, root, "run", "show", nc.id[:12], "--meta"); code != 0 {
		t.Errorf("run show on the recorded --no-cache id: exit %d, %s", code, stderr)
	}
}

func TestRunLogShowRm(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	awa(t, root, "run", "--", h, "-out", "first")
	awa(t, root, "run", "--", h, "-out", "second")

	code, stdout, _ := awa(t, root, "run", "log")
	if code != 0 {
		t.Fatalf("run log exit %d", code)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("run log lines = %d, want 2\n%s", len(lines), stdout)
	}
	// Newest first: the "second" run leads.
	if !strings.Contains(lines[0], "second") {
		t.Errorf("first log line = %q, want newest (second)", lines[0])
	}

	// run log --json
	_, jsonOut, _ := awa(t, root, "run", "log", "--json")
	var env struct {
		Data []struct {
			ID      string   `json:"id"`
			Command []string `json:"command"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &env); err != nil {
		t.Fatalf("run log --json: %v\n%s", err, jsonOut)
	}
	if len(env.Data) != 2 {
		t.Fatalf("run log json entries = %d, want 2", len(env.Data))
	}
	id := env.Data[0].ID

	// run show --stdout returns the stored stdout of that run.
	_, showOut, _ := awa(t, root, "run", "show", id[:12], "--stdout")
	if showOut != "second" {
		t.Errorf("run show --stdout = %q, want second", showOut)
	}

	// run rm removes the run; a later identical run misses (its entry is gone).
	if code, _, stderr := awa(t, root, "run", "rm", id); code != 0 {
		t.Fatalf("run rm: exit %d, %s", code, stderr)
	}
	if _, _, stderr := awa(t, root, "run", "--", h, "-out", "second"); isHit(stderr) {
		t.Error("a run whose entry was removed must not hit")
	}
}

// TestRunShowMetadataOnlyPayloadHealth pins the split: metadata inspection is
// byte-free, so a missing payload does not make `run show` (default or --json) fail —
// it reports the stream's inspectability as "missing" and stays a clean success. Only
// an explicit output read (--stdout) opens and verifies the payload and fails loud.
func TestRunShowMetadataOnlyPayloadHealth(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	// Store a run and learn its id.
	_, stdout, _ := awa(t, root, "run", "--json", "--", h, "-out", "x")
	var env struct {
		Data struct {
			Run struct {
				ID string `json:"id"`
			} `json:"run"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	id := env.Data.Run.ID
	if id == "" {
		t.Fatal("no run id reported")
	}

	// Remove the stored stdout payload.
	payload := filepath.Join(root, ".awa", "runs", "entries", id[:2], id, "stdout.log")
	if err := os.Remove(payload); err != nil {
		t.Fatalf("remove payload: %v", err)
	}

	// Metadata inspection (default meta view) is byte-free and stays a clean success.
	if code, _, stderr := awa(t, root, "run", "show", id[:12], "--meta"); code != 0 {
		t.Errorf("run show --meta on a missing payload exit = %d, want 0 (metadata-only); stderr=%q", code, stderr)
	}

	// The JSON evidence document succeeds and reports the missing stream as inspectability
	// "missing" with unverified integrity — never claiming the bytes are intact.
	code, js, _ := awa(t, root, "run", "show", id[:12], "--json")
	if code != 0 {
		t.Fatalf("run show --json on a missing payload exit = %d, want 0 (metadata-only)", code)
	}
	var showEnv struct {
		Data struct {
			Output struct {
				Stdout struct {
					Inspectability struct {
						Presence  string `json:"presence"`
						Integrity string `json:"integrity"`
					} `json:"inspectability"`
				} `json:"stdout"`
			} `json:"output"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(js), &showEnv); err != nil {
		t.Fatalf("invalid run show JSON: %v\n%s", err, js)
	}
	if got := showEnv.Data.Output.Stdout.Inspectability.Presence; got != "missing" {
		t.Errorf("stdout inspectability presence = %q, want missing", got)
	}
	if got := showEnv.Data.Output.Stdout.Inspectability.Integrity; got != "unverified" {
		t.Errorf("stdout inspectability integrity = %q, want unverified", got)
	}

	// An explicit output read opens and verifies the payload, so it fails loud.
	if code, _, _ := awa(t, root, "run", "show", id[:12], "--stdout"); code != 5 {
		t.Errorf("run show --stdout on a missing payload exit = %d, want 5 (storage corruption)", code)
	}
}

func TestRunLogMarksCorruptEntry(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	_, stdout, _ := awa(t, root, "run", "--json", "--", h, "-out", "x")
	var env struct {
		Data struct {
			Run struct {
				ID string `json:"id"`
			} `json:"run"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	id := env.Data.Run.ID

	// Remove the stored stdout payload. run log must report the entry as not healthy
	// rather than presenting it as a replayable run.
	payload := filepath.Join(root, ".awa", "runs", "entries", id[:2], id, "stdout.log")
	if err := os.Remove(payload); err != nil {
		t.Fatalf("remove payload: %v", err)
	}

	_, logOut, _ := awa(t, root, "run", "log", "--json")
	var logEnv struct {
		Data []struct {
			ID      string `json:"id"`
			Healthy bool   `json:"healthy"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(logOut), &logEnv); err != nil {
		t.Fatalf("invalid run log JSON: %v\n%s", err, logOut)
	}
	if len(logEnv.Data) != 1 {
		t.Fatalf("run log entries = %d, want 1", len(logEnv.Data))
	}
	if logEnv.Data[0].Healthy {
		t.Error("run log should report a corrupt entry as not healthy")
	}

	// Human output flags it too.
	if _, humanOut, _ := awa(t, root, "run", "log"); !strings.Contains(humanOut, "[corrupt]") {
		t.Errorf("run log human output = %q, want a [corrupt] marker", humanOut)
	}
}

func TestRunRmCorruptMetadata(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	_, stdout, _ := awa(t, root, "run", "--json", "--", h, "-out", "x")
	var env struct {
		Data struct {
			Run struct {
				ID string `json:"id"`
			} `json:"run"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	id := env.Data.Run.ID

	// Corrupt the metadata so show/replay reject it; run rm <id> must still clean it
	// up by id rather than leaving it wedged.
	meta := filepath.Join(root, ".awa", "runs", "entries", id[:2], id, "meta.json")
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatalf("chmod meta: %v", err)
	}
	if err := os.WriteFile(meta, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("corrupt meta: %v", err)
	}
	if code, _, stderr := awa(t, root, "run", "rm", id[:12]); code != 0 {
		t.Fatalf("run rm of corrupt run: exit %d, %s", code, stderr)
	}
	// The run is gone: a later identical command misses.
	if _, _, stderr := awa(t, root, "run", "--", h, "-out", "x"); isHit(stderr) {
		t.Error("a removed corrupt run must not hit")
	}
}

func TestRunLogListsCorruptMetadata(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	awa(t, root, "run", "--", h, "-out", "healthy")
	_, stdout, _ := awa(t, root, "run", "--json", "--", h, "-out", "broken")
	var env struct {
		Data struct {
			Run struct {
				ID string `json:"id"`
			} `json:"run"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	badID := env.Data.Run.ID

	// Corrupt one entry's metadata. run log must still list both runs — the healthy
	// one normally and the broken one flagged corrupt — not fail the whole command.
	meta := filepath.Join(root, ".awa", "runs", "entries", badID[:2], badID, "meta.json")
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := os.WriteFile(meta, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("corrupt meta: %v", err)
	}

	code, logOut, stderr := awa(t, root, "run", "log", "--json")
	if code != 0 {
		t.Fatalf("run log exit = %d, want 0; stderr=%q", code, stderr)
	}
	var logEnv struct {
		Data []struct {
			ID      string `json:"id"`
			Corrupt bool   `json:"corrupt"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(logOut), &logEnv); err != nil {
		t.Fatalf("invalid run log JSON: %v\n%s", err, logOut)
	}
	if len(logEnv.Data) != 2 {
		t.Fatalf("run log entries = %d, want 2\n%s", len(logEnv.Data), logOut)
	}
	var sawCorrupt bool
	for _, e := range logEnv.Data {
		if e.Corrupt && e.ID == badID {
			sawCorrupt = true
		}
	}
	if !sawCorrupt {
		t.Errorf("run log did not flag the corrupt entry %s\n%s", badID, logOut)
	}
}

func TestRunShowRmMalformedIDIsUsageError(t *testing.T) {
	root := initProject(t)

	// A malformed run id reference (non-hex) is a user mistake, not a runtime or
	// storage failure, so it must exit with the usage code (2), not generic (1).
	for _, args := range [][]string{
		{"run", "show", "zzz"},
		{"run", "rm", "zzz"},
	} {
		if code, _, stderr := awa(t, root, args...); code != 2 {
			t.Errorf("awa %v exit = %d, want 2 (usage); stderr=%q", args, code, stderr)
		}
	}
}

func TestRunReservedSubcommandVsCommand(t *testing.T) {
	root := initProject(t)
	// "awa run log" is the subcommand: with no runs it prints the empty listing.
	code, stdout, _ := awa(t, root, "run", "log")
	if code != 0 || !strings.Contains(stdout, "no runs recorded") {
		t.Errorf("run log = exit %d stdout %q, want empty listing", code, stdout)
	}
	// "awa run -- log" is a command named "log"; with no such executable it fails
	// to start (generic error), proving it was not treated as the subcommand.
	code, _, stderr := awa(t, root, "run", "--", "log-does-not-exist-xyz")
	if code == 0 {
		t.Errorf("running a missing executable should fail, stderr = %q", stderr)
	}
}

// mutationWarned reports whether run's human output warned that the command changed
// observed state and was not cached for replay.
func mutationWarned(stderr string) bool {
	return strings.Contains(stderr, "changed observed state")
}

func TestRunLsShowsReusableEntry(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	write(t, root, "data.txt", "v1")

	// Cache a deterministic check.
	if code, _, _ := awa(t, root, "run", "--", h, "-out", "ok"); code != 0 {
		t.Fatalf("run exit = %d", code)
	}

	// run ls shows it as reusable for the current state.
	code, stdout, stderr := awa(t, root, "run", "ls")
	if code != 0 {
		t.Fatalf("run ls exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "reusable") || strings.Contains(stdout, "not-reusable") {
		t.Errorf("run ls did not list a reusable entry:\n%s", stdout)
	}

	// Editing a relevant file makes the entry non-reusable: it drops from the default
	// view but appears under --all with the input-tree-differs reason.
	write(t, root, "data.txt", "v2")
	if _, stdout, _ = awa(t, root, "run", "ls"); strings.Contains(stdout, h) {
		t.Errorf("run ls still lists a stale entry by default:\n%s", stdout)
	}
	code, stdout, _ = awa(t, root, "run", "ls", "--all")
	if code != 0 {
		t.Fatalf("run ls --all exit = %d", code)
	}
	if !strings.Contains(stdout, "not-reusable(input-tree-differs)") {
		t.Errorf("run ls --all did not show the stale reason:\n%s", stdout)
	}
}

func TestRunFixerMutationIsNotReusable(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	write(t, root, "code.txt", "unformatted")

	// A fixer that changes a file mutates observed state: the run warns and is not
	// cached, so the next identical invocation is not a hit.
	code, _, stderr := awa(t, root, "run", "--", h, "-fix", "code.txt", "-content", "formatted")
	if code != 0 {
		t.Fatalf("fixer run exit = %d, stderr = %q", code, stderr)
	}
	if !mutationWarned(stderr) {
		t.Errorf("mutating fixer run did not warn about observed state:\n%s", stderr)
	}
	if isHit(stderr) {
		t.Error("mutating run must not be a hit")
	}
	// The mutating run is now durable history: the default run ls (reusable-now view)
	// must not present it at all, but run ls --all surfaces it as not-reusable with the
	// mutated-state reason — history the user can see, never a reusable hit.
	if _, stdout, _ := awa(t, root, "run", "ls"); strings.Contains(stdout, "code.txt") {
		t.Errorf("default run ls surfaced a mutating attempt as reusable:\n%s", stdout)
	}
	if _, stdout, _ := awa(t, root, "run", "ls", "--all"); !strings.Contains(stdout, "not-reusable(mutated-state)") {
		t.Errorf("run ls --all must surface the mutating run as not-reusable(mutated-state):\n%s", stdout)
	}

	// Running the same fixer again is now a true no-op (file already formatted): state
	// is unchanged, so the result becomes cacheable, and a third run hits.
	code, _, stderr = awa(t, root, "run", "--", h, "-fix", "code.txt", "-content", "formatted")
	if code != 0 {
		t.Fatalf("no-op fixer run exit = %d", code)
	}
	if mutationWarned(stderr) {
		t.Errorf("no-op fixer run wrongly warned about mutation:\n%s", stderr)
	}
	if isHit(stderr) {
		t.Error("no-op fixer run should be a miss that stores a reusable entry")
	}
	_, _, stderr = awa(t, root, "run", "--", h, "-fix", "code.txt", "-content", "formatted")
	if !isHit(stderr) {
		t.Errorf("third identical no-op run should hit, stderr = %q", stderr)
	}
}

func TestRunMutationJSON(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	write(t, root, "code.txt", "raw")

	code, stdout, _ := awa(t, root, "run", "--json", "--", h, "-fix", "code.txt", "-content", "done")
	if code != 0 {
		t.Fatalf("run --json exit = %d\n%s", code, stdout)
	}
	var env struct {
		Data struct {
			Cache struct {
				Reason string `json:"reason"`
			} `json:"cache"`
			Run struct {
				Cacheable bool   `json:"cacheable"`
				Mutation  string `json:"mutation"`
			} `json:"run"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("run --json invalid: %v\n%s", err, stdout)
	}
	if env.Data.Run.Mutation != "changed" {
		t.Errorf("run.mutation = %q, want changed:\n%s", env.Data.Run.Mutation, stdout)
	}
	if env.Data.Run.Cacheable {
		t.Errorf("run.cacheable = true, want false:\n%s", stdout)
	}
	if env.Data.Cache.Reason != "mutated-state" {
		t.Errorf("cache.reason = %q, want mutated-state", env.Data.Cache.Reason)
	}
}

func TestRunLsJSON(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	write(t, root, "data.txt", "v1")
	if code, _, _ := awa(t, root, "run", "--", h, "-out", "ok"); code != 0 {
		t.Fatalf("run exit = %d", code)
	}

	code, stdout, _ := awa(t, root, "run", "ls", "--json")
	if code != 0 {
		t.Fatalf("run ls --json exit = %d\n%s", code, stdout)
	}
	// Plain and --near are one object family under data, distinguished by near; both
	// carry reusable/near_misses/corrupt and an always-present diagnostics.performance.
	var env struct {
		Command string `json:"command"`
		Data    struct {
			Near     bool `json:"near"`
			Reusable []struct {
				ID       string   `json:"id"`
				Command  []string `json:"command"`
				CWD      string   `json:"cwd"`
				Reusable bool     `json:"reusable"`
				Reason   string   `json:"reason"`
			} `json:"reusable"`
			NearMisses  []json.RawMessage `json:"near_misses"`
			Corrupt     int               `json:"corrupt"`
			Diagnostics struct {
				Performance []json.RawMessage `json:"performance"`
			} `json:"diagnostics"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("run ls --json invalid: %v\n%s", err, stdout)
	}
	if env.Command != "run.ls" {
		t.Errorf("envelope command = %q, want run.ls", env.Command)
	}
	if env.Data.Near {
		t.Errorf("plain run ls near = true, want false:\n%s", stdout)
	}
	if env.Data.Diagnostics.Performance == nil {
		t.Errorf("diagnostics.performance must be a present array:\n%s", stdout)
	}
	if len(env.Data.Reusable) != 1 {
		t.Fatalf("run ls --json reusable has %d entries, want 1:\n%s", len(env.Data.Reusable), stdout)
	}
	if !env.Data.Reusable[0].Reusable {
		t.Errorf("entry reusable = false, want true:\n%s", stdout)
	}
	if len(env.Data.Reusable[0].Command) == 0 {
		t.Errorf("entry command empty; ls must show exact replay guidance:\n%s", stdout)
	}
}

func TestRunLsJSONCorrupt(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	write(t, root, "data.txt", "v1")
	if code, _, _ := awa(t, root, "run", "--", h, "-out", "ok"); code != 0 {
		t.Fatalf("run exit = %d", code)
	}

	// Corrupt the stored metadata on disk: persisted metadata is hostile input, so
	// run ls --all must report it through its stable JSON shape (corrupt, non-reusable,
	// reason "corrupt") rather than crash or read it as reusable.
	metas, err := filepath.Glob(filepath.Join(root, ".awa", "runs", "entries", "*", "*", "meta.json"))
	if err != nil || len(metas) != 1 {
		t.Fatalf("found %d meta.json files (err %v), want 1", len(metas), err)
	}
	if err := os.Chmod(metas[0], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metas[0], []byte("{ not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, _ := awa(t, root, "run", "ls", "--all", "--json")
	if code != 0 {
		t.Fatalf("run ls --all --json exit = %d\n%s", code, stdout)
	}
	// The corrupt row is carried in the object family's reusable section under --all,
	// with its stable corrupt/non-reusable/reason fields.
	var env struct {
		Data struct {
			Reusable []struct {
				ID       string `json:"id"`
				Reusable bool   `json:"reusable"`
				Reason   string `json:"reason"`
				Corrupt  bool   `json:"corrupt"`
			} `json:"reusable"`
			Corrupt int `json:"corrupt"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("run ls --all --json invalid: %v\n%s", err, stdout)
	}
	if len(env.Data.Reusable) != 1 {
		t.Fatalf("run ls --all --json reusable has %d entries, want 1:\n%s", len(env.Data.Reusable), stdout)
	}
	got := env.Data.Reusable[0]
	if !got.Corrupt || got.Reusable || got.Reason != "corrupt" {
		t.Errorf("corrupt entry JSON = %+v, want corrupt=true reusable=false reason=corrupt", got)
	}
}

// The end-to-end contract: an awa-global flag after the wrapped command in short
// form is an ambiguity error (exit 2), while the explicit "--" form forwards every
// child flag verbatim, proving awa does not steal them.
func TestRunChildFlagBoundary(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	// Short form with a colliding flag after the child: loud ambiguity error.
	code, stdout, stderr := awa(t, root, "run", h, "--json")
	if code != 2 {
		t.Fatalf("run <child> --json exit = %d, want 2; stderr=%q", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "ambiguous flag") || !strings.Contains(stderr, "--") {
		t.Errorf("stderr = %q, want an ambiguity error pointing at --", stderr)
	}

	// Explicit -- form: --json belongs to the child and reaches it verbatim.
	code, stdout, stderr = awa(t, root, "run", "--", h, "-echo-argv", "--json")
	if code != 0 {
		t.Fatalf("run -- <child> -echo-argv --json exit = %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "ARGV\t") {
		t.Fatalf("child did not echo its argv:\n%s", stdout)
	}
	if !strings.Contains(stdout, "--json") {
		t.Errorf("child argv did not include --json (it was stolen):\n%s", stdout)
	}
}

// run --json exposes a stable exit_origin fact so a consumer can classify a
// wrapped-command result without guessing from $?. It is "child" on both a miss and
// a hit replay, and the child's exit code is preserved.
func TestRunJSONExitOrigin(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	decodeRun := func(stdout string) (int, string) {
		t.Helper()
		var env struct {
			Data struct {
				Run struct {
					ExitCode   int    `json:"exit_code"`
					ExitOrigin string `json:"exit_origin"`
				} `json:"run"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(stdout), &env); err != nil {
			t.Fatalf("invalid run --json: %v\n%s", err, stdout)
		}
		return env.Data.Run.ExitCode, env.Data.Run.ExitOrigin
	}

	// Miss: the child exits 3 (overlapping awa's NotFound code); exit_origin marks it
	// as the child's result, and the code passes through.
	_, stdout, _ := awa(t, root, "run", "--json", "--", h, "-out", "x", "-exit", "3")
	if ec, origin := decodeRun(stdout); ec != 3 || origin != "child" {
		t.Errorf("miss: exit_code=%d exit_origin=%q, want 3/child\n%s", ec, origin, stdout)
	}

	// Hit replay of the same command: still child-origin.
	_, stdout, _ = awa(t, root, "run", "--json", "--", h, "-out", "x", "-exit", "3")
	if ec, origin := decodeRun(stdout); ec != 3 || origin != "child" {
		t.Errorf("hit: exit_code=%d exit_origin=%q, want 3/child\n%s", ec, origin, stdout)
	}
}

// TestRunJSONValidOnNonZeroChildExit pins the consumer contract's first half: a non-zero
// *wrapped* command is not a JSON protocol failure. The awa process exits non-zero (the
// child's code), yet stdout is still one complete, valid JSON document marked child-origin
// — so a consumer must never treat "the child failed" as "there is no JSON".
func TestRunJSONValidOnNonZeroChildExit(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	code, stdout, _ := awa(t, root, "run", "--json", "--", h, "-out", "x", "-exit", "1")
	if code != 1 {
		t.Errorf("awa exit = %d, want the child's 1", code)
	}
	if !json.Valid([]byte(stdout)) {
		t.Fatalf("stdout is not valid JSON on a non-zero child exit:\n%s", stdout)
	}
	var env struct {
		Data struct {
			Run struct {
				ExitCode   int    `json:"exit_code"`
				ExitOrigin string `json:"exit_origin"`
			} `json:"run"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("run --json: %v\n%s", err, stdout)
	}
	if env.Data.Run.ExitCode != 1 || env.Data.Run.ExitOrigin != "child" {
		t.Errorf("exit_code=%d exit_origin=%q, want 1/child", env.Data.Run.ExitCode, env.Data.Run.ExitOrigin)
	}
}
