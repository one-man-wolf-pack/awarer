package cli

import (
	"strings"
	"time"

	apprun "awarer/internal/app/run"
	"awarer/internal/domain/runcache"
)

// This file holds JSON shapes shared across the machine-readable surfaces. Most are
// the review vocabulary (run explain candidates, run ls --near, and the status review
// dashboard): "why is this run not reusable" must look the same no matter which command
// an agent asks it through, so a near miss under `run explain` and under `run ls --near`
// decode into one shape. The bounded-output vocabulary below is the sibling: one shared
// completeness fact for every surface that intentionally caps its record list.

// reasonBoundedOutput is the stable token a JSON surface uses when it intentionally
// caps its record list to keep machine output bounded, rather than emit an unbounded
// dump of a large history or store. It travels in a boundedOutputView.Reason.
const reasonBoundedOutput = "bounded-output"

// boundedOutputView is the shared completeness fact for JSON surfaces whose record
// list is intentionally capped for boundedness (log --all, gc). Complete is always
// present so a consumer never has to infer it; when false, Shown/Limit/Reason explain
// the cap and the surrounding document carries the known full count (e.g. total), so a
// consumer can tell a complete list from a capped prefix and knows the excerpt's size.
// It deliberately reuses one vocabulary across surfaces rather than inventing a
// per-command partial-output shape.
type boundedOutputView struct {
	Complete bool   `json:"complete"`
	Shown    int    `json:"shown"`
	Limit    int    `json:"limit,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// boundedOutputOf builds the completeness fact for a list capped at limit out of total
// available. A non-positive limit means "no cap requested", which is always complete.
func boundedOutputOf(shown, total, limit int) boundedOutputView {
	v := boundedOutputView{Complete: true, Shown: shown}
	if limit > 0 && total > shown {
		v.Complete = false
		v.Limit = limit
		v.Reason = reasonBoundedOutput
	}
	return v
}

// changedPathsDoc is the machine-readable form of a bounded changed-path sample.
// Its single completeness token — sourced from the domain SampleCompleteness — is
// the authority on how the sample relates to the full change set, so a consumer is
// never misled into reading a bounded prefix as the whole set or an unavailable
// sample as "no changes". Shown/Total quantify the excerpt.
type changedPathsDoc struct {
	// Completeness is "complete" (items lists every changed path), "truncated"
	// (items is a bounded prefix; total names the real count), or "unavailable" (no
	// sample could be computed — distinct from a complete empty sample).
	Completeness string            `json:"completeness"`
	Shown        int               `json:"shown"`
	Total        int               `json:"total"`
	Items        []changedPathItem `json:"items"`
}

// changedPathItem is one changed input path: the path and its stable single-letter
// status code (A/M/D/R/T/S) from the compare vocabulary.
type changedPathItem struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// changedPathsView converts a domain changed-path sample into its JSON shape. A nil
// pointer is the only way absence is represented, and it renders as the unavailable
// completeness token with an empty item set; a present sample is always complete or
// truncated.
func changedPathsView(s *runcache.ChangedPathSample) changedPathsDoc {
	if s == nil {
		return changedPathsDoc{Completeness: runcache.SampleUnavailable.String(), Items: []changedPathItem{}}
	}
	paths := s.Paths()
	items := make([]changedPathItem, 0, len(paths))
	for _, p := range paths {
		items = append(items, changedPathItem{Path: p.Path, Status: p.Status})
	}
	return changedPathsDoc{
		Completeness: s.Completeness().String(),
		Shown:        len(items),
		Total:        s.Total(),
		Items:        items,
	}
}

// hintDoc is the machine-readable form of an advisory hint. It is deliberately
// separate from any mismatch reason: a hint never explains a cache decision, it
// only suggests how a user might scope the run cache.
type hintDoc struct {
	Path       string `json:"path"`
	Suggestion string `json:"suggestion"`
	Message    string `json:"message"`
}

// hintDocs converts advisory hints into their JSON shape, always returning a
// non-nil slice so the field is a stable empty array rather than null.
func hintDocs(hints []runcache.AdvisoryHint) []hintDoc {
	out := make([]hintDoc, 0, len(hints))
	for _, h := range hints {
		out = append(out, hintDoc{Path: h.Path, Suggestion: h.Suggestion, Message: h.Message})
	}
	return out
}

// scoreDoc is the matched/different key-input category score of a near-miss
// candidate, shared by every surface that ranks near misses.
type scoreDoc struct {
	Matched   []string `json:"matched"`
	Different []string `json:"different"`
}

// keyDiffDoc is one typed key-input difference: its stable DiffCode, the field
// name, and either redacted old/new values or an env-variable change marker.
type keyDiffDoc struct {
	Code      string `json:"code"`
	Field     string `json:"field"`
	Old       string `json:"old,omitempty"`
	New       string `json:"new,omitempty"`
	EnvName   string `json:"env_name,omitempty"`
	EnvChange string `json:"env_change,omitempty"`
}

// diffDocs projects a key comparison's typed differences into their JSON shape.
func diffDocs(c runcache.KeyComparison) []keyDiffDoc {
	out := make([]keyDiffDoc, 0, len(c.Differences))
	for _, d := range c.Differences {
		out = append(out, keyDiffDoc{
			Code:      string(d.Code),
			Field:     d.Field,
			Old:       d.Old,
			New:       d.New,
			EnvName:   d.EnvName,
			EnvChange: string(d.EnvChange),
		})
	}
	return out
}

// nonNil normalizes a string slice to a stable empty array so JSON emits [] not null.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// nearMissDoc is the machine-readable form of one near-miss candidate: a recorded
// run that is not reusable for the current state, together with the canonical
// reason, the matched/different score categories, the typed key-input differences,
// a bounded changed-path sample, and advisory hints. It is the single shape both
// run explain candidates and run ls --near near misses decode into.
type nearMissDoc struct {
	RunID     string   `json:"run_id"`
	ShortID   string   `json:"short_id"`
	StartedAt string   `json:"started_at"`
	Command   []string `json:"command"`
	CWD       string   `json:"cwd"`
	ExitCode  int      `json:"exit_code"`
	// Reason is the canonical runcache.MismatchReason token for why the run is not
	// reusable now.
	Reason        string          `json:"reason"`
	Healthy       bool            `json:"healthy"`
	HealthChecked bool            `json:"health_checked"`
	Score         scoreDoc        `json:"score"`
	Differences   []keyDiffDoc    `json:"differences"`
	ChangedPaths  changedPathsDoc `json:"changed_paths"`
	Hints         []hintDoc       `json:"hints"`
	// Effect is the shared effect-state diagnosis, present only when Reason is
	// effect-state-differs or effect-state-unavailable and absent otherwise. It is the
	// same object `awa run --json` emits for a newly executed non-reusable run, so a
	// consumer decodes an effect diagnosis identically wherever it appears.
	Effect *effectDiagnosisDoc `json:"effect,omitempty"`
}

// nearMissView converts a near-miss candidate into its JSON shape.
func nearMissView(c apprun.RunReuseCandidate) nearMissDoc {
	e := c.Entry
	return nearMissDoc{
		RunID:         e.ID.String(),
		ShortID:       e.ID.Short(),
		StartedAt:     e.StartedAt.UTC().Format(time.RFC3339Nano),
		Command:       e.KeyInput.Command.Argv,
		CWD:           e.KeyInput.CWD.String(),
		ExitCode:      e.Exit.Code,
		Reason:        string(c.Reason),
		Healthy:       c.Healthy,
		HealthChecked: c.HealthChecked,
		Score:         scoreDoc{Matched: nonNil(c.Score.Matched), Different: nonNil(c.Score.Different)},
		Differences:   diffDocs(c.Comparison),
		ChangedPaths:  changedPathsView(c.Changed),
		Hints:         hintDocs(c.Hints),
		// The record-mode action names this candidate's own stored command, not the
		// invocation that asked for the diagnosis: the advice is about the run that keeps
		// disturbing watched output.
		Effect: effectDiagnosisView(c.Effect, e.KeyInput.Command.Argv),
	}
}

// nearMissViews projects a slice of candidates into JSON near misses, always
// returning a non-nil slice so the field is a stable empty array rather than null.
func nearMissViews(cands []apprun.RunReuseCandidate) []nearMissDoc {
	out := make([]nearMissDoc, 0, len(cands))
	for _, c := range cands {
		out = append(out, nearMissView(c))
	}
	return out
}

// runDiagnosisDoc is the machine-readable form of the inline cache diagnosis that
// `awa run` shows in its footer: whether a nearest previous run was found, that
// near-miss candidate as the same nearMissDoc shape the deeper surfaces emit, and
// structured follow-up command suggestions (kind + argv). It carries facts, not
// prose: the human sentences live in the footer, and a machine consumer acts on
// available/nearest/suggestions without parsing text.
type runDiagnosisDoc struct {
	Available bool         `json:"available"`
	Nearest   *nearMissDoc `json:"nearest,omitempty"`
	// Suggestions is always a non-nil array so the field is a stable [] rather than null.
	Suggestions []reviewNextDoc `json:"suggestions"`
}

// runDiagnosisView projects a run result and its inline diagnosis into the JSON
// diagnosis doc, reusing nearMissView for the candidate so run explain, run ls
// --near, and the inline run diagnosis all decode a near miss the same way.
func runDiagnosisView(res apprun.Result, diag apprun.RunInlineDiagnosis) runDiagnosisDoc {
	doc := runDiagnosisDoc{
		Available:   diag.Available(),
		Suggestions: runDiagnosisSuggestions(res, diag),
	}
	if diag.Nearest != nil {
		nm := nearMissView(*diag.Nearest)
		doc.Nearest = &nm
	}
	return doc
}

// runDiagnosisSuggestions builds the structured follow-up commands for a run's
// inline diagnosis, reusing the shared kind+argv reviewNextDoc vocabulary so an
// agent acts on them without parsing footer text. A cache hit needs no follow-up, so
// it returns an empty (non-nil) slice; a miss always suggests `run explain`, adds
// `run ls --near` when a nearest candidate exists, and adds the pre/post compare when
// the run recorded a non-reusable before/after it is worth diffing.
func runDiagnosisSuggestions(res apprun.Result, diag apprun.RunInlineDiagnosis) []reviewNextDoc {
	out := []reviewNextDoc{}
	if res.Status == runcache.CacheHit {
		return out
	}

	argv := res.Execution.KeyInput.Command.Argv
	if len(argv) > 0 {
		out = append(out, reviewNextDoc{
			Kind:    "run-explain",
			Reason:  "cache-miss",
			Argv:    append([]string{"awa", "run", "explain", "--"}, argv...),
			Display: "awa run explain -- " + strings.Join(argv, " "),
		})
	}
	if diag.Nearest != nil {
		out = append(out, reviewNextDoc{
			Kind:    "run-ls-near",
			Reason:  "near-miss",
			Argv:    []string{"awa", "run", "ls", "--near"},
			Display: "awa run ls --near",
		})
	}
	if sum, err := apprun.NewRunExecutionSummary(res); err == nil && !sum.Reusable && sum.PrePostCompareAvailable() {
		ref := prePostCompareRef(sum.RunID.String())
		out = append(out, reviewNextDoc{
			Kind:    "run-compare",
			Reason:  string(sum.Reason),
			Argv:    []string{"awa", "changes", ref},
			Display: "awa changes " + ref,
		})
	}
	return out
}
