package cli

import (
	"fmt"

	apprun "awarer/internal/app/run"
	"awarer/internal/domain/runcache"
	"awarer/internal/output"
)

// This file is the single owner of how the application's effect-state diagnosis
// reaches a user. One typed fact — apprun.EffectDiagnosis — feeds every projection
// here: the machine object each surface emits, the compact one-line sentence every
// near-miss surface prints, and the fuller block the `awa run` footer prints for the
// run's own non-reusable verdict. A surface picks a projection; it never re-derives the
// fact, invents a root, or writes its own wording, so one diagnosis reads the same way
// wherever it surfaces — `awa run` (footer and `--json`), `run ls --near`, `run explain`,
// and `status`.
//
// What can still differ between commands is which diagnosis they are handed. Two of them
// can reach different reasons for the same stored run — run ls --near short-circuits to a
// recorded non-reusable verdict where run explain re-keys the entry — and the application
// layer decides from that provenance whether a root is evidence at all. Nothing here
// re-opens that: an absent root is a decided fact, not a rendering choice. What this file
// does decide is whether a decided fact is worth a human line, and the answer for a
// candidate is "only when it adds one" — see effectDetail.
//
// The two human renderers differ in density, not in fact. The footer block answers "why
// is the command I just ran not reusable?" and so also teaches the
// exclude/effect-root/record decision; the one-line detail answers "why is this OTHER
// stored run not reusable?" inside a listing that already points at the deeper commands.
// They cannot both describe one invocation: an effect-state verdict makes the run
// non-reusable history, which is exactly the state whose nearest-candidate block the
// footer suppresses.

// effectDiagnosisDoc is the machine form of the effect-state diagnosis. Reason and
// Sample are stable tokens; Root names the dominant watched root when the observation
// identified one and is omitted when it named none — never derived from a path
// convention or a rescan. Sample is always "unavailable": effect state is a bounded
// stat signature, not a per-path manifest, so a consumer never has to parse prose to
// learn that no changed-path sample exists.
//
// It is one shape for two subjects — the run just executed, and a near-miss candidate
// considered against current state — because both answer the same question with the
// same vocabulary. A consumer decodes them identically.
type effectDiagnosisDoc struct {
	Reason  string            `json:"reason"`
	Root    string            `json:"root,omitempty"`
	Sample  string            `json:"sample"`
	Actions []effectActionDoc `json:"actions"`
}

// effectActionDoc is one typed next-step in the exclude/effect-root/record decision:
// Condition and Action are stable tokens, and Argv is present for the record action so
// a consumer can run it without building the command line.
type effectActionDoc struct {
	Condition string   `json:"condition"`
	Action    string   `json:"action"`
	Argv      []string `json:"argv,omitempty"`
}

// effectDiagnosisView projects an application effect diagnosis into its machine form,
// or nil when there is none. argv is the command the record action should name — the
// executed invocation for a run, the candidate's own stored command for a near miss —
// so the suggested `awa run --record` is always the command the diagnosis is about.
func effectDiagnosisView(d *apprun.EffectDiagnosis, argv []string) *effectDiagnosisDoc {
	if d == nil {
		return nil
	}
	recordArgv := append([]string{"awa", "run", "--record", "--"}, argv...)
	return &effectDiagnosisDoc{
		Reason: d.Reason.String(),
		Root:   d.Root,
		Sample: "unavailable",
		Actions: []effectActionDoc{
			{Condition: "self-generated", Action: "exclude"},
			{Condition: "read-only-dependency", Action: "effect-root"},
			{Condition: "side-effecting", Action: "record", Argv: recordArgv},
		},
	}
}

// effectDetail renders the one compact sentence a human near-miss surface prints for an
// effect-state candidate: which watched-state fact is wrong, and the dominant watched
// root the observation named. It is deliberately a single line with no action tutorial —
// the deeper `run explain` / `run ls --near` commands each surface already points at
// carry that — and it never fabricates a changed-path sample.
//
// The root is what makes the line worth printing. Every human near-miss surface already
// prints the canonical `not-reusable(<reason>)` token beside the candidate, so a rootless
// diagnosis has nothing to add: it would restate that token as prose, once per candidate,
// and a listing of build runs that each disturbed watched output is exactly where that
// repeats. So a rootless diagnosis — an observation that named no root, or a candidate
// short-circuited to a stored historical verdict, which the application layer strips the
// root from — yields the empty string, and every caller treats that as "print no line".
// The reason token remains the complete human rendering there.
//
// This suppression is presentation only and candidate-only. The structured `effect`
// object still travels (its sample fact and typed actions inform a machine consumer that
// no reason token carries), and the fuller diagnosis the `awa run` footer prints for the
// run just executed keeps its sample and action guidance with or without a root.
//
// Every human near-miss surface renders through this one function, so status, the run
// listing, the explanation, and the inline footer cannot silently choose different
// vocabulary — or a different print/suppress rule — for the same diagnosis.
func effectDetail(d *apprun.EffectDiagnosis) string {
	if d == nil || d.Root == "" {
		return ""
	}
	switch d.Reason {
	case runcache.ReasonEffectStateDiffers:
		return fmt.Sprintf("watched generated-output state differs (dominant root %q)", d.Root)
	case runcache.ReasonEffectStateUnavailable:
		return fmt.Sprintf("watched generated-output state could not be observed safely (root %q)", d.Root)
	default:
		return ""
	}
}

// emitEffectDiagnosis explains a run left non-reusable because its watched
// generated-output (effect) state changed or could not be observed. Effect state is
// recorded as a bounded stat signature, not a per-path manifest, so this names the
// dominant watched root when the observation identified one and is otherwise honest
// that no changed-path sample exists — it never fabricates one. It then teaches the
// exclude/effect-root/record decision so the reader knows the safe next step. The
// exact before/after inspect command is already printed by the footer when the run
// recorded both observations, so it is not repeated here.
func emitEffectDiagnosis(w *output.Writer, diag *apprun.EffectDiagnosis) {
	if diag == nil {
		return
	}
	switch diag.Reason {
	case runcache.ReasonEffectStateDiffers:
		if diag.Root != "" {
			w.Diagnostic(fmt.Sprintf("  diagnosis: watched generated-output state changed (dominant root %q)", diag.Root))
		} else {
			w.Diagnostic("  diagnosis: watched generated-output state changed during the run")
		}
	case runcache.ReasonEffectStateUnavailable:
		if diag.Root != "" {
			w.Diagnostic(fmt.Sprintf("  diagnosis: watched generated-output state could not be observed safely (root %q)", diag.Root))
		} else {
			w.Diagnostic("  diagnosis: watched generated-output state could not be observed safely")
		}
	default:
		return
	}
	w.Diagnostic("  sample:    changed-path sample unavailable (effect state is a bounded stat signature)")
	w.Diagnostic("  hint:      self-generated output -> exclude it ([run].extra_excludes / .awaignore);")
	w.Diagnostic("             read-only dependency -> keep it a [run].extra_effect_roots root;")
	w.Diagnostic("             side-effecting command -> awa run --record")
}
