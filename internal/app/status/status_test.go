package status

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"awarer/internal/app/initcmd"
	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/config"
	"awarer/internal/domain/evidence"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/checkpointjson"
	"awarer/internal/infra/projfs"
	"awarer/internal/infra/runstore"
	"awarer/internal/scantest"
)

// TestSubsystemVerdictCouplesStateAndDegradation proves the type-owned invariant at its
// seam: a degraded verdict is unrepresentable without a matching degradation, and a
// healthy/empty verdict carries none. This backstops the state↔degradation coupling
// structurally, so a future edit cannot set a degraded state token without its fact.
func TestSubsystemVerdictCouplesStateAndDegradation(t *testing.T) {
	// Non-degraded verdicts carry no degradation.
	for _, v := range []subsystemVerdict{healthyVerdict(), emptyVerdict()} {
		if len(v.degradations) != 0 {
			t.Errorf("non-degraded verdict %q carries %d degradation(s), want none", v.state, len(v.degradations))
		}
		if evidence.DiagnosticToken(v.state).Valid() && v.state != stateStoreEmpty {
			t.Errorf("non-degraded verdict has degraded-looking state %q", v.state)
		}
	}

	// A degraded verdict derives its state from the degradation and always carries it.
	deg := mustDegrade(evidence.ComponentRuns, evidence.TokenMetadataCorrupt, "boom")
	v := degradedVerdict(deg)
	if v.state != string(evidence.TokenMetadataCorrupt) {
		t.Errorf("degradedVerdict state = %q, want it derived from the degradation token", v.state)
	}
	if len(v.degradations) != 1 {
		t.Fatalf("degradedVerdict carries %d degradation(s), want exactly the one it was built from", len(v.degradations))
	}

	// An aggregate degraded verdict keeps an explicit roll-up state but still requires a fact.
	agg := degradedAggregate(string(evidence.TokenReadPartial), []evidence.Degradation{deg})
	if agg.state != string(evidence.TokenReadPartial) || len(agg.degradations) != 1 {
		t.Errorf("degradedAggregate = %+v, want read-partial state with its degradation", agg)
	}

	// Building a degraded aggregate without any matching fact is a programming error.
	func() {
		defer func() {
			if recover() == nil {
				t.Error("degradedAggregate with no degradation did not panic; the invariant is not enforced")
			}
		}()
		degradedAggregate(string(evidence.TokenReadPartial), nil)
	}()
}

// mustResolved wraps a plain config in a ResolvedConfig for status tests, which
// exercise durable facts rather than config provenance.
func mustResolved(t *testing.T, cfg config.Config) config.ResolvedConfig {
	t.Helper()
	r, err := config.NewResolvedConfig(cfg, config.DefaultOrigins(), nil)
	if err != nil {
		t.Fatalf("resolved config: %v", err)
	}
	return r
}

func openProject(t *testing.T) (projfs.Project, string) {
	t.Helper()
	root := t.TempDir()
	if _, err := initcmd.Run(initcmd.Request{Root: root, Profile: initcmd.ProfileDefault}); err != nil {
		t.Fatalf("init: %v", err)
	}
	p, err := projfs.Open(root)
	if err != nil {
		t.Fatalf("open project: %v", err)
	}
	return p, root
}

func TestRunReportsHonestStatus(t *testing.T) {
	p, root := openProject(t)
	l := paths.New(root)

	res, err := Run(context.Background(), p, mustResolved(t, config.Defaults()))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Initialized {
		t.Error("Initialized = false, want true")
	}
	if res.Checkpoints.Recorded != 0 || res.Checkpoints.Populated {
		t.Errorf("checkpoints = %+v, want empty/unpopulated", res.Checkpoints)
	}
	if res.RunCache.Recorded != 0 || res.RunCache.Populated {
		t.Errorf("run cache = %+v, want empty/unpopulated", res.RunCache)
	}
	if res.Store.Path != l.StoreDir() {
		t.Errorf("store path = %q, want %q", res.Store.Path, l.StoreDir())
	}
	if res.Store.Footprint.Available {
		t.Error("store footprint should be unavailable in ordinary status")
	}
	if res.Store.Footprint.Reason != "bounded" {
		t.Errorf("store footprint reason = %q, want bounded", res.Store.Footprint.Reason)
	}
	if len(res.Degradations) != 0 {
		t.Errorf("degradations = %+v, want none for a fresh project", res.Degradations)
	}
	if res.Checkpoints.State != "store-empty" {
		t.Errorf("checkpoints.state = %q, want store-empty", res.Checkpoints.State)
	}
	if res.RunCache.State != "store-empty" {
		t.Errorf("run_cache.state = %q, want store-empty", res.RunCache.State)
	}
}

