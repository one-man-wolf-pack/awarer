package stateprovider_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"awarer/internal/app/scanner"
	"awarer/internal/app/state"
	"awarer/internal/app/stateprovider"
	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/provider"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/blobstore"
	"awarer/internal/infra/checkpointjson"
	"awarer/internal/infra/runstore"
	"awarer/internal/scantest"
)

// fixtureRoot is an absolute, cleaned path on whichever host runs the test. A checkpoint
// records the project root it was captured from and Checkpoint.Validate requires exactly
// that shape, so a fixture spelling a POSIX literal like "/abs/project" is not cosmetic:
// filepath.IsAbs rejects a volume-less path on Windows, which would fail every checkpoint
// built here and confine this package's coverage to unix.
func fixtureRoot(t testing.TB) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("abs", "project"))
	if err != nil {
		t.Fatalf("resolving a fixture root: %v", err)
	}
	return root
}

func newAssessor(t *testing.T) (*stateprovider.Assessor, *checkpointjson.Repo, paths.Layout) {
	t.Helper()
	layout := paths.New(t.TempDir())
	repo := checkpointjson.NewRepo(layout)
	hasher := blake3hash.New()
	r := state.NewResolver(state.Deps{
		Checkpoints: repo,
		Blobs:       blobstore.New(layout, hasher),
		Hasher:      hasher,
		Runs:        runstore.New(layout, hasher),
	})
	return stateprovider.NewAssessor(r), repo, layout
}

// putCP writes a one-entry checkpoint whose tree hash is driven by contentHex and whose
// scan-config identity is driven by cfgNibble, so tests can vary content and policy
// independently.
func putCP(t *testing.T, repo *checkpointjson.Repo, idByte byte, contentHex, cfgNibble string) checkpoint.CheckpointID {
	t.Helper()
	id, err := checkpoint.NewCheckpointID(bytes.NewReader(bytes.Repeat([]byte{idByte}, 20)))
	if err != nil {
		t.Fatalf("NewCheckpointID: %v", err)
	}
	ch, err := hashing.ParseContentHash("blake3:" + contentHex)
	if err != nil {
		t.Fatalf("ParseContentHash: %v", err)
	}
	p, err := worktree.ParseRelPath("a.txt")
	if err != nil {
		t.Fatalf("ParseRelPath: %v", err)
	}
	omitted := worktree.FieldSet(0).With(worktree.FieldCtime).With(worktree.FieldDev).With(worktree.FieldIno).With(worktree.FieldNlink)
	entry, err := worktree.NewRegularEntry(p, ch, worktree.StorageBlob,
		worktree.StatSignature{Size: 10, MtimeNs: 5, Mode: 0o644, Omitted: omitted}, worktree.TraversalInfo{})
	if err != nil {
		t.Fatalf("NewRegularEntry: %v", err)
	}
	cfg, err := hashing.ParseConfigHash("blake3:" + strings.Repeat(cfgNibble, 64))
	if err != nil {
		t.Fatalf("ParseConfigHash: %v", err)
	}
	putManifest(t, repo, id, cfg, omitted, []worktree.Entry{entry})
	return id
}

// putManifest publishes a checkpoint through the streaming write contract. The tree
// hash, stats, and record count are derived by the store from the records themselves,
// so a fixture never hands the store a derived fact.
func putManifest(t *testing.T, repo *checkpointjson.Repo, id checkpoint.CheckpointID,
	cfg hashing.ConfigHash, omitted worktree.FieldSet, entries []worktree.Entry) {
	t.Helper()
	build := checkpoint.CheckpointBuild{
		ID:                   id,
		CreatedAt:            time.Unix(1_700_000_000, 0).UTC(),
		Root:                 fixtureRoot(t),
		CommandCwd:           ".",
		AwaVersion:           "0.0.0-dev",
		ScanConfigHash:       cfg,
		CheckpointPolicyHash: cfg,
		TrustMode:            config.TrustNormal,
		OmittedStatFields:    omitted,
	}
	if _, err := repo.PutManifest(context.Background(), build, scantest.CanonicalStream(entries, nil)); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
}

const hexA = "1111111111111111111111111111111111111111111111111111111111111111"
const hexB = "2222222222222222222222222222222222222222222222222222222222222222"

