package checkpointjson

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/scantest"
)

const fullHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func idFrom(t *testing.T, b byte) checkpoint.CheckpointID {
	t.Helper()
	id, err := checkpoint.NewCheckpointID(bytes.NewReader(bytes.Repeat([]byte{b}, 20)))
	if err != nil {
		t.Fatalf("NewCheckpointID: %v", err)
	}
	return id
}

func chash(t *testing.T) hashing.ContentHash {
	t.Helper()
	h, err := hashing.ParseContentHash("blake3:" + fullHex)
	if err != nil {
		t.Fatalf("ParseContentHash: %v", err)
	}
	return h
}

func thash(t *testing.T) hashing.TreeHash {
	t.Helper()
	h, err := hashing.ParseTreeHash("blake3:" + fullHex)
	if err != nil {
		t.Fatalf("ParseTreeHash: %v", err)
	}
	return h
}

func rel(t *testing.T, s string) worktree.RelPath {
	t.Helper()
	p, err := worktree.ParseRelPath(s)
	if err != nil {
		t.Fatalf("ParseRelPath: %v", err)
	}
	return p
}

// checkpointFixture is exactly what a write consumes: the non-derived build metadata
// and the manifest records. It carries no tree hash, stats, or record count, because
// the repository derives those from the records as it streams them — a fixture cannot
// hand the store a derived fact to trust.
type checkpointFixture struct {
	build   checkpoint.CheckpointBuild
	entries []worktree.Entry
	skipped []worktree.SkippedInput
}

func (f checkpointFixture) id() checkpoint.CheckpointID { return f.build.ID }

func (f checkpointFixture) records() int { return len(f.entries) + len(f.skipped) }

// stream is the fixture's manifest as the re-openable stream PutManifest consumes.
func (f checkpointFixture) stream() worktree.ManifestStream {
	return scantest.CanonicalStream(f.entries, f.skipped)
}

// derivedHeader computes the header the fixture's records imply, by folding them with
// the same reducer the repository uses. It is the independent oracle a test compares a
// stored header against, and the source of header bytes for tests that publish a
// hand-edited document without going through a store.
func (f checkpointFixture) derivedHeader(t *testing.T) checkpoint.CheckpointHeader {
	t.Helper()
	red, err := worktree.ReduceCursor(blake3hash.New(), scantest.CanonicalCursor(f.entries, f.skipped))
	if err != nil {
		t.Fatalf("reducing fixture manifest: %v", err)
	}
	return f.build.Header(red.Hash, checkpoint.StatsFromReduced(red.Stats), red.Count)
}

// put publishes the fixture through the production streaming write contract.
func (f checkpointFixture) put(t *testing.T, repo *Repo) checkpoint.CheckpointHeader {
	t.Helper()
	h, err := repo.PutManifest(context.Background(), f.build, f.stream())
	if err != nil {
		t.Fatalf("PutManifest(%s): %v", f.id().Short(), err)
	}
	return h
}

// putErr publishes the fixture and returns the write's error for inspection.
func (f checkpointFixture) putErr(repo *Repo) error {
	_, err := repo.PutManifest(context.Background(), f.build, f.stream())
	return err
}

// readCheckpoint performs a full read of a stored checkpoint — the header plus a
// complete drain of its manifest — and returns the first error either half reports.
// It is the read a test uses when it only cares that a damaged store fails loudly.
func readCheckpoint(repo *Repo, id checkpoint.CheckpointID) error {
	if _, err := repo.Header(id); err != nil {
		return err
	}
	stream, err := repo.OpenManifest(id)
	if err != nil {
		return err
	}
	cur, err := stream.Open(context.Background())
	if err != nil {
		return err
	}
	defer func() { _ = cur.Close() }()
	for cur.Next() {
	}
	return cur.Err()
}