// commitStatusCheckpoint publishes one minimal checkpoint into the project's store, so
// a test can populate a checkpoint history larger than the header window status
// retains. It writes through the production repository, so the records are exactly what
// status reads back.
func commitStatusCheckpoint(t *testing.T, layout paths.Layout, idByte byte, created time.Time) checkpoint.CheckpointID {
	t.Helper()
	id, err := checkpoint.NewCheckpointID(strings.NewReader(strings.Repeat(string(rune('a'+idByte%26)), 32)))
	if err != nil {
		t.Fatalf("NewCheckpointID: %v", err)
	}
	cfgHash := hashing.ConfigHashFromTree(blake3hash.New().HashBytes([]byte("cfg")))
	build := checkpoint.CheckpointBuild{
		ID:                   id,
		CreatedAt:            created,
		Root:                 layout.Root(),
		CommandCwd:           ".",
		AwaVersion:           "0.0.0-dev",
		ScanConfigHash:       cfgHash,
		CheckpointPolicyHash: cfgHash,
		TrustMode:            config.TrustNormal,
	}
	if _, err := checkpointjson.NewRepo(layout).PutManifest(context.Background(), build, scantest.CanonicalStream(nil, nil)); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	return id
}

// TestCheckpointStatusCountsAllButRetainsTheNewest proves the bounded checkpoint read
// stays honest: status keeps one header, so it must report the newest one, while the
// recorded count describes the whole store. A window that kept an arbitrary header
// instead of the newest, or a total taken from the window, fails here.
func TestCheckpointStatusCountsAllButRetainsTheNewest(t *testing.T) {
	p, root := openProject(t)
	layout := paths.New(root)
	base := time.Unix(1_700_000_000, 0).UTC()
	var newest checkpoint.CheckpointID
	for i := 0; i < 5; i++ {
		newest = commitStatusCheckpoint(t, layout, byte(i), base.Add(time.Duration(i)*time.Minute))
	}

	res, err := Run(context.Background(), p, mustResolved(t, config.Defaults()))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Checkpoints.Recorded != 5 {
		t.Errorf("Recorded = %d, want the store-wide total 5", res.Checkpoints.Recorded)
	}
	if res.Checkpoints.Latest == nil {
		t.Fatal("Latest = nil, want the newest checkpoint")
	}
	if res.Checkpoints.Latest.ID != newest.String() {
		t.Errorf("Latest = %s, want the newest %s", res.Checkpoints.Latest.ID, newest.Short())
	}
	if res.Checkpoints.State != "healthy" {
		t.Errorf("checkpoints.state = %q, want healthy", res.Checkpoints.State)
	}
}

