package runcache_test

import (
	"testing"

	"awarer/internal/domain/runcache"
	"awarer/internal/infra/blake3hash"
)

var probeDefs = []runcache.ProbeDefinition{
	{Label: "tool", Argv: []string{"tool", "--version"}},
	{Label: "linker", Argv: []string{"linker", "-v"}},
}

func probedInput(t *testing.T, defs []runcache.ProbeDefinition, facts ...runcache.ExternalFact) runcache.KeyInput {
	t.Helper()
	v, err := runcache.NewValidationScope("check", []string{"target"}, defs)
	if err != nil {
		t.Fatal(err)
	}
	if facts != nil {
		if v, err = v.WithFacts(facts); err != nil {
			t.Fatal(err)
		}
	}
	in := baseKeyInput(t)
	in.Validation = &v
	return in
}

func TestFactsBindTheKeyOnlyWhenProbesAreDeclared(t *testing.T) {
	h := blake3hash.New()
	fp := func(s string) runcache.ExternalFact { return runcache.AcquiredFact(h.HashBytes([]byte(s))) }
	plain := baseKeyInput(t)
	plain.Validation = validationScope(t, "check", "target")
	none, err := runcache.NewValidationScope("check", []string{"target"}, []runcache.ProbeDefinition{})
	if err != nil {
		t.Fatal(err)
	}
	empty := baseKeyInput(t)
	empty.Validation = &none
	if plain.Compute(h) != empty.Compute(h) {
		t.Error("a policy without probes no longer keys like its predecessor")
	}

	base := probedInput(t, probeDefs, fp("linker 7"), fp("tool 1\n"))
	if base.Compute(h) == plain.Compute(h) {
		t.Error("declared probes left the key unchanged")
	}
	trailing := probedInput(t, probeDefs, fp("linker 7"), fp("tool 1\n "))
	if trailing.Compute(h) == base.Compute(h) {
		t.Error("the fingerprint is not over the exact bytes")
	}
	relabeled := probedInput(t, []runcache.ProbeDefinition{
		{Label: "compiler", Argv: probeDefs[0].Argv}, probeDefs[1],
	}, fp("linker 7"), fp("tool 1\n"))
	if relabeled.Compute(h) != base.Compute(h) || !runcache.CompareKeyInputs(base, relabeled).Equal() {
		t.Error("a probe label is part of identity")
	}
	denied := probedInput(t, probeDefs, runcache.UnavailableFact(runcache.FactDenied), runcache.UnavailableFact(runcache.FactDenied))
	if denied.Compute(h) == base.Compute(h) {
		t.Error("an unavailable phase keys like a complete one")
	}

	cmp := runcache.CompareKeyInputs(base, trailing)
	if cmp.PrimaryReason() != runcache.DiffExternalFactsChanged || runcache.ReasonFromDiffCode(cmp.PrimaryReason()) != runcache.ReasonExternalFactsChanged {
		t.Errorf("changed report compares as %+v", cmp.Differences)
	}
	cmp = runcache.CompareKeyInputs(base, denied)
	if cmp.PrimaryReason() != runcache.DiffExternalFactsUnavailable || runcache.ReasonFromDiffCode(cmp.PrimaryReason()) != runcache.ReasonExternalFactsUnavailable {
		t.Errorf("unavailable report compares as %+v", cmp.Differences)
	}
	redefined := probedInput(t, []runcache.ProbeDefinition{
		{Label: "tool", Argv: []string{"tool", "-V"}}, probeDefs[1],
	}, fp("linker 7"), fp("tool 1\n"))
	if cmp := runcache.CompareKeyInputs(base, redefined); cmp.Equal() || cmp.PrimaryReason() != runcache.DiffScopeChanged {
		t.Errorf("a changed probe argv compares as %+v", cmp.Differences)
	}
}

func TestUnassessedFactsNeverMatchAndAreNeverEvidence(t *testing.T) {
	h := blake3hash.New()
	acquired := runcache.AcquiredFact(h.HashBytes([]byte("x")))
	current := probedInput(t, probeDefs)
	if !current.Validation.FactsUnassessed() {
		t.Fatal("a scope without acquisition claims facts")
	}
	recorded := probedInput(t, probeDefs, acquired, acquired)
	if cmp := runcache.CompareKeyInputs(recorded, current); cmp.PrimaryReason() == runcache.DiffExternalFactsChanged || cmp.PrimaryReason() == runcache.DiffExternalFactsUnavailable {
		t.Errorf("an unassessed side was compared as evidence: %+v", cmp.Differences)
	}
	if current.Compute(h) == recorded.Compute(h) {
		t.Error("an unassessed scope keys like an acquired one")
	}
	if err := current.Validation.Validate(); err == nil {
		t.Error("an unassessed scope validates as persisted evidence")
	}
	if _, err := current.Validation.WithFacts([]runcache.ExternalFact{{Status: runcache.FactDenied, Fingerprint: acquired.Fingerprint}, acquired}); err == nil {
		t.Error("an unavailable fact accepted a fingerprint")
	}
	if _, err := current.Validation.WithFacts([]runcache.ExternalFact{acquired}); err == nil {
		t.Error("a phase with the wrong fact count validated")
	}
}

func TestARecordedUnavailablePhaseIsNotAChangedFact(t *testing.T) {
	h := blake3hash.New()
	denied := probedInput(t, probeDefs, runcache.UnavailableFact(runcache.FactDenied), runcache.UnavailableFact(runcache.FactDenied))
	acquired := probedInput(t, probeDefs, runcache.AcquiredFact(h.HashBytes([]byte("a"))), runcache.AcquiredFact(h.HashBytes([]byte("b"))))
	if cmp := runcache.CompareKeyInputs(denied, acquired); cmp.PrimaryReason() != runcache.DiffExternalFactsUnavailable {
		t.Fatalf("recorded denial vs acquired compares as %+v", cmp.Differences)
	}
}
