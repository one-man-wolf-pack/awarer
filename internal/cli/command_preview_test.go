package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	apprun "awarer/internal/app/run"
	"awarer/internal/app/timeline"
	domainconfig "awarer/internal/domain/config"
	"awarer/internal/domain/runcache"
	"awarer/internal/output"
)

// bidiControls lists every explicit Unicode bidirectional control: the Arabic letter
// mark, the LRM/RLM marks, the embeddings, overrides and their pop, and the isolates.
const bidiControls = "\u061c\u200e\u200f\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069"

// checkPreviewLine asserts what every preview must hold, whatever its input: at most
// the limit in runes, one line, no raw control or bidi control character, valid UTF-8.
func checkPreviewLine(t *testing.T, name, got string) {
	t.Helper()
	if n := utf8.RuneCountInString(got); n > commandPreviewLimit {
		t.Errorf("%s: %d runes, want at most %d: %q", name, n, commandPreviewLimit, got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("%s: invalid UTF-8: %q", name, got)
	}
	for _, r := range got {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' || strings.ContainsRune(bidiControls, r) {
			t.Errorf("%s: raw control, line separator or bidi control %U in %q", name, r, got)
		}
	}
}

func TestCommandPreview(t *testing.T) {
	a := strings.Repeat
	const L = commandPreviewLimit
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"short command unchanged", []string{"go", "test", "./..."}, "go test ./..."},
		{"empty", nil, ""},
		{"exactly the limit", []string{a("a", L)}, a("a", L)},
		{"limit across arguments", []string{a("a", L/2), a("b", L-L/2-1)}, a("a", L/2) + " " + a("b", L-L/2-1)},
		{"one over the limit", []string{a("a", L+1)}, a("a", L-1) + "…"},
		{"oversized argument", []string{"echo", a("z", 10_000) + "TAIL"}, "echo " + a("z", L-6) + "…"},
		{"separator at the edge", []string{a("a", L-1), "b"}, a("a", L-1) + "…"},
		{"multiline", []string{"sh", "-c", "echo hi\nrm x"}, `sh -c echo hi\nrm x`},
		{"tab and carriage return", []string{"a\tb\rc"}, `a\tb\rc`},
		{"terminal escape", []string{"printf", "\x1b[2J\x1b]0;title\x07"}, `printf \x1b[2J\x1b]0;title\x07`},
		{"delete and C1", []string{"x\x7fy\u0085z\u009b"}, `x\x7fy\u0085z\u009b`},
		{"unicode line separators", []string{"a\u2028b\u2029c"}, `a\u2028b\u2029c`},
		{"invalid UTF-8", []string{"a\xffb"}, `a\xffb`},
		{"multibyte unchanged", []string{"echo", "привет", "🙂"}, "echo привет 🙂"},
		{"multibyte at the edge", []string{a("я", L+1)}, a("я", L-1) + "…"},
		{"emoji at the edge", []string{a("🙂", L)}, a("🙂", L)},
		{"escape is not split at the edge", []string{a("a", L-2) + "\nb"}, a("a", L-2) + "…"},
		{"escape fits exactly", []string{a("a", L-2) + "\n"}, a("a", L-2) + `\n`},
		{"override", []string{"echo", "abc\u202edef\u202c"}, `echo abc\u202edef\u202c`},
		{"embeddings", []string{"\u202aa\u202bb\u202dc"}, `\u202aa\u202bb\u202dc`},
		{"isolates", []string{"x\u2066l\u2069\u2067r\u2069\u2068f\u2069"}, `x\u2066l\u2069\u2067r\u2069\u2068f\u2069`},
		{"marks", []string{"a\u200eb\u200fc\u061cd"}, `a\u200eb\u200fc\u061cd`},
		{"zero-width joiner unchanged", []string{"echo", "👩\u200d💻", "a\u200cb"}, "echo 👩\u200d💻 a\u200cb"},
		{"right-to-left text unchanged", []string{"echo", "שלום", "مرحبا"}, "echo שלום مرحبا"},
		// Each escape is six runes, so expansion still stops inside the cap.
		{"bidi escapes obey the cap", []string{a("\u202e", L)}, a(`\u202e`, (L-1)/6) + "…"},
		{"bidi escape is not split at the edge", []string{a("a", L-3) + "\u2066"}, a("a", L-3) + "…"},
	} {
		got := commandPreview(tc.argv)
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
		checkPreviewLine(t, tc.name, got)
	}
}

func TestCommandPreviewManyArguments(t *testing.T) {
	argv := make([]string, 0, 1000)
	for range 1000 {
		argv = append(argv, "arg")
	}
	argv = append(argv, "LAST")
	got := commandPreview(argv)
	checkPreviewLine(t, "many arguments", got)
	if !strings.HasPrefix(got, "arg arg ") || !strings.HasSuffix(got, "…") || strings.Contains(got, "LAST") {
		t.Errorf("many arguments: got %q", got)
	}
}

