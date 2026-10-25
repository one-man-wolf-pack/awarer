package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"testing"
	"time"

	"awarer/internal/app/scanner"
	"awarer/internal/domain/config"
	"awarer/internal/domain/runcache"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/projfs"
)

// The stubs below satisfy run service ports the verdict path never calls.

type stubScanner struct{}

func (stubScanner) Scan(context.Context, projfs.Project, config.Config, config.ScanScope, scanner.Options) (scanner.Result, error) {
	return scanner.Result{}, nil
}

type stubRunner struct{}

func (stubRunner) Run(context.Context, runcache.RunSpec) (runcache.RunResult, error) {
	return runcache.RunResult{}, nil
}

type stubProber struct{}

func (stubProber) Probe(context.Context, runcache.ProbeSpec) runcache.ProbeResult {
	return runcache.ProbeResult{Status: runcache.FactStartFailed}
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

func (stubEffectObserver) Observe(string, []string, []string) (runcache.EffectObservation, runcache.EffectReport, error) {
	panic("verdict path must not observe effect state")
}

type stubClock struct{}

func (stubClock) Now() time.Time { return time.Unix(0, 0) }

func newVerdictService(t *testing.T) *Service {
	t.Helper()
	hasher := blake3hash.New()
	return New(Deps{
		Scanner:        stubScanner{},
		Store:          verdictStore{},
		Runner:         stubRunner{},
		Prober:         stubProber{},
		Resolver:       stubResolver{},
		EffectObserver: stubEffectObserver{},
		Env:            stubEnv{},
		Clock:          stubClock{},
		Hasher:         hasher,
		AwaVersion:     "test",
	})
}

type verdictStore struct {
	runcache.Store
	entry     *runcache.RunEntry
	lookupErr error
	stdoutErr error
	// onLookup and onStdout model a reader that ignores cancellation: they run inside
	// the read, which then completes normally.
	onLookup func()
	onStdout func()
}

func (s verdictStore) Lookup(runcache.RunKey) (runcache.CacheEntry, bool, error) {
	if s.onLookup != nil {
		s.onLookup()
	}
	switch {
	case s.lookupErr != nil:
		return runcache.CacheEntry{}, false, s.lookupErr
	case s.entry == nil:
		return runcache.CacheEntry{}, false, nil
	}
	return runcache.NewCacheEntryUnchecked(*s.entry), true, nil
}

func (s verdictStore) OpenStdout(runcache.RunID) (io.ReadCloser, error) {
	if s.onStdout != nil {
		s.onStdout()
	}
	if s.stdoutErr != nil {
		return nil, s.stdoutErr
	}
	return io.NopCloser(strings.NewReader("")), nil
}

func (verdictStore) OpenStderr(runcache.RunID) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func TestCurrentVerdictAssessesTheExactEntryIndependentlyOfTheDecision(t *testing.T) {
	id, err := runcache.NewRunID(1, strings.NewReader(strings.Repeat("e", 16)))
	if err != nil {
		t.Fatal(err)
	}
	entry := runcache.RunEntry{ID: id}
	ioErr := &fs.PathError{Op: "open", Path: "entry", Err: fs.ErrPermission}
	corrupt := fmt.Errorf("%w: damaged", runcache.ErrCorruptStore)

	reads := []struct {
		name    string
		store   verdictStore
		want    ExactAssessment
		found   bool
		healthy bool
		warns   bool
	}{
		{"absent", verdictStore{}, ExactComplete, false, false, false},
		{"found healthy", verdictStore{entry: &entry}, ExactComplete, true, true, false},
		{"found unhealthy", verdictStore{entry: &entry, stdoutErr: corrupt}, ExactComplete, true, false, true},
		{"lookup io error", verdictStore{lookupErr: ioErr}, ExactLookupUnavailable, false, false, true},
		{"lookup corrupt", verdictStore{lookupErr: corrupt}, ExactLookupUnavailable, false, false, true},
		{"verification io error", verdictStore{entry: &entry, stdoutErr: ioErr}, ExactHealthUnavailable, true, false, true},
	}
	policies := []struct {
		name    string
		req     Request
		kc      keyContext
		outcome Outcome
		reason  string
	}{
		{"no-cache", Request{Policy: Policy{NoCache: true}}, keyContext{Reason: runcache.ReasonNoCache}, OutcomeDisabled, "no-cache"},
		{"stdin pipe", Request{StdinMode: runcache.StdinPipe}, keyContext{Reason: runcache.ReasonStdinNotKeyed}, OutcomeUncached, "stdin-not-keyed"},
		{"stdin tty", Request{StdinMode: runcache.StdinTTY}, keyContext{Reason: runcache.ReasonStdinNotKeyed}, OutcomeUncached, "tty-not-keyed"},
		{"capture disabled", Request{}, keyContext{Reason: runcache.ReasonCaptureDisabled}, OutcomeUncached, "capture-disabled"},
		{"skipped inputs", Request{}, keyContext{Reason: runcache.ReasonSkippedInputs}, OutcomeUncached, "skipped-inputs"},
		{"facts denied", Request{}, keyContext{Reason: runcache.ReasonExternalFactsUnavailable}, OutcomeUncached, "external-facts-unavailable"},
		{"refresh", Request{Policy: Policy{Refresh: true}}, keyContext{}, OutcomeMiss, reasonRefreshRequested},
	}
	for _, p := range policies {
		for _, r := range reads {
			t.Run(p.name+"/"+r.name, func(t *testing.T) {
				s := newVerdictService(t)
				s.deps.Store = r.store
				v, err := s.currentVerdict(context.Background(), p.req, p.kc)
				if err != nil {
					t.Fatalf("informational verdict failed: %v", err)
				}
				if v.Outcome != p.outcome || v.Reason != p.reason {
					t.Errorf("verdict = %s/%s, want %s/%s", v.Outcome, v.Reason, p.outcome, p.reason)
				}
				if v.Exact.Assessment != r.want || (v.Exact.Hit != nil) != r.found || (len(v.Warnings) > 0) != r.warns {
					t.Fatalf("exact = %+v warnings %q, want assessment %d found %v", v.Exact, v.Warnings, r.want, r.found)
				}
				if r.found && (v.Exact.Hit.RunID != id || v.Exact.Hit.Healthy != r.healthy) {
					t.Errorf("hit = %+v, want run %s healthy %v", v.Exact.Hit, id, r.healthy)
				}
			})
		}
	}
}

func TestCurrentVerdictAuthoritativeReadsKeepTheirContract(t *testing.T) {
	id, err := runcache.NewRunID(1, strings.NewReader(strings.Repeat("a", 16)))
	if err != nil {
		t.Fatal(err)
	}
	entry := runcache.RunEntry{ID: id}
	ioErr := &fs.PathError{Op: "open", Path: "entry", Err: fs.ErrPermission}
	corrupt := fmt.Errorf("%w: damaged", runcache.ErrCorruptStore)
	cases := []struct {
		name    string
		store   verdictStore
		fails   bool
		outcome Outcome
		want    ExactAssessment
	}{
		{"absent", verdictStore{}, false, OutcomeMiss, ExactComplete},
		{"found healthy", verdictStore{entry: &entry}, false, OutcomeHit, ExactComplete},
		{"found unhealthy", verdictStore{entry: &entry, stdoutErr: corrupt}, false, OutcomeCorrupt, ExactComplete},
		{"corrupt lookup", verdictStore{lookupErr: corrupt}, false, OutcomeCorrupt, ExactLookupUnavailable},
		{"lookup io error", verdictStore{lookupErr: ioErr}, true, "", 0},
		{"verification io error", verdictStore{entry: &entry, stdoutErr: ioErr}, true, "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newVerdictService(t)
			s.deps.Store = c.store
			v, err := s.currentVerdict(context.Background(), Request{}, keyContext{CanRead: true})
			if c.fails {
				if !errors.Is(err, ioErr) {
					t.Fatalf("err = %v, want the read failure", err)
				}
				return
			}
			if err != nil || v.Outcome != c.outcome || v.Exact.Assessment != c.want {
				t.Fatalf("verdict = %s exact %+v err %v, want %s with assessment %d", v.Outcome, v.Exact, err, c.outcome, c.want)
			}
		})
	}
}