// TestCheckpointStatusSeesUnreadableRecordsOutsideTheWindow is the completeness guard
// on the bounded read: the incompatible record is the oldest in the store, so a scan
// that stopped once its one-header window filled would report a healthy store with a
// resolvable latest — exactly the answer status must never give.
func TestCheckpointStatusSeesUnreadableRecordsOutsideTheWindow(t *testing.T) {
	p, root := openProject(t)
	layout := paths.New(root)
	base := time.Unix(1_700_000_000, 0).UTC()
	stale := commitStatusCheckpoint(t, layout, 0, base)
	for i := 1; i < 4; i++ {
		commitStatusCheckpoint(t, layout, byte(i), base.Add(time.Duration(i)*time.Minute))
	}
	// Declare a schema this build does not speak on the oldest record.
	header := filepath.Join(layout.CheckpointsDir(), stale.String(), "header.json")
	if err := os.Chmod(header, 0o644); err != nil {
		t.Fatalf("chmod header: %v", err)
	}
	if err := os.WriteFile(header, []byte(`{"schema_version":99}`), 0o644); err != nil {
		t.Fatalf("rewrite header: %v", err)
	}

	res, err := Run(context.Background(), p, mustResolved(t, config.Defaults()))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Checkpoints.Recorded != 3 || res.Checkpoints.Unreadable != 1 || res.Checkpoints.Incompatible != 1 {
		t.Fatalf("counts = recorded %d unreadable %d incompatible %d, want 3/1/1",
			res.Checkpoints.Recorded, res.Checkpoints.Unreadable, res.Checkpoints.Incompatible)
	}
	if res.Checkpoints.Latest != nil {
		t.Errorf("a degraded store reported latest %+v, want it omitted", res.Checkpoints.Latest)
	}
	if res.Checkpoints.State != "read-partial" {
		t.Errorf("checkpoints.state = %q, want read-partial", res.Checkpoints.State)
	}
}

// commitStatusRun publishes one reusable run into the project's run store, so a test
// can populate the cache above the bounded status sample without driving a real
// process. It mirrors the runstore's own commit shape through public APIs.
func commitStatusRun(t *testing.T, layout paths.Layout, h hashing.Hasher, disc string, unixNano int64) {
	t.Helper()
	repo := runstore.New(layout, h)
	pending, err := repo.Begin(runcache.CaptureLimits{MaxStdout: 1 << 20, MaxStderr: 1 << 20})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := io.WriteString(pending.Stdout(), "o"); err != nil {
		t.Fatalf("stdout: %v", err)
	}
	if _, err := io.WriteString(pending.Stderr(), "e"); err != nil {
		t.Fatalf("stderr: %v", err)
	}
	so, se, err := pending.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	id, err := runcache.NewRunID(unixNano, strings.NewReader(strings.Repeat(disc, 8)))
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	cwd, _ := runcache.NewExecutionCWD(".")
	red, err := worktree.ReduceCursor(h, scantest.CanonicalCursor(nil, nil))
	if err != nil {
		t.Fatalf("empty tree hash: %v", err)
	}
	ki := runcache.KeyInput{
		CacheSchemaVersion: runcache.CacheSchemaVersion,
		AwaVersion:         "test",
		InvocationMode:     runcache.InvocationArgv,
		Command:            runcache.Command{Argv: []string{"echo", disc}, RawExecutable: "echo"},
		CWD:                cwd,
		InputTreeHash:      red.Hash,
		Effect:             mustObservedEffect(t, h),
		IncludeScope:       []string{"."},
		TrustMode:          config.TrustNormal,
		RunConfigHash:      hashing.ConfigHashFromTree(h.HashBytes([]byte("cfg"))),
		Env:                runcache.NewEnvironment(nil),
		Platform:           runcache.Platform{GOOS: "linux", GOARCH: "amd64"},
		StdinMode:          runcache.StdinNull,
	}
	start := time.Unix(0, unixNano)
	entry := runcache.RunEntry{
		ID:          id,
		Key:         ki.Compute(h),
		KeyInput:    ki,
		StartedAt:   start,
		FinishedAt:  start.Add(time.Second),
		Exit:        runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 0},
		Stdout:      so,
		Stderr:      se,
		Decision:    runcache.CacheDecision{Cacheable: true},
		Reuse:       runcache.ReusableCacheEntry(),
		Mutation:    mustUnchanged(t),
		EffectGuard: mustUnchangedEffect(t),
	}
	scanCfg := hashing.ConfigHashFromTree(h.HashBytes([]byte("scancfg")))
	obs := runcache.RunObservations{
		Before:               scantest.CanonicalStream(nil, nil),
		After:                scantest.CanonicalStream(nil, nil),
		BeforeScanConfigHash: scanCfg,
		AfterScanConfigHash:  scanCfg,
	}
	if err := pending.Commit(context.Background(), entry, obs); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func mustUnchanged(t *testing.T) runcache.MutationStatus {
	t.Helper()
	st, err := runcache.NewMutationStatus(runcache.MutationUnchanged)
	if err != nil {
		t.Fatalf("MutationStatus: %v", err)
	}
	return st
}

