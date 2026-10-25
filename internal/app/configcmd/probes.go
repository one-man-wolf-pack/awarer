package configcmd

import (
	"errors"
	"slices"

	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/runcache"
	"awarer/internal/infra/probegrants"
)

// ErrNoProbesDeclared is returned by AllowProbes when no effective policy declares a
// probe, so there is nothing to approve and no record is written.
var ErrNoProbesDeclared = errors.New("no validation policy declares identity probes; there is nothing to approve")

// ProbeAuthorizationView is the operator grant for the current project as inspection
// reports it: separate from configuration and its layer origins, and never a claim
// that a probe ran or that any fact was acquired.
type ProbeAuthorizationView struct {
	Root     string                 `json:"root"`
	State    string                 `json:"state"`
	Cause    string                 `json:"cause,omitempty"`
	Policies []ProbePolicyGrantView `json:"policies"`
	detail   string
}

// Detail may carry local paths, so it stays out of the machine view.
func (v ProbeAuthorizationView) Detail() string { return v.detail }

type ProbePolicyGrantView struct {
	Name     string `json:"name"`
	Approved bool   `json:"approved"`
}

type ProbeApprovalView struct {
	Name           string              `json:"name"`
	Argv           []string            `json:"argv"`
	CWD            string              `json:"cwd"`
	IdentityProbes []IdentityProbeView `json:"identity_probes"`
}

// InspectProbes never executes or resolves a probe.
func InspectProbes(projectRoot string, cfg config.Config, grants *probegrants.Store, h hashing.Hasher) ProbeAuthorizationView {
	g := grants.Load(projectRoot)
	v := ProbeAuthorizationView{Root: g.Root, State: string(g.State), Cause: string(g.Cause), Policies: []ProbePolicyGrantView{}, detail: g.Detail}
	for _, p := range probePolicies(cfg) {
		v.Policies = append(v.Policies, ProbePolicyGrantView{Name: p.Name, Approved: g.Check(runcache.PolicyGrantContext(h, p)).Granted()})
	}
	return v
}

// AllowProbes approves exactly the current effective probe contexts for projectRoot,
// replacing any earlier approval there. It records definitions only: nothing runs and
// no executable is resolved.
func AllowProbes(projectRoot string, cfg config.Config, grants *probegrants.Store, h hashing.Hasher) (string, []ProbeApprovalView, error) {
	policies := probePolicies(cfg)
	if len(policies) == 0 {
		return "", nil, ErrNoProbesDeclared
	}
	contexts := make([]hashing.TreeHash, len(policies))
	approved := make([]ProbeApprovalView, len(policies))
	for i, p := range policies {
		contexts[i] = runcache.PolicyGrantContext(h, p)
		probes := make([]IdentityProbeView, len(p.IdentityProbes))
		for j, pr := range p.IdentityProbes {
			probes[j] = IdentityProbeView{Name: pr.Name, Argv: slices.Clone(pr.Argv)}
		}
		approved[i] = ProbeApprovalView{Name: p.Name, Argv: slices.Clone(p.Argv), CWD: p.CWD, IdentityProbes: probes}
	}
	root, err := grants.Allow(projectRoot, contexts)
	if err != nil {
		return "", nil, err
	}
	return root, approved, nil
}

func probePolicies(cfg config.Config) []config.ValidationPolicy {
	var out []config.ValidationPolicy
	for _, p := range cfg.Run.Validation {
		if len(p.IdentityProbes) > 0 {
			out = append(out, p)
		}
	}
	return out
}
