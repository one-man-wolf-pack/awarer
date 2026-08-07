package cli

import (
	"fmt"
	"strings"
	"time"

	apprestore "awarer/internal/app/restore"
	domainconfig "awarer/internal/domain/config"
	domrestore "awarer/internal/domain/restore"
	"awarer/internal/output"
)

// restoreContract is the versioned machine contract every restore JSON document
// carries, alongside the envelope's own schema version. A consumer branches on this
// token, not on the command name.
const restoreContract = "awa-restore/v1"

// maxSampledPathsShown bounds how many blocked or failed paths the human report
// lists before it says how many more there are. The counts above it stay exact.
const maxSampledPathsShown = 5

// --- human ----------------------------------------------------------------

// renderRestore prints the human report. Preview leads with what the restore is
// proven from and what it would do, and ends with the exact command that would do
// it — using the resolved immutable id, never the moving reference the user typed,
// so running the suggestion cannot act on a different state than the preview
// described.
func renderRestore(w *output.Writer, res apprestore.Result, td domainconfig.TimeDisplay) {
	r := res.Result
	src := r.Source()

	// The canonical reference — not the bare id — is what leads, because it is what
	// a reader can paste back into a command. For a checkpoint the two are the same
	// string; for a run or recovery observation the bare id is not a reference at
	// all, and printing it would hand back something awa cannot resolve.
	w.Linef("source: %s (%s)%s", src.CanonicalRef(), src.Kind(), sourceAgeSuffix(res, td))
	if res.SourceMessage != "" {
		w.Linef("        %q", res.SourceMessage)
	}
	if src.Requested() != src.CanonicalRef() {
		w.Linef("        requested as %s", src.Requested())
	}
	w.Linef("selection: %s", r.Selection().String())

	renderRestoreCounts(w, r.Counts())
	if r.Outcome() == domrestore.OutcomePreview {
		// An apply outcome reports the same per-path facts under its own heading, so
		// only the preview prints them here; printing both would list every blocked path
		// twice.
		renderRestoreBlocked(w, r)
		renderRestoreBoundary(w, r)
		renderPreviewNext(w, r)
		return
	}
	renderRestoreBoundary(w, r)
	renderApplyOutcome(w, res)
}

// sourceAgeSuffix renders when the source state was observed, when that is known.
func sourceAgeSuffix(res apprestore.Result, td domainconfig.TimeDisplay) string {
	if !res.HasSourceTime {
		return ""
	}
	return " " + FormatTime(td, res.SourceTime, time.Now())
}

// renderRestoreCounts prints the per-kind totals. Only non-zero kinds are listed,
// so a focused restore reads as a short line rather than a table of zeroes; a plan
// with nothing to do says so outright.
func renderRestoreCounts(w *output.Writer, c domrestore.Counts) {
	var parts []string
	add := func(n int, label string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, label))
		}
	}
	add(c.Create, "create")
	add(c.Replace, "replace")
	add(c.TypeChange, "type-change")
	add(c.Symlink, "symlink")
	add(c.DeleteFile, "delete")
	add(c.DeleteDirectory, "delete-dir")
	add(c.Equal, "equal")
	if len(parts) == 0 {
		w.Line("operations: none")
		return
	}
	w.Linef("operations: %s", strings.Join(parts, ", "))
}

// renderRestoreBlocked prints the evidence gaps: exact counts plus a bounded
// sample of paths and the closed reasons. This is the part a reader acts on, so it
// names paths rather than only totals — a bare count would leave "which path, and
// why" to be guessed, which is exactly the silent refusal restore exists to avoid.
func renderRestoreBlocked(w *output.Writer, r domrestore.ApplyResult) {
	c := r.Counts()
	if c.Unavailable() == 0 {
		return
	}
	w.Linef("blocked: %d operation(s) cannot run", c.Unavailable())
	shown := 0
	for _, f := range r.Failures() {
		if shown >= maxSampledPathsShown {
			break
		}
		w.Linef("  %s: %s", f.Path, reasonList(f.Reasons))
		shown++
	}
	if shown == 0 && len(r.Reasons()) > 0 {
		w.Linef("  %s", reasonList(r.Reasons()))
	}
	if more := c.Unavailable() - shown; shown > 0 && more > 0 {
		w.Linef("  (+%d more)", more)
	}
}

// renderRestoreBoundary prints the scope and policy caveats. The ignored-paths
// caveat is unconditional: an empty or small plan is exactly when a reader is most
// tempted to read it as "the worktree is accounted for".
func renderRestoreBoundary(w *output.Writer, r domrestore.ApplyResult) {
	b := r.Boundary()
	if !b.PolicyCompatible {
		w.Diagnostic("note: the source was observed under a different scan policy; absence in it is not proof of absence, so deletions are not planned")
	}
	if b.ScopeBounded {
		w.Diagnostic("note: this source is a recovery observation; it proves only the paths the restore that created it was going to change")
	}
	if b.SourceSkipped > 0 || b.CurrentSkipped > 0 {
		w.Diagnostic(fmt.Sprintf("note: %d source and %d current input(s) were skipped; operations that depend on them are blocked", b.SourceSkipped, b.CurrentSkipped))
	}
	w.Diagnostic("note: ignored paths are outside awa evidence; restore never creates or deletes them")
}

