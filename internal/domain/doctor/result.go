package doctor

// Summary counts findings by disposition. It is derived from a result's findings,
// never assembled by hand, so the numbers a report shows cannot drift from the
// findings it lists.
type Summary struct {
	Passed     int
	Warnings   int
	Errors     int
	Repairable int
	Repaired   int
}

// DoctorResult aggregates everything one doctor run observed: the checks that
// passed (a count, since a pass has nothing to describe) and the findings it
// recorded. A repair marks its originating finding repaired, so the findings alone
// carry what was fixed; Health and Summary are computed from them, and the verdict
// therefore always matches the evidence.
type DoctorResult struct {
	passed   int
	findings []Finding
}

// NewResult builds a result from a pass count and the findings recorded. The
// findings are copied so later mutation of the input slice cannot change a built
// result.
func NewResult(passed int, findings []Finding) DoctorResult {
	fs := make([]Finding, len(findings))
	copy(fs, findings)
	return DoctorResult{passed: passed, findings: fs}
}

// Findings returns the recorded findings. The returned slice is a copy.
func (r DoctorResult) Findings() []Finding {
	out := make([]Finding, len(r.findings))
	copy(out, r.findings)
	return out
}

// Summary derives the finding counts. Passed mirrors the pass count; Warnings
// counts non-error findings (info and warning severities) and Errors counts error
// findings, so the two partition every finding. Repairable counts findings that can
// be repaired (including ones already repaired); Repaired counts those repaired.
func (r DoctorResult) Summary() Summary {
	s := Summary{Passed: r.passed}
	for _, f := range r.findings {
		switch f.severity {
		case SeverityWarning, SeverityInfo:
			s.Warnings++
		case SeverityError:
			s.Errors++
		}
		if f.repairable {
			s.Repairable++
		}
		if f.repaired {
			s.Repaired++
		}
	}
	return s
}

// Health derives the overall verdict so that "failed" is exactly the condition
// the CLI returns the state-action-required exit code for: any unrepaired error or
// any unrepaired repairable finding. A run is "warning" when only non-repairable
// warnings remain (these are informational and exit 0), and "ok" when nothing
// remains. Repair clears a finding's repaired flag, so fixing the last orphan temp
// turns a failed run ok. This keeps the human "health:" line and the exit code in
// agreement.
func (r DoctorResult) Health() Health {
	if r.NeedsAttention() {
		return HealthFailed
	}
	for _, f := range r.findings {
		if !f.repaired {
			return HealthWarning
		}
	}
	return HealthOK
}

// NeedsAttention reports whether anything actionable remains: an unrepaired error
// or an unrepaired repairable finding. It is the single condition the CLI maps to
// the state-action-required exit code, per the doctor exit contract ("errors or
// unrepaired repairable findings").
func (r DoctorResult) NeedsAttention() bool {
	for _, f := range r.findings {
		if f.repaired {
			continue
		}
		if f.severity == SeverityError || f.repairable {
			return true
		}
	}
	return false
}