// buildCheckpoint assembles a checkpoint fixture exercising every entry/skipped shape
// so the round-trip test covers the full schema.
func buildCheckpoint(t *testing.T, id checkpoint.CheckpointID, created time.Time, withGit bool) checkpointFixture {
	t.Helper()
	fullStat := worktree.StatSignature{Size: 10, MtimeNs: 5, CtimeNs: 6, Mode: 0o644, Dev: 1, Ino: 2, Nlink: 1}
	// An entry whose platform omitted ctime/dev/ino/nlink.
	omittedStat := worktree.StatSignature{Size: 20, MtimeNs: 7, Mode: 0o644,
		Omitted: worktree.FieldSet(0).With(worktree.FieldCtime).With(worktree.FieldDev).With(worktree.FieldIno).With(worktree.FieldNlink)}

	blobEntry, err := worktree.NewRegularEntry(rel(t, "a.txt"), chash(t), worktree.StorageBlob, fullStat, worktree.TraversalInfo{})
	if err != nil {
		t.Fatal(err)
	}
	hashOnly, err := worktree.NewRegularEntry(rel(t, "big.bin"), chash(t), worktree.StorageHashOnly, omittedStat, worktree.TraversalInfo{})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := worktree.NewDirEntry(rel(t, "sub"), worktree.TraversalInfo{})
	if err != nil {
		t.Fatal(err)
	}
	target, _ := worktree.NewSymlinkTarget("a.txt")
	// A symlink reached by following another symlink, to exercise traversal.
	link, err := worktree.NewSymlinkEntry(rel(t, "sub/link"), target, worktree.StatSignature{Mode: 0o777},
		worktree.TraversalInfo{Followed: true, SourcePath: rel(t, "sub"), Depth: 1})
	if err != nil {
		t.Fatal(err)
	}

	special, err := worktree.NewSkippedInput(rel(t, "dev/null"), worktree.ReasonSpecialFile, 0, worktree.StatSignature{}, "", worktree.SymlinkTarget{}, worktree.TraversalInfo{})
	if err != nil {
		t.Fatal(err)
	}
	readErr, err := worktree.NewSkippedInput(rel(t, "secret"), worktree.ReasonReadError, 99, fullStat, "permission-denied", worktree.SymlinkTarget{}, worktree.TraversalInfo{})
	if err != nil {
		t.Fatal(err)
	}
	cycleTarget, _ := worktree.NewSymlinkTarget("loop")
	cycle, err := worktree.NewSkippedInput(rel(t, "loop"), worktree.ReasonSymlinkCycle, 0, worktree.StatSignature{Mode: 0o777}, "", cycleTarget, worktree.TraversalInfo{})
	if err != nil {
		t.Fatal(err)
	}

	entries := []worktree.Entry{blobEntry, hashOnly, dir, link}
	skipped := []worktree.SkippedInput{special, readErr, cycle}

	var git *checkpoint.GitMetadata
	if withGit {
		git = &checkpoint.GitMetadata{InWorktree: true, Branch: "main", Commit: fullHex, ShortCommit: "0123456",
			Dirty: checkpoint.DirtySummary{Modified: 2, Untracked: 1}}
	}

	return checkpointFixture{
		build: checkpoint.CheckpointBuild{
			ID:                   id,
			CreatedAt:            created,
			Root:                 fixtureRoot(t),
			CommandCwd:           "sub",
			AwaVersion:           "0.0.0-dev",
			ScanConfigHash:       hashing.ConfigHashFromTree(thash(t)),
			CheckpointPolicyHash: hashing.ConfigHashFromTree(thash(t)),
			TrustMode:            config.TrustNormal,
			OmittedStatFields:    worktree.FieldSet(0).With(worktree.FieldCtime),
			Git:                  git,
		},
		entries: entries,
		skipped: skipped,
	}
}

func TestCheckpointHeaderRoundTrip(t *testing.T) {
	for _, withGit := range []bool{true, false} {
		s := buildCheckpoint(t, idFrom(t, 0x11), time.Unix(1_700_000_000, 123).UTC(), withGit)
		data, err := encodeHeader(s.derivedHeader(t))
		if err != nil {
			t.Fatalf("encodeHeader: %v", err)
		}
		got, err := decodeHeader(data)
		if err != nil {
			t.Fatalf("decodeHeader: %v", err)
		}
		// Re-encoding the decoded header must reproduce identical bytes: a strong,
		// field-agnostic check that nothing was lost or altered.
		again, err := encodeHeader(got)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		if !bytes.Equal(data, again) {
			t.Fatalf("round trip changed bytes (withGit=%v):\nfirst:\n%s\nsecond:\n%s", withGit, data, again)
		}
		if (got.Git == nil) != (!withGit) {
			t.Fatalf("git presence mismatch: got nil=%v withGit=%v", got.Git == nil, withGit)
		}
	}
}