// renderPreviewNext prints the exact apply command and the follow-ups. It always
// spells the resolved immutable id, so the suggested command cannot silently act on
// a state that moved after the preview.
func renderPreviewNext(w *output.Writer, r domrestore.ApplyResult) {
	if r.Counts().Unavailable() > 0 {
		w.Line("apply: refused while any operation is blocked; resolve the reasons above or narrow the selection")
		return
	}
	if r.Counts().Mutating() == 0 {
		w.Line("apply: nothing to do; the selected paths already match this source")
		return
	}
	w.Linef("apply: awa restore --apply %s%s", r.Source().CanonicalRef(), pathOperands(r.Selection()))
	w.Linef("next:  awa checkpoint -m \"before restore\"   # optional: mark the current state first")
}

// renderApplyOutcome prints what an apply actually did, the immutable recovery
// observation it can be undone from, and the exact inspection command.
func renderApplyOutcome(w *output.Writer, res apprestore.Result) {
	r := res.Result
	w.Linef("outcome: %s", r.Outcome())
	if r.Outcome().Mutated() || r.Remaining() > 0 {
		w.Linef("completed: %d   remaining: %d", r.Completed(), r.Remaining())
	}
	if len(r.Reasons()) > 0 && r.Outcome() != domrestore.OutcomePreview {
		w.Linef("reason: %s", reasonList(r.Reasons()))
	}
	for i, f := range r.Failures() {
		if i >= maxSampledPathsShown {
			w.Linef("  (+%d more)", len(r.Failures())-maxSampledPathsShown)
			break
		}
		w.Linef("  %s: %s", f.Path, reasonList(f.Reasons))
	}
	// A failure that could not be returned as an error because the worktree had already
	// changed. The reason token above says what class it was; this says what happened,
	// which is the part a person needs to act on.
	if res.Diagnostic != "" {
		w.Diagnostic("note: the commit stopped on: " + res.Diagnostic)
	}
	if res.RecoveryRef != "" {
		w.Linef("recovery: %s", res.RecoveryRef)
		w.Linef("undo:    awa restore --apply %s%s", res.RecoveryRef, pathOperands(r.Selection()))
		// Retention is a policy with an explicit override, so the line states the
		// policy rather than promising a date awa cannot guarantee.
		w.Diagnostic("note: the recovery observation is retained per [gc].keep_restores_for and can be reclaimed by an explicit 'awa gc --older-than'")
	}
	w.Linef("inspect: awa changes %s..now%s", r.Source().CanonicalRef(), pathOperands(r.Selection()))
}

func reasonList(reasons []domrestore.Reason) string {
	parts := make([]string, len(reasons))
	for i, r := range reasons {
		parts[i] = r.String()
	}
	return strings.Join(parts, ", ")
}

// --- JSON -----------------------------------------------------------------

// The documents below are the awa-restore/v1 wire shape. Ids are always full, times
// are always RFC3339Nano, reasons are always closed tokens, and no document ever
// carries restored bytes, blob addresses, staging paths, or internal store
// filenames — a machine consumer gets identity and outcome, never awa's internals.

type restoreSourceDoc struct {
	Kind         string `json:"kind"`
	ID           string `json:"id"`
	CanonicalRef string `json:"canonical_ref"`
	Requested    string `json:"requested"`
	Message      string `json:"message,omitempty"`
	ObservedAt   string `json:"observed_at,omitempty"`
}

type restoreSelectionDoc struct {
	All   bool     `json:"all"`
	Paths []string `json:"paths"`
}

type restoreCountsDoc struct {
	Create          int `json:"create"`
	Replace         int `json:"replace"`
	TypeChange      int `json:"type_change"`
	Symlink         int `json:"symlink"`
	DeleteFile      int `json:"delete_file"`
	DeleteDirectory int `json:"delete_directory"`
	Equal           int `json:"equal"`
	Blocked         int `json:"blocked"`
}

type restoreBoundaryDoc struct {
	SourceSkippedInputs         int  `json:"source_skipped_inputs"`
	CurrentSkippedInputs        int  `json:"current_skipped_inputs"`
	PolicyCompatible            bool `json:"policy_compatible"`
	ScopeBounded                bool `json:"scope_bounded"`
	IgnoredPathsOutsideEvidence bool `json:"ignored_paths_outside_evidence"`
}

