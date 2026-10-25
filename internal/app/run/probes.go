package run

import (
	"context"
	"time"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/runcache"
)

const (
	probeTimeout      = 10 * time.Second
	probePhaseTimeout = 30 * time.Second
	probeMaxStdout    = 64 << 10
	probeMaxStderr    = 64 << 10
)

// ReasonExternalFactsUnacquired is a current-assessment token, not a historical reuse
// reason: no run records it, so it stays outside runcache's persisted vocabulary.
const ReasonExternalFactsUnacquired = "external-facts-unacquired"

type factAcquirer func(ctx context.Context, v *runcache.ValidationScope) ([]runcache.ExternalFact, runcache.ProbeAuthorization, error)

// ProbeGrants never executes or resolves a probe.
type ProbeGrants interface {
	Check(projectRoot string, context hashing.TreeHash) runcache.ProbeAuthorization
}

// The context is built from the run's own argv and cwd: they equal the policy's
// selector, so it is the same context awa config probes allow approved for that policy.
func (s *Service) authorizeProbes(req Request, v *runcache.ValidationScope) runcache.ProbeAuthorization {
	if s.deps.Grants == nil {
		return runcache.ProbeAuthorization{State: runcache.ProbeDenied, Cause: runcache.ProbeCauseNoGrant}
	}
	l, err := req.Project.Paths()
	if err != nil {
		return runcache.ProbeAuthorization{State: runcache.ProbeUnavailable, Cause: runcache.ProbeCauseUnresolvedRoot}
	}
	return s.deps.Grants.Check(l.Root(), runcache.ProbeGrantContext(s.deps.Hasher, req.Argv, req.CWD.String(), v.Probes))
}

func deniedFacts(v *runcache.ValidationScope, st runcache.FactStatus) []runcache.ExternalFact {
	facts := make([]runcache.ExternalFact, len(v.Probes))
	for i := range facts {
		facts[i] = runcache.UnavailableFact(st)
	}
	return facts
}

// acquirerFor checks the grant at the start of every phase it runs, so a revocation
// between the pre-run and post-run phases stops the later one. A probe already
// started is not interrupted by a revocation.
func (s *Service) acquirerFor(req Request, childEnv []string) factAcquirer {
	return func(ctx context.Context, v *runcache.ValidationScope) ([]runcache.ExternalFact, runcache.ProbeAuthorization, error) {
		auth := s.authorizeProbes(req, v)
		if !auth.Granted() {
			return deniedFacts(v, runcache.FactDenied), auth, nil
		}
		facts := make([]runcache.ExternalFact, len(v.Probes))
		phase, cancel := context.WithTimeout(ctx, probePhaseTimeout)
		defer cancel()
		failed := false
		for i, p := range v.Probes {
			if failed {
				facts[i] = runcache.UnavailableFact(runcache.FactSkipped)
				continue
			}
			res := s.deps.Prober.Probe(phase, runcache.ProbeSpec{
				Argv:      p.Argv,
				Dir:       req.AbsDir,
				Env:       childEnv,
				Timeout:   probeTimeout,
				MaxStdout: probeMaxStdout,
				MaxStderr: probeMaxStderr,
			})
			if err := ctx.Err(); err != nil {
				return nil, auth, err
			}
			if res.Status != runcache.FactAcquired {
				facts[i] = runcache.UnavailableFact(res.Status)
				failed = true
				continue
			}
			facts[i] = runcache.AcquiredFact(s.deps.Hasher.HashBytes(res.Stdout))
		}
		return facts, auth, nil
	}
}

func probesDeclared(v *runcache.ValidationScope) bool { return v != nil && len(v.Probes) > 0 }

func postRunFactsReason(pre *runcache.ValidationScope, post []runcache.ExternalFact) runcache.MismatchReason {
	switch {
	case !runcache.FactsComplete(post):
		return runcache.ReasonExternalFactsUnavailablePostRun
	case !runcache.SameFacts(pre.Facts, post):
		return runcache.ReasonExternalFactsChanged
	default:
		return ""
	}
}
