package runcache

import (
	"bytes"
	"slices"

	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
)

// probeGrantContextTag versions the approved-context encoding: a change to what a grant
// approves needs a new tag, so an old approval can never cover a new meaning.
const probeGrantContextTag = "probe-grant-context/v1"

// ProbeDefinitions converts a policy's configured probes to the definitions a run keys
// and a grant approves, so both read one interpretation of the same declaration.
func ProbeDefinitions(probes []config.IdentityProbe) []ProbeDefinition {
	out := make([]ProbeDefinition, len(probes))
	for i, p := range probes {
		out[i] = ProbeDefinition{Label: p.Name, Argv: slices.Clone(p.Argv)}
	}
	return out
}

// ProbeGrantContext identifies the probe context a grant approves for one policy: the
// policy's invocation selector, its literal probe argv and their interpretation. Labels
// stay out, so renaming keeps approval; observed fingerprints and executable bytes stay
// out too, because a grant is consent to run these commands, not proof of what they are.
func ProbeGrantContext(h hashing.Hasher, argv []string, cwd string, probes []ProbeDefinition) hashing.TreeHash {
	defs := slices.Clone(probes)
	sortProbeDefinitions(defs)
	var b bytes.Buffer
	putString(&b, probeGrantContextTag)
	putList(&b, argv)
	putString(&b, cwd)
	putString(&b, FactInterpretation)
	putUint(&b, uint64(len(defs)))
	for _, d := range defs {
		putList(&b, d.Argv)
	}
	return h.HashBytes(b.Bytes())
}

func PolicyGrantContext(h hashing.Hasher, p config.ValidationPolicy) hashing.TreeHash {
	return ProbeGrantContext(h, p.Argv, p.CWD, ProbeDefinitions(p.IdentityProbes))
}

type ProbeAuthorizationState string

const (
	ProbeGranted     ProbeAuthorizationState = "granted"
	ProbeDenied      ProbeAuthorizationState = "denied"
	ProbeUnavailable ProbeAuthorizationState = "unavailable"
)

// ProbeAuthorizationCause names why a check did not grant. Every non-granted state
// is denial for acquisition; the cause only tells the operator what to fix.
type ProbeAuthorizationCause string

const (
	ProbeCauseNone               ProbeAuthorizationCause = ""
	ProbeCauseNoGrant            ProbeAuthorizationCause = "no-grant"
	ProbeCauseContextChanged     ProbeAuthorizationCause = "context-not-approved"
	ProbeCauseUnresolvedRoot     ProbeAuthorizationCause = "unresolved-root"
	ProbeCauseStoreUnreadable    ProbeAuthorizationCause = "store-unreadable"
	ProbeCauseStoreInvalid       ProbeAuthorizationCause = "store-invalid"
	ProbeCauseStoreInsideProject ProbeAuthorizationCause = "store-inside-project"
)

type ProbeAuthorization struct {
	State ProbeAuthorizationState
	Cause ProbeAuthorizationCause
}

func (a ProbeAuthorization) Granted() bool { return a.State == ProbeGranted }
