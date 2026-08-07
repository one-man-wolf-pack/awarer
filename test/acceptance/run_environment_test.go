package acceptance

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// localeReaderSrc models a locale-sensitive reader — the class of program a dropped
// locale breaks: a Ruby Markdown check that decodes UTF-8 when run directly and
// US-ASCII when run under `awa run` with no locale passed through.
//
// It has to model the behavior rather than exhibit it, because Go is locale-blind: a Go
// program's decoding does not depend on LANG at all, so a naive helper would pass
// whether or not awa preserved the locale and would prove nothing. So the helper reads
// the locale variables itself, applies POSIX precedence (LC_ALL beats the category,
// which beats LANG), derives an external encoding the way a locale-aware runtime would,
// and then fails on non-ASCII input exactly as such a runtime does. Keeping the rule
// here, in the fixture, is also what keeps the test's oracle independent of awa's
// implementation: the fixture never asks awa what the locale is.
//
// -touch is a side effect deliberately placed outside the project, so a test can prove a
// cache hit executed nothing without the write itself making the run non-reusable.
const localeReaderSrc = `package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// externalEncoding derives the character encoding a locale-aware runtime would use for
// reading files, following POSIX: LC_ALL overrides everything, then the specific
// category (LC_CTYPE), then LANG. An unset or empty selection means the C/POSIX locale,
// whose charset is US-ASCII. LANGUAGE is deliberately ignored here: it selects message
// language, never the charset.
func externalEncoding() string {
	for _, name := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		v, ok := os.LookupEnv(name)
		if !ok || v == "" {
			continue
		}
		if _, codeset, found := strings.Cut(v, "."); found && codeset != "" {
			return strings.ToUpper(codeset)
		}
		return "US-ASCII"
	}
	return "US-ASCII"
}

func main() {
	read := flag.String("read", "", "file to read under the effective locale")
	touch := flag.String("touch", "", "file to create, as an observable side effect")
	dumpEnv := flag.Bool("dump-env", false, "print the received environment, one NAME=VALUE per line")
	// require compares one received variable against the content of a file, and reports
	// only whether they matched. It exists so a test can prove the child received a
	// secret-shaped value without that value passing through argv or captured output,
	// both of which awa stores verbatim by design.
	requireName := flag.String("require", "", "environment variable to verify")
	requireFile := flag.String("require-file", "", "file holding the expected value of -require")
	flag.Parse()

	if *requireName != "" {
		want, err := os.ReadFile(*requireFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "require-file:", err)
			os.Exit(1)
		}
		if os.Getenv(*requireName) != string(want) {
			fmt.Fprintln(os.Stderr, "MISMATCH: "+*requireName+" is not the expected value")
			os.Exit(1)
		}
		fmt.Println("MATCH: " + *requireName)
	}

	if *touch != "" {
		if err := os.WriteFile(*touch, []byte("executed"), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "touch:", err)
			os.Exit(1)
		}
	}
	if *dumpEnv {
		for _, e := range os.Environ() {
			fmt.Println(e)
		}
	}
	if *read != "" {
		enc := externalEncoding()
		fmt.Println("external=" + enc)
		b, err := os.ReadFile(*read)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read:", err)
			os.Exit(1)
		}
		if enc != "UTF-8" {
			for _, c := range b {
				if c > 127 {
					fmt.Fprintf(os.Stderr, "%s: invalid byte sequence in %s (ArgumentError)\n", *read, enc)
					os.Exit(1)
				}
			}
		}
		fmt.Println("OK: read under " + enc)
	}
}
`

var (
	localeReaderOnce sync.Once
	localeReaderBin  string
	localeReaderErr  error
)

// localeReader builds the locale-sensitive fixture once and returns its path.
func localeReader(t *testing.T) string {
	t.Helper()
	localeReaderOnce.Do(func() {
		dir, err := os.MkdirTemp("", "awa-locale-reader-*")
		if err != nil {
			localeReaderErr = err
			return
		}
		src := filepath.Join(dir, "main.go")
		if err := os.WriteFile(src, []byte(localeReaderSrc), 0o644); err != nil {
			localeReaderErr = err
			return
		}
		localeReaderBin = filepath.Join(dir, "localereader")
		build := exec.Command("go", "build", "-o", localeReaderBin, src)
		build.Stderr = os.Stderr
		localeReaderErr = build.Run()
	})
	if localeReaderErr != nil {
		t.Fatalf("building locale reader: %v", localeReaderErr)
	}
	return localeReaderBin
}

