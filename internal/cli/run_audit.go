package cli

import (
	"fmt"
	"strconv"
	"strings"

	apprun "awarer/internal/app/run"
	"awarer/internal/domain/runcache"
	"awarer/internal/output"
)

// runRunAudit handles "awa run audit": a metadata-only summary of the primary stored
// reuse classifications in the newest window of retained executions. It accepts only
// a window count; every other operand or flag — command/time filters and child
// execution options alike — is a usage error.
func runRunAudit(w *output.Writer, inv invocation) error {
	args := inv.args[1:]
	limit := 0
	i := 0
	for i < len(args) {
		tok := args[i]
		i++
		name, value, hasValue := splitFlag(tok)
		switch name {
		case "--limit", "-n":
			v, err := requireValue(name, value, hasValue, args, &i)
			if err != nil {
				return err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > apprun.MaxAuditLimit {
				return usageErrorf("invalid value %q for %s: want a positive integer up to %d", v, name, apprun.MaxAuditLimit)
			}
			limit = n
		default:
			return rejectExtra("run audit", tok)
		}
	}

	svc, cleanup, err := buildRunService2(inv.invCtx(), inv.options)
	if err != nil {
		return err
	}
	defer cleanup()

	res, err := svc.Audit(inv.invCtx(), apprun.AuditRequest{Limit: limit})
	if err != nil {
		return classifyRunError(err)
	}
	if inv.options.JSON {
		return w.JSON("run.audit", auditView(res, limit == 0))
	}
	renderAudit(w, res)
	return nil
}

// auditCoverage names how much retained history the window covers.
func auditCoverage(res apprun.AuditResult) string {
	switch {
	case res.Selected == 0:
		return "none"
	case res.Older:
		return "newest-window"
	default:
		return "all-retained"
	}
}

type auditDoc struct {
	Window          auditWindowDoc  `json:"window"`
	Reads           auditReadsDoc   `json:"reads"`
	Classifications []auditClassDoc `json:"classifications"`
	Suggestions     []reviewNextDoc `json:"suggestions"`
}

type auditWindowDoc struct {
	// History is "present", "empty", or "missing": an empty window over existing but
	// empty history is distinct from a store with no run history at all.
	History   string `json:"history"`
	Requested int    `json:"requested"`
	Default   bool   `json:"default"`
	Max       int    `json:"max"`
	Selected  int    `json:"selected"`
	// Older is "present" when retained records exist beyond the window and "none"
	// when the window covers all retained history.
	Older    string `json:"older"`
	Coverage string `json:"coverage"`
}

type auditReadsDoc struct {
	Classified         int             `json:"classified"`
	Unavailable        int             `json:"unavailable"`
	Complete           bool            `json:"complete"`
	RecordByteLimit    int64           `json:"record_byte_limit"`
	AggregateByteLimit int64           `json:"aggregate_byte_limit"`
	BytesRead          int64           `json:"bytes_read"`
	Availability       []auditAvailDoc `json:"availability"`
}

type auditAvailDoc struct {
	State    string          `json:"state"`
	Count    int             `json:"count"`
	Examples auditExampleDoc `json:"examples"`
}

type auditClassDoc struct {
	Group    string          `json:"group"`
	Kind     string          `json:"kind"`
	Reason   string          `json:"reason,omitempty"`
	Count    int             `json:"count"`
	Examples auditExampleDoc `json:"examples"`
}

type auditExampleDoc struct {
	RunIDs  []string          `json:"run_ids"`
	Bounded boundedOutputView `json:"bounded"`
}

func auditExamplesView(e apprun.AuditExamples) auditExampleDoc {
	ids := make([]string, 0, len(e.IDs))
	for _, id := range e.IDs {
		ids = append(ids, id.String())
	}
	return auditExampleDoc{RunIDs: ids, Bounded: boundedOutputOf(len(ids), e.Total, apprun.AuditExampleLimit)}
}

func auditView(res apprun.AuditResult, defaulted bool) auditDoc {
	older := "none"
	if res.Older {
		older = "present"
	}
	doc := auditDoc{
		Window: auditWindowDoc{
			History:   string(res.History),
			Requested: res.Requested,
			Default:   defaulted,
			Max:       apprun.MaxAuditLimit,
			Selected:  res.Selected,
			Older:     older,
			Coverage:  auditCoverage(res),
		},
		Reads: auditReadsDoc{
			Classified:         res.Classified,
			Unavailable:        res.Unavailable,
			Complete:           res.Unavailable == 0,
			RecordByteLimit:    res.RecordBudget,
			AggregateByteLimit: res.AggregateBudget,
			BytesRead:          res.BytesRead,
			Availability:       []auditAvailDoc{},
		},
		Classifications: []auditClassDoc{},
		Suggestions:     auditSuggestions(res),
	}
	for _, a := range res.Availability {
		doc.Reads.Availability = append(doc.Reads.Availability, auditAvailDoc{State: string(a.State), Count: a.Count, Examples: auditExamplesView(a.Examples)})
	}
	for _, c := range res.Classes {
		doc.Classifications = append(doc.Classifications, auditClassDoc{
			Group:    string(c.Group),
			Kind:     c.Kind.String(),
			Reason:   string(c.Reason),
			Count:    c.Count,
			Examples: auditExamplesView(c.Examples),
		})
	}
	return doc
}

// auditSuggestions offers one drilldown per observed group, each anchored on a full
// example run id. They inspect stored evidence or explain a stored run against the
// current state; none changes configuration, input, or recording policy, and a
// deliberate recording gets none.
func auditSuggestions(res apprun.AuditResult) []reviewNextDoc {
	out := []reviewNextDoc{}
	seen := map[apprun.AuditGroup]bool{}
	helpRun := false
	for _, c := range res.Classes {
		if seen[c.Group] || len(c.Examples.IDs) == 0 {
			continue
		}
		seen[c.Group] = true
		id := c.Examples.IDs[0].String()
		switch c.Group {
		case apprun.AuditReusableWhenStored:
			out = append(out, reviewNextDoc{
				Kind:    "run-explain-from-run",
				Reason:  string(c.Group),
				Argv:    []string{"awa", "run", "explain", "--from-run", id, "--to-now"},
				Display: "awa run explain --from-run " + id + " --to-now",
			})
		case apprun.AuditChangedState:
			// Only mutated-state means the inputs differ between a run's own before and
			// after; an effect-state-differs run kept its inputs unchanged (mutation takes
			// precedence), so comparing them would show nothing.
			if c.Reason == runcache.ReasonMutatedState {
				ref := prePostCompareRef(id)
				out = append(out, reviewNextDoc{
					Kind:    "run-compare",
					Reason:  string(c.Group),
					Argv:    []string{"awa", "changes", ref},
					Display: "awa changes " + ref,
				})
			} else {
				out = append(out, auditShowMeta(c.Group, id))
			}
		case apprun.AuditPolicyBlocker, apprun.AuditObservationUnavailable, apprun.AuditFailureCachePolicy:
			out = append(out, auditShowMeta(c.Group, id))
			helpRun = true
		}
	}
	if helpRun {
		out = append(out, reviewNextDoc{
			Kind:    "help-topic",
			Reason:  "reason-tokens",
			Argv:    []string{"awa", "help", "run"},
			Display: "awa help run",
		})
	}
	for _, a := range res.Availability {
		switch a.State {
		case apprun.AuditCorrupt, apprun.AuditIncompatible, apprun.AuditUnreadable:
			return append(out, reviewNextDoc{
				Kind:    "doctor",
				Reason:  "degraded-evidence",
				Argv:    []string{"awa", "doctor"},
				Display: "awa doctor",
			})
		}
	}
	return out
}

func auditShowMeta(group apprun.AuditGroup, id string) reviewNextDoc {
	return reviewNextDoc{
		Kind:    "run-show-meta",
		Reason:  string(group),
		Argv:    []string{"awa", "run", "show", id, "--meta"},
		Display: "awa run show " + id + " --meta",
	}
}

// auditClassLabel is the canonical stored pair as one token: kind, or kind/reason.
func auditClassLabel(c apprun.AuditClassCount) string {
	if c.Reason == "" {
		return c.Kind.String()
	}
	return c.Kind.String() + "/" + string(c.Reason)
}

func auditExampleShort(e apprun.AuditExamples) string {
	ids := make([]string, 0, len(e.IDs))
	for _, id := range e.IDs {
		ids = append(ids, id.Short())
	}
	s := strings.Join(ids, " ")
	if e.Total > len(e.IDs) {
		s += fmt.Sprintf(" (+%d more)", e.Total-len(e.IDs))
	}
	return s
}

func renderAudit(w *output.Writer, res apprun.AuditResult) {
	switch res.History {
	case apprun.AuditHistoryMissing:
		w.Line("no runs recorded (no run history exists)")
		return
	case apprun.AuditHistoryEmpty:
		w.Line("no runs recorded (run history is empty)")
		return
	}
	older := "no older records"
	if res.Older {
		older = "older records exist"
	}
	w.Linef("window:   newest %d requested, %d selected, %s", res.Requested, res.Selected, older)
	w.Linef("read:     %d classified, %d unavailable, %d metadata bytes read", res.Classified, res.Unavailable, res.BytesRead)

	if len(res.Classes) > 0 {
		groupW, classW := 0, 0
		for _, c := range res.Classes {
			groupW = max(groupW, len(c.Group))
			classW = max(classW, len(auditClassLabel(c)))
		}
		w.Line("")
		w.Line("primary stored reasons:")
		for _, c := range res.Classes {
			w.Linef("  %-*s  %-*s  %5d  %s", groupW, c.Group, classW, auditClassLabel(c), c.Count, auditExampleShort(c.Examples))
		}
	}
	if len(res.Availability) > 0 {
		stateW := 0
		for _, a := range res.Availability {
			stateW = max(stateW, len(a.State))
		}
		w.Line("")
		w.Line("unavailable:")
		for _, a := range res.Availability {
			w.Linef("  %-*s  %5d  %s", stateW, a.State, a.Count, auditExampleShort(a.Examples))
		}
		w.Diagnostic(fmt.Sprintf("warning: %d of %d selected runs were not classified; counts cover only the %d that were", res.Unavailable, res.Selected, res.Classified))
	}

	w.Line("")
	w.Line("These are retained recorded executions, not every invocation: a replayed hit adds")
	w.Line("no record, and capture-disabled or removed runs are absent. Each run counts once by")
	w.Line("its primary stored reason; reusable-when-stored does not mean reusable now.")

	if next := auditSuggestions(res); len(next) > 0 {
		w.Line("")
		w.Line("next:")
		for _, n := range next {
			w.Line("  " + n.Display)
		}
	}
}