// longArgv is a multiline agent-style command whose unique tail lies past any preview.
func longArgv() []string {
	return []string{"agent", "--prompt", "line one\nline two\n" + strings.Repeat("word ", 40) + "UNIQUE_TAIL"}
}

// checkPreviewRows asserts a rendered report kept each command inside its preview: no
// longArgv tail, no line started by the command's own line breaks, no raw bidi control,
// and each wanted fact (escaped command text, trailing markers) still on the row.
func checkPreviewRows(t *testing.T, name, got string, wants ...string) {
	t.Helper()
	if strings.Contains(got, "UNIQUE_TAIL") {
		t.Errorf("%s: the preview reached the command tail:\n%s", name, got)
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "line two") {
			t.Errorf("%s: argv started a report row:\n%s", name, got)
		}
	}
	if strings.ContainsAny(got, bidiControls) {
		t.Errorf("%s: a raw bidi control reached the report: %q", name, got)
	}
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("%s: missing %q:\n%s", name, want, got)
		}
	}
}

func TestRunListingsBoundCommandPreview(t *testing.T) {
	rid, err := runcache.ParseRunID("18e32e3d74d80be05b5a6ee1e5a03d80")
	if err != nil {
		t.Fatal(err)
	}
	e := runcache.RunEntry{ID: rid, StartedAt: time.Unix(1_800_000_000, 0), FinishedAt: time.Unix(1_800_000_001, 0)}
	e.KeyInput.Command.Argv = longArgv()
	e.Stdout.Truncated = true
	notReusable, _ := runcache.NotReusable(runcache.ReasonInputTreeDiffers)
	reusableRow := apprun.LsEntry{ID: rid, Entry: e, Reusability: runcache.Reusable()}
	staleRow := apprun.LsEntry{ID: rid, Entry: e, Reusability: notReusable}
	near := apprun.RunReuseCandidate{Entry: e, Reason: runcache.ReasonInputTreeDiffers}

	render := func(res apprun.LsResult, all, near bool) string {
		var out, errb bytes.Buffer
		renderLsHuman(output.New(&out, &errb), res, all, near, "", domainconfig.TimeUTC, time.Unix(1_800_000_100, 0))
		return out.String()
	}
	// A captured stream over its limit keeps its own [truncated] marker, separate from
	// the preview's elision.
	checkPreviewRows(t, "plain", render(apprun.LsResult{Reusable: []apprun.LsEntry{reusableRow}}, false, false),
		"…  [truncated]  reusable\n")
	checkPreviewRows(t, "all", render(apprun.LsResult{Reusable: []apprun.LsEntry{staleRow}}, true, false),
		"…  [truncated]  not-reusable(input-tree-differs)\n")
	checkPreviewRows(t, "near", render(apprun.LsResult{NearMisses: []apprun.RunReuseCandidate{near}}, false, true),
		"…  not-reusable(input-tree-differs)\n")
	checkPreviewRows(t, "unassessed", render(apprun.LsResult{Unassessed: []apprun.LsEntry{{ID: rid, Entry: e}}}, false, false),
		"…  [truncated]  unassessed("+apprun.ReasonExternalFactsUnacquired+")\n")

	mutated, err := runcache.NewMutationStatus(runcache.MutationChanged)
	if err != nil {
		t.Fatal(err)
	}
	logged := e
	logged.Mutation = mutated
	logged.Reuse = runcache.ReusableCacheEntry()
	line := logLine(apprun.LogEntry{ID: rid, Entry: logged, Healthy: true}, domainconfig.TimeUTC, time.Unix(1_800_000_100, 0))
	checkPreviewRows(t, "run log", line, "18e32e3d74d8  ", "… [mutated] [truncated]")
	if strings.Contains(line, "\n") {
		t.Errorf("run log: row spans lines: %q", line)
	}
}

func TestTimelineAndStatusBoundCommandPreview(t *testing.T) {
	rid, err := runcache.ParseRunID("18e32e3d74d80be05b5a6ee1e5a03d80")
	if err != nil {
		t.Fatal(err)
	}
	mutated, err := runcache.NewMutationStatus(runcache.MutationChanged)
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	renderTimelineHuman(output.New(&out, &errb), timeline.Result{Entries: []timeline.TimelineEntry{{
		Kind: timeline.EntryRun,
		Ref:  "run:18e32e3d74d8",
		At:   time.Unix(1_800_000_000, 0),
		Run:  &timeline.RunSummary{ID: rid, Command: longArgv(), DurationMs: 1200, Reuse: runcache.ReusableCacheEntry(), Mutation: mutated},
	}}}, domainconfig.TimeUTC, time.Unix(1_800_000_100, 0))
	got := out.String()
	checkPreviewRows(t, "log --all", got, "run:18e32e3d74d8  ", "…  ", "[mutated]\n")
	if n := strings.Count(got, "\n"); n != 1 {
		t.Errorf("log --all: %d lines, want one:\n%s", n, got)
	}

	out.Reset()
	renderStatusNearest(output.New(&out, &errb), reviewRunsDoc{Nearest: &nearMissDoc{
		ShortID: "18e32e3d74d8", Command: longArgv(), Reason: string(runcache.ReasonInputTreeDiffers),
	}}, "nearest:")
	got = out.String()
	checkPreviewRows(t, "status", got, "nearest: 18e32e3d74d8 agent --prompt line one\\nline two\\n", "… not-reusable(input-tree-differs)\n")
	if n := strings.Count(got, "\n"); n != 1 {
		t.Errorf("status: %d lines, want one:\n%s", n, got)
	}
}