// putCPEntries writes a checkpoint with n entries whose paths are shared across sides but
// whose content is perturbed by side, so a left/right pair yields n modifications. It
// exercises the streaming merge at scale.
func putCPEntries(t *testing.T, repo *checkpointjson.Repo, idByte byte, cfgNibble string, n int, side byte) checkpoint.CheckpointID {
	t.Helper()
	id, err := checkpoint.NewCheckpointID(bytes.NewReader(bytes.Repeat([]byte{idByte}, 20)))
	if err != nil {
		t.Fatalf("NewCheckpointID: %v", err)
	}
	omitted := worktree.FieldSet(0).With(worktree.FieldCtime).With(worktree.FieldDev).With(worktree.FieldIno).With(worktree.FieldNlink)
	entries := make([]worktree.Entry, 0, n)
	for i := 0; i < n; i++ {
		p, err := worktree.ParseRelPath(fmt.Sprintf("f%09d.txt", i))
		if err != nil {
			t.Fatalf("ParseRelPath: %v", err)
		}
		ch, err := hashing.ParseContentHash("blake3:" + fmt.Sprintf("%02x%062x", side, uint64(i)))
		if err != nil {
			t.Fatalf("ParseContentHash: %v", err)
		}
		e, err := worktree.NewRegularEntry(p, ch, worktree.StorageBlob,
			worktree.StatSignature{Size: 1, MtimeNs: 1, Mode: 0o644, Omitted: omitted}, worktree.TraversalInfo{})
		if err != nil {
			t.Fatalf("NewRegularEntry: %v", err)
		}
		entries = append(entries, e)
	}
	cfg, err := hashing.ParseConfigHash("blake3:" + strings.Repeat(cfgNibble, 64))
	if err != nil {
		t.Fatalf("ParseConfigHash: %v", err)
	}
	putManifest(t, repo, id, cfg, omitted, entries)
	return id
}

func idRef(id checkpoint.CheckpointID) state.Ref {
	return state.Ref{Kind: state.RefCheckpointPrefix, Display: id.String(), Raw: id.String()}
}

func TestResolveLatestResolved(t *testing.T) {
	a, repo, _ := newAssessor(t)
	id := putCP(t, repo, 0x11, hexA, "a")
	got, err := a.Resolve(context.Background(), state.Ref{Kind: state.RefLatest, Display: "latest"}, "latest", state.NowContext{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Outcome() != provider.OutcomeResolved {
		t.Fatalf("outcome = %v, want resolved", got.Outcome())
	}
	idn, ok := got.Identity()
	if !ok {
		t.Fatal("resolved assessment must carry an identity")
	}
	if idn.Kind() != provider.KindCheckpoint {
		t.Errorf("kind = %v, want checkpoint", idn.Kind())
	}
	if idn.CheckpointID() != id.String() {
		t.Errorf("checkpoint id = %q, want full %q (never shortened)", idn.CheckpointID(), id.String())
	}
	if !idn.Reconstructable() || !idn.Scan().Valid() {
		t.Errorf("checkpoint identity should be reconstructable with a known scan identity")
	}
	if got.InputRef() != "latest" {
		t.Errorf("input ref = %q, want latest (echoed verbatim)", got.InputRef())
	}
}

// changingWalker deterministically yields one regular node whose content read reports
// the node changed during the scan, modelling an in-scope input that moved mid-observation.
type changingWalker struct{}

func (changingWalker) Walk(_ context.Context, _ paths.Layout, _ config.ScanScope, visit func(worktree.Node) error) error {
	p, err := worktree.ParseRelPath("a.txt")
	if err != nil {
		return err
	}
	return visit(worktree.Node{
		Path: p,
		Kind: worktree.KindRegular,
		Stat: worktree.StatSignature{Size: 5, MtimeNs: 1, Mode: 0o644},
		Open: func() (io.ReadCloser, error) { return nil, worktree.ErrObservationChanged },
	})
}

// TestResolveNowUnstableIsObservationUnstable proves the full chain: a strict "now"
// observation whose input moves mid-scan resolves to a complete unavailable assessment
// with reason observation-unstable (exit 0 at the CLI), not a fault and not a partial
// identity.
func TestResolveNowUnstableIsObservationUnstable(t *testing.T) {
	proj := scantest.InitProject(t, t.TempDir())
	hasher := blake3hash.New()
	r := state.NewResolver(state.Deps{Scanner: scanner.New(changingWalker{}, hasher, nil), Hasher: hasher})
	a := stateprovider.NewAssessor(r)

	got, err := a.Resolve(context.Background(), state.Ref{Kind: state.RefNow}, "now",
		state.NowContext{Project: proj, Config: config.Defaults(), RequireStableObservation: true})
	if err != nil {
		t.Fatalf("Resolve returned a fault, want unavailable assessment: %v", err)
	}
	reason, ok := got.Reason()
	if !ok || reason != provider.ReasonObservationUnstable {
		t.Errorf("reason = (%v,%v), want (observation-unstable,true)", reason, ok)
	}
	if _, ok := got.Identity(); ok {
		t.Error("an unstable observation must carry no state identity")
	}
}

// scriptedWalker yields one set of nodes on its first Walk and another on every later
// Walk, modelling a whole-scope worktree change that lands between the provider's two "now"
// scans and reads cleanly in each pass, so the narrow mid-scan guard never fires. It is the
// deterministic seam for the two-scan stability proof.
type scriptedWalker struct {
	calls int
	first []worktree.Node
	rest  []worktree.Node
}

func (w *scriptedWalker) Walk(_ context.Context, _ paths.Layout, _ config.ScanScope, visit func(worktree.Node) error) error {
	w.calls++
	nodes := w.rest
	if w.calls == 1 {
		nodes = w.first
	}
	for _, n := range nodes {
		if err := visit(n); err != nil {
			return err
		}
	}
	return nil
}

func regularNode(t *testing.T, path, content string) worktree.Node {
	t.Helper()
	p, err := worktree.ParseRelPath(path)
	if err != nil {
		t.Fatalf("ParseRelPath(%q): %v", path, err)
	}
	return worktree.Node{
		Path: p,
		Kind: worktree.KindRegular,
		Stat: worktree.StatSignature{Size: int64(len(content)), MtimeNs: 1, Mode: 0o644},
		Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(content)), nil },
	}
}

