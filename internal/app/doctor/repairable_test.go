package doctor

import (
	"strings"
	"testing"

	dom "awarer/internal/domain/doctor"
)

// This file pins the repairability contract between a checker and the repair pass.
//
// The contract is deliberately one-directional. repairable is a property of the
// individual finding, decided by observed state, not a property of its code: the same
// code is repairable in one situation and not in another. What must never happen is
// the other half — a finding that claims to be repairable when no repair mechanism
// exists for its code, because `--repair` would then leave it standing and an
// unrepaired repairable finding holds the exit code at 5 forever.

// TestRepairableRequiresARepairKind proves the dangerous direction fails loud at
// construction rather than producing a report `--repair` can never satisfy.
func TestRepairableRequiresARepairKind(t *testing.T) {
	// A code with a repair kind may be marked repairable.
	f := finding(dom.CodeOrphanTemp, dom.SeverityWarning, dom.SubsystemTemp,
		".awa/store/tmp/x", "store/tmp/x", "orphan temp file", true)
	if !f.Repairable() {
		t.Error("orphan-temp with repairable=true should be repairable")
	}

	// A code with no repair kind may not. state-permissions-too-broad is diagnosed
	// and left to the user (its message names the chmod to run), so no repair exists.
	// The panic message is asserted too: it is what tells whoever added the checker
	// which half of the contract they broke.
	func() {
		defer func() {
			msg, _ := recover().(string)
			if msg == "" {
				t.Fatal("marking a code with no repair kind as repairable did not panic; the invariant is not enforced")
			}
			for _, want := range []string{"state-permissions-too-broad", "no repair kind", "exit 5"} {
				if !strings.Contains(msg, want) {
					t.Errorf("panic message %q does not mention %q", msg, want)
				}
			}
		}()
		finding(dom.CodeStatePermissionsTooBroad, dom.SeverityWarning, dom.SubsystemLayout,
			".awa", ".awa", "group-accessible", true)
	}()

	// The same code is fine when it does not claim repairability.
	if f := finding(dom.CodeStatePermissionsTooBroad, dom.SeverityWarning, dom.SubsystemLayout,
		".awa", ".awa", "group-accessible", false); f.Repairable() {
		t.Error("state-permissions-too-broad must not be repairable")
	}
}

// TestSameCodeIsRepairableOrNotByObservedState proves the contract does NOT collapse
// into "the code decides". Both values stay legal for the same code, which is the
// model the checkers actually rely on: a temp artifact old enough to be provably
// abandoned is repairable while a possibly in-flight one is not, and an unreadable
// index is repairable when the file is corrupt but not when it is merely busy or
// blocked by permissions.
func TestSameCodeIsRepairableOrNotByObservedState(t *testing.T) {
	cases := []struct {
		code       dom.FindingCode
		subsystem  dom.Subsystem
		repairable bool
		situation  string
	}{
		{dom.CodeOrphanTemp, dom.SubsystemTemp, true, "old enough to be provably abandoned"},
		{dom.CodeOrphanTemp, dom.SubsystemTemp, false, "recent, so it may belong to a live operation"},
		{dom.CodeIndexUnreadable, dom.SubsystemIndex, true, "the index file is corrupt"},
		{dom.CodeIndexUnreadable, dom.SubsystemIndex, false, "busy, or unreadable for permission or I/O reasons"},
	}
	for _, c := range cases {
		f := finding(c.code, dom.SeverityWarning, c.subsystem, ".awa/x", "x", c.situation, c.repairable)
		if f.Repairable() != c.repairable {
			t.Errorf("%s (%s): Repairable() = %v, want %v", c.code, c.situation, f.Repairable(), c.repairable)
		}
	}
}
