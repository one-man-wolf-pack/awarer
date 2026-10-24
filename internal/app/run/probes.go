package run

import (
	"context"
	"errors"
	"time"

	"awarer/internal/domain/runcache"
)

const (
	probeTimeout      = 10 * time.Second
	probePhaseTimeout = 30 * time.Second
	probeMaxStdout    = 64 << 10
	probeMaxStderr    = 64 << 10
)

var ErrExternalFactsUnassessed = errors.New("the selected validation policy requires identity facts that only awa run acquires; this diagnostic cannot assess current reuse without them")

type factAcquirer func(ctx context.Context, v *runcache.ValidationScope) ([]runcache.ExternalFact, error)

// acquirerFor does not check the grant: buildKeyContext creates one only after it.
func (s *Service) acquirerFor(req Request, childEnv []string) factAcquirer {
	return func(ctx context.Context, v *runcache.ValidationScope) ([]runcache.ExternalFact, error) {
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
				return nil, err
			}
			if res.Status != runcache.FactAcquired {
				facts[i] = runcache.UnavailableFact(res.Status)
				failed = true
				continue
			}
			facts[i] = runcache.AcquiredFact(s.deps.Hasher.HashBytes(res.Stdout))
		}
		return facts, nil
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