// TestResolveNowDetectsWholeScopeChangeBetweenScans proves the two-scan stability contract:
// a "now" observation whose whole tree changes between the provider's two scans — an add,
// delete, or modify of an already-visited path, each reading cleanly per pass — resolves to
// a complete observation-unstable assessment (exit 0), never a resolved identity built from
// a mixed tree; an unchanged tree resolves. Every case runs exactly two scans.
//
// Mutation proof: make resolveCandidate call resolveOnce for a "now" ref (a single scan) —
// the three "changed" subtests go red (they resolve instead of reporting
// observation-unstable) and the scan-count assertion fails (calls == 1).
func TestResolveNowDetectsWholeScopeChangeBetweenScans(t *testing.T) {
	proj := scantest.InitProject(t, t.TempDir())
	hasher := blake3hash.New()
	a1 := regularNode(t, "a.txt", "one")
	b := regularNode(t, "b.txt", "bee")
	cases := map[string]struct {
		first, rest []worktree.Node
		stable      bool
	}{
		"modify visited path": {[]worktree.Node{a1}, []worktree.Node{regularNode(t, "a.txt", "one-changed")}, false},
		"add path":            {[]worktree.Node{a1}, []worktree.Node{a1, b}, false},
		"delete path":         {[]worktree.Node{a1, b}, []worktree.Node{a1}, false},
		"unchanged tree":      {[]worktree.Node{a1}, []worktree.Node{a1}, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			w := &scriptedWalker{first: c.first, rest: c.rest}
			r := state.NewResolver(state.Deps{Scanner: scanner.New(w, hasher, nil), Hasher: hasher})
			a := stateprovider.NewAssessor(r)

			got, err := a.Resolve(context.Background(), state.Ref{Kind: state.RefNow}, "now",
				state.NowContext{Project: proj, Config: config.Defaults()})
			if err != nil {
				t.Fatalf("Resolve returned a fault, want an assessment: %v", err)
			}
			if w.calls != 2 {
				t.Errorf("now resolution ran %d scans, want exactly 2 (a fixed, un-retried cost)", w.calls)
			}
			if c.stable {
				if _, ok := got.Identity(); !ok {
					reason, _ := got.Reason()
					t.Errorf("an unchanged tree must resolve, got unavailable %q", reason)
				}
				return
			}
			reason, ok := got.Reason()
			if !ok || reason != provider.ReasonObservationUnstable {
				t.Errorf("a whole-scope change: reason = (%v,%v), want observation-unstable", reason, ok)
			}
			if _, ok := got.Identity(); ok {
				t.Error("an unstable observation must carry no state identity")
			}
		})
	}
}

// reuseIndex is a fake worktree index that returns one pre-recorded (stat, content) for a
// single path, modelling the normal-trust-mode reuse fast path: a scan whose node stat
// matches the recorded stat reuses the recorded hash without reading content. Its writes
// are no-ops.
type reuseIndex struct {
	path  worktree.RelPath
	entry worktree.IndexedEntry
}

func (r reuseIndex) Lookup(_ context.Context, p worktree.RelPath) (worktree.IndexedEntry, bool, error) {
	if p.String() == r.path.String() {
		return r.entry, true, nil
	}
	return worktree.IndexedEntry{}, false, nil
}
func (reuseIndex) Close() error { return nil }
func (reuseIndex) BeginScan(context.Context, worktree.ScanMetadata) (worktree.ScanTx, error) {
	return noopScanTx{}, nil
}

type noopScanTx struct{}

func (noopScanTx) Upsert(context.Context, worktree.Entry) error               { return nil }
func (noopScanTx) RecordSkipped(context.Context, worktree.SkippedInput) error { return nil }
func (noopScanTx) Commit(context.Context, hashing.TreeHash, time.Time) error  { return nil }
func (noopScanTx) Rollback() error                                            { return nil }

