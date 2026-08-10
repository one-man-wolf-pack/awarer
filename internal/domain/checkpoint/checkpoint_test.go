package checkpoint

import (
	"bytes"
	"cmp"
	"io"
	"testing"
	"time"

	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
	"awarer/internal/scantest"
)

// TestCheckpointStatsValidate proves the structural stats guard accepts a possible
// block and rejects mechanically impossible ones.
func TestCheckpointStatsValidate(t *testing.T) {
	ok := CheckpointStats{Files: 3, Dirs: 1, Symlinks: 1, Blobs: 2, HashOnly: 1, Skipped: 0, TotalBytes: 100}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid stats rejected: %v", err)
	}
	bad := map[string]CheckpointStats{
		"negative files":    {Files: -1},
		"negative total":    {Files: 1, Blobs: 1, TotalBytes: -1},
		"storage not files": {Files: 3, Blobs: 1, HashOnly: 1},
		"negative skipped":  {Skipped: -2},
	}
	for name, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("%s: Validate accepted impossible stats %+v", name, s)
		}
	}
}

func mustID(t *testing.T) CheckpointID {
	t.Helper()
	id, err := NewCheckpointID(bytes.NewReader(bytes.Repeat([]byte{0x01}, idBytes)))
	if err != nil {
		t.Fatalf("NewCheckpointID: %v", err)
	}
	return id
}

func mustTime() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

func relPath(t *testing.T, s string) worktree.RelPath {
	t.Helper()
	p, err := worktree.ParseRelPath(s)
	if err != nil {
		t.Fatalf("ParseRelPath(%q): %v", s, err)
	}
	return p
}

func contentHash(t *testing.T) hashing.ContentHash {
	t.Helper()
	h, err := hashing.ParseContentHash("blake3:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("ParseContentHash: %v", err)
	}
	return h
}