type restorePathFactDoc struct {
	Path    string   `json:"path"`
	Kind    string   `json:"kind,omitempty"`
	Reasons []string `json:"reasons"`
}

type restoreRecoveryDoc struct {
	OperationID string `json:"operation_id"`
	Ref         string `json:"ref"`
	// RetentionPolicyKey names the configuration key that governs how long the
	// observation is kept. There is deliberately no "available until" timestamp: an
	// explicit `awa gc --older-than` can shorten the window at any time, so a date
	// here would be a promise awa cannot keep.
	RetentionPolicyKey string `json:"retention_policy_key"`
}

type restoreDoc struct {
	RestoreContract string               `json:"restore_contract"`
	Mode            string               `json:"mode"`
	Outcome         string               `json:"outcome"`
	Source          restoreSourceDoc     `json:"source"`
	Selection       restoreSelectionDoc  `json:"selection"`
	Counts          restoreCountsDoc     `json:"counts"`
	Boundary        restoreBoundaryDoc   `json:"evidence_boundary"`
	Complete        bool                 `json:"complete"`
	Completed       int                  `json:"completed"`
	Remaining       int                  `json:"remaining"`
	Reasons         []string             `json:"reasons"`
	Failures        []restorePathFactDoc `json:"failures"`
	Recovery        *restoreRecoveryDoc  `json:"recovery,omitempty"`
}

// emitRestoreJSON writes the typed single-document result. It projects the same
// validated domain result the human renderer consumes — there is no second
// interpretation of the plan — and it never embeds a human command line: the
// structured source, selection, and recovery fields are what a consumer builds one
// from.
func emitRestoreJSON(w *output.Writer, res apprestore.Result) error {
	r := res.Result
	src := r.Source()
	c := r.Counts()

	doc := restoreDoc{
		RestoreContract: restoreContract,
		Mode:            restoreMode(r.Outcome()),
		Outcome:         r.Outcome().String(),
		Source: restoreSourceDoc{
			Kind:         src.Kind().String(),
			ID:           src.ID(),
			CanonicalRef: src.CanonicalRef(),
			Requested:    src.Requested(),
			Message:      res.SourceMessage,
		},
		Selection: restoreSelectionDoc{
			All:   r.Selection().All(),
			Paths: selectionPaths(r.Selection()),
		},
		Counts: restoreCountsDoc{
			Create: c.Create, Replace: c.Replace, TypeChange: c.TypeChange, Symlink: c.Symlink,
			DeleteFile: c.DeleteFile, DeleteDirectory: c.DeleteDirectory, Equal: c.Equal,
			Blocked: c.Blocked,
		},
		Boundary: restoreBoundaryDoc{
			SourceSkippedInputs:         r.Boundary().SourceSkipped,
			CurrentSkippedInputs:        r.Boundary().CurrentSkipped,
			PolicyCompatible:            r.Boundary().PolicyCompatible,
			ScopeBounded:                r.Boundary().ScopeBounded,
			IgnoredPathsOutsideEvidence: r.Boundary().IgnoredOutsideEvidence(),
		},
		Complete:  c.Unavailable() == 0,
		Completed: r.Completed(),
		Remaining: r.Remaining(),
		Reasons:   reasonTokens(r.Reasons()),
		Failures:  failureDocs(r.Failures()),
	}
	if res.HasSourceTime {
		doc.Source.ObservedAt = res.SourceTime.UTC().Format(time.RFC3339Nano)
	}
	if id := r.Recovery(); !id.IsZero() {
		doc.Recovery = &restoreRecoveryDoc{
			OperationID:        id.String(),
			Ref:                id.BeforeRef(),
			RetentionPolicyKey: "gc.keep_restores_for",
		}
	}
	// The selection and failure arrays are bounded by what the user typed and by
	// the domain's sample cap, so the whole document is bounded regardless of how
	// large the worktree is. That is why this is a single marshalled document
	// rather than a streamed one.
	return w.JSON("restore", doc)
}

// restoreMode reports which of the two modes produced this result, so a consumer
// can tell a preview from an apply without inferring it from the outcome.
func restoreMode(o domrestore.Outcome) string {
	if o == domrestore.OutcomePreview {
		return "preview"
	}
	return "apply"
}

func reasonTokens(reasons []domrestore.Reason) []string {
	out := make([]string, 0, len(reasons))
	for _, r := range reasons {
		out = append(out, r.String())
	}
	return out
}

func failureDocs(failures []domrestore.Failure) []restorePathFactDoc {
	out := make([]restorePathFactDoc, 0, len(failures))
	for _, f := range failures {
		out = append(out, restorePathFactDoc{
			Path:    f.Path.String(),
			Kind:    f.Kind.String(),
			Reasons: reasonTokens(f.Reasons),
		})
	}
	return out
}