// TestResolveNowIndexReuseChangeIsUnstable proves the two-scan stability contract on the
// index-reuse fast path — the common production path, which the scripted-walker tests (no
// index) do not exercise: the first scan reuses the indexed hash for an unchanged stat, the
// stat then changes before the second scan, reuse is rejected, the rehash observes
// different content, and the result is observation-unstable. Detecting a change hidden
// behind a deliberately-preserved stat signature is out of scope — that is the chosen trust
// policy's boundary, not this contract's.
func TestResolveNowIndexReuseChangeIsUnstable(t *testing.T) {
	proj := scantest.InitProject(t, t.TempDir())
	hasher := blake3hash.New()
	pa, err := worktree.ParseRelPath("a.txt")
	if err != nil {
		t.Fatalf("ParseRelPath: %v", err)
	}
	statFirst := worktree.StatSignature{Size: 3, MtimeNs: 1, Mode: 0o644}
	indexedContent, err := hashing.ParseContentHash("blake3:" + strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("ParseContentHash: %v", err)
	}
	idx := reuseIndex{path: pa, entry: worktree.IndexedEntry{Stat: statFirst, Content: indexedContent, Storage: worktree.StorageBlob}}

	var firstRead, secondRead bool
	w := &scriptedWalker{
		first: []worktree.Node{{
			Path: pa, Kind: worktree.KindRegular, Stat: statFirst,
			Open: func() (io.ReadCloser, error) { firstRead = true; return io.NopCloser(strings.NewReader("abc")), nil },
		}},
		// A changed stat signature (size and mtime) before the second scan: reuse is rejected
		// and the content is read, yielding a different hash than the reused one.
		rest: []worktree.Node{{
			Path: pa, Kind: worktree.KindRegular, Stat: worktree.StatSignature{Size: 7, MtimeNs: 2, Mode: 0o644},
			Open: func() (io.ReadCloser, error) {
				secondRead = true
				return io.NopCloser(strings.NewReader("changed")), nil
			},
		}},
	}
	r := state.NewResolver(state.Deps{Scanner: scanner.New(w, hasher, idx), Hasher: hasher})
	a := stateprovider.NewAssessor(r)

	got, err := a.Resolve(context.Background(), state.Ref{Kind: state.RefNow}, "now",
		state.NowContext{Project: proj, Config: config.Defaults()})
	if err != nil {
		t.Fatalf("Resolve returned a fault, want an assessment: %v", err)
	}
	if firstRead {
		t.Error("first scan must reuse the indexed hash, not read content")
	}
	if !secondRead {
		t.Error("second scan must reject reuse (stat changed) and read content")
	}
	if w.calls != 2 {
		t.Errorf("ran %d scans, want exactly 2", w.calls)
	}
	reason, ok := got.Reason()
	if !ok || reason != provider.ReasonObservationUnstable {
		t.Errorf("reason = (%v,%v), want observation-unstable", reason, ok)
	}
	if _, ok := got.Identity(); ok {
		t.Error("an unstable observation must carry no identity")
	}
}

// missingPrefix is a syntactically valid checkpoint id prefix that no fixture id
// starts with, so resolving it exercises the not-found outcome rather than the
// malformed-syntax one the parser owns.
const missingPrefix = "zzzzzzzz"

func TestResolveMissingIsUnavailableNotFound(t *testing.T) {
	a, _, _ := newAssessor(t)
	got, err := a.Resolve(context.Background(), state.Ref{Kind: state.RefCheckpointPrefix, Display: missingPrefix, Raw: missingPrefix}, missingPrefix, state.NowContext{})
	if err != nil {
		t.Fatalf("Resolve returned a fault, want unavailable assessment: %v", err)
	}
	reason, ok := got.Reason()
	if !ok || reason != provider.ReasonNotFound {
		t.Errorf("reason = (%v,%v), want (not-found,true)", reason, ok)
	}
}

