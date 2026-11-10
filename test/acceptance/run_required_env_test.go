package acceptance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// envProbeSrc is the scratch child for command environment requirements. It reports
// whether one variable reached it as unset, empty, or a value whose SHA-256 equals or
// differs from an expected digest, and never prints the value itself; only the digest
// is on its command line (argv is stored verbatim), so the test proves exact
// transmission without putting the value into captured output. -touch writes a file
// outside the project, so a test can tell an executed child from a refusal or a replay
// without the write making the run non-reusable.
const envProbeSrc = `package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
)

func main() {
	name := flag.String("name", "", "")
	want := flag.String("want", "", "")
	touch := flag.String("touch", "", "")
	flag.Parse()
	if *touch != "" {
		_ = os.WriteFile(*touch, []byte("x"), 0o644)
	}
	v, ok := os.LookupEnv(*name)
	switch {
	case !ok:
		fmt.Println(*name + ": unset")
	case v == "":
		fmt.Println(*name + ": empty")
	case func() bool { d := sha256.Sum256([]byte(v)); return hex.EncodeToString(d[:]) == *want }():
		fmt.Println(*name + ": matches")
	default:
		fmt.Println(*name + ": differs")
	}
}
`

var (
	envProbeOnce sync.Once
	envProbeBin  string
	envProbeErr  error
)

func envProbe(t *testing.T) string {
	t.Helper()
	envProbeOnce.Do(func() {
		dir, err := os.MkdirTemp(buildDir, "env-probe-*")
		if err != nil {
			envProbeErr = err
			return
		}
		src := filepath.Join(dir, "main.go")
		if err := os.WriteFile(src, []byte(envProbeSrc), 0o644); err != nil {
			envProbeErr = err
			return
		}
		envProbeBin = filepath.Join(dir, "envprobe")
		build := exec.Command("go", "build", "-o", envProbeBin, src)
		build.Stderr = os.Stderr
		envProbeErr = build.Run()
	})
	if envProbeErr != nil {
		t.Fatalf("building env probe: %v", envProbeErr)
	}
	return envProbeBin
}

const (
	reqName  = "AWA_ACCEPT_PKG_DIR"
	reqValue = "/accept/secret-pkg-dir-7f3a"
)

func reqValueDigest() string {
	d := sha256.Sum256([]byte(reqValue))
	return hex.EncodeToString(d[:])
}

// requiredEnvFixture returns the probe argv for the declared invocation, the
// side-effect marker it touches, and the [[run.required_env]] declaration selecting
// exactly that argv in the project root.
func requiredEnvFixture(t *testing.T) (argv []string, marker, decl string) {
	t.Helper()
	marker = filepath.Join(t.TempDir(), "executed")
	argv = []string{envProbe(t), "-name", reqName, "-want", reqValueDigest(), "-touch", marker}
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = strconv.Quote(a)
	}
	decl = "[[run.required_env]]\nargv = [" + strings.Join(quoted, ", ") + "]\ncwd = \".\"\nnames = [\"" + reqName + "\"]\n"
	return argv, marker, decl
}

func runLogCount(t *testing.T, root string, env []string) int {
	t.Helper()
	code, stdout, stderr := awaExactEnv(t, root, env, "run", "log", "--json")
	if code != 0 {
		t.Fatalf("run log = %d: %s", code, stderr)
	}
	var doc struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("run log JSON: %v\n%s", err, stdout)
	}
	return len(doc.Data)
}

func executed(t *testing.T, marker string) bool {
	t.Helper()
	_, err := os.Stat(marker)
	if err == nil {
		if rerr := os.Remove(marker); rerr != nil {
			t.Fatal(rerr)
		}
		return true
	}
	return false
}

// TestRequiredEnvTransmitsOnlyTheDeclaredInvocation proves the wrapper boundary: an
// undeclared variable is filtered as before, the exact declared invocation receives the
// exact nonempty value without an allowlist entry, a different argv does not, and the
// value appears nowhere in captured output or awa's own evidence.
func TestRequiredEnvTransmitsOnlyTheDeclaredInvocation(t *testing.T) {
	root := initProject(t)
	argv, marker, decl := requiredEnvFixture(t)
	env := append(minimalEnv(), reqName+"="+reqValue)
	run := func(args ...string) string {
		t.Helper()
		code, stdout, stderr := awaExactEnv(t, root, env, append([]string{"run", "--display", "full", "--"}, args...)...)
		if code != 0 {
			t.Fatalf("run %q = %d: %s", args, code, stderr)
		}
		return stdout
	}

	if out := run(argv...); !strings.Contains(out, reqName+": unset") {
		t.Fatalf("undeclared variable reached the child: %q", out)
	}
	write(t, root, "awa.toml", decl)
	if out := run(argv...); !strings.Contains(out, reqName+": matches") {
		t.Fatalf("declared invocation did not receive the exact value: %q", out)
	}
	if !executed(t, marker) {
		t.Fatal("declared run did not execute")
	}
	if out := run(append(argv, "extra")...); !strings.Contains(out, reqName+": unset") {
		t.Fatalf("a different argv received the variable: %q", out)
	}

	if hits := grepTree(t, filepath.Join(root, ".awa"), reqValue); len(hits) != 0 {
		t.Fatalf("raw value persisted in %v", hits)
	}
	code, stdout, _ := awaExactEnv(t, root, env, "run", "show", "--last", "--json")
	if code != 0 || strings.Contains(stdout, reqValue) {
		t.Fatalf("run show leaked the value or failed (%d)", code)
	}
}