func mustUnchangedEffect(t *testing.T) runcache.EffectGuardStatus {
	t.Helper()
	g, err := runcache.NewEffectGuardStatus(runcache.EffectGuardUnchanged)
	if err != nil {
		t.Fatalf("EffectGuardStatus: %v", err)
	}
	return g
}

// mustObservedEffect is the effect identity every execution keys on: production
// always observes the non-empty built-in watch set.
func mustObservedEffect(t *testing.T, h hashing.Hasher) runcache.EffectObservation {
	t.Helper()
	o, err := runcache.ObservedEffect(runcache.EffectHashFromTree(h.HashBytes([]byte("effect"))), 1)
	if err != nil {
		t.Fatalf("ObservedEffect: %v", err)
	}
	return o
}

// TestRunCacheStatusCountsAllButSamplesBounded proves status reports the true total
// run count (folded in O(1) memory, not a full id slice) while verifying only the
// bounded newest sample — so a cache larger than the sample stays cheap and honest.
func TestRunCacheStatusCountsAllButSamplesBounded(t *testing.T) {
	p, root := openProject(t)
	layout := paths.New(root)
	h := blake3hash.New()

	total := statusRunSampleSize + 5
	base := int64(1_700_000_000) * int64(time.Second)
	for i := 0; i < total; i++ {
		commitStatusRun(t, layout, h, string(rune('a'+i%26)), base+int64(i))
	}

	res, err := Run(context.Background(), p, mustResolved(t, config.Defaults()))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RunCache.Recorded != total {
		t.Errorf("Recorded = %d, want the full total %d", res.RunCache.Recorded, total)
	}
	if !res.RunCache.Populated {
		t.Error("Populated = false, want true")
	}
	if res.RunCache.SampleSize != statusRunSampleSize {
		t.Errorf("SampleSize = %d, want it capped at %d", res.RunCache.SampleSize, statusRunSampleSize)
	}
	if res.RunCache.Latest == nil {
		t.Fatal("Latest = nil, want the newest readable run")
	}
	if res.RunCache.CorruptInSample != 0 || res.RunCache.UnhealthyInSample != 0 {
		t.Errorf("sample health = corrupt %d / unhealthy %d, want 0/0 for freshly written runs",
			res.RunCache.CorruptInSample, res.RunCache.UnhealthyInSample)
	}
	// A clean sample of a cache LARGER than the sample is read-bounded, not healthy:
	// status verified only the newest sample, so it discloses the bound rather than
	// overclaiming full health.
	if res.RunCache.State != "read-bounded" {
		t.Errorf("run_cache.state = %q, want read-bounded (checked %d of %d)",
			res.RunCache.State, res.RunCache.SampleSize, res.RunCache.Recorded)
	}
	var bounded *DegradationView
	for i := range res.Degradations {
		if res.Degradations[i].Component == "runs" && res.Degradations[i].Token == "read-bounded" {
			bounded = &res.Degradations[i]
		}
	}
	if bounded == nil {
		t.Fatalf("no runs read-bounded degradation: %+v", res.Degradations)
	}
	if bounded.Sample == nil {
		t.Fatal("read-bounded degradation carries no bounded sample")
	}
	if bounded.Sample.Shown != statusRunSampleSize || bounded.Sample.Total == nil || *bounded.Sample.Total != total {
		t.Errorf("bounded sample = %+v, want shown %d total %d", bounded.Sample, statusRunSampleSize, total)
	}
	if bounded.Sample.Complete {
		t.Error("bounded sample marked complete, want incomplete for a capped scan")
	}
	if !bounded.Advisory() {
		t.Error("read-bounded degradation is not advisory, want a calm note")
	}
}