func TestResolveCorruptIsUnavailable(t *testing.T) {
	a, repo, layout := newAssessor(t)
	id := putCP(t, repo, 0x11, hexA, "a")
	header := filepath.Join(layout.CheckpointsDir(), id.String(), "header.json")
	if err := os.Chmod(header, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := os.WriteFile(header, []byte("{garbage"), 0o644); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	got, err := a.Resolve(context.Background(), state.Ref{Kind: state.RefLatest, Display: "latest"}, "latest", state.NowContext{})
	if err != nil {
		t.Fatalf("Resolve returned a fault, want unavailable assessment: %v", err)
	}
	reason, ok := got.Reason()
	if !ok || reason != provider.ReasonMetadataCorrupt {
		t.Errorf("reason = (%v,%v), want (metadata-corrupt,true)", reason, ok)
	}
}

func TestCompareSameCheckpointEqual(t *testing.T) {
	a, repo, _ := newAssessor(t)
	id := putCP(t, repo, 0x11, hexA, "a")
	cmp, err := a.Compare(context.Background(), state.Range{Left: idRef(id), Right: idRef(id)}, state.NowContext{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if cmp.Relationship() != provider.RelEqual {
		t.Errorf("relationship = %v, want equal", cmp.Relationship())
	}
	if _, ok := cmp.Counts(); ok {
		t.Error("equal comparison must not carry counts")
	}
}

func TestCompareDifferentCounts(t *testing.T) {
	a, repo, _ := newAssessor(t)
	left := putCP(t, repo, 0x11, hexA, "a")
	right := putCP(t, repo, 0x22, hexB, "a") // same scan config, different content
	cmp, err := a.Compare(context.Background(), state.Range{Left: idRef(left), Right: idRef(right)}, state.NowContext{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if cmp.Relationship() != provider.RelDifferent {
		t.Fatalf("relationship = %v, want different", cmp.Relationship())
	}
	counts, ok := cmp.Counts()
	if !ok {
		t.Fatal("different comparison must carry counts")
	}
	// Same path a.txt, same kind, different content hash -> one modification.
	if counts.Modified() != 1 || counts.Added() != 0 || counts.Deleted() != 0 || counts.TypeChanged() != 0 {
		t.Errorf("counts = a=%d m=%d d=%d t=%d, want only modified=1", counts.Added(), counts.Modified(), counts.Deleted(), counts.TypeChanged())
	}
	if !cmp.Complete() {
		t.Error("different comparison must be complete")
	}
}

func TestCompareIncompatibleScanConfig(t *testing.T) {
	a, repo, _ := newAssessor(t)
	left := putCP(t, repo, 0x11, hexA, "a")
	right := putCP(t, repo, 0x22, hexB, "b") // different scan config
	cmp, err := a.Compare(context.Background(), state.Range{Left: idRef(left), Right: idRef(right)}, state.NowContext{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if cmp.Relationship() != provider.RelIncomparable {
		t.Fatalf("relationship = %v, want incomparable", cmp.Relationship())
	}
	reason, ok := cmp.Reason()
	if !ok || reason != provider.ReasonIncompatibleIdentityPolicy {
		t.Errorf("reason = (%v,%v), want (incompatible-identity-policy,true)", reason, ok)
	}
}

// TestCompareCancelledContextIsFault proves cancellation is a genuine fault (a non-nil
// error, exit 130 at the CLI), never dressed up as an in-band unavailable assessment.
func TestCompareCancelledContextIsFault(t *testing.T) {
	a, repo, _ := newAssessor(t)
	left := putCP(t, repo, 0x11, hexA, "a")
	right := putCP(t, repo, 0x22, hexB, "a")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.Compare(ctx, state.Range{Left: idRef(left), Right: idRef(right)}, state.NowContext{})
	if err == nil {
		t.Fatal("cancelled compare must return a fault, not a nil error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("fault = %v, want context.Canceled", err)
	}
}

// TestCompareLargeTreeCounts drives the real assessor's streaming merge over two
// many-entry checkpoints, proving mergeCounts produces correct counts at a scale where a
// per-path buffer would be visible, while retaining only a bounded Summary.
func TestCompareLargeTreeCounts(t *testing.T) {
	a, repo, _ := newAssessor(t)
	const n = 2000
	left := putCPEntries(t, repo, 0x11, "a", n, 0)
	right := putCPEntries(t, repo, 0x22, "a", n, 1) // same paths, every content perturbed
	cmp, err := a.Compare(context.Background(), state.Range{Left: idRef(left), Right: idRef(right)}, state.NowContext{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if cmp.Relationship() != provider.RelDifferent {
		t.Fatalf("relationship = %v, want different", cmp.Relationship())
	}
	counts, _ := cmp.Counts()
	if counts.Modified() != n || counts.Added() != 0 || counts.Deleted() != 0 {
		t.Errorf("counts = a=%d m=%d d=%d, want only modified=%d", counts.Added(), counts.Modified(), counts.Deleted(), n)
	}
}

// comparePeakLiveBytes builds two n-entry checkpoints and runs the real Assessor.Compare
// over their on-disk (streamed) manifests while a background sampler forces a GC and reads
// the live heap on a tight loop, returning the peak live heap observed *during* the
// comparison (above the pre-comparison baseline). Forcing a GC before each sample isolates
// live memory from collectable per-record garbage: a streaming merge keeps only the
// fixed-size Summary live, so its peak is flat in n, whereas a merge that materializes a
// per-path []Change holds it live for the whole drain, so its peak grows with n — even if
// it drops the slice before returning, which a post-return sample cannot catch.
func comparePeakLiveBytes(t *testing.T, n int) int64 {
	t.Helper()
	a, repo, _ := newAssessor(t)
	left := putCPEntries(t, repo, 0x11, "a", n, 0)
	right := putCPEntries(t, repo, 0x22, "a", n, 1)

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)

	var peak atomic.Int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			runtime.GC()
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			if live := int64(ms.HeapAlloc) - int64(base.HeapAlloc); live > peak.Load() {
				peak.Store(live)
			}
			time.Sleep(100 * time.Microsecond)
		}
	}()

	cmp, err := a.Compare(context.Background(), state.Range{Left: idRef(left), Right: idRef(right)}, state.NowContext{})
	close(stop)
	<-done

	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if cmp.Relationship() != provider.RelDifferent {
		t.Fatalf("relationship = %v, want different", cmp.Relationship())
	}
	if counts, _ := cmp.Counts(); counts.Modified() != n {
		t.Fatalf("modified = %d, want %d", counts.Modified(), n)
	}
	runtime.KeepAlive(cmp)
	return peak.Load()
}

// TestCompareBoundedPeakMemoryAtScale is the production-path *peak* working-set guarantee:
// the real Assessor.Compare, streaming two large on-disk manifests, must hold only the
// fixed-size Summary live at any instant, so its peak live heap is flat in tree
// cardinality. Because the oracle samples live memory (post-GC) *during* the comparison, it
// catches the core regression a post-return sample cannot — a merge that materializes a
// per-path []Change, counts it, and drops it before returning a compact Comparison. The
// structural TestMergeCountsUsesStreamingMerge is kept only as an auxiliary name-based
// guard, not the primary proof.
//
// Mutation proof: in mergeCounts, accumulate the drained changes into a local
// `[]compare.Change` (append in the loop) and drop it before returning — the
// large-cardinality peak jumps by many MiB and this test goes red.
func TestCompareBoundedPeakMemoryAtScale(t *testing.T) {
	// Streaming holds only the Summary plus bounded decode/merge buffers live; a
	// materialized 100k-path change list is ~15+ MiB live. The band must sit above the
	// streaming path's transient decode/merge + GC-lag noise (observed ~6-7 MiB at 100k)
	// yet well below that ~15 MiB materialization regression. 10 MiB gives ~3 MiB headroom
	// over the noise and ~5 MiB below the regression, so the oracle stays sharp without
	// flaking on GC timing.
	const band = 10 << 20 // 10 MiB
	small := comparePeakLiveBytes(t, 10000)
	large := comparePeakLiveBytes(t, 100000)
	t.Logf("peak live heap: n=10000 -> %d bytes, n=100000 -> %d bytes (delta %d)", small, large, large-small)
	if large-small > band {
		t.Errorf("peak live heap grew by %d bytes across a 10x cardinality increase (band %d): the production comparison materializes changes, it does not stream", large-small, band)
	}
}

// deleteManifest removes a checkpoint's manifest.jsonl, leaving its header intact — a
// partial (corrupt) store that a header-only read would not notice.
func deleteManifest(t *testing.T, layout paths.Layout, id checkpoint.CheckpointID) {
	t.Helper()
	dir := filepath.Join(layout.CheckpointsDir(), id.String())
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "manifest.jsonl")); err != nil {
		t.Fatalf("remove manifest: %v", err)
	}
}

// TestResolveVerifiesManifest proves resolve does not publish a resolved,
// reconstructable identity for a checkpoint whose manifest is unreadable: the manifest is
// streamed through its verifying cursor first, and an absent manifest payload resolves to
// an unavailable/manifest-unavailable assessment instead of a false resolved. A missing
// payload is distinct from corrupt content (metadata-corrupt) — see TestResolveCorruptIsUnavailable.
func TestResolveVerifiesManifest(t *testing.T) {
	a, repo, layout := newAssessor(t)
	id := putCP(t, repo, 0x11, hexA, "a")
	deleteManifest(t, layout, id)
	got, err := a.Resolve(context.Background(), state.Ref{Kind: state.RefLatest, Display: "latest"}, "latest", state.NowContext{})
	if err != nil {
		t.Fatalf("Resolve returned a fault, want unavailable assessment: %v", err)
	}
	if got.Outcome() != provider.OutcomeUnavailable {
		t.Fatalf("outcome = %v, want unavailable (a checkpoint with no manifest is not reconstructable)", got.Outcome())
	}
	if reason, ok := got.Reason(); !ok || reason != provider.ReasonManifestUnavailable {
		t.Errorf("reason = (%v,%v), want (manifest-unavailable,true) for an absent payload", reason, ok)
	}
	if _, ok := got.Identity(); ok {
		t.Error("an unverifiable manifest must yield no identity")
	}
}

// TestResolveCorruptManifestContentIsMetadataCorrupt proves the missing-vs-corrupt
// distinction: a present but malformed manifest is metadata-corrupt, not the
// manifest-unavailable an absent payload yields.
func TestResolveCorruptManifestContentIsMetadataCorrupt(t *testing.T) {
	a, repo, layout := newAssessor(t)
	id := putCP(t, repo, 0x11, hexA, "a")
	dir := filepath.Join(layout.CheckpointsDir(), id.String())
	manifest := filepath.Join(dir, "manifest.jsonl")
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	if err := os.Chmod(manifest, 0o644); err != nil {
		t.Fatalf("chmod manifest: %v", err)
	}
	if err := os.WriteFile(manifest, []byte("{not valid jsonl\n"), 0o644); err != nil {
		t.Fatalf("corrupt manifest content: %v", err)
	}
	got, err := a.Resolve(context.Background(), state.Ref{Kind: state.RefLatest, Display: "latest"}, "latest", state.NowContext{})
	if err != nil {
		t.Fatalf("Resolve returned a fault, want unavailable: %v", err)
	}
	if reason, ok := got.Reason(); !ok || reason != provider.ReasonMetadataCorrupt {
		t.Errorf("reason = (%v,%v), want (metadata-corrupt,true) for malformed content", reason, ok)
	}
}

// TestCompareEqualFastPathVerifiesManifest proves the hash-based equal fast path does not
// declare equal without confirming both operands' manifests are readable, and that an
// unavailable comparison never publishes an unreadable operand as a resolved identity:
// comparing a manifest-less checkpoint to itself is unavailable with no operand
// identities.
func TestCompareEqualFastPathVerifiesManifest(t *testing.T) {
	a, repo, layout := newAssessor(t)
	id := putCP(t, repo, 0x11, hexA, "a")
	deleteManifest(t, layout, id)
	cmp, err := a.Compare(context.Background(), state.Range{Left: idRef(id), Right: idRef(id)}, state.NowContext{})
	if err != nil {
		t.Fatalf("Compare returned a fault, want unavailable: %v", err)
	}
	if cmp.Relationship() != provider.RelUnavailable {
		t.Errorf("relationship = %v, want unavailable (an unreadable manifest cannot be declared equal)", cmp.Relationship())
	}
	if _, ok := cmp.Left(); ok {
		t.Error("both operands are unreadable; neither may be published as a resolved identity")
	}
	if _, ok := cmp.Right(); ok {
		t.Error("both operands are unreadable; neither may be published as a resolved identity")
	}
}

// TestCompareMergeFailurePublishesOnlyReadableOperand proves a mid-merge read failure
// yields an unavailable comparison that publishes only the operand whose manifest still
// verifies readable — never the unreadable one as a reconstructable identity.
func TestCompareMergeFailurePublishesOnlyReadableOperand(t *testing.T) {
	a, repo, layout := newAssessor(t)
	left := putCP(t, repo, 0x11, hexA, "a")
	right := putCP(t, repo, 0x22, hexB, "a") // same scan config, different content -> different (merge)
	deleteManifest(t, layout, right)         // right's manifest is gone; the merge fails on it
	cmp, err := a.Compare(context.Background(), state.Range{Left: idRef(left), Right: idRef(right)}, state.NowContext{})
	if err != nil {
		t.Fatalf("Compare returned a fault, want unavailable: %v", err)
	}
	if cmp.Relationship() != provider.RelUnavailable {
		t.Fatalf("relationship = %v, want unavailable", cmp.Relationship())
	}
	if _, ok := cmp.Left(); !ok {
		t.Error("left manifest is readable; its identity must be published")
	}
	if _, ok := cmp.Right(); ok {
		t.Error("right manifest is unreadable; it must not be published as a resolved identity")
	}
}

// TestCompareUnavailableReasonIsLeftFirstAfterVerification proves the comparison reason
// reflects each operand's final outcome, left-first: a resolved-but-unreadable left
// (manifest-unavailable) beats a not-found right, rather than reporting the right's
// resolution reason because the left only failed later, at verification.
func TestCompareUnavailableReasonIsLeftFirstAfterVerification(t *testing.T) {
	a, repo, layout := newAssessor(t)
	left := putCP(t, repo, 0x11, hexA, "a")
	deleteManifest(t, layout, left) // left resolves, but its manifest is gone
	gone := state.Ref{Kind: state.RefCheckpointPrefix, Display: missingPrefix, Raw: missingPrefix}

	cmp, err := a.Compare(context.Background(), state.Range{Left: idRef(left), Right: gone}, state.NowContext{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if cmp.Relationship() != provider.RelUnavailable {
		t.Fatalf("relationship = %v, want unavailable", cmp.Relationship())
	}
	if reason, ok := cmp.Reason(); !ok || reason != provider.ReasonManifestUnavailable {
		t.Errorf("reason = (%v,%v), want (manifest-unavailable,true): the left operand's verification failure is left-first", reason, ok)
	}
	if _, ok := cmp.Left(); ok {
		t.Error("left manifest is unreadable; it must not be published")
	}
	if _, ok := cmp.Right(); ok {
		t.Error("right did not resolve; it must not be published")
	}
}

// TestCompareResolvesOperandsIndependently proves an unavailable operand does not
// short-circuit the other: the comparison keeps whichever side resolved, regardless of
// operand order.
func TestCompareResolvesOperandsIndependently(t *testing.T) {
	a, repo, _ := newAssessor(t)
	valid := putCP(t, repo, 0x11, hexA, "a")
	gone := state.Ref{Kind: state.RefCheckpointPrefix, Display: missingPrefix, Raw: missingPrefix}

	// missing..valid: left unavailable, but the right (valid) identity is preserved.
	cmp, err := a.Compare(context.Background(), state.Range{Left: gone, Right: idRef(valid)}, state.NowContext{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if cmp.Relationship() != provider.RelUnavailable {
		t.Fatalf("relationship = %v, want unavailable", cmp.Relationship())
	}
	if _, ok := cmp.Left(); ok {
		t.Error("left operand was missing; it must be absent")
	}
	if _, ok := cmp.Right(); !ok {
		t.Error("right operand resolved; it must be preserved even though left failed")
	}
	if reason, ok := cmp.Reason(); !ok || reason != provider.ReasonNotFound {
		t.Errorf("reason = (%v,%v), want (not-found,true)", reason, ok)
	}
}

// countingRepo counts how many times a stored manifest is actually opened for reading
// (drained), by wrapping the streams its OpenManifest hands out. Embedding the interface
// forwards every other method to the real repo unchanged.
type countingRepo struct {
	checkpoint.Repository
	opens *int32
}

func (c countingRepo) OpenManifest(id checkpoint.CheckpointID) (worktree.ManifestStream, error) {
	s, err := c.Repository.OpenManifest(id)
	if err != nil {
		return nil, err
	}
	return countingStream{ManifestStream: s, opens: c.opens}, nil
}

type countingStream struct {
	worktree.ManifestStream
	opens *int32
}

func (c countingStream) Open(ctx context.Context) (worktree.ManifestCursor, error) {
	atomic.AddInt32(c.opens, 1)
	return c.ManifestStream.Open(ctx)
}

// TestCompareReadsEachManifestOncePerOperand guards the single-pass property: a compare
// reads each operand's stored manifest exactly once. A different comparison confirms
// readability via the merge itself, and an equal one via a lone verify pass — never both,
// so the count is one per operand (it was two per operand while verification and the
// merge each drained the manifest).
func TestCompareReadsEachManifestOncePerOperand(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := checkpointjson.NewRepo(layout)
	hasher := blake3hash.New()
	var opens int32
	r := state.NewResolver(state.Deps{
		Checkpoints: countingRepo{Repository: repo, opens: &opens},
		Blobs:       blobstore.New(layout, hasher),
		Hasher:      hasher,
		Runs:        runstore.New(layout, hasher),
	})
	a := stateprovider.NewAssessor(r)
	left := putCP(t, repo, 0x11, hexA, "a")
	right := putCP(t, repo, 0x22, hexB, "a") // same scan config, different content -> different (merge)

	atomic.StoreInt32(&opens, 0)
	cmp, err := a.Compare(context.Background(), state.Range{Left: idRef(left), Right: idRef(right)}, state.NowContext{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if cmp.Relationship() != provider.RelDifferent {
		t.Fatalf("relationship = %v, want different", cmp.Relationship())
	}
	if n := atomic.LoadInt32(&opens); n != 2 {
		t.Errorf("different comparison read manifests %d times, want 2 (one full pass per operand)", n)
	}

	// Equal (same checkpoint id): no merge, one verify pass per operand.
	atomic.StoreInt32(&opens, 0)
	if _, err := a.Compare(context.Background(), state.Range{Left: idRef(left), Right: idRef(left)}, state.NowContext{}); err != nil {
		t.Fatalf("Compare equal: %v", err)
	}
	if n := atomic.LoadInt32(&opens); n != 2 {
		t.Errorf("equal comparison read manifests %d times, want 2 (one verify pass per operand)", n)
	}
}

func TestCompareMissingOperandUnavailable(t *testing.T) {
	a, repo, _ := newAssessor(t)
	left := putCP(t, repo, 0x11, hexA, "a")
	cmp, err := a.Compare(context.Background(), state.Range{Left: idRef(left), Right: state.Ref{Kind: state.RefCheckpointPrefix, Display: missingPrefix, Raw: missingPrefix}}, state.NowContext{})
	if err != nil {
		t.Fatalf("Compare returned a fault, want unavailable: %v", err)
	}
	if cmp.Relationship() != provider.RelUnavailable {
		t.Fatalf("relationship = %v, want unavailable", cmp.Relationship())
	}
	if _, ok := cmp.Left(); !ok {
		t.Error("unavailable comparison should still carry the resolved left operand")
	}
	if _, ok := cmp.Right(); ok {
		t.Error("unavailable comparison must not carry an unresolved right operand")
	}
	reason, ok := cmp.Reason()
	if !ok || reason != provider.ReasonNotFound {
		t.Errorf("reason = (%v,%v), want (not-found,true)", reason, ok)
	}
}