func TestCurrentVerdictInformationalReadsDegradeOnlyExpectedFailures(t *testing.T) {
	id, err := runcache.NewRunID(1, strings.NewReader(strings.Repeat("f", 16)))
	if err != nil {
		t.Fatal(err)
	}
	entry := runcache.RunEntry{ID: id}
	faults := map[string]error{
		"cancellation":        fmt.Errorf("read entry: %w", context.Canceled),
		"deadline":            fmt.Errorf("read entry: %w", context.DeadlineExceeded),
		"cancelled path read": &fs.PathError{Op: "read", Path: "entry", Err: context.Canceled},
		"unexpected fault":    errors.New("permission denied"),
	}
	for name, fault := range faults {
		stores := map[string]verdictStore{
			"lookup":       {lookupErr: fault},
			"verification": {entry: &entry, stdoutErr: fault},
		}
		for stage, store := range stores {
			t.Run(stage+"/"+name, func(t *testing.T) {
				s := newVerdictService(t)
				s.deps.Store = store
				v, err := s.currentVerdict(context.Background(), Request{}, keyContext{Reason: runcache.ReasonNoCache})
				if !errors.Is(err, fault) {
					t.Fatalf("verdict = %+v err %v, want the fault %v", v, err, fault)
				}
			})
		}
	}
}

func TestCurrentVerdictObservesCancellationDuringAnUnawareRead(t *testing.T) {
	id, err := runcache.NewRunID(1, strings.NewReader(strings.Repeat("c", 16)))
	if err != nil {
		t.Fatal(err)
	}
	entry := runcache.RunEntry{ID: id}
	for _, stage := range []string{"lookup", "verification"} {
		for _, kc := range []keyContext{{Reason: runcache.ReasonNoCache}, {CanRead: true}} {
			t.Run(fmt.Sprintf("%s/authoritative=%v", stage, kc.CanRead), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				store := verdictStore{entry: &entry}
				if stage == "lookup" {
					store.onLookup = cancel
				} else {
					store.onStdout = cancel
				}
				s := newVerdictService(t)
				s.deps.Store = store
				v, err := s.currentVerdict(ctx, Request{}, kc)
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("verdict = %+v err %v, want the cancellation", v, err)
				}
			})
		}
	}
}
