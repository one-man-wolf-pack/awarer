package cli

import (
	"fmt"
	"strings"

	"awarer/internal/domain/runcache"
)

type validationPolicyDoc struct {
	Policy         string   `json:"policy"`
	Replay         string   `json:"replay"`
	UnobservedDirs []string `json:"unobserved_dirs"`
	// Facts project status and fingerprint only: probe output is never shown.
	IdentityProbes []identityProbeDoc `json:"identity_probes,omitempty"`
}

type identityProbeDoc struct {
	Probe  string           `json:"probe"`
	Argv   []string         `json:"argv"`
	Before identityFactDoc  `json:"before"`
	After  *identityFactDoc `json:"after,omitempty"`
}

type identityFactDoc struct {
	Status      string `json:"status"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

func factView(f runcache.ExternalFact) identityFactDoc {
	return identityFactDoc{Status: f.Status.String(), Fingerprint: f.Fingerprint.String()}
}

func validationPolicyView(v *runcache.ValidationScope, post []runcache.ExternalFact) *validationPolicyDoc {
	if v == nil {
		return nil
	}
	doc := &validationPolicyDoc{
		Policy:         v.Label,
		Replay:         runcache.ValidationResultOnly,
		UnobservedDirs: append([]string{}, v.UnobservedDirs...),
	}
	for i, p := range v.Probes {
		pd := identityProbeDoc{Probe: p.Label, Argv: append([]string{}, p.Argv...), Before: factView(v.Facts[i])}
		if i < len(post) {
			after := factView(post[i])
			pd.After = &after
		}
		doc.IdentityProbes = append(doc.IdentityProbes, pd)
	}
	return doc
}

// identityFactsLine summarizes the facts; auth is the live grant check, zero for a
// stored run whose authorization cause was never recorded.
func identityFactsLine(v *runcache.ValidationScope, post []runcache.ExternalFact, hit bool, auth runcache.ProbeAuthorization) string {
	if v == nil || len(v.Probes) == 0 {
		return ""
	}
	n := len(v.Probes)
	if failed, ok := firstUnavailable(v.Probes, v.Facts); ok {
		if failed.status == runcache.FactNotAcquired {
			return "identity probes not run: this run could not be reused anyway"
		}
		if failed.status == runcache.FactDenied {
			return "reuse unavailable: " + probeDenialLine(auth)
		}
		return fmt.Sprintf("reuse unavailable: identity probe %s %s before the run", failed.label, failed.status)
	}
	switch {
	case hit:
		return fmt.Sprintf("%s reported the recorded bytes (declared reports only, not a complete environment identity)", probeCount(n))
	case post == nil:
		return fmt.Sprintf("%s acquired before the run; not reacquired", probeCount(n))
	}
	if failed, ok := firstUnavailable(v.Probes, post); ok {
		return fmt.Sprintf("identity probe %s %s after the run; result not reusable", failed.label, failed.status)
	}
	for i := range post {
		if post[i] != v.Facts[i] {
			return fmt.Sprintf("identity probe %s reported different bytes after the run; result not reusable", v.Probes[i].Label)
		}
	}
	return fmt.Sprintf("%s reported the same bytes before and after the run", probeCount(n))
}

// probeDenialLine says why probes were not run and where to act. It never suggests
// the command itself failed: denial costs reuse, not the run.
func probeDenialLine(auth runcache.ProbeAuthorization) string {
	switch auth.Cause {
	case runcache.ProbeCauseContextChanged:
		return "identity probes changed since they were approved for this project (review with 'awa config effective', approve with 'awa config probes allow')"
	case runcache.ProbeCauseNoGrant:
		return "identity probes not approved for this project (review with 'awa config effective', approve with 'awa config probes allow')"
	case runcache.ProbeCauseNone:
		return "identity probes were not authorized for this project (see 'awa config effective')"
	default:
		return fmt.Sprintf("the identity probe grant is unavailable (%s), so probes were not run (see 'awa config effective')", auth.Cause)
	}
}

type unavailableProbe struct {
	label  string
	status runcache.FactStatus
}

func firstUnavailable(defs []runcache.ProbeDefinition, facts []runcache.ExternalFact) (unavailableProbe, bool) {
	for i, f := range facts {
		if f.Status != runcache.FactAcquired && f.Status != runcache.FactSkipped {
			return unavailableProbe{label: defs[i].Label, status: f.Status}, true
		}
	}
	return unavailableProbe{}, false
}

func probeCount(n int) string {
	if n == 1 {
		return "1 identity probe"
	}
	return fmt.Sprintf("%d identity probes", n)
}

const maxValidationDirsShown = 3

func validationDirsLabel(v *runcache.ValidationScope) string {
	dirs := v.UnobservedDirs
	label := strings.Join(dirs[:min(len(dirs), maxValidationDirsShown)], ", ")
	if extra := len(dirs) - maxValidationDirsShown; extra > 0 {
		label += fmt.Sprintf(" (+%d more)", extra)
	}
	return label
}

func validationReplayLine(v *runcache.ValidationScope) string {
	return fmt.Sprintf("result only (policy %s); not observed or recreated: %s", v.Label, validationDirsLabel(v))
}

func validationPolicyLine(v *runcache.ValidationScope) string {
	return fmt.Sprintf("%s (result-only replay); not observed: %s", v.Label, validationDirsLabel(v))
}
