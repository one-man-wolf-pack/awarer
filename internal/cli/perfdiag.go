package cli

import (
	"fmt"
	"strconv"
	"time"

	"awarer/internal/domain/perfdiag"
	"awarer/internal/output"
)

// perfEvidenceDoc is the JSON projection of a latency diagnostic's bounded evidence. It
// is a sum type on the wire: a fully-observed root carries an exact file_count; a root
// that failed the observation closed on its entry budget carries threshold_crossed +
// threshold and never a fabricated count. Exactly one of file_count /
// (threshold_crossed, threshold) is present.
type perfEvidenceDoc struct {
	Path             string `json:"path"`
	FileCount        *int64 `json:"file_count,omitempty"`
	ThresholdCrossed bool   `json:"threshold_crossed,omitempty"`
	Threshold        *int   `json:"threshold,omitempty"`
}

// perfHintDoc is the typed next-action of a latency diagnostic: a stable kind token and
// the tokenized invocation, mirroring the review surfaces' typed suggestions so JSON
// never carries a prose-only command string.
type perfHintDoc struct {
	Kind string   `json:"kind"`
	Argv []string `json:"argv"`
}

// perfDiagDoc is the machine-readable form of one latency diagnostic, matching the
// output-style contract's performance-diagnostic shape: a stable kebab-case code, a
// numeric millisecond duration, the stage it is attributed to (the JSON key stays
// "component" to match the contract), bounded typed evidence, and an optional typed
// hint.
type perfDiagDoc struct {
	Code       string          `json:"code"`
	DurationMs int64           `json:"duration_ms"`
	Component  string          `json:"component"`
	Evidence   perfEvidenceDoc `json:"evidence"`
	Hint       *perfHintDoc    `json:"hint,omitempty"`
}

// perfDiagnosticsDoc is the diagnostics block a command payload carries for latency
// facts. Performance is always a non-nil array so the field is a stable [] rather than
// null.
type perfDiagnosticsDoc struct {
	Performance []perfDiagDoc `json:"performance"`
}

// perfDiagnosticsView projects the app-layer diagnostics into an optional JSON block,
// nil when there are none so a payload that omits an empty diagnostics object (run,
// status) can do so.
func perfDiagnosticsView(diags []perfdiag.Diagnostic) *perfDiagnosticsDoc {
	if len(diags) == 0 {
		return nil
	}
	return &perfDiagnosticsDoc{Performance: perfDiagDocs(diags)}
}

// perfDiagnosticsBlock projects the app-layer diagnostics into an always-present JSON
// block whose performance array is empty rather than absent when nothing crossed the
// threshold. It is for the run.ls family, which carries a stable shape so a consumer
// always finds diagnostics.performance without a presence check.
func perfDiagnosticsBlock(diags []perfdiag.Diagnostic) perfDiagnosticsDoc {
	return perfDiagnosticsDoc{Performance: perfDiagDocs(diags)}
}

// perfDiagDocs projects a slice of diagnostics into their JSON docs.
func perfDiagDocs(diags []perfdiag.Diagnostic) []perfDiagDoc {
	out := make([]perfDiagDoc, 0, len(diags))
	for _, d := range diags {
		out = append(out, perfDiagView(d))
	}
	return out
}

// perfDiagView projects one diagnostic into its JSON doc.
func perfDiagView(d perfdiag.Diagnostic) perfDiagDoc {
	doc := perfDiagDoc{
		Code:       d.Cause().String(),
		DurationMs: d.DurationMs(),
		Component:  d.Stage().String(),
		Evidence:   perfEvidenceView(d.Evidence()),
	}
	if h, ok := d.Hint(); ok {
		doc.Hint = &perfHintDoc{Kind: h.Kind().String(), Argv: h.Argv()}
	}
	return doc
}

// perfEvidenceView projects bounded evidence, emitting exactly the exact-count or the
// threshold-crossing shape.
func perfEvidenceView(e perfdiag.Evidence) perfEvidenceDoc {
	doc := perfEvidenceDoc{Path: e.Path()}
	if count, ok := e.ExactCount(); ok {
		c := count
		doc.FileCount = &c
		return doc
	}
	if threshold, ok := e.Threshold(); ok {
		doc.ThresholdCrossed = true
		t := threshold
		doc.Threshold = &t
	}
	return doc
}