func TestPutRejectsDuplicateID(t *testing.T) {
	repo := NewRepo(paths.New(t.TempDir()))
	s := buildCheckpoint(t, idFrom(t, 0x22), time.Unix(1_700_000_000, 0).UTC(), false)
	s.put(t, repo)
	if err := s.putErr(repo); !errors.Is(err, checkpoint.ErrIDCollision) {
		t.Fatalf("second Put err = %v, want ErrIDCollision", err)
	}
}

// TestPutRejectsMalformedBuildBeforeWriting proves the write validates its
// non-derived metadata before it consumes the manifest stream, so malformed metadata
// costs no I/O and leaves no partial record behind. The derived fields cannot be
// wrong here by construction — the write derives them from the records themselves
// (TestCheckpointPutManifestDerivesHeader) — so metadata is what a caller can still
// get wrong.
func TestPutRejectsMalformedBuildBeforeWriting(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)

	bad := buildCheckpoint(t, idFrom(t, 0x31), time.Unix(1, 0).UTC(), false)
	bad.build.Root = "relative/path"
	if err := bad.putErr(repo); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("Put with a relative root err = %v, want a root rejection", err)
	}

	weak := buildCheckpoint(t, idFrom(t, 0x32), time.Unix(1, 0).UTC(), false)
	weak.build.FastModeWeakSignature = true // contradicts the recorded trust mode
	if err := weak.putErr(repo); err == nil || !strings.Contains(err.Error(), "weak signature") {
		t.Fatalf("Put with a contradictory weak-signature flag err = %v, want a rejection", err)
	}

	// Neither refused write left anything on disk — not even a manifest without its
	// committing header.
	if health, _ := repo.StoreHealthAll(context.Background()); health.Recorded() != 0 {
		t.Fatalf("malformed checkpoints were persisted: %d", health.Recorded())
	}
	for _, id := range []checkpoint.CheckpointID{bad.id(), weak.id()} {
		if _, err := os.Stat(filepath.Join(layout.CheckpointsDir(), id.String())); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a refused write left %s on disk: %v", id.Short(), err)
		}
	}
}

func TestListNewestFirstWithTieBreak(t *testing.T) {
	repo := NewRepo(paths.New(t.TempDir()))
	t0 := time.Unix(1_700_000_000, 0).UTC()
	t1 := time.Unix(1_700_000_100, 0).UTC()
	// Two share t1 (tie -> id desc), one older at t0.
	sLowAtT1 := buildCheckpoint(t, idFrom(t, 0x01), t1, false)
	sHighAtT1 := buildCheckpoint(t, idFrom(t, 0xff), t1, false)
	sOld := buildCheckpoint(t, idFrom(t, 0x05), t0, false)
	for _, s := range []checkpointFixture{sLowAtT1, sHighAtT1, sOld} {
		s.put(t, repo)
	}
	health, err := repo.StoreHealthAll(context.Background())
	if err != nil {
		t.Fatalf("StoreHealthAll: %v", err)
	}
	list := health.NewestHeaders()
	if len(list) != 3 {
		t.Fatalf("len = %d, want 3", len(list))
	}
	// Newest first; within t1, higher id first.
	if list[0].ID != sHighAtT1.id() || list[1].ID != sLowAtT1.id() || list[2].ID != sOld.id() {
		t.Fatalf("order = %s, %s, %s", list[0].ID.Short(), list[1].ID.Short(), list[2].ID.Short())
	}
	latest, ok := health.Latest()
	if !ok || latest.ID != sHighAtT1.id() {
		t.Fatalf("Latest = %v ok=%v", latest.ID.Short(), ok)
	}
}