// TestRunCacheSmallCacheIsHealthy proves a clean cache no larger than the verified
// sample is plain healthy, so read-bounded never adds noise to a small cache.
func TestRunCacheSmallCacheIsHealthy(t *testing.T) {
	p, root := openProject(t)
	layout := paths.New(root)
	h := blake3hash.New()
	base := int64(1_700_000_000) * int64(time.Second)
	for i := 0; i < 3; i++ {
		commitStatusRun(t, layout, h, string(rune('a'+i)), base+int64(i))
	}

	res, err := Run(context.Background(), p, mustResolved(t, config.Defaults()))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RunCache.State != "healthy" {
		t.Errorf("run_cache.state = %q, want healthy for a small clean cache", res.RunCache.State)
	}
	if len(res.Degradations) != 0 {
		t.Errorf("small clean cache has degradations: %+v", res.Degradations)
	}
}

// TestRunCacheUnhealthyPayloadDegrades proves a run whose stored payload is missing
// makes the run cache read-partial with a structured degradation, never a healthy or
// empty cache — a durable read failure is not collapsed into absence.
func TestRunCacheUnhealthyPayloadDegrades(t *testing.T) {
	p, root := openProject(t)
	layout := paths.New(root)
	h := blake3hash.New()
	commitStatusRun(t, layout, h, "z", int64(1_700_000_000)*int64(time.Second))

	// Remove the newest run's stdout payload so its health verification fails.
	var removed bool
	_ = filepath.Walk(layout.RunsDir(), func(path string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() && strings.HasSuffix(path, "stdout.log") {
			if rmErr := os.Remove(path); rmErr == nil {
				removed = true
			}
		}
		return nil
	})
	if !removed {
		t.Fatal("did not find a stdout payload to remove")
	}

	res, err := Run(context.Background(), p, mustResolved(t, config.Defaults()))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RunCache.State == "store-empty" || res.RunCache.State == "healthy" {
		t.Errorf("run_cache.state = %q, want a degraded token", res.RunCache.State)
	}
	if res.RunCache.UnhealthyInSample != 1 {
		t.Errorf("UnhealthyInSample = %d, want 1", res.RunCache.UnhealthyInSample)
	}
	found := false
	for _, d := range res.Degradations {
		if d.Component == "runs" {
			found = true
		}
	}
	if !found {
		t.Errorf("degradations missing a runs entry: %+v", res.Degradations)
	}
}

func TestRunRejectsUnresolvedProject(t *testing.T) {
	if _, err := Run(context.Background(), projfs.Project{}, mustResolved(t, config.Defaults())); !errors.Is(err, projfs.ErrUnresolvedProject) {
		t.Errorf("Run(zero Project) = %v, want ErrUnresolvedProject", err)
	}
}

func TestRunWarnsWhenRequiredPathIsNotADirectory(t *testing.T) {
	p, root := openProject(t)
	l := paths.New(root)

	// Replace a required directory with a regular file: status must flag it as
	// "not a directory", distinct from "missing directory".
	logs := l.LogsDir()
	if err := os.RemoveAll(logs); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Run(context.Background(), p, mustResolved(t, config.Defaults()))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	found := false
	for _, d := range res.Degradations {
		if strings.Contains(d.Detail, "not a directory") && strings.Contains(d.Detail, logs) {
			found = true
			if d.Token != "metadata-corrupt" {
				t.Errorf("not-a-directory token = %q, want metadata-corrupt", d.Token)
			}
		}
		if strings.Contains(d.Detail, "missing directory") && strings.Contains(d.Detail, logs) {
			t.Errorf("logs file reported as missing, want not-a-directory: %q", d.Detail)
		}
	}
	if !found {
		t.Errorf("expected a not-a-directory degradation for %s, got %+v", logs, res.Degradations)
	}
}

// Config loading and validation now live at the CLI boundary (loadProjectConfig),
// which composes the layers once and passes the validated config into Run — so an
// invalid config is surfaced there (see the CLI status test), not re-validated here.
// Run trusts the config it is handed.
