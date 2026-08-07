package cli

import (
	"strings"
	"time"

	"awarer/internal/domain/provider"
	"awarer/internal/output"
)

// The JSON documents below are the awa-state/v1 wire shape. Every document carries the
// provider contract token in addition to the envelope schema version; ids and hashes are
// always full (never shortened) in JSON. Human output may abbreviate for readability.

type scanIdentityDoc struct {
	ScanConfigHash string `json:"scan_config_hash"`
}

type evidenceBoundaryDoc struct {
	SkippedInputs               int  `json:"skipped_inputs"`
	IgnoredPathsOutsideEvidence bool `json:"ignored_paths_outside_evidence"`
}

type stateProjectionDoc struct {
	Kind             string              `json:"kind"`
	CanonicalRef     string              `json:"canonical_ref"`
	CheckpointID     string              `json:"checkpoint_id,omitempty"`
	RunID            string              `json:"run_id,omitempty"`
	TreeHash         string              `json:"tree_hash"`
	ScanIdentity     scanIdentityDoc     `json:"scan_identity"`
	ObservationTime  string              `json:"observation_time,omitempty"`
	Comparable       bool                `json:"comparable"`
	Reconstructable  bool                `json:"reconstructable"`
	EvidenceBoundary evidenceBoundaryDoc `json:"evidence_boundary"`
}

type resolveDoc struct {
	ProviderContract string              `json:"provider_contract"`
	InputRef         string              `json:"input_ref"`
	Outcome          string              `json:"outcome"`
	State            *stateProjectionDoc `json:"state,omitempty"`
	Reason           string              `json:"reason,omitempty"`
}

// countsDoc is the complete change cardinality of a difference, split by status. Every
// field is required, skipped included: a zero is an exact counted fact, not an
// inapplicable concept, and omitting it would leave a consumer unable to tell "no skipped
// transitions" from "this build does not report them". It is the delta between the two
// operands and is unrelated to their evidence_boundary.skipped_inputs.
type countsDoc struct {
	Added       int `json:"added"`
	Modified    int `json:"modified"`
	Deleted     int `json:"deleted"`
	TypeChanged int `json:"type_changed"`
	Skipped     int `json:"skipped"`
}

type completenessDoc struct {
	Complete bool `json:"complete"`
}

type compareDoc struct {
	ProviderContract string              `json:"provider_contract"`
	Outcome          string              `json:"outcome"`
	Left             *stateProjectionDoc `json:"left,omitempty"`
	Right            *stateProjectionDoc `json:"right,omitempty"`
	Counts           *countsDoc          `json:"counts,omitempty"`
	Completeness     *completenessDoc    `json:"completeness,omitempty"`
	Reason           string              `json:"reason,omitempty"`
}

func projectionDoc(id provider.Identity) stateProjectionDoc {
	sc := id.Scan()
	// Every resolved identity carries a complete scan policy identity, so scan_config_hash
	// is always present in the awa-state/v1 wire shape.
	scan := scanIdentityDoc{
		ScanConfigHash: sc.ScanConfig().String(),
	}
	d := stateProjectionDoc{
		Kind:            id.Kind().String(),
		CanonicalRef:    id.CanonicalRef(),
		CheckpointID:    id.CheckpointID(),
		RunID:           id.RunID(),
		TreeHash:        id.TreeHash().String(),
		ScanIdentity:    scan,
		Comparable:      id.Comparable(),
		Reconstructable: id.Reconstructable(),
		EvidenceBoundary: evidenceBoundaryDoc{
			SkippedInputs:               id.Boundary().SkippedInputs(),
			IgnoredPathsOutsideEvidence: id.Boundary().IgnoredOutsideEvidence(),
		},
	}
	if t, ok := id.ObservedAt(); ok {
		d.ObservationTime = t.UTC().Format(time.RFC3339Nano)
	}
	return d
}

func projectionDocPtr(id provider.Identity, ok bool) *stateProjectionDoc {
	if !ok {
		return nil
	}
	p := projectionDoc(id)
	return &p
}

// assessmentDoc projects one provider assessment into the awa-state/v1 wire document.
// It is the single projection path for a resolved-state assessment: `state resolve`
// renders it directly, and the run-evidence document nests it verbatim as
// states.before/after, so the standalone command and the nested value can never drift.
func assessmentDoc(a provider.Assessment) resolveDoc {
	doc := resolveDoc{
		ProviderContract: provider.ContractStateV1.String(),
		InputRef:         a.InputRef(),
		Outcome:          a.Outcome().String(),
	}
	if id, ok := a.Identity(); ok {
		doc.State = projectionDocPtr(id, true)
	}
	if reason, ok := a.Reason(); ok {
		doc.Reason = reason.String()
	}
	return doc
}

