package run

import (
	"context"
	"errors"
	"testing"
	"time"

	"awarer/internal/app/scanner"
	"awarer/internal/domain/config"
	"awarer/internal/domain/runcache"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/projfs"
)

// The stubs below satisfy the run service's ports for verdict-path tests, which
// only exercise Store.Lookup. The other methods are never called.

type stubScanner struct{}

func (stubScanner) Scan(context.Context, projfs.Project, config.Config, config.ScanScope, scanner.Options) (scanner.Result, error) {
	return scanner.Result{}, nil
}

type stubRunner struct{}

func (stubRunner) Run(context.Context, runcache.RunSpec) (runcache.RunResult, error) {
	return runcache.RunResult{}, nil
}

type stubResolver struct{}

func (stubResolver) Resolve(string, string, []string) (string, string, bool) { return "", "", false }

type stubEnv struct{}

func (stubEnv) Lookup(string) (string, bool) { return "", false }

// stubEffectObserver satisfies the required observer dependency. Unlike the stubs
// above it cannot return a zero value: the zero EffectObservation is an invalid
// identity no execution can produce, so handing one back would recreate the very
// state this model forbids. The verdict path takes a prebuilt key context and never
// observes, so the call fails loudly instead.
type stubEffectObserver struct{}

func (stubEffectObserver) Observe(string, []string) (runcache.EffectObservation, runcache.EffectReport, error) {
	panic("verdict path must not observe effect state")
}

type stubClock struct{}

func (stubClock) Now() time.Time { return time.Unix(0, 0) }

// errStore returns a non-corruption I/O error from Lookup; every other method is
// inherited from the embedded nil interface and is unused on the verdict path.
type errStore struct {
	runcache.Store
}

func (errStore) Lookup(runcache.RunKey) (runcache.CacheEntry, bool, error) {
	return runcache.CacheEntry{}, false, errors.New("disk on fire")
}

func newVerdictService(t *testing.T) *Service {
	t.Helper()
	hasher := blake3hash.New()
	return New(Deps{
		Scanner:        stubScanner{},
		Store:          errStore{},
		Runner:         stubRunner{},
		Resolver:       stubResolver{},
		EffectObserver: stubEffectObserver{},
		Env:            stubEnv{},
		Clock:          stubClock{},
		Hasher:         hasher,
		AwaVersion:     "test",
	})
}

func TestCurrentVerdictNonCacheableToleratesLookupError(t *testing.T) {
	s := newVerdictService(t)
	// A non-cacheable run never reads the cache, so the informational lookup error
	// must not fail the diagnostic: the outcome stays policy-driven and the trouble
	// is surfaced as a warning.
	v, err := s.currentVerdict(
		Request{Policy: Policy{NoCache: true}},
		keyContext{Reason: "no-cache", BaseStatus: runcache.CacheDisabled},
	)
	if err != nil {
		t.Fatalf("non-cacheable verdict must tolerate a lookup error, got %v", err)
	}
	if v.Outcome != OutcomeDisabled || v.Reason != "no-cache" {
		t.Errorf("verdict = %s/%s, want disabled/no-cache", v.Outcome, v.Reason)
	}
	if len(v.Warnings) == 0 {
		t.Error("expected a warning about the unreadable exact entry")
	}
}

func TestCurrentVerdictAuthoritativePropagatesLookupError(t *testing.T) {
	s := newVerdictService(t)
	// When the run would actually read the cache, a lookup I/O error is real and must
	// propagate, matching Service.Run rather than being masked as a miss.
	if _, err := s.currentVerdict(Request{}, keyContext{CanRead: true}); err == nil {
		t.Error("authoritative verdict should propagate a lookup error")
	}
}
