package doctor

import (
	"path/filepath"
	"testing"
)

// TestFindingCodesIsTheClosedVocabulary proves the published enumeration and the
// membership check are one vocabulary rather than two lists that happen to agree:
// every enumerated code is Valid, none is empty or duplicated, and the count is
// pinned so adding a code is a deliberate edit that every consumer walking the
// vocabulary — notably the documentation coverage matrix — sees.
func TestFindingCodesIsTheClosedVocabulary(t *testing.T) {
	codes := FindingCodes()
	if len(codes) != 34 {
		t.Errorf("FindingCodes() has %d entries, want 34 — update the documentation coverage matrix too", len(codes))
	}
	seen := map[FindingCode]bool{}
	for _, c := range codes {
		switch {
		case c == "":
			t.Error("FindingCodes() contains an empty code")
			continue
		case seen[c]:
			t.Errorf("FindingCodes() lists %q twice", c)
		case !c.Valid():
			t.Errorf("enumerated code %q is not Valid; membership and enumeration disagree", c)
		}
		seen[c] = true
	}
}

// TestFindingCodesIsCopySafe proves a consumer cannot reorder or truncate the catalog
// every other consumer reads.
func TestFindingCodesIsCopySafe(t *testing.T) {
	first := FindingCodes()
	original := first[0]
	first[0] = "mutated"
	if got := FindingCodes()[0]; got != original {
		t.Errorf("FindingCodes() shares its backing array: first entry is now %q", got)
	}
}

func TestNewFindingRejectsInvalidInputs(t *testing.T) {
	cases := []struct {
		name      string
		code      FindingCode
		severity  Severity
		subsystem Subsystem
		subject   string
		message   string
	}{
		{"unknown code", FindingCode("not-a-code"), SeverityError, SubsystemRuns, "x", "m"},
		{"invalid severity", CodeRunCorrupt, Severity(0), SubsystemRuns, "x", "m"},
		{"invalid subsystem", CodeRunCorrupt, SeverityError, Subsystem("nope"), "x", "m"},
		{"empty subject", CodeRunCorrupt, SeverityError, SubsystemRuns, "  ", "m"},
		{"empty message", CodeRunCorrupt, SeverityError, SubsystemRuns, "x", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewFinding(c.code, c.severity, c.subsystem, "", c.subject, c.message, false); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

func TestNewFindingValid(t *testing.T) {
	f, err := NewFinding(CodeOrphanTemp, SeverityWarning, SubsystemTemp, ".awa/store/tmp/x", "store/tmp/x", "orphan temp file", true)
	if err != nil {
		t.Fatalf("NewFinding: %v", err)
	}
	if f.Code() != CodeOrphanTemp || f.Severity() != SeverityWarning || !f.Repairable() || f.Repaired() {
		t.Fatalf("unexpected finding state: %+v", f)
	}
}

func TestMarkRepairedRequiresRepairable(t *testing.T) {
	notRepairable, _ := NewFinding(CodeRunCorrupt, SeverityError, SubsystemRuns, "", "run 018f", "corrupt run", false)
	if _, err := notRepairable.MarkRepaired(); err == nil {
		t.Fatalf("expected MarkRepaired to fail for a non-repairable finding")
	}

	repairable, _ := NewFinding(CodeOrphanTemp, SeverityWarning, SubsystemTemp, "p", "s", "m", true)
	got, err := repairable.MarkRepaired()
	if err != nil {
		t.Fatalf("MarkRepaired: %v", err)
	}
	if !got.Repaired() {
		t.Fatalf("expected repaired finding")
	}
}

func TestSummaryAndHealthDerivation(t *testing.T) {
	mkErr := func() Finding {
		f, _ := NewFinding(CodeRunCorrupt, SeverityError, SubsystemRuns, "", "run", "corrupt", false)
		return f
	}
	mkWarn := func() Finding {
		f, _ := NewFinding(CodeAwaTrackedByGit, SeverityWarning, SubsystemGit, "", ".awa", "tracked", false)
		return f
	}
	mkRepairable := func() Finding {
		f, _ := NewFinding(CodeOrphanTemp, SeverityWarning, SubsystemTemp, "p", "s", "m", true)
		return f
	}

	t.Run("clean is ok", func(t *testing.T) {
		r := NewResult(12, nil)
		if r.Health() != HealthOK {
			t.Fatalf("Health = %v, want ok", r.Health())
		}
		if r.NeedsAttention() {
			t.Fatalf("clean result should not need attention")
		}
		if s := r.Summary(); s.Passed != 12 || s.Errors != 0 || s.Warnings != 0 {
			t.Fatalf("unexpected summary %+v", s)
		}
	})

	t.Run("warnings only exit 0 warning health", func(t *testing.T) {
		r := NewResult(3, []Finding{mkWarn()})
		if r.Health() != HealthWarning {
			t.Fatalf("Health = %v, want warning", r.Health())
		}
		if r.NeedsAttention() {
			t.Fatalf("non-repairable warning should not need attention")
		}
	})

	t.Run("unrepaired repairable needs attention", func(t *testing.T) {
		r := NewResult(3, []Finding{mkRepairable()})
		if !r.NeedsAttention() {
			t.Fatalf("unrepaired repairable should need attention")
		}
		if r.Health() != HealthFailed {
			t.Fatalf("Health = %v, want failed", r.Health())
		}
		if s := r.Summary(); s.Repairable != 1 || s.Repaired != 0 {
			t.Fatalf("unexpected summary %+v", s)
		}
	})

	t.Run("repaired repairable becomes ok", func(t *testing.T) {
		fixed, _ := mkRepairable().MarkRepaired()
		r := NewResult(3, []Finding{fixed})
		if r.NeedsAttention() {
			t.Fatalf("repaired finding should not need attention")
		}
		if r.Health() != HealthOK {
			t.Fatalf("Health = %v, want ok", r.Health())
		}
		if s := r.Summary(); s.Repaired != 1 {
			t.Fatalf("unexpected summary %+v", s)
		}
	})

	t.Run("error is failed and needs attention", func(t *testing.T) {
		r := NewResult(3, []Finding{mkErr(), mkWarn()})
		if r.Health() != HealthFailed {
			t.Fatalf("Health = %v, want failed", r.Health())
		}
		if !r.NeedsAttention() {
			t.Fatalf("error should need attention")
		}
		if s := r.Summary(); s.Errors != 1 || s.Warnings != 1 {
			t.Fatalf("unexpected summary %+v", s)
		}
	})
}

func TestNewRepairActionContainment(t *testing.T) {
	awa := filepath.FromSlash("/proj/.awa")

	good := filepath.FromSlash("/proj/.awa/store/tmp/x")
	if _, err := NewRepairAction(RepairRemoveTemp, awa, good, CodeOrphanTemp); err != nil {
		t.Fatalf("expected valid action, got %v", err)
	}

	bad := []string{
		filepath.FromSlash("/proj/other/x"),
		filepath.FromSlash("/proj/.awa/../escape"),
		awa, // the .awa dir itself
	}
	for _, target := range bad {
		if _, err := NewRepairAction(RepairRemoveTemp, awa, target, CodeOrphanTemp); err == nil {
			t.Fatalf("expected rejection for target %q", target)
		}
	}

	if _, err := NewRepairAction(RepairKind(0), awa, good, CodeOrphanTemp); err == nil {
		t.Fatalf("expected rejection for invalid kind")
	}
	if _, err := NewRepairAction(RepairRemoveTemp, awa, good, FindingCode("bogus")); err == nil {
		t.Fatalf("expected rejection for invalid code")
	}
}