// emitResolve writes the resolve assessment as JSON or a compact human report.
func emitResolve(w *output.Writer, inv invocation, a provider.Assessment) error {
	if inv.options.JSON {
		return w.JSON("state.resolve", assessmentDoc(a))
	}
	renderResolveHuman(w, a)
	return nil
}

func renderResolveHuman(w *output.Writer, a provider.Assessment) {
	w.Linef("%s  %s", a.Outcome(), a.InputRef())
	if reason, ok := a.Reason(); ok {
		w.Linef("  reason           %s", reason)
		return
	}
	id, ok := a.Identity()
	if !ok {
		return
	}
	// kind + ref fully identify the state for a human; the record id equals the
	// canonical ref (a checkpoint) or is a subset of it (a run), so it is not repeated
	// here. JSON still carries checkpoint_id/run_id explicitly for machine consumers.
	w.Linef("  kind             %s", id.Kind())
	w.Linef("  ref              %s", shortRef(id.CanonicalRef()))
	w.Linef("  tree             %s", shortDigest(id.TreeHash().String()))
	w.Linef("  scan             %s", scanHuman(id.Scan()))
	if t, ok := id.ObservedAt(); ok {
		w.Linef("  observed         %s", t.UTC().Format(time.RFC3339))
	}
	w.Linef("  comparable       %s", yesNo(id.Comparable()))
	w.Linef("  reconstructable  %s", yesNo(id.Reconstructable()))
	w.Linef("  skipped inputs   %d", id.Boundary().SkippedInputs())
}

// emitCompare writes the comparison as JSON or a compact human report.
func emitCompare(w *output.Writer, inv invocation, c provider.Comparison) error {
	if inv.options.JSON {
		doc := compareDoc{
			ProviderContract: provider.ContractStateV1.String(),
			Outcome:          c.Relationship().String(),
		}
		left, lok := c.Left()
		right, rok := c.Right()
		doc.Left = projectionDocPtr(left, lok)
		doc.Right = projectionDocPtr(right, rok)
		if counts, ok := c.Counts(); ok {
			doc.Counts = &countsDoc{
				Added:       counts.Added(),
				Modified:    counts.Modified(),
				Deleted:     counts.Deleted(),
				TypeChanged: counts.TypeChanged(),
				Skipped:     counts.Skipped(),
			}
		}
		if c.Complete() {
			doc.Completeness = &completenessDoc{Complete: true}
		}
		if reason, ok := c.Reason(); ok {
			doc.Reason = reason.String()
		}
		return w.JSON("state.compare", doc)
	}
	renderCompareHuman(w, c)
	return nil
}

func renderCompareHuman(w *output.Writer, c provider.Comparison) {
	w.Linef("%s", c.Relationship())
	if left, ok := c.Left(); ok {
		w.Linef("  left             %s %s", left.Kind(), shortRef(left.CanonicalRef()))
	}
	if right, ok := c.Right(); ok {
		w.Linef("  right            %s %s", right.Kind(), shortRef(right.CanonicalRef()))
	}
	if counts, ok := c.Counts(); ok {
		w.Linef("  changes          +%d ~%d -%d (type-changed %d, skipped %d)", counts.Added(), counts.Modified(), counts.Deleted(), counts.TypeChanged(), counts.Skipped())
	}
	if c.Complete() {
		w.Line("  complete         yes")
	}
	if reason, ok := c.Reason(); ok {
		w.Linef("  reason           %s", reason)
	}
}

// shortDigest abbreviates a "blake3:hex" digest for human output, keeping the
// prefix and the first 12 hex characters. JSON always carries the full digest.
func shortDigest(s string) string {
	i := strings.IndexByte(s, ':')
	if i < 0 || len(s)-i-1 <= 12 {
		return s
	}
	return s[:i+1+12] + "…"
}

// shortID abbreviates a full record id for human output. JSON always carries the full id.
func shortID(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12] + "…"
}

// shortRef abbreviates a canonical reference for human output: a run reference keeps its
// shape but shortens the embedded id; anything else is treated as a plain id/token.
func shortRef(s string) string {
	if strings.HasPrefix(s, "run:") {
		parts := strings.SplitN(s[len("run:"):], ":", 2)
		if len(parts) == 2 {
			return "run:" + shortID(parts[0]) + ":" + parts[1]
		}
	}
	if strings.Contains(s, ":") {
		return shortDigest(s)
	}
	return shortID(s)
}

func scanHuman(s provider.ScanIdentity) string {
	return shortDigest(s.ScanConfig().String())
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