// TestRenderersEscapeUnclosedBidiControls feeds each scoped renderer a command ending in
// an unclosed override or isolate, the case where a raw control would reorder the row's
// trailing facts. The control must arrive escaped, right before those facts unchanged.
func TestRenderersEscapeUnclosedBidiControls(t *testing.T) {
	rid, err := runcache.ParseRunID("18e32e3d74d80be05b5a6ee1e5a03d80")
	if err != nil {
		t.Fatal(err)
	}
	mutated, err := runcache.NewMutationStatus(runcache.MutationChanged)
	if err != nil {
		t.Fatal(err)
	}
	notReusable, _ := runcache.NotReusable(runcache.ReasonInputTreeDiffers)
	now := time.Unix(1_800_000_100, 0)
	for _, tc := range []struct {
		name, arg, shown string
	}{
		{"override", "txt.exe\u202egpj", `txt.exe\u202egpj`},
		{"isolate", "\u2067שלום", `\u2067שלום`},
	} {
		e := runcache.RunEntry{ID: rid, StartedAt: time.Unix(1_800_000_000, 0), FinishedAt: time.Unix(1_800_000_001, 0)}
		e.KeyInput.Command.Argv = []string{"open", tc.arg}
		e.Stdout.Truncated = true
		e.Mutation = mutated
		e.Reuse = runcache.ReusableCacheEntry()
		cmd := "open " + tc.shown

		render := func(res apprun.LsResult, all, near bool) string {
			var out, errb bytes.Buffer
			renderLsHuman(output.New(&out, &errb), res, all, near, "", domainconfig.TimeUTC, now)
			return out.String()
		}
		checkPreviewRows(t, tc.name+" plain", render(apprun.LsResult{Reusable: []apprun.LsEntry{{ID: rid, Entry: e, Reusability: runcache.Reusable()}}}, false, false),
			cmd+"  [truncated]  reusable\n")
		checkPreviewRows(t, tc.name+" all", render(apprun.LsResult{Reusable: []apprun.LsEntry{{ID: rid, Entry: e, Reusability: notReusable}}}, true, false),
			cmd+"  [truncated]  not-reusable(input-tree-differs)\n")
		checkPreviewRows(t, tc.name+" near", render(apprun.LsResult{NearMisses: []apprun.RunReuseCandidate{{Entry: e, Reason: runcache.ReasonInputTreeDiffers}}}, false, true),
			cmd+"  not-reusable(input-tree-differs)\n")
		checkPreviewRows(t, tc.name+" unassessed", render(apprun.LsResult{Unassessed: []apprun.LsEntry{{ID: rid, Entry: e}}}, false, false),
			cmd+"  [truncated]  unassessed("+apprun.ReasonExternalFactsUnacquired+")\n")
		checkPreviewRows(t, tc.name+" run log", logLine(apprun.LogEntry{ID: rid, Entry: e, Healthy: true}, domainconfig.TimeUTC, now),
			cmd+" [mutated] [truncated]")

		var out, errb bytes.Buffer
		renderTimelineHuman(output.New(&out, &errb), timeline.Result{Entries: []timeline.TimelineEntry{{
			Kind: timeline.EntryRun, Ref: "run:18e32e3d74d8", At: e.StartedAt,
			Run: &timeline.RunSummary{ID: rid, Command: e.KeyInput.Command.Argv, DurationMs: 1200, Reuse: e.Reuse, Mutation: mutated},
		}}}, domainconfig.TimeUTC, now)
		checkPreviewRows(t, tc.name+" log --all", out.String(), cmd+"  exit=", "[mutated]\n")

		out.Reset()
		renderStatusNearest(output.New(&out, &errb), reviewRunsDoc{Nearest: &nearMissDoc{
			ShortID: "18e32e3d74d8", Command: e.KeyInput.Command.Argv, Reason: string(runcache.ReasonInputTreeDiffers),
		}}, "nearest:")
		checkPreviewRows(t, tc.name+" status", out.String(), "nearest: 18e32e3d74d8 "+cmd+" not-reusable(input-tree-differs)\n")
	}
}