// TestStoreHealthNewestPicksTheSameNewestWindow proves the bounded selection and the
// full sort agree on which checkpoints are newest, including the id tie-break at an
// equal creation time. The oracle is the full read's own prefix, which is ordered by
// the aggregate rather than by the bounded heap under test.
func TestStoreHealthNewestPicksTheSameNewestWindow(t *testing.T) {
	repo := NewRepo(paths.New(t.TempDir()))
	t0 := time.Unix(1_700_000_000, 0).UTC()
	t1 := time.Unix(1_700_000_100, 0).UTC()
	sLowAtT1 := buildCheckpoint(t, idFrom(t, 0x01), t1, false)
	sHighAtT1 := buildCheckpoint(t, idFrom(t, 0xff), t1, false)
	sOld := buildCheckpoint(t, idFrom(t, 0x05), t0, false)
	for _, s := range []checkpointFixture{sLowAtT1, sHighAtT1, sOld} {
		s.put(t, repo)
	}
	full, err := repo.StoreHealthAll(context.Background())
	if err != nil {
		t.Fatalf("StoreHealthAll: %v", err)
	}
	all := full.NewestHeaders()
	// A bound of one must land on the tie-break winner, and a bound past the store's
	// size must return everything without padding or truncating.
	for _, n := range []int{1, 2, 3, 20} {
		got, err := repo.StoreHealthNewest(context.Background(), n)
		if err != nil {
			t.Fatalf("StoreHealthNewest(%d): %v", n, err)
		}
		if got.Recorded() != 3 {
			t.Errorf("StoreHealthNewest(%d).Recorded() = %d, want the exact readable total 3", n, got.Recorded())
		}
		window := got.NewestHeaders()
		if len(window) != min(n, 3) {
			t.Fatalf("StoreHealthNewest(%d) retained %d headers, want %d", n, len(window), min(n, 3))
		}
		for i, h := range window {
			if h.ID != all[i].ID {
				t.Errorf("StoreHealthNewest(%d)[%d] = %s, want %s", n, i, h.ID.Short(), all[i].ID.Short())
			}
		}
	}
}

// TestStoreHealthNewestRejectsNonPositiveWindow pins the loud boundary: a bounded read
// with no window is a caller error, never a silent full history.
func TestStoreHealthNewestRejectsNonPositiveWindow(t *testing.T) {
	repo := NewRepo(paths.New(t.TempDir()))
	s := buildCheckpoint(t, idFrom(t, 0x07), time.Unix(1_700_000_000, 0).UTC(), false)
	s.put(t, repo)
	for _, n := range []int{0, -1} {
		health, err := repo.StoreHealthNewest(context.Background(), n)
		if err == nil {
			t.Fatalf("StoreHealthNewest(%d) = %d recorded, nil error; want a rejection", n, health.Recorded())
		}
	}
}

// TestStoreHealthOnEmptyStore proves an absent store reads as empty rather than as an
// error, on both operations.
func TestStoreHealthOnEmptyStore(t *testing.T) {
	repo := NewRepo(paths.New(t.TempDir()))
	full, err := repo.StoreHealthAll(context.Background())
	if err != nil || full.State() != checkpoint.StoreEmpty || full.Recorded() != 0 {
		t.Fatalf("StoreHealthAll on an empty store = state %v recorded %d err %v, want empty/0/nil", full.State(), full.Recorded(), err)
	}
	bounded, err := repo.StoreHealthNewest(context.Background(), 20)
	if err != nil || bounded.State() != checkpoint.StoreEmpty || len(bounded.NewestHeaders()) != 0 {
		t.Fatalf("StoreHealthNewest on an empty store = state %v %d headers err %v, want empty/0/nil",
			bounded.State(), len(bounded.NewestHeaders()), err)
	}
}

// TestStoreHealthNewestCountsRecordsBeyondTheWindow is the honesty guard on the
// bounded read: the retained window is small, but the scan behind it must still reach
// every committed record. The unreadable records are deliberately the OLDEST in the
// store, so a scan that stopped once the newest window filled would report a healthy
// store — the one answer that must never be given.
func TestStoreHealthNewestCountsRecordsBeyondTheWindow(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	base := time.Unix(1_700_000_000, 0).UTC()

	// Six readable checkpoints, newest last.
	var readable []checkpointFixture
	for i := 0; i < 6; i++ {
		s := buildCheckpoint(t, idFrom(t, byte(0x10+i)), base.Add(time.Duration(i)*time.Minute), false)
		s.put(t, repo)
		readable = append(readable, s)
	}
	// One incompatible and one corrupt record, both older than every readable one.
	incompatible := buildCheckpoint(t, idFrom(t, 0x01), base.Add(-2*time.Minute), false)
	incompatible.put(t, repo)
	bumpHeaderSchema(t, repo.headerFor(incompatible.id()), headerSchemaVersion+1)

	corrupt := buildCheckpoint(t, idFrom(t, 0x02), base.Add(-time.Minute), false)
	corrupt.put(t, repo)
	corruptHeader := repo.headerFor(corrupt.id())
	if err := os.Chmod(corruptHeader, 0o644); err != nil {
		t.Fatalf("chmod header: %v", err)
	}
	if err := os.WriteFile(corruptHeader, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("corrupt header: %v", err)
	}

	health, err := repo.StoreHealthNewest(context.Background(), 2)
	if err != nil {
		t.Fatalf("StoreHealthNewest(2): %v", err)
	}
	if health.Recorded() != 6 || health.Incompatible() != 1 || health.Corrupt() != 1 {
		t.Fatalf("counts = recorded %d incompatible %d corrupt %d, want 6/1/1",
			health.Recorded(), health.Incompatible(), health.Corrupt())
	}
	if health.State() != checkpoint.StorePartial {
		t.Fatalf("State() = %v, want partial", health.State())
	}
	window := health.NewestHeaders()
	if len(window) != 2 {
		t.Fatalf("retained %d headers, want 2", len(window))
	}
	if window[0].ID != readable[5].id() || window[1].ID != readable[4].id() {
		t.Fatalf("window = %s, %s, want the two newest %s, %s",
			window[0].ID.Short(), window[1].ID.Short(), readable[5].id().Short(), readable[4].id().Short())
	}
}