// TestStatsFromReduced proves the one derivation of a checkpoint's durable counts:
// the manifest records are folded once by the worktree reducer and mapped straight
// into CheckpointStats. The expected block is written out, so the check does not
// depend on a second counting rule that could drift from the reducer.
func TestStatsFromReduced(t *testing.T) {
	stat := worktree.StatSignature{Size: 100, MtimeNs: 1, Mode: 0o644}
	blobEntry, err := worktree.NewRegularEntry(relPath(t, "a.txt"), contentHash(t), worktree.StorageBlob, stat, worktree.TraversalInfo{})
	if err != nil {
		t.Fatalf("blob entry: %v", err)
	}
	hashOnly, err := worktree.NewRegularEntry(relPath(t, "big.bin"), contentHash(t), worktree.StorageHashOnly, worktree.StatSignature{Size: 50, MtimeNs: 1, Mode: 0o644}, worktree.TraversalInfo{})
	if err != nil {
		t.Fatalf("hash-only entry: %v", err)
	}
	dir, err := worktree.NewDirEntry(relPath(t, "sub"), worktree.TraversalInfo{})
	if err != nil {
		t.Fatalf("dir entry: %v", err)
	}
	target, _ := worktree.NewSymlinkTarget("a.txt")
	link, err := worktree.NewSymlinkEntry(relPath(t, "link"), target, worktree.StatSignature{Mode: 0o777}, worktree.TraversalInfo{})
	if err != nil {
		t.Fatalf("symlink entry: %v", err)
	}
	skipped, err := worktree.NewSkippedInput(relPath(t, "dev"), worktree.ReasonSpecialFile, 0, worktree.StatSignature{}, "", worktree.SymlinkTarget{}, worktree.TraversalInfo{})
	if err != nil {
		t.Fatalf("skipped: %v", err)
	}

	red, err := worktree.ReduceCursor(fixedHasher{}, scantest.CanonicalCursor(
		[]worktree.Entry{blobEntry, hashOnly, dir, link},
		[]worktree.SkippedInput{skipped},
	))
	if err != nil {
		t.Fatalf("ReduceCursor: %v", err)
	}
	got := StatsFromReduced(red.Stats)
	want := CheckpointStats{Files: 2, Dirs: 1, Symlinks: 1, Blobs: 1, HashOnly: 1, Skipped: 1, TotalBytes: 150}
	if got != want {
		t.Fatalf("StatsFromReduced = %+v, want %+v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("stats derived from a real manifest failed the structural guard: %v", err)
	}
	if red.Count != 5 {
		t.Errorf("record count = %d, want 5", red.Count)
	}
}

// TestCheckpointMetadataValidate covers the metadata rules a checkpoint is written
// and read under. They live in one place (validateMeta) and are reached through both
// projections, so the build a write consumes and the header a read trusts cannot
// disagree about what well-formed metadata is.
func TestCheckpointMetadataValidate(t *testing.T) {
	base := func() CheckpointBuild {
		return CheckpointBuild{
			ID:                   mustID(t),
			CreatedAt:            mustTime(),
			Root:                 "/abs/root",
			CommandCwd:           ".",
			AwaVersion:           "0.0.0-dev",
			ScanConfigHash:       hashing.ConfigHashFromTree(treeHash(t)),
			CheckpointPolicyHash: hashing.ConfigHashFromTree(treeHash(t)),
			TrustMode:            config.TrustNormal,
		}
	}
	stats := CheckpointStats{Files: 1, Blobs: 1, TotalBytes: 10}
	header := func(b CheckpointBuild) CheckpointHeader { return b.Header(treeHash(t), stats, 1) }

	if err := base().Validate(); err != nil {
		t.Fatalf("valid build rejected: %v", err)
	}
	if err := header(base()).Validate(); err != nil {
		t.Fatalf("valid header rejected: %v", err)
	}

	bad := base()
	bad.Root = "relative/path"
	if err := bad.Validate(); err == nil {
		t.Error("build: expected error for relative root")
	}
	if err := header(bad).Validate(); err == nil {
		t.Error("header: expected error for relative root")
	}

	weak := base()
	weak.FastModeWeakSignature = true // contradicts TrustNormal
	if err := weak.Validate(); err == nil {
		t.Error("build: expected error for weak-signature/trust-mode contradiction")
	}
	if err := header(weak).Validate(); err == nil {
		t.Error("header: expected error for weak-signature/trust-mode contradiction")
	}
}

func TestGitMetadataValidate(t *testing.T) {
	ok := GitMetadata{InWorktree: true, Commit: "abc", ShortCommit: "abc", Dirty: DirtySummary{Clean: true}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid git metadata rejected: %v", err)
	}
	if err := (GitMetadata{InWorktree: false}).Validate(); err == nil {
		t.Error("git metadata not in a worktree should be rejected")
	}
	if err := (GitMetadata{InWorktree: true, ShortCommit: "abc"}).Validate(); err == nil {
		t.Error("short commit without full commit should be rejected")
	}
	if err := (GitMetadata{InWorktree: true, Commit: "abcdef", ShortCommit: "xyz"}).Validate(); err == nil {
		t.Error("short commit that is not a prefix of the full commit should be rejected")
	}
}

func TestDirtySummaryValidate(t *testing.T) {
	if err := (DirtySummary{Clean: true}).Validate(); err != nil {
		t.Errorf("clean summary rejected: %v", err)
	}
	if err := (DirtySummary{Modified: 2}).Validate(); err != nil {
		t.Errorf("dirty summary with matching counts rejected: %v", err)
	}
	if err := (DirtySummary{Clean: true, Modified: 1}).Validate(); err == nil {
		t.Error("clean=true with non-zero counts should be rejected")
	}
	if err := (DirtySummary{Modified: -1}).Validate(); err == nil {
		t.Error("negative count should be rejected")
	}
}

// TestCompareNewestFirstOrdersTimeThenIDDescending proves the one comparison the
// checkpoint domain owns for chronological position: a later creation time is newer,
// and only on an exact tie does the lexically larger id win. Every expectation is
// written out by hand — the wanted direction is stated per case, never computed through
// the comparison under test — and each pair is checked in both argument orders so a
// reversed sign cannot pass as a symmetric result.
func TestCompareNewestFirstOrdersTimeThenIDDescending(t *testing.T) {
	early := time.Unix(1_700_000_000, 0).UTC()
	late := time.Unix(1_700_000_060, 0).UTC()
	// The same instant carried in another zone: chronological position is the instant,
	// not the wall clock it is displayed in.
	lateElsewhere := late.In(time.FixedZone("plus-two", 2*60*60))
	// idByte repeats one alphabet character, so "111…" is lexically below "222…".
	lowID, highID := idByte(t, 1), idByte(t, 2)

	cases := []struct {
		name string
		aAt  time.Time
		aID  CheckpointID
		bAt  time.Time
		bID  CheckpointID
		want int // wanted sign: -1 when a is newer, +1 when b is, 0 for the same record
	}{
		{name: "later time is newer", aAt: late, aID: lowID, bAt: early, bID: lowID, want: -1},
		{name: "later time outranks a larger id", aAt: late, aID: lowID, bAt: early, bID: highID, want: -1},
		{name: "equal time picks the larger id", aAt: late, aID: highID, bAt: late, bID: lowID, want: -1},
		{name: "equal instant across zones still picks the larger id", aAt: lateElsewhere, aID: highID, bAt: late, bID: lowID, want: -1},
		{name: "same time and id compare equal", aAt: late, aID: highID, bAt: late, bID: highID, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cmp.Compare(CompareNewestFirst(tc.aAt, tc.aID, tc.bAt, tc.bID), 0)
			if got != tc.want {
				t.Fatalf("CompareNewestFirst sign = %d, want %d", got, tc.want)
			}
			mirrored := cmp.Compare(CompareNewestFirst(tc.bAt, tc.bID, tc.aAt, tc.aID), 0)
			if mirrored != -tc.want {
				t.Fatalf("mirrored CompareNewestFirst sign = %d, want %d", mirrored, -tc.want)
			}
		})
	}
}

// fixedHasher is the minimal hashing.Hasher the reducer needs. The stats assertions
// above are about counting, not about the digest, so a constant tree hash keeps this
// package free of an infrastructure hashing dependency.
type fixedHasher struct{}

func (fixedHasher) HashReader(io.Reader) (hashing.ContentHash, error) {
	return hashing.ContentHash{}, nil
}

func (fixedHasher) HashBytes([]byte) hashing.TreeHash {
	h, err := hashing.ParseTreeHash("blake3:" + "1111111111111111111111111111111111111111111111111111111111111111")
	if err != nil {
		panic(err)
	}
	return h
}

func treeHash(t *testing.T) hashing.TreeHash {
	t.Helper()
	h, err := hashing.ParseTreeHash("blake3:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("ParseTreeHash: %v", err)
	}
	return h
}