// TestRequiredEnvRefusesBeforeAnyExecutionOrRecord proves the preflight on the real
// binary: unset and empty values exit 1 in every run mode, the child never executes,
// nothing is recorded, an earlier reusable result is not replayed, and explain reports
// the refusal successfully without the value.
func TestRequiredEnvRefusesBeforeAnyExecutionOrRecord(t *testing.T) {
	root := initProject(t)
	argv, marker, decl := requiredEnvFixture(t)
	write(t, filepath.Join(root, ".awa"), "config.toml", decl) // local one-off declaration

	satisfied := append(minimalEnv(), reqName+"="+reqValue)
	if code, _, stderr := awaExactEnv(t, root, satisfied, append([]string{"run", "--"}, argv...)...); code != 0 {
		t.Fatalf("satisfied run = %d: %s", code, stderr)
	}
	executed(t, marker)
	if code, _, stderr := awaExactEnv(t, root, satisfied, append([]string{"run", "--"}, argv...)...); code != 0 || !isHit(stderr) {
		t.Fatalf("second satisfied run = %d, want a hit: %s", code, stderr)
	}
	before := runLogCount(t, root, satisfied)

	for _, state := range []struct {
		name string
		env  []string
		want string
	}{
		{"unset", minimalEnv(), reqName + " is unset"},
		{"empty", append(minimalEnv(), reqName+"="), reqName + " is empty"},
	} {
		for _, mode := range [][]string{nil, {"--record"}, {"--no-cache"}, {"--refresh"}, {"--json"}} {
			args := append(append([]string{"run"}, mode...), "--")
			code, stdout, stderr := awaExactEnv(t, root, state.env, append(args, argv...)...)
			if code != 1 {
				t.Fatalf("%s %v exit = %d, want awa failure 1: %s", state.name, mode, code, stderr)
			}
			if stdout != "" || !strings.Contains(stderr, "awa: run refused before execution") || !strings.Contains(stderr, state.want) {
				t.Fatalf("%s %v stdout=%q stderr=%q", state.name, mode, stdout, stderr)
			}
			if executed(t, marker) {
				t.Fatalf("%s %v executed the child", state.name, mode)
			}
		}
		if n := runLogCount(t, root, state.env); n != before {
			t.Fatalf("%s refusal recorded runs: %d -> %d", state.name, before, n)
		}

		code, stdout, stderr := awaExactEnv(t, root, state.env, append([]string{"run", "explain", "--json", "--"}, argv...)...)
		if code != 0 || !strings.Contains(stdout, `"outcome": "refused"`) || !strings.Contains(stdout, `"reason": "env-requirement-unmet"`) || !strings.Contains(stdout, `"origin": "local"`) {
			t.Fatalf("%s explain = %d %s\n%s", state.name, code, stderr, stdout)
		}
		code, stdout, _ = awaExactEnv(t, root, state.env, "run", "explain", "--last", "--json")
		if code != 0 || !strings.Contains(stdout, `"outcome": "refused"`) {
			t.Fatalf("%s stored explain = %d\n%s", state.name, code, stdout)
		}
		code, stdout, _ = awaExactEnv(t, root, state.env, "run", "show", "--last", "--json")
		if code != 0 || !strings.Contains(stdout, `"id"`) {
			t.Fatalf("%s: retained run no longer readable (%d)", state.name, code)
		}
	}
}

// TestOptionalAllowlistKeepsUnsetAndEmptySemantics proves the predecessor optional
// transmission is unchanged: an allowlisted empty or unset variable is passed as is and
// the run proceeds.
func TestOptionalAllowlistKeepsUnsetAndEmptySemantics(t *testing.T) {
	root := initProject(t)
	argv, _, _ := requiredEnvFixture(t)
	write(t, root, "awa.toml", "[run]\nenv_allowlist = [\""+reqName+"\"]\n")
	for _, tc := range []struct {
		env  []string
		want string
	}{
		{minimalEnv(), ": unset"},
		{append(minimalEnv(), reqName+"="), ": empty"},
		{append(minimalEnv(), reqName+"="+reqValue), ": matches"},
	} {
		code, stdout, stderr := awaExactEnv(t, root, tc.env, append([]string{"run", "--display", "full", "--"}, argv...)...)
		if code != 0 || !strings.Contains(stdout, reqName+tc.want) {
			t.Fatalf("optional allowlist = %d %q %s", code, stdout, stderr)
		}
	}
}