// TestStoreHealthHonorsCancellation proves the checkpoint header walk honors a cancelled
// context mid-read rather than reading every header: with checkpoints present, a
// pre-cancelled context makes both store-health reads return context.Canceled promptly,
// so a Ctrl+C during a long local history is an interruption rather than a completed
// scan. The bounded read is asked for a window smaller than the store, so a cancellation
// it reports cannot be an artifact of having finished anyway.
func TestStoreHealthHonorsCancellation(t *testing.T) {
	repo := NewRepo(paths.New(t.TempDir()))
	base := time.Unix(1_700_000_000, 0).UTC()
	for i := 0; i < 3; i++ {
		s := buildCheckpoint(t, idFrom(t, byte(i+1)), base.Add(time.Duration(i)*time.Minute), false)
		s.put(t, repo)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// The id enumeration itself honors cancellation: it streams the directory in batches and
	// checks ctx per entry, so the walk returns context.Canceled rather than materializing the
	// whole directory (and stat-ing every committed header) first. This assertion fails if
	// cancellation is only checked after a full ReadDir(-1) — the enumerator would then visit
	// every id with no error and only a caller's later loop would notice the cancellation.
	if err := repo.eachCommittedID(ctx, func(checkpoint.CheckpointID) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("eachCommittedID err = %v, want context.Canceled (enumeration not interruptible mid-scan?)", err)
	}

	if _, err := repo.StoreHealthAll(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("StoreHealthAll err = %v, want context.Canceled", err)
	}
	if _, err := repo.StoreHealthNewest(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Errorf("StoreHealthNewest err = %v, want context.Canceled", err)
	}
	if _, err := repo.ListIDs(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("ListIDs err = %v, want context.Canceled", err)
	}
	// ResolvePrefix enumerates the ids to match a prefix, so it honors cancellation too.
	if _, err := repo.ResolvePrefix(ctx, "0"); !errors.Is(err, context.Canceled) {
		t.Errorf("ResolvePrefix err = %v, want context.Canceled", err)
	}
}

// TestEachCommittedIDDoesNotReadACallbackFailureAsAnEmptyStore pins how the walk
// classifies errors. It reads a missing checkpoints directory as an empty store, and
// the callback reads real files — so a per-record failure whose cause is "not exist"
// (a checkpoint deleted mid-scan) must reach the caller as that failure. Reporting a
// store with checkpoints in it as empty is the one answer that must never be given.
func TestEachCommittedIDDoesNotReadACallbackFailureAsAnEmptyStore(t *testing.T) {
	repo := NewRepo(paths.New(t.TempDir()))
	s := buildCheckpoint(t, idFrom(t, 0x33), time.Unix(1_700_000_000, 0).UTC(), false)
	s.put(t, repo)

	boom := fmt.Errorf("reading checkpoint header: %w", os.ErrNotExist)
	err := repo.eachCommittedID(context.Background(), func(checkpoint.CheckpointID) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("eachCommittedID err = %v, want the callback's failure (a not-exist cause must not read as an empty store)", err)
	}
}

// TestFileAtReservedCheckpointAddressIsCorrupt pins the reserved-address rule. The
// checkpoints directory owns the id namespace and a checkpoint's only address is its
// directory, so a bare "<id>.json" is a foreign node on that namespace: structural
// corruption, on every path, and never quietly skipped.
//
// The two wrong answers this guards are the dangerous ones. Skipping it would let a
// directory full of such files read back as an EMPTY store — a listing that looks
// healthy while every record is invisible. Classifying it as merely incompatible
// would imply awa inspected it and recognized a schema; it does not open the file at
// all, precisely because a document at an unowned address cannot be trusted to mean
// what its contents claim.
func TestFileAtReservedCheckpointAddressIsCorrupt(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)

	id := idFrom(t, 0xbb)
	if err := os.MkdirAll(layout.CheckpointsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	// The bytes are deliberately a syntactically valid, current-schema-looking
	// document: the address is what condemns it, not its contents.
	body := []byte(`{"schema_version":1,"id":"` + id.String() + `"}`)
	if err := os.WriteFile(filepath.Join(layout.CheckpointsDir(), id.String()+checkpointExt), body, 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.ListIDs(context.Background()); !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("ListIDs err = %v, want ErrCorruptStore", err)
	}
	if _, err := repo.ResolvePrefix(context.Background(), id.String()[:4]); !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("ResolvePrefix err = %v, want ErrCorruptStore", err)
	}
	// The store-health reads are the resilient ones, but structural corruption of the id
	// set is not something they can degrade around: both must fail rather than report an
	// empty or healthy store, and the bounded one must not read as healthy just because
	// its window never needed the bad address.
	health, err := repo.StoreHealthAll(context.Background())
	if !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("StoreHealthAll err = %v (state %v), want ErrCorruptStore", err, health.State())
	}
	bounded, err := repo.StoreHealthNewest(context.Background(), 1)
	if !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("StoreHealthNewest err = %v (state %v), want ErrCorruptStore", err, bounded.State())
	}
	// And the file is left alone: classifying is not reclaiming.
	if _, err := os.Stat(filepath.Join(layout.CheckpointsDir(), id.String()+checkpointExt)); err != nil {
		t.Fatalf("reserved-address node was removed by a read: %v", err)
	}
}