// awaExactEnv runs the built binary with exactly env as its environment, returning exit
// code, stdout, and stderr.
//
// It is the piece awaEnv cannot provide: awaEnv appends to os.Environ(), so a variable
// the developer's own shell defines can never be made absent. Unset, empty, and set are
// three distinct states in the run key, and two of them are unreachable without full
// control of the child environment.
func awaExactEnv(t *testing.T, dir string, env []string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(awaBin, args...)
	cmd.Dir = dir
	cmd.Env = env
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running awa %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return code, out.String(), errBuf.String()
}

// minimalEnv is the smallest ambient environment awa itself needs, with every locale
// selector deliberately absent so a test states the locale facts it wants.
//
// The names carried through are the platform's own execution needs — PATH and HOME on a
// Unix host, plus SystemRoot, COMSPEC, and the rest on Windows, where a process given an
// environment without them can fail to start for reasons that have nothing to do with
// the property under test. Locale names are excluded by construction, which is the whole
// point: the caller's real locale must not leak into a test that says a variable is unset.
func minimalEnv() []string {
	var env []string
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "SystemRoot", "WINDIR", "COMSPEC", "PATHEXT", "TMP", "TEMP"} {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// childEnvFromDump parses the fixture's -dump-env output into name/value pairs.
func childEnvFromDump(stdout string) map[string]string {
	got := map[string]string{}
	for _, line := range strings.Split(stdout, "\n") {
		if line == "" {
			continue
		}
		name, value, found := strings.Cut(line, "=")
		if found {
			got[name] = value
		}
	}
	return got
}

// cyrillic is the non-ASCII fixture content: any byte above 127 reproduces the failure.
const cyrillic = "# Заголовок\n\nПроверка кириллицы.\n"

// The end-to-end regression proof. The same command, over the same file, must
// succeed under `awa run` exactly as it does when the caller runs it directly —
// because awa passes the caller's locale through rather than dropping it and leaving
// the child in the C locale.
func TestRunPreservesCallerLocale(t *testing.T) {
	root := initProject(t)
	reader := localeReader(t)
	write(t, root, "doc.md", cyrillic)

	utf8Env := append(minimalEnv(), "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8")
	code, stdout, stderr := awaExactEnv(t, root, utf8Env, "run", "--", reader, "-read", "doc.md")
	if code != 0 {
		t.Fatalf("run under a UTF-8 caller locale exit = %d, want 0\nstdout=%q\nstderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "external=UTF-8") {
		t.Errorf("child reported %q, want external=UTF-8: the caller's locale must reach the command", stdout)
	}

	// The negative half, so the assertion above is known to discriminate: a caller with
	// no locale at all genuinely fails on the same input.
	code2, stdout2, _ := awaExactEnv(t, root, minimalEnv(), "run", "--", reader, "-read", "doc.md")
	if code2 == 0 {
		t.Errorf("a caller with no locale should still fail on non-ASCII input; got exit 0, stdout=%q", stdout2)
	}
	if !strings.Contains(stdout2, "external=US-ASCII") {
		t.Errorf("child reported %q, want external=US-ASCII when the caller set no locale — awa must not invent one", stdout2)
	}
}

// TestRunPreservesLocalePresenceStates proves the three states survive the wrapper
// independently: an unset variable stays unset in the child (awa synthesizes nothing),
// an empty one stays present-and-empty, and a set one arrives byte for byte.
func TestRunPreservesLocalePresenceStates(t *testing.T) {
	root := initProject(t)
	reader := localeReader(t)

	_, stdout, stderr := awaExactEnv(t, root,
		append(minimalEnv(), "LANG=en_US.UTF-8", "LC_TIME="),
		"run", "--", reader, "-dump-env")
	child := childEnvFromDump(stdout)

	if got, ok := child["LANG"]; !ok || got != "en_US.UTF-8" {
		t.Errorf("child LANG = %q (present=%v), want en_US.UTF-8", got, ok)
	}
	if got, ok := child["LC_TIME"]; !ok || got != "" {
		t.Errorf("child LC_TIME = %q (present=%v), want present and empty — empty differs from absent", got, ok)
	}
	for _, absent := range []string{"LC_ALL", "LC_CTYPE", "LC_COLLATE", "LC_MESSAGES", "LC_MONETARY", "LC_NUMERIC", "LANGUAGE"} {
		if got, ok := child[absent]; ok {
			t.Errorf("child has %s=%q, but the caller did not set it; awa must never synthesize a locale (stderr=%q)", absent, got, stderr)
		}
	}
}

// TestRunInjectsTheWrapperMarker proves the child always receives exactly AWA_RUN=1,
// whatever the caller's environment claims under that name. The marker is awa's own
// statement about the execution, so an ambient value must not survive into it.
func TestRunInjectsTheWrapperMarker(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ambient []string
	}{
		{"absent", nil},
		{"empty", []string{"AWA_RUN="}},
		{"falsey", []string{"AWA_RUN=0"}},
		{"attacker-chosen", []string{"AWA_RUN=0 supervised=yes trusted"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := initProject(t)
			reader := localeReader(t)

			_, stdout, stderr := awaExactEnv(t, root, append(minimalEnv(), tc.ambient...),
				"run", "--", reader, "-dump-env")

			var markers []string
			for _, line := range strings.Split(stdout, "\n") {
				if strings.HasPrefix(line, "AWA_RUN=") {
					markers = append(markers, line)
				}
			}
			if want := []string{"AWA_RUN=1"}; !slices.Equal(markers, want) {
				t.Errorf("child marker lines = %v, want %v (stderr=%q)", markers, want, stderr)
			}
		})
	}
}

// TestRunMarkerIsRejectedByConfig proves the marker cannot be configured. A user who
// could allowlist it could redirect what awa says about its own execution.
func TestRunMarkerIsRejectedByConfig(t *testing.T) {
	root := initProject(t)
	write(t, root, filepath.Join(".awa", "config.toml"), "[run]\nenv_allowlist = [\"AWA_RUN\"]\n")

	code, _, stderr := awa(t, root, "config", "validate")
	if code == 0 {
		t.Fatalf("config listing AWA_RUN validated; the marker is reserved")
	}
	if !strings.Contains(stderr, "reserved by awa") {
		t.Errorf("validation error = %q, want it to name the reservation", stderr)
	}
}

// TestCacheHitExecutesNoChild proves a replay runs nothing. The fixture writes a marker
// file outside the project (so the write itself does not make the run non-mutating), the
// file is deleted, and the hit must not recreate it.
func TestCacheHitExecutesNoChild(t *testing.T) {
	root := initProject(t)
	reader := localeReader(t)
	// Outside the project root, so the side effect is invisible to the mutation guard
	// and the run stays reusable — the property under test is execution, not mutation.
	sideEffect := filepath.Join(t.TempDir(), "executed.txt")

	env := append(minimalEnv(), "LANG=en_US.UTF-8")
	if code, _, stderr := awaExactEnv(t, root, env, "run", "--", reader, "-touch", sideEffect); code != 0 {
		t.Fatalf("first run exit = %d, stderr = %q", code, stderr)
	}
	if _, err := os.Stat(sideEffect); err != nil {
		t.Fatalf("the first run did not execute the fixture: %v", err)
	}
	if err := os.Remove(sideEffect); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := awaExactEnv(t, root, env, "run", "--", reader, "-touch", sideEffect)
	if code != 0 {
		t.Fatalf("second run exit = %d, stderr = %q", code, stderr)
	}
	if !isHit(stderr) {
		t.Fatalf("second identical run did not hit; the rest of this test proves nothing (stderr=%q)", stderr)
	}
	if _, err := os.Stat(sideEffect); err == nil {
		t.Error("a cache hit executed the child: a replay must run nothing, and therefore inject nothing")
	}
}

// TestNestedRunGainsNoAuthority proves the marker is not a capability. awa never reads
// AWA_RUN, so an inner `awa run` behaves exactly as an outer one: it sanitizes the
// environment the same way and states the marker itself rather than passing through the
// one it was handed.
func TestNestedRunGainsNoAuthority(t *testing.T) {
	root := initProject(t)
	reader := localeReader(t)

	env := append(minimalEnv(), "LANG=en_US.UTF-8", "SECRET_OUTER=leak")
	_, stdout, stderr := awaExactEnv(t, root, env,
		"run", "--no-cache", "--", awaBin, "run", "--no-cache", "--", reader, "-dump-env")

	child := childEnvFromDump(stdout)
	if got, ok := child["AWA_RUN"]; !ok || got != "1" {
		t.Errorf("nested child AWA_RUN = %q (present=%v), want 1 (stderr=%q)", got, ok, stderr)
	}
	if got, ok := child["LANG"]; !ok || got != "en_US.UTF-8" {
		t.Errorf("nested child LANG = %q (present=%v), want the caller's locale to survive both wrappers", got, ok)
	}
	if _, ok := child["SECRET_OUTER"]; ok {
		t.Error("an unallowlisted variable reached the nested child: nesting must not widen the sanitized environment")
	}
}

// TestRunNeverPersistsRawLocaleValue extends the standing redaction proof to the newly
// inherited locale family, and to the rendered surfaces as well as the store: a value
// that identifies a machine or a user must not be recoverable from evidence or from
// ordinary diagnostic output.
func TestRunNeverPersistsRawLocaleValue(t *testing.T) {
	root := initProject(t)
	reader := localeReader(t)
	const sentinel = "xx_XX.SENTINEL-7d2c9e10"

	// The expected value lives outside the project and reaches the child only through
	// the environment. Neither argv nor the child's stdout carries it, because awa
	// stores both verbatim by design — so a hit on those would say nothing about the
	// redaction this test is about.
	expected := filepath.Join(t.TempDir(), "expected.txt")
	if err := os.WriteFile(expected, []byte(sentinel), 0o644); err != nil {
		t.Fatal(err)
	}

	env := append(minimalEnv(), "LC_CTYPE="+sentinel)
	code, stdout, stderr := awaExactEnv(t, root, env, "run", "--", reader, "-require", "LC_CTYPE", "-require-file", expected)
	if code != 0 {
		t.Fatalf("the child did not receive the raw locale value (exit %d); the redaction assertions below would prove nothing\nstdout=%q stderr=%q", code, stdout, stderr)
	}

	// The child saw it. Nothing durable may have.
	if hits := grepTree(t, filepath.Join(root, ".awa"), sentinel); len(hits) != 0 {
		t.Errorf("raw locale value found in durable evidence: %v", hits)
	}

	// Nor may any ordinary diagnostic surface render it. These are the commands a user
	// or agent runs to understand a miss, which is exactly when a leaked value would be
	// pasted into a bug report.
	for _, args := range [][]string{
		{"run", "explain", "--", reader, "-require", "LC_CTYPE", "-require-file", expected},
		{"run", "ls", "--near"},
		{"run", "show", "--last"},
		{"config", "effective"},
		{"config", "effective", "--json"},
	} {
		_, stdout, stderr := awaExactEnv(t, root, env, args...)
		if strings.Contains(stdout, sentinel) || strings.Contains(stderr, sentinel) {
			t.Errorf("`awa %s` rendered the raw locale value", strings.Join(args, " "))
		}
	}
}

// TestConfigEffectiveSeparatesInheritedFromInjected proves the projection tells the truth
// about origin: the locale names are inherited, the marker is injected, and the two are
// reported as different things so a user does not try to configure the one they cannot.
func TestConfigEffectiveSeparatesInheritedFromInjected(t *testing.T) {
	root := initProject(t)

	code, stdout, stderr := awa(t, root, "config", "effective")
	if code != 0 {
		t.Fatalf("config effective exit = %d, stderr = %q", code, stderr)
	}
	inherited := lineWithPrefix(stdout, "effective_env_allowlist = ")
	injected := lineWithPrefix(stdout, "injected_env = ")

	if inherited == "" || injected == "" {
		t.Fatalf("config effective must project both inherited names and injected facts; got:\n%s", stdout)
	}
	for _, name := range []string{"LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE", "LC_TIME"} {
		if !strings.Contains(inherited, `"`+name+`"`) {
			t.Errorf("effective_env_allowlist is missing %s: %s", name, inherited)
		}
	}
	if !strings.Contains(injected, `"AWA_RUN=1"`) {
		t.Errorf("injected_env = %s, want it to name AWA_RUN=1", injected)
	}
	if strings.Contains(inherited, "AWA_RUN") {
		t.Errorf("effective_env_allowlist claims the injected marker as an inherited name: %s", inherited)
	}
}

func lineWithPrefix(s, prefix string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}
