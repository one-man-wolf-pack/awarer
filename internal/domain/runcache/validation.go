package runcache

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"awarer/internal/domain/config"
)

const ValidationResultOnly = "result-only"

// ValidationScope holds no argv or cwd because the policy's selectors are the key's own
// command and cwd.
type ValidationScope struct {
	// Label is the policy's diagnostic name. It is retained for presentation and
	// deliberately kept out of the key and of CompareKeyInputs: renaming a policy
	// must not invalidate the results it already permits.
	Label          string
	UnobservedDirs []string
	Probes         []ProbeDefinition
	Facts          []ExternalFact
}

func NewValidationScope(label string, dirs []string, probes []ProbeDefinition) (ValidationScope, error) {
	v := ValidationScope{Label: label, UnobservedDirs: slices.Clone(dirs)}
	slices.Sort(v.UnobservedDirs)
	for _, p := range probes {
		v.Probes = append(v.Probes, ProbeDefinition{Label: p.Label, Argv: slices.Clone(p.Argv)})
	}
	sortProbeDefinitions(v.Probes)
	for range v.Probes {
		v.Facts = append(v.Facts, UnavailableFact(FactUnassessed))
	}
	if err := v.validate(false); err != nil {
		return ValidationScope{}, err
	}
	return v, nil
}

func (v ValidationScope) WithFacts(facts []ExternalFact) (ValidationScope, error) {
	out := v
	out.Facts = slices.Clone(facts)
	if err := out.validate(true); err != nil {
		return ValidationScope{}, err
	}
	return out, nil
}

func (v *ValidationScope) FactsUnassessed() bool {
	return v != nil && factsUnassessed(v.Facts)
}

func (v *ValidationScope) FactsComplete() bool {
	return v == nil || len(v.Probes) == 0 || FactsComplete(v.Facts)
}

func (v ValidationScope) Validate() error { return v.validate(true) }

func (v ValidationScope) validate(persisted bool) error {
	if err := validateProbeDefinitions(v.Probes); err != nil {
		return fmt.Errorf("validation policy %q: %w", v.Label, err)
	}
	if len(v.Facts) != len(v.Probes) {
		return fmt.Errorf("validation policy %q has %d external facts for %d identity probes", v.Label, len(v.Facts), len(v.Probes))
	}
	for i, f := range v.Facts {
		if err := f.validate(persisted); err != nil {
			return fmt.Errorf("validation policy %q identity probe %d: %w", v.Label, i, err)
		}
	}
	if v.Label == "" || v.Label != strings.TrimSpace(v.Label) || strings.ContainsAny(v.Label, "\x00\r\n") {
		return fmt.Errorf("validation policy label %q is invalid", v.Label)
	}
	if len(v.UnobservedDirs) == 0 {
		return fmt.Errorf("validation policy %q waives no directory", v.Label)
	}
	for i, d := range v.UnobservedDirs {
		if p := config.ValidationDirProblem(d); p != "" {
			return fmt.Errorf("validation policy %q unobserved directory %d: %s", v.Label, i, p)
		}
		if i > 0 {
			prev := v.UnobservedDirs[i-1]
			if prev >= d {
				return fmt.Errorf("validation policy %q unobserved directories are not sorted and unique", v.Label)
			}
		}
		for _, other := range v.UnobservedDirs[:i] {
			if config.PathWithin(d, other) {
				return fmt.Errorf("validation policy %q unobserved directory %q lies within %q", v.Label, d, other)
			}
		}
	}
	return nil
}

func sameValidationSemantics(a, b *ValidationScope) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return slices.Equal(a.UnobservedDirs, b.UnobservedDirs) && sameProbeDefinitions(a.Probes, b.Probes)
}

func validationString(v *ValidationScope) string {
	if v == nil {
		return "<none>"
	}
	if len(v.Probes) > 0 {
		return fmt.Sprintf("%s unobserved=%v probes=%s", ValidationResultOnly, v.UnobservedDirs, probesString(v.Probes))
	}
	return fmt.Sprintf("%s unobserved=%v", ValidationResultOnly, v.UnobservedDirs)
}

// putValidation appends the keyed facts of a selected policy. An unselected run
// appends nothing, so an ordinary key keeps its predecessor bytes; because every
// earlier field is length-prefixed, the suffix's presence alone separates the two.
func putValidation(b *bytes.Buffer, v *ValidationScope) {
	if v == nil {
		return
	}
	putString(b, "validation")
	putString(b, ValidationResultOnly)
	putList(b, v.UnobservedDirs)
	// A policy without probes keeps its predecessor key bytes.
	if len(v.Probes) == 0 {
		return
	}
	putString(b, "identity-probes")
	putString(b, FactInterpretation)
	putUint(b, uint64(len(v.Probes)))
	for i, p := range v.Probes {
		putList(b, p.Argv)
		putString(b, v.Facts[i].Status.String())
		putString(b, v.Facts[i].Fingerprint.String())
	}
}