// TestIncompatibleHeaderSchemaIsNotCorrupt is the other half of the boundary: a
// header at the correct address whose declared schema is simply not this build's is
// incompatible, not damage, and not an empty store.
func TestIncompatibleHeaderSchemaIsNotCorrupt(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	s := buildCheckpoint(t, idFrom(t, 0xbc), time.Unix(1, 0).UTC(), false)
	s.put(t, repo)
	bumpHeaderSchema(t, repo.headerFor(s.id()), headerSchemaVersion+1)

	for _, tc := range []struct {
		name string
		read func() error
	}{
		{"Header", func() error { _, err := repo.Header(s.id()); return err }},
		{"full read", func() error { return readCheckpoint(repo, s.id()) }},
		{"OpenManifest", func() error { _, err := repo.OpenManifest(s.id()); return err }},
	} {
		err := tc.read()
		if !errors.Is(err, checkpoint.ErrIncompatibleFormat) {
			t.Errorf("%s err = %v, want ErrIncompatibleFormat", tc.name, err)
		}
		if errors.Is(err, checkpoint.ErrCorruptStore) {
			t.Errorf("%s classified an incompatible schema as corruption: %v", tc.name, err)
		}
	}

	health, err := repo.StoreHealthAll(context.Background())
	if err != nil {
		t.Fatalf("StoreHealthAll err = %v", err)
	}
	if health.State() != checkpoint.StoreIncompatible || health.Incompatible() != 1 {
		t.Fatalf("StoreHealthAll = state %v incompatible %d, want incompatible/1", health.State(), health.Incompatible())
	}
	if health.Recorded() != 0 || !health.AnyUnreadable() {
		t.Fatalf("an incompatible store reported %d readable / unreadable=%v, want 0/true",
			health.Recorded(), health.AnyUnreadable())
	}
}

// bumpHeaderSchema rewrites a published header's declared schema_version in place,
// leaving every other byte untouched. It is how the tests build a record from a
// generation this build does not speak without keeping a copy of any real retired
// shape around: the version alone is what the reader classifies on.
func bumpHeaderSchema(t *testing.T, headerPath string, version int) {
	t.Helper()
	editHeader(t, headerPath, func(m map[string]json.RawMessage) {
		m["schema_version"] = json.RawMessage(strconv.Itoa(version))
	})
}

