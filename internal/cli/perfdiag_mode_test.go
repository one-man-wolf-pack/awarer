package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"awarer/internal/app/effectobserve"
	"awarer/internal/app/initcmd"
	apprun "awarer/internal/app/run"
	"awarer/internal/app/scanner"
	"awarer/internal/domain/config"
	"awarer/internal/domain/runcache"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/effectfs"
	"awarer/internal/infra/process"
	"awarer/internal/infra/projfs"
	"awarer/internal/infra/runstore"
	"awarer/internal/infra/worktreefs"
	"awarer/internal/output"
	"awarer/internal/output/inspect"
)

// runOverBudgetEffectRoot executes "true" in a fresh project whose watched target/ root
// exceeds a tiny per-root observation budget, under the given execution policy. The
// tiny budget is the deterministic stand-in for a huge generated tree: no real delay and
// no million-file fixture.
func runOverBudgetEffectRoot(t *testing.T, policy apprun.Policy) apprun.Result {
	t.Helper()
	root := t.TempDir()
	if _, err := initcmd.Run(initcmd.Request{Root: root, Profile: initcmd.ProfileDefault}); err != nil {
		t.Fatalf("init: %v", err)
	}
	proj, err := projfs.Open(root)
	if err != nil {
		t.Fatalf("open project: %v", err)
	}
	layout, err := proj.Paths()
	if err != nil {
		t.Fatalf("paths: %v", err)
	}
	dir := filepath.Join(root, "target")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		if err := os.WriteFile(filepath.Join(dir, "f"+strconv.Itoa(i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const perRoot = 8
	hasher := blake3hash.New()
	proc := process.New()
	svc := apprun.New(apprun.Deps{
		Scanner:        scanner.New(worktreefs.New(), hasher, nil),
		Store:          runstore.New(layout, hasher),
		Runner:         proc,
		Prober:         proc,
		Resolver:       proc,
		EffectObserver: effectobserve.New(effectfs.NewWithLimits(hasher, perRoot, perRoot*100, 2_000_000), hasher),
		Env:            osEnv{},
		Clock:          realClock{},
		Hasher:         hasher,
		AwaVersion:     "test",
	})
	cwd, err := runcache.NewExecutionCWD(".")
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Run(t.Context(), apprun.Request{
		Project:   proj,
		Config:    config.Defaults(),
		Argv:      []string{"true"},
		CWD:       cwd,
		AbsDir:    root,
		StdinMode: runcache.StdinNull,
		Policy:    policy,
		Stdout:    io.Discard,
		Stderr:    io.Discard,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res
}

// TestRecordModeHintFollowsTheSelectedRequestThroughBothProjections is the
// mode-to-projection pair: the same over-budget root, once as an ordinary run and once
// as an explicit --record run, rendered through the human footer and the run JSON.
// Both keep the identical measured fact; only the explicit record run loses the advice
// to select record mode, in both projections, because both read one decision.
func TestRecordModeHintFollowsTheSelectedRequestThroughBothProjections(t *testing.T) {
	type projected struct {
		stderr string
		doc    perfDiagDoc
		raw    []byte
	}
	project := func(policy apprun.Policy) projected {
		res := runOverBudgetEffectRoot(t, policy)
		var out, errb bytes.Buffer
		emitRunFooter(output.New(&out, &errb), res, apprun.RunInlineDiagnosis{})
		if out.Len() != 0 {
			t.Errorf("footer wrote to stdout: %q", out.String())
		}
		view := runView(res, inspect.DefaultDisplayMode(), apprun.RunInlineDiagnosis{})
		if view.Diagnostics == nil || len(view.Diagnostics.Performance) != 1 {
			t.Fatalf("record=%v: run JSON carries %+v, want one performance diagnostic", policy.Record, view.Diagnostics)
		}
		doc := view.Diagnostics.Performance[0]
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return projected{stderr: errb.String(), doc: doc, raw: raw}
	}
	plain := project(apprun.Policy{})
	record := project(apprun.Policy{Record: true})

	// The measured fact is identical in both projections. Duration is a real
	// measurement and differs between two executions; it is present, not compared.
	for name, p := range map[string]projected{"plain": plain, "record": record} {
		d := p.doc
		if d.Code != "large-effect-root" || d.Component != "run.effect-observation" {
			t.Errorf("%s: code/component = %s/%s", name, d.Code, d.Component)
		}
		if d.Evidence.Path != "target" || !d.Evidence.ThresholdCrossed || d.Evidence.Threshold == nil || *d.Evidence.Threshold != 8 || d.Evidence.FileCount != nil {
			t.Errorf("%s: evidence = %+v, want target threshold-crossed at 8, no count", name, d.Evidence)
		}
		if !bytes.Contains(p.raw, []byte(`"duration_ms":`)) {
			t.Errorf("%s: JSON lost duration_ms: %s", name, p.raw)
		}
		if !strings.Contains(p.stderr, `note: run state observation took `) || !strings.Contains(p.stderr, `effect root "target" exceeds 8 files`) {
			t.Errorf("%s: human note lost the measured fact:\n%s", name, p.stderr)
		}
		if !strings.Contains(p.stderr, "review [run] effect roots") {
			t.Errorf("%s: human output lost the effect-root review guidance:\n%s", name, p.stderr)
		}
	}

	// Only the advice to select record mode differs.
	if plain.doc.Hint == nil || plain.doc.Hint.Kind != "record-mode" || !strings.Contains(plain.stderr, "hint: use `awa run --record`") {
		t.Errorf("ordinary run lost its record-mode advice: hint=%+v\n%s", plain.doc.Hint, plain.stderr)
	}
	if record.doc.Hint != nil || bytes.Contains(record.raw, []byte(`"hint"`)) {
		t.Errorf("explicit record JSON still carries a hint: %s", record.raw)
	}
	if strings.Contains(record.stderr, "--record") {
		t.Errorf("explicit record human output still advises --record:\n%s", record.stderr)
	}
}
