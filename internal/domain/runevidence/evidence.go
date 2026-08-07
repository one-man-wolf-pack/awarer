// Package runevidence is the run-evidence aggregate domain: one validated run record
// composed with its before/after observed-state assessments and typed output
// inspectability, projected as the awa-evidence/v1 provider contract. It owns the
// invariants that make an impossible evidence document unrepresentable — a record that
// did not observe the states it is evidence about, an absent before/after assessment, or a
// before/after pair that belongs to a different run — so the CLI only projects the
// aggregate and never reconstructs availability from booleans and strings.
package runevidence

import (
	"fmt"

	"awarer/internal/domain/provider"
	"awarer/internal/domain/runcache"
)

// Contract is the run-evidence provider contract token. It is versioned
// independently of the nested awa-state/v1 identity semantics: run-evidence
// composition can evolve without changing state-identity meaning, and vice versa.
type Contract string

// ContractEvidenceV1 is the current run-evidence provider contract.
const ContractEvidenceV1 Contract = "awa-evidence/v1"

// Valid reports whether c is a known contract token.
func (c Contract) Valid() bool { return c == ContractEvidenceV1 }

// String renders the contract token.
func (c Contract) String() string { return string(c) }

// Evidence is the composed run-evidence aggregate: one validated immutable run
// record, its before/after observed-state assessments (each a complete awa-state/v1
// assessment — resolved or typed unavailable), and typed output inspectability for
// each captured stream. It is built only through New, which forbids the impossible
// states: an absent before/after assessment, or a before/after assessment that
// belongs to a different run than the record. Neither assessment is nullable — an
// absent, removed, or unreadable observation is represented by a normal unavailable
// assessment, not by a missing field.
type Evidence struct {
	record runcache.RunEntry
	before provider.Assessment
	after  provider.Assessment
	stdout OutputInspectability
	stderr OutputInspectability
}

// New composes the run-evidence aggregate. It requires a validated run record, whose
// own cardinality invariant already guarantees the record observed the states it is
// evidence about — a before observation always, and an after observation unless the
// post-execution scan failed — so an evidence document can never be missing a state
// it claims to describe. The two assessments are the resolution of those observations:
// a before/after pair from a different run (or a swapped before/after) is rejected by
// echoed reference; a resolved assessment must agree with the observation the record
// persisted for that slot (same canonical ref, tree hash, and scan-config policy), so a
// resolved side cannot drift from the durable record or claim a slot the record never
// recorded; and an unavailable assessment is the honest degradation of a persisted
// observation that was since GC-removed or unreadable. Output inspectability facts are
// typed value objects whose zero value is undefined, so the aggregate cannot be
// constructed from a forgotten (falsely "present") one.
func New(record runcache.RunEntry, before, after provider.Assessment, stdout, stderr OutputInspectability) (Evidence, error) {
	if err := record.Validate(); err != nil {
		return Evidence{}, fmt.Errorf("run evidence: invalid run record: %w", err)
	}
	if err := bindsToSlot(before, RunObservationRef(record.ID, true), record.Before); err != nil {
		return Evidence{}, fmt.Errorf("run evidence: before assessment: %w", err)
	}
	if err := bindsToSlot(after, RunObservationRef(record.ID, false), record.After); err != nil {
		return Evidence{}, fmt.Errorf("run evidence: after assessment: %w", err)
	}
	// stdout/stderr are value objects, but their zero value is undefined
	// (NewOutputInspectability is the only valid constructor), so New rejects an
	// undefined one rather than let a forgotten initialization project a false "present".
	if !stdout.Valid() {
		return Evidence{}, fmt.Errorf("run evidence: stdout inspectability is undefined")
	}
	if !stderr.Valid() {
		return Evidence{}, fmt.Errorf("run evidence: stderr inspectability is undefined")
	}
	return Evidence{record: record, before: before, after: after, stdout: stdout, stderr: stderr}, nil
}

// bindsToSlot verifies an assessment belongs to the given before/after run slot and, when
// resolved, agrees with the observation the record actually persisted for that slot. It
// requires the echoed input reference to match; and when the assessment is resolved, it
// requires the resolved identity's canonical reference to match, a persisted observation
// to exist for the slot, and that observation's tree hash and scan-config policy to equal
// the resolved identity's. Together these make an impossible evidence document
// unrepresentable: a resolved assessment naming a different run or slot, a resolved After
// on a record that recorded no after (a post-scan-failed run), and a resolved identity
// whose tree/scan-config drifted from the durable observation between the two independent
// reads. An unavailable assessment carries no identity — an absent/removed observation is
// the honest, non-fatal degradation — so it is bound by its input reference alone.
func bindsToSlot(a provider.Assessment, want string, persisted *runcache.Observation) error {
	if a.InputRef() != want {
		return fmt.Errorf("input ref %q does not match %q", a.InputRef(), want)
	}
	id, resolved := a.Identity()
	if !resolved {
		return nil
	}
	if id.CanonicalRef() != want {
		return fmt.Errorf("resolved identity %q does not match %q", id.CanonicalRef(), want)
	}
	if persisted == nil {
		return fmt.Errorf("resolved %q but the record persisted no observation for this slot", want)
	}
	if id.TreeHash() != persisted.Manifest.TreeHash {
		return fmt.Errorf("resolved tree hash %v does not match the persisted observation's %v", id.TreeHash(), persisted.Manifest.TreeHash)
	}
	if id.Scan().ScanConfig() != persisted.ScanConfigHash {
		return fmt.Errorf("resolved scan-config identity does not match the persisted observation's")
	}
	return nil
}

// RunObservationRef builds the canonical full-id run observation reference
// ("run:<id>:before" / "run:<id>:after"). It is the single authority for the string
// the aggregate's before/after assessments must echo, so the app builder resolves its
// assessments through the very same reference the aggregate validates against — the
// two cannot silently disagree on the grammar.
func RunObservationRef(id runcache.RunID, before bool) string {
	sel := "after"
	if before {
		sel = "before"
	}
	return "run:" + id.String() + ":" + sel
}

// Contract returns the run-evidence provider contract token this aggregate projects.
func (e Evidence) Contract() Contract { return ContractEvidenceV1 }

// Record returns the validated immutable run record.
func (e Evidence) Record() runcache.RunEntry { return e.record }

// Before returns the pre-execution observed-state assessment.
func (e Evidence) Before() provider.Assessment { return e.before }

// After returns the post-execution observed-state assessment.
func (e Evidence) After() provider.Assessment { return e.after }

// Stdout returns the stdout stream's metadata-only inspectability.
func (e Evidence) Stdout() OutputInspectability { return e.stdout }

// Stderr returns the stderr stream's metadata-only inspectability.
func (e Evidence) Stderr() OutputInspectability { return e.stderr }