func TestLatestEmpty(t *testing.T) {
	repo := NewRepo(paths.New(t.TempDir()))
	health, err := repo.StoreHealthNewest(context.Background(), 1)
	if err != nil {
		t.Fatalf("StoreHealthNewest on empty: %v", err)
	}
	if _, ok := health.Latest(); ok {
		t.Fatal("Latest on an empty store reported a checkpoint")
	}
}

func TestResolvePrefix(t *testing.T) {
	repo := NewRepo(paths.New(t.TempDir()))
	a := buildCheckpoint(t, idFrom(t, 0x00), time.Unix(1, 0).UTC(), false) // id "0000..."
	b := buildCheckpoint(t, idFrom(t, 0xff), time.Unix(2, 0).UTC(), false)
	a.put(t, repo)
	b.put(t, repo)
	got, err := repo.ResolvePrefix(context.Background(), b.id().Short())
	if err != nil || got != b.id() {
		t.Fatalf("ResolvePrefix(short) = %v err=%v, want %v", got.Short(), err, b.id().Short())
	}
	// Neither id ("0000…" from 0x00, "zzzz…" from 0xff) starts with "1".
	if _, err := repo.ResolvePrefix(context.Background(), "1"); !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("missing prefix err = %v, want ErrNotFound", err)
	}
	// An empty prefix is malformed input, not "match all": it must be rejected
	// distinctly from not-found and ambiguous.
	if _, err := repo.ResolvePrefix(context.Background(), ""); err == nil || errors.Is(err, checkpoint.ErrNotFound) || errors.Is(err, checkpoint.ErrAmbiguousPrefix) {
		t.Fatalf("empty prefix err = %v, want a malformed-prefix error", err)
	}
	// A garbage character is also malformed, distinct from not-found.
	if _, err := repo.ResolvePrefix(context.Background(), "!!"); err == nil || errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("garbage prefix err = %v, want a malformed-prefix error", err)
	}
}

// TestResolvePrefixAmbiguousAmongManyIDs proves the streamed resolution still counts
// every match rather than returning the first one it happens to see: two ids share the
// prefix among unrelated ones, so a resolution that stopped at its first match would
// report a single confident answer instead of ambiguity.
func TestResolvePrefixAmbiguousAmongManyIDs(t *testing.T) {
	repo := NewRepo(paths.New(t.TempDir()))
	base := time.Unix(1_700_000_000, 0).UTC()
	for i := 0; i < 3; i++ {
		s := buildCheckpoint(t, idFrom(t, byte(0x10+i)), base.Add(time.Duration(i)*time.Minute), false)
		s.put(t, repo)
	}
	// Two ids built from 0x00 and 0x01 share their leading character; every other id
	// in the store starts elsewhere in the alphabet.
	first := buildCheckpoint(t, idFrom(t, 0x00), base, false)
	second := buildCheckpoint(t, idFrom(t, 0x01), base.Add(time.Hour), false)
	first.put(t, repo)
	second.put(t, repo)
	shared := first.id().String()[:1]
	if second.id().String()[:1] != shared {
		t.Fatalf("fixture ids %s and %s do not share a leading character", first.id().Short(), second.id().Short())
	}

	_, err := repo.ResolvePrefix(context.Background(), shared)
	if !errors.Is(err, checkpoint.ErrAmbiguousPrefix) {
		t.Fatalf("ResolvePrefix(%q) err = %v, want ErrAmbiguousPrefix", shared, err)
	}
	// The full id of one of them is still unambiguous in the same store.
	got, err := repo.ResolvePrefix(context.Background(), second.id().String())
	if err != nil || got != second.id() {
		t.Fatalf("ResolvePrefix(full id) = %s err = %v, want %s", got.Short(), err, second.id().Short())
	}
}

// TestResolvePrefixFailsOnStructuralCorruptionBesideAMatch proves the prefix scan runs
// to completion. A matching id and a foreign node on the reserved namespace both exist,
// so a resolution that stopped at its match could return a checkpoint out of a store
// this build otherwise refuses to list. The assertion is order-independent: whichever
// entry the directory yields first, the answer must be corruption.
func TestResolvePrefixFailsOnStructuralCorruptionBesideAMatch(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	base := time.Unix(1_700_000_000, 0).UTC()
	for i := 0; i < 3; i++ {
		s := buildCheckpoint(t, idFrom(t, byte(0x10+i)), base.Add(time.Duration(i)*time.Minute), false)
		s.put(t, repo)
	}
	match := buildCheckpoint(t, idFrom(t, 0x00), base, false)
	match.put(t, repo)

	foreign := idFrom(t, 0xbb)
	body := []byte(`{"schema_version":1,"id":"` + foreign.String() + `"}`)
	if err := os.WriteFile(filepath.Join(layout.CheckpointsDir(), foreign.String()+checkpointExt), body, 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.ResolvePrefix(context.Background(), match.id().String()); !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("ResolvePrefix over a structurally corrupt store err = %v, want ErrCorruptStore", err)
	}
}