// emitPerformanceNotes writes the human latency diagnostics as compact note/hint lines
// to stderr (advisory, never part of a JSON or raw payload). The diagnostics are already
// bounded to one or two by perfdiag.Top at the app layer, so this prints them as-is.
func emitPerformanceNotes(w *output.Writer, diags []perfdiag.Diagnostic) {
	for _, d := range diags {
		w.Diagnostic("note: " + performanceNoteLine(d))
		if line, ok := performanceHintLine(d); ok {
			w.Diagnostic("hint: " + line)
		}
	}
}

// performanceNoteLine renders the compact human note for one latency diagnostic: how
// long the stage took and the bounded evidence behind it. The wording is chosen per
// cause, because the two causes ask different things of the reader — a large effect
// root is a configuration fact worth acting on, while a full input rehash is a cost
// worth understanding — and a shared phrasing would blur that into "something was big".
func performanceNoteLine(d perfdiag.Diagnostic) string {
	dur := durationLabel(time.Duration(d.DurationMs()) * time.Millisecond)
	ev := d.Evidence()
	switch d.Cause() {
	case perfdiag.CauseFullInputRehash:
		count, ok := ev.ExactCount()
		if !ok {
			// The cause is only ever constructed with an exact count; a diagnostic without
			// one would have to invent a number to say anything about size, so it says
			// nothing about size instead.
			return fmt.Sprintf("run input observation took %s; the run-cache identity is read from file content, not from stat signatures, so a rewrite that leaves size and timestamps unchanged cannot replay a stale result", dur)
		}
		return fmt.Sprintf("run input observation took %s; hashed %s files under %q — the run-cache identity is read from file content, not from stat signatures, so a rewrite that leaves size and timestamps unchanged cannot replay a stale result",
			dur, groupThousands(count), ev.Path())
	default:
		var rootPhrase string
		if count, ok := ev.ExactCount(); ok {
			rootPhrase = fmt.Sprintf("effect root %q contains %s files", ev.Path(), groupThousands(count))
		} else if threshold, ok := ev.Threshold(); ok {
			rootPhrase = fmt.Sprintf("effect root %q exceeds %s files", ev.Path(), groupThousands(int64(threshold)))
		}
		return fmt.Sprintf("run state observation took %s; %s", dur, rootPhrase)
	}
}

// performanceHintLine renders the safe next-action prose for a diagnostic's hint. The
// wording is fixed per hint kind (the JSON carries the machine argv); it deliberately
// leads with the record-mode workflow and only mentions reviewing effect roots as an
// explicit policy decision, never a casual "exclude target".
func performanceHintLine(d perfdiag.Diagnostic) (string, bool) {
	h, ok := d.Hint()
	if !ok {
		return "", false
	}
	switch h.Kind() {
	case perfdiag.LargeEffectRootHintKind:
		return "use `awa run --record` for evidence-only workflows, or review [run] effect roots", true
	case perfdiag.ReviewRunScopeHintKind:
		// The warning is not decoration. The only lever this hint points at is the size
		// of the scope, and the reader who acts on it fastest is the one who excludes the
		// most — which is exactly the reader who breaks their own cache. An input the key
		// does not see is an input whose change cannot miss, so the failure is silent: a
		// hit that replays a result computed from a file that has since changed.
		return "review what the run input scope covers with `awa help ignores`; never exclude a file the command actually reads — an unobserved input cannot invalidate a hit", true
	default:
		return "", false
	}
}

// groupThousands renders n with comma thousands separators (100000 -> "100,000") so a
// large count is scannable in a human note.
func groupThousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := ""
	if n < 0 {
		neg = "-"
		s = s[1:]
	}
	if len(s) <= 3 {
		return neg + s
	}
	var b []byte
	pre := len(s) % 3
	if pre > 0 {
		b = append(b, s[:pre]...)
	}
	for i := pre; i < len(s); i += 3 {
		if len(b) > 0 {
			b = append(b, ',')
		}
		b = append(b, s[i:i+3]...)
	}
	return neg + string(b)
}
