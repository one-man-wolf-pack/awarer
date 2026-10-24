package runcache

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"awarer/internal/domain/hashing"
)

// FactInterpretation is keyed: trimming, parsing or normalizing the stdout would give
// different reports the same identity, so any change needs a new token.
const FactInterpretation = "stdout-exact-bytes"

type FactStatus string

const (
	FactAcquired          FactStatus = "acquired"
	FactDenied            FactStatus = "denied"
	FactMissingExecutable FactStatus = "missing-executable"
	FactStartFailed       FactStatus = "start-failed"
	FactReadFailed        FactStatus = "read-failed"
	FactExitNonzero       FactStatus = "exit-nonzero"
	FactSignaled          FactStatus = "signaled"
	FactTimedOut          FactStatus = "timed-out"
	FactEmptyStdout       FactStatus = "empty-stdout"
	FactStdoutOverflow    FactStatus = "stdout-overflow"
	FactStderrOverflow    FactStatus = "stderr-overflow"
	FactSkipped           FactStatus = "skipped"
	FactNotAcquired       FactStatus = "not-acquired"
	// FactUnassessed is a diagnostic placeholder, never evidence: persisting it or
	// comparing it as a fact would let a diagnostic claim identity it never acquired.
	FactUnassessed FactStatus = "unassessed"
)

var factStatuses = []FactStatus{
	FactAcquired, FactDenied, FactMissingExecutable, FactStartFailed, FactReadFailed,
	FactExitNonzero, FactSignaled, FactTimedOut, FactEmptyStdout, FactStdoutOverflow,
	FactStderrOverflow, FactSkipped, FactNotAcquired,
}

func ParseFactStatus(s string) (FactStatus, error) {
	if st := FactStatus(s); slices.Contains(factStatuses, st) {
		return st, nil
	}
	return "", fmt.Errorf("invalid external fact status %q", s)
}

func (s FactStatus) String() string { return string(s) }

type ProbeDefinition struct {
	// Label is display-only and stays out of the key, so renaming a probe keeps the
	// results it already permits.
	Label string
	Argv  []string
}

type ExternalFact struct {
	Status      FactStatus
	Fingerprint hashing.TreeHash
}

func AcquiredFact(fp hashing.TreeHash) ExternalFact {
	return ExternalFact{Status: FactAcquired, Fingerprint: fp}
}

func UnavailableFact(st FactStatus) ExternalFact { return ExternalFact{Status: st} }

func (f ExternalFact) validate(persisted bool) error {
	if f.Status == FactUnassessed {
		if persisted {
			return fmt.Errorf("an unassessed fact is not recorded evidence")
		}
	} else if !slices.Contains(factStatuses, f.Status) {
		return fmt.Errorf("invalid external fact status %q", f.Status)
	}
	if (f.Status == FactAcquired) == f.Fingerprint.IsZero() {
		return fmt.Errorf("external fact %s has an inconsistent fingerprint", f.Status)
	}
	return nil
}

func (f ExternalFact) String() string {
	if f.Status == FactAcquired {
		return f.Fingerprint.String()
	}
	return "<" + string(f.Status) + ">"
}

func FactsComplete(facts []ExternalFact) bool {
	if len(facts) == 0 {
		return false
	}
	for _, f := range facts {
		if f.Status != FactAcquired {
			return false
		}
	}
	return true
}

func factsUnassessed(facts []ExternalFact) bool {
	return slices.ContainsFunc(facts, func(f ExternalFact) bool { return f.Status == FactUnassessed })
}

func SameFacts(a, b []ExternalFact) bool {
	return FactsComplete(a) && FactsComplete(b) && slices.Equal(a, b)
}

func ValidateFacts(defs []ProbeDefinition, facts []ExternalFact) error {
	if len(facts) != len(defs) {
		return fmt.Errorf("%d external facts recorded for %d identity probes", len(facts), len(defs))
	}
	for i, f := range facts {
		if err := f.validate(true); err != nil {
			return fmt.Errorf("identity probe %d: %w", i, err)
		}
	}
	return nil
}

func validateProbeDefinitions(defs []ProbeDefinition) error {
	labels := make(map[string]bool, len(defs))
	for i, d := range defs {
		if d.Label == "" || d.Label != strings.TrimSpace(d.Label) || strings.ContainsAny(d.Label, "\x00\r\n") {
			return fmt.Errorf("identity probe %d label %q is invalid", i, d.Label)
		}
		if labels[d.Label] {
			return fmt.Errorf("identity probe label %q is duplicated", d.Label)
		}
		labels[d.Label] = true
		if len(d.Argv) == 0 || d.Argv[0] == "" {
			return fmt.Errorf("identity probe %q has no executable", d.Label)
		}
		if i > 0 && bytes.Compare(encodeProbeArgv(defs[i-1].Argv), encodeProbeArgv(d.Argv)) >= 0 {
			return fmt.Errorf("identity probes are not sorted and unique by argv")
		}
	}
	return nil
}

func sortProbeDefinitions(defs []ProbeDefinition) {
	slices.SortFunc(defs, func(a, b ProbeDefinition) int {
		return bytes.Compare(encodeProbeArgv(a.Argv), encodeProbeArgv(b.Argv))
	})
}

func encodeProbeArgv(argv []string) []byte {
	var b bytes.Buffer
	putList(&b, argv)
	return b.Bytes()
}

func sameProbeDefinitions(a, b []ProbeDefinition) bool {
	return slices.EqualFunc(a, b, func(x, y ProbeDefinition) bool { return slices.Equal(x.Argv, y.Argv) })
}

func probesString(defs []ProbeDefinition) string {
	parts := make([]string, len(defs))
	for i, d := range defs {
		parts[i] = d.Label + "=" + fmt.Sprint(d.Argv)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func factsString(defs []ProbeDefinition, facts []ExternalFact) string {
	parts := make([]string, len(facts))
	for i, f := range facts {
		label := ""
		if i < len(defs) {
			label = defs[i].Label
		}
		parts[i] = label + "=" + f.String()
	}
	return "[" + strings.Join(parts, " ") + "]"
}