func TestRepoDeleteIsIdempotent(t *testing.T) {
	repo := NewRepo(paths.New(t.TempDir()))
	s := buildCheckpoint(t, idFrom(t, 0x44), time.Unix(1, 0).UTC(), false)
	s.put(t, repo)
	if err := repo.Delete(s.id()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := readCheckpoint(repo, s.id()); !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}
	// Deleting an absent id is not an error.
	if err := repo.Delete(s.id()); err != nil {
		t.Fatalf("second Delete = %v, want nil", err)
	}
}

func TestEncodeUsesCanonicalHashFieldNames(t *testing.T) {
	s := buildCheckpoint(t, idFrom(t, 0x55), time.Unix(1, 0).UTC(), false)
	data, err := encodeHeader(s.derivedHeader(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"scan_config_hash", "checkpoint_policy_hash", "tree_hash"} {
		if !strings.Contains(string(data), field) {
			t.Errorf("checkpoint JSON missing canonical field %q", field)
		}
	}
	if strings.Contains(string(data), `"config_hash"`) {
		t.Error("checkpoint JSON must not write the ambiguous config_hash field")
	}
}

func TestRepoRejectsSymlinkCheckpointFile(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	if err := os.MkdirAll(layout.CheckpointsDir(), 0o755); err != nil {
		t.Fatal(err)
	}

	// A valid header stored outside the store, reachable only via a symlink at the
	// checkpoint's own header address. Its bytes could be swapped after validation.
	s := buildCheckpoint(t, idFrom(t, 0xcc), time.Unix(1, 0).UTC(), false)
	data, err := encodeHeader(s.derivedHeader(t))
	if err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "ext.json")
	if err := os.WriteFile(external, data, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(layout.CheckpointsDir(), s.id().String()), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(layout.CheckpointsDir(), s.id().String(), headerName)
	mustSymlink(t, external, link)

	if err := readCheckpoint(repo, s.id()); !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("Get at symlink err = %v, want ErrCorruptStore", err)
	}
	if _, err := repo.StoreHealthAll(context.Background()); !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("List at symlink err = %v, want ErrCorruptStore", err)
	}
}

// editHeader applies edit to a published header's JSON object and rewrites it.
func editHeader(t *testing.T, headerPath string, edit func(map[string]json.RawMessage)) {
	t.Helper()
	data, err := os.ReadFile(headerPath)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(headerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(headerPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(headerPath, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// mustSymlink creates a symlink or ends the test — fatally where symlink coverage is
// required, with a named skip otherwise.
//
// Windows grants the privilege only in developer mode or to an elevated process, so a
// symlink case really can be unavailable rather than broken. But the Windows lane runs
// this package to prove exactly which nodes its enumeration accepts and refuses, and it
// sets AWA_REQUIRE_SYMLINK_TESTS to say so: there, an unavailable privilege must fail
// the job rather than silently remove the cases it was configured to run and report
// green.
func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	err := os.Symlink(target, link)
	if err == nil {
		return
	}
	if os.Getenv("AWA_REQUIRE_SYMLINK_TESTS") != "" {
		t.Fatalf("AWA_REQUIRE_SYMLINK_TESTS is set, so symlink coverage is required, but this platform will not create a symlink: %v", err)
	}
	t.Skipf("this platform will not create a symlink: %v", err)
}

// fixtureRoot is an absolute, cleaned path on every platform. A checkpoint records
// the project root it was captured from and the value object requires that shape, so
// a fixture hardcoding a POSIX path like "/abs/project" is not merely cosmetic on
// Windows: filepath.IsAbs rejects a volume-less path there, which fails every
// fixture in the package and silently confines its coverage to unix.
func fixtureRoot(t testing.TB) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("abs", "project"))
	if err != nil {
		t.Fatalf("resolving a fixture root: %v", err)
	}
	return root
}
