package cli

import (
	"context"
	"time"

	apprun "awarer/internal/app/run"
	apprunevidence "awarer/internal/app/runevidence"
	"awarer/internal/app/stateprovider"
	domainconfig "awarer/internal/domain/config"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/runevidence"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/runstore"
)

// buildRunShowContext resolves the project store-only — discovering the .awa/ store
// without loading or validating current project config — and wires both the run
// service (for verified explicit stream reads and bounded JSON samples) and the
// run-evidence builder (for the awa-evidence/v1 metadata document). run show is
// durable-evidence archaeology: a malformed awa.toml/config.toml must not block it,
// and it never scans the current worktree, so it uses default config for the store
// hasher only. It deliberately does not call loadProjectConfig; leaving that seam out
// is what keeps run show inspectable under a broken config.
func buildRunShowContext(ctx context.Context, opts GlobalOptions) (*apprun.Service, *apprunevidence.Service, func(), error) {
	proj, err := resolveProject(opts)
	if err != nil {
		return nil, nil, nil, err
	}
	layout, err := proj.Paths()
	if err != nil {
		return nil, nil, nil, err
	}
	cfg := domainconfig.Defaults()
	svc, cleanup, err := buildRunService(ctx, layout, cfg, indexNone)
	if err != nil {
		return nil, nil, nil, err
	}
	resolver, resolverCleanup, err := buildStateResolver(layout, cfg)
	if err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	hasher := blake3hash.New()
	evidence := apprunevidence.NewService(runstore.New(layout, hasher), stateprovider.NewAssessor(resolver))
	combined := func() {
		resolverCleanup()
		cleanup()
	}
	return svc, evidence, combined, nil
}

// The documents below are the awa-evidence/v1 wire shape for "run show --json". The
// enclosing document carries the run-evidence contract; the nested states.before/after
// are complete awa-state/v1 assessments rendered by the shared assessmentDoc projector,
// so a nested identity can never drift from `awa state resolve run:<id>:before|after`.

// evExitDoc carries the recorded exit plus its protocol provenance. Origin lets a
// consumer tell the wrapped child's exit from an awa-origin error without guessing across
// overlapping numeric codes; it shares the run-record token with the run --json surface.
type evExitDoc struct {
	Kind   string `json:"kind"`
	Code   int    `json:"code"`
	Signal string `json:"signal,omitempty"`
	Origin string `json:"origin"`
}

type evRunDoc struct {
	ID      string   `json:"id"`
	Command []string `json:"command"`
	CWD     string   `json:"cwd"`
}

type evExecutionDoc struct {
	StartedAt  string    `json:"started_at"`
	FinishedAt string    `json:"finished_at"`
	DurationMs int64     `json:"duration_ms"`
	Exit       evExitDoc `json:"exit"`
}

type evReuseDoc struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

type evMutationDoc struct {
	Outcome string `json:"outcome"`
}

type evStatesDoc struct {
	Before resolveDoc `json:"before"`
	After  resolveDoc `json:"after"`
}

// evInspectDoc is one stream's no-byte-read inspectability: presence/readability and
// content integrity (always "unverified" for metadata inspection).
type evInspectDoc struct {
	Presence  string `json:"presence"`
	Integrity string `json:"integrity"`
}

// evStreamDoc carries one captured stream's byte/hash/truncation facts (from the run
// record) plus its typed inspectability (obtained without reading payload bytes).
type evStreamDoc struct {
	showStreamDoc
	Inspectability evInspectDoc `json:"inspectability"`
}

type evOutputDoc struct {
	Stdout evStreamDoc `json:"stdout"`
	Stderr evStreamDoc `json:"stderr"`
}

type evidenceDoc struct {
	ProviderContract string         `json:"provider_contract"`
	Run              evRunDoc       `json:"run"`
	Execution        evExecutionDoc `json:"execution"`
	Reuse            evReuseDoc     `json:"reuse"`
	Mutation         evMutationDoc  `json:"mutation"`
	Inputs           runInputsDoc   `json:"inputs"`
	States           evStatesDoc    `json:"states"`
	Output           evOutputDoc    `json:"output"`
	// Samples is present only when --tail/--grep is requested; it carries a bounded,
	// structured selection of output lines and is the one place a run show --json opens
	// and reads payload bytes (an explicit, opt-in output selection).
	Samples *showSamplesDoc `json:"samples,omitempty"`
	// SkippedLatest is present only for a --last selection that passed over newer
	// records it could not read. Its absence means the returned run really is the
	// newest stored one.
	SkippedLatest *skippedLatestDoc `json:"skipped_latest,omitempty"`
}

// evidenceDocOf projects the run-evidence aggregate into the awa-evidence/v1 wire
// document. Output byte/hash/truncation facts come from the run record; the nested
// before/after assessments and per-stream presence come from the aggregate, so the CLI
// never reconstructs availability or reuse from booleans. stdoutIntegrity/stderrIntegrity
// are the strongest verification this invocation performed on each stream: the aggregate's
// metadata inspection is always unverified, and the caller upgrades a stream to verified
// only after an explicit --tail/--grep read opened and hash-verified it.
func evidenceDocOf(ev runevidence.Evidence, stdoutIntegrity, stderrIntegrity runevidence.Integrity) evidenceDoc {
	e := ev.Record()
	return evidenceDoc{
		ProviderContract: ev.Contract().String(),
		Run: evRunDoc{
			ID:      e.ID.String(),
			Command: e.KeyInput.Command.Argv,
			CWD:     e.KeyInput.CWD.String(),
		},
		Execution: evExecutionDoc{
			StartedAt:  e.StartedAt.UTC().Format(time.RFC3339Nano),
			FinishedAt: e.FinishedAt.UTC().Format(time.RFC3339Nano),
			DurationMs: e.DurationMs(),
			Exit:       evExitDoc{Kind: e.Exit.Kind.String(), Code: e.Exit.Code, Signal: e.Exit.Signal, Origin: e.Exit.Origin().String()},
		},
		Reuse:    evReuseDoc{State: e.Reuse.String(), Reason: e.Reuse.Reason().String()},
		Mutation: evMutationDoc{Outcome: e.Mutation.Outcome().String()},
		Inputs: runInputsDoc{
			TreeHash:       e.KeyInput.InputTreeHash.String(),
			TrustMode:      e.KeyInput.TrustMode.String(),
			Scope:          e.KeyInput.IncludeScope,
			SkippedCount:   e.Skipped.Count,
			SkippedAllowed: e.Skipped.Allowed,
		},
		States: evStatesDoc{
			Before: assessmentDoc(ev.Before()),
			After:  assessmentDoc(ev.After()),
		},
		Output: evOutputDoc{
			Stdout: evStreamDocOf(e.Stdout, ev.Stdout(), stdoutIntegrity),
			Stderr: evStreamDocOf(e.Stderr, ev.Stderr(), stderrIntegrity),
		},
	}
}

// evStreamDocOf projects one stream: byte/hash/truncation facts from the record, presence
// from the aggregate's no-byte-read inspection, and integrity as the invocation's strongest
// verification of that stream (unverified for a metadata-only view; verified once an
// explicit read hash-checked it).
func evStreamDocOf(c runcache.OutputCapture, insp runevidence.OutputInspectability, integrity runevidence.Integrity) evStreamDoc {
	return evStreamDoc{
		showStreamDoc: showStreamView(c),
		Inspectability: evInspectDoc{
			Presence:  insp.Presence().String(),
			Integrity: integrity.String(),
		},
	}
}
