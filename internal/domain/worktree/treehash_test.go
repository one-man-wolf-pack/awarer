package worktree_test

import (
	"io"
	"testing"

	"github.com/zeebo/blake3"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
	"awarer/internal/scantest"
)

// fakeHasher implements hashing.Hasher with nothing but the two required methods,
// so the domain tree tests stay independent of the infrastructure adapter while
// still digesting the real canonical byte stream. Deliberately omitting
// NewTreeWriter is the point: it keeps the reducer's buffered fallback path — the
// one taken by any Hasher that cannot stream — under test.
type fakeHasher struct{}

func (fakeHasher) HashReader(r io.Reader) (hashing.ContentHash, error) {
	sum := blake3.New()
	if _, err := io.Copy(sum, r); err != nil {
		return hashing.ContentHash{}, err
	}
	return hashing.NewContentHash(sum.Sum(nil))
}

func (fakeHasher) HashBytes(b []byte) hashing.TreeHash {
	sum := blake3.New()
	_, _ = sum.Write(b)
	th, err := hashing.NewTreeHash(sum.Sum(nil))
	if err != nil {
		panic(err)
	}
	return th
}

func mustPath(t *testing.T, s string) worktree.RelPath {
	t.Helper()
	p, err := worktree.ParseRelPath(s)
	if err != nil {
		t.Fatalf("ParseRelPath(%q): %v", s, err)
	}
	return p
}

func mustTarget(t *testing.T, s string) worktree.SymlinkTarget {
	t.Helper()
	tgt, err := worktree.NewSymlinkTarget(s)
	if err != nil {
		t.Fatal(err)
	}
	return tgt
}

func mustContent(t *testing.T, hexPayload string) hashing.ContentHash {
	t.Helper()
	h, err := hashing.ParseContentHash("blake3:" + hexPayload)
	if err != nil {
		t.Fatalf("ParseContentHash: %v", err)
	}
	return h
}

const hexA = "1111111111111111111111111111111111111111111111111111111111111111"
const hexB = "2222222222222222222222222222222222222222222222222222222222222222"

func regular(t *testing.T, path, contentHex string) worktree.Entry {
	return worktree.Entry{
		Path:    mustPath(t, path),
		Kind:    worktree.KindRegular,
		Content: mustContent(t, contentHex),
		Storage: worktree.StorageBlob,
		Stat:    worktree.StatSignature{Mode: 0o644},
	}
}

// reduceSlices folds an in-memory record set the way a real source does:
// scantest.CanonicalCursor yields one path-ordered, Ordered-guarded cursor and
// ReduceCursor derives the hash, stats, taint, and count from that single pass. The
// reduction is the production path, so a hash observed here is the hash a scan or a
// stored manifest produces for the same records.
func reduceSlices(h hashing.Hasher, entries []worktree.Entry, skipped []worktree.SkippedInput) (worktree.TreeReduction, error) {
	return worktree.ReduceCursor(h, scantest.CanonicalCursor(entries, skipped))
}

// treeHash is reduceSlices for the identity properties below, which are only about
// the digest and treat a reduction failure as a broken fixture.
func treeHash(t *testing.T, h hashing.Hasher, entries []worktree.Entry, skipped []worktree.SkippedInput) hashing.TreeHash {
	t.Helper()
	red, err := reduceSlices(h, entries, skipped)
	if err != nil {
		t.Fatalf("ReduceCursor: %v", err)
	}
	return red.Hash
}

// TestTreeHashIndependentOfOrder proves the tree hash depends only on the set of
// records, not the order they were supplied (i.e. not filesystem walk order).
func TestTreeHashIndependentOfOrder(t *testing.T) {
	h := fakeHasher{}
	forward := []worktree.Entry{
		regular(t, "a.go", hexA),
		regular(t, "b/c.go", hexB),
		regular(t, "b/a.go", hexA),
	}
	reversed := []worktree.Entry{
		regular(t, "b/a.go", hexA),
		regular(t, "b/c.go", hexB),
		regular(t, "a.go", hexA),
	}
	if treeHash(t, h, forward, nil).String() != treeHash(t, h, reversed, nil).String() {
		t.Errorf("tree hash depends on input order")
	}
	// The canonical cursor is what makes that true: it yields ascending paths whatever
	// order the slices arrived in.
	cur := scantest.CanonicalCursor(reversed, nil)
	var order []string
	for cur.Next() {
		order = append(order, cur.Record().Path().String())
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("cursor err: %v", err)
	}
	want := []string{"a.go", "b/a.go", "b/c.go"}
	if len(order) != len(want) {
		t.Fatalf("cursor yielded %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("cursor yielded %v, want %v", order, want)
		}
	}
}

// TestTreeHashChangesWithContent proves a content change changes the tree hash.
func TestTreeHashChangesWithContent(t *testing.T) {
	h := fakeHasher{}
	base := treeHash(t, h, []worktree.Entry{regular(t, "a.go", hexA)}, nil)
	changed := treeHash(t, h, []worktree.Entry{regular(t, "a.go", hexB)}, nil)
	if base.String() == changed.String() {
		t.Errorf("tree hash unchanged after content change")
	}
}

// TestTreeHashIgnoresVolatileStat proves mtime/ctime/dev/ino/nlink do not affect
// the tree hash, so the same content hashes identically across machines.
func TestTreeHashIgnoresVolatileStat(t *testing.T) {
	h := fakeHasher{}
	e1 := regular(t, "a.go", hexA)
	e1.Stat = worktree.StatSignature{Size: 10, MtimeNs: 1, CtimeNs: 2, Dev: 3, Ino: 4, Nlink: 1}
	e2 := regular(t, "a.go", hexA)
	e2.Stat = worktree.StatSignature{Size: 10, MtimeNs: 999, CtimeNs: 888, Dev: 77, Ino: 66, Nlink: 5}
	if treeHash(t, h, []worktree.Entry{e1}, nil).String() != treeHash(t, h, []worktree.Entry{e2}, nil).String() {
		t.Errorf("tree hash changed with volatile stat fields")
	}
}

// TestTreeHashChangesWithMode proves permission bits contribute to identity.
func TestTreeHashChangesWithMode(t *testing.T) {
	h := fakeHasher{}
	e1 := regular(t, "a.go", hexA)
	e1.Stat.Mode = 0o644
	e2 := regular(t, "a.go", hexA)
	e2.Stat.Mode = 0o755
	if treeHash(t, h, []worktree.Entry{e1}, nil).String() == treeHash(t, h, []worktree.Entry{e2}, nil).String() {
		t.Errorf("tree hash unchanged after mode change")
	}
}

// TestTreeHashIgnoresStoragePolicy proves the tree hash describes observed state,
// not storage choices: a regular file recorded as blob versus hash-only, with
// otherwise identical path/content/stat, hashes identically. This is what keeps
// tree_hash stable across checkpoint.store_file_contents toggles.
func TestTreeHashIgnoresStoragePolicy(t *testing.T) {
	h := fakeHasher{}
	stored := regular(t, "big.bin", hexA)
	stored.Storage = worktree.StorageBlob
	hashOnly := regular(t, "big.bin", hexA)
	hashOnly.Storage = worktree.StorageHashOnly
	if treeHash(t, h, []worktree.Entry{stored}, nil).String() != treeHash(t, h, []worktree.Entry{hashOnly}, nil).String() {
		t.Errorf("tree hash changed across storage policy")
	}
}

// TestTreeHashChangesWithTraversal proves traversal provenance is part of the
// tree's semantic identity: a direct file and a file reached through
// follow_symlinks hash differently even when path, content, and mode match.
func TestTreeHashChangesWithTraversal(t *testing.T) {
	h := fakeHasher{}
	direct := regular(t, "a.go", hexA)
	followed := regular(t, "a.go", hexA)
	followed.Traversal = worktree.TraversalInfo{Followed: true, SourcePath: mustPath(t, "link"), Depth: 1}
	if treeHash(t, h, []worktree.Entry{direct}, nil).String() == treeHash(t, h, []worktree.Entry{followed}, nil).String() {
		t.Errorf("tree hash must differ for a direct vs followed file")
	}
}

// TestTreeHashSymlinkTarget proves a symlink's identity is its target.
func TestTreeHashSymlinkTarget(t *testing.T) {
	h := fakeHasher{}
	mk := func(target string) worktree.Entry {
		st, err := worktree.NewSymlinkTarget(target)
		if err != nil {
			t.Fatal(err)
		}
		return worktree.Entry{Path: mustPath(t, "link"), Kind: worktree.KindSymlink, Symlink: st, Storage: worktree.StorageInlineSymlinkTarget}
	}
	if treeHash(t, h, []worktree.Entry{mk("../a")}, nil).String() == treeHash(t, h, []worktree.Entry{mk("../b")}, nil).String() {
		t.Errorf("tree hash unchanged after symlink target change")
	}
}

// TestTreeHashSkippedContributes proves a present->skipped transition changes the
// tree hash, and that skipped reason matters.
func TestTreeHashSkippedContributes(t *testing.T) {
	h := fakeHasher{}
	present := treeHash(t, h, []worktree.Entry{regular(t, "big.bin", hexA)}, nil)
	skipped := treeHash(t, h, nil, []worktree.SkippedInput{{
		Path:   mustPath(t, "big.bin"),
		Kind:   worktree.KindRegular,
		Reason: worktree.ReasonLargeFileSkipPolicy,
		Size:   123,
	}})
	if present.String() == skipped.String() {
		t.Errorf("tree hash unchanged for present vs skipped")
	}
}

// TestTreeHashSkippedStatChanges proves a skipped input's change-signaling stat
// (mtime, mode) is part of the tree identity — so a skipped file that changes
// alters the tree hash even though its path, reason, and size are unchanged.
func TestTreeHashSkippedStatChanges(t *testing.T) {
	h := fakeHasher{}
	mk := func(mtime int64, mode uint32) hashing.TreeHash {
		return treeHash(t, h, nil, []worktree.SkippedInput{{
			Path:   mustPath(t, "big.bin"),
			Kind:   worktree.KindRegular,
			Reason: worktree.ReasonLargeFileSkipPolicy,
			Size:   100,
			Stat:   worktree.StatSignature{Size: 100, MtimeNs: mtime, Mode: mode},
		}})
	}
	base := mk(1000, 0o644)
	if mk(2000, 0o644).String() == base.String() {
		t.Errorf("skipped mtime change did not change tree hash")
	}
	if mk(1000, 0o755).String() == base.String() {
		t.Errorf("skipped mode change did not change tree hash")
	}
	// But non-portable inode fields do not, so the same skipped file hashes
	// identically across machines.
	withInode := func(dev, ino, nlink uint64) hashing.TreeHash {
		return treeHash(t, h, nil, []worktree.SkippedInput{{
			Path:   mustPath(t, "big.bin"),
			Kind:   worktree.KindRegular,
			Reason: worktree.ReasonLargeFileSkipPolicy,
			Size:   100,
			Stat:   worktree.StatSignature{Size: 100, MtimeNs: 1000, Mode: 0o644, Dev: dev, Ino: ino, Nlink: nlink},
		}})
	}
	if withInode(1, 2, 1).String() != withInode(9, 9, 9).String() {
		t.Errorf("skipped inode fields changed the tree hash; cross-machine determinism broken")
	}
}

// TestTreeHashSkippedModeMasked proves only the permission bits of a skipped
// input's mode contribute to identity: differing type/high bits (raw st_mode) do
// not change the tree hash, so the same skipped file hashes identically across
// platforms.
func TestTreeHashSkippedModeMasked(t *testing.T) {
	h := fakeHasher{}
	mk := func(mode uint32) hashing.TreeHash {
		return treeHash(t, h, nil, []worktree.SkippedInput{{
			Path:   mustPath(t, "big.bin"),
			Kind:   worktree.KindRegular,
			Reason: worktree.ReasonLargeFileSkipPolicy,
			Size:   100,
			Stat:   worktree.StatSignature{Size: 100, Mode: mode},
		}})
	}
	// 0o100644 (regular-file st_mode with perms 644) vs 0o644 (perms only).
	if mk(0o100644).String() != mk(0o644).String() {
		t.Errorf("skipped mode type/high bits changed the tree hash")
	}
	// Different permission bits still change it.
	if mk(0o644).String() == mk(0o755).String() {
		t.Errorf("skipped permission bits did not change the tree hash")
	}
}

// TestReductionRejectsInvalidEntries proves the reduction rejects records that
// cannot represent a real worktree node, rather than silently encoding them. The
// reducer re-checks the entry constructors' invariants as a boundary guard, so a
// record that bypassed a constructor still cannot reach a tree hash.
func TestReductionRejectsInvalidEntries(t *testing.T) {
	h := fakeHasher{}
	path := mustPath(t, "x")

	cases := map[string]worktree.Entry{
		"regular without content hash": {Path: path, Kind: worktree.KindRegular, Storage: worktree.StorageBlob},
		"regular with wrong storage":   {Path: path, Kind: worktree.KindRegular, Content: mustContent(t, hexA), Storage: worktree.StorageNone},
		"dir carrying content":         {Path: path, Kind: worktree.KindDir, Content: mustContent(t, hexA), Storage: worktree.StorageNone},
		"dir with wrong storage":       {Path: path, Kind: worktree.KindDir, Storage: worktree.StorageBlob},
		"symlink without target":       {Path: path, Kind: worktree.KindSymlink, Storage: worktree.StorageInlineSymlinkTarget},
		"symlink with wrong storage":   {Path: path, Kind: worktree.KindSymlink, Symlink: mustTarget(t, "x"), Storage: worktree.StorageBlob},
		"special as entry":             {Path: path, Kind: worktree.KindSpecial},
	}
	for name, e := range cases {
		if _, err := reduceSlices(h, []worktree.Entry{e}, nil); err == nil {
			t.Errorf("%s: reduction accepted an invalid entry", name)
		}
	}
}

// TestReductionRejectsInvalidSkipped proves the reduction rejects skipped inputs
// that cannot represent a real skip: unknown reason, a reason that does not match
// the kind, or a negative size.
func TestReductionRejectsInvalidSkipped(t *testing.T) {
	h := fakeHasher{}
	x := mustPath(t, "x")
	cases := map[string]worktree.SkippedInput{
		"unknown reason":       {Path: x, Kind: worktree.KindRegular, Reason: worktree.SkippedReason(99)},
		"reason/kind mismatch": {Path: x, Kind: worktree.KindRegular, Reason: worktree.ReasonSymlinkCycle},
		"negative size":        {Path: x, Kind: worktree.KindRegular, Reason: worktree.ReasonLargeFileSkipPolicy, Size: -1},
	}
	for name, s := range cases {
		if _, err := reduceSlices(h, nil, []worktree.SkippedInput{s}); err == nil {
			t.Errorf("%s: reduction accepted an invalid skipped input", name)
		}
	}
}

// TestReductionRejectsDuplicateEntryPaths proves one path appearing twice among the
// entries is an impossible tree and fails loudly rather than hashing. The
// entry/skip collision is the sibling case, covered by TestReducerRejectsDuplicate.
func TestReductionRejectsDuplicateEntryPaths(t *testing.T) {
	_, err := reduceSlices(fakeHasher{}, []worktree.Entry{
		regular(t, "a.go", hexA),
		regular(t, "a.go", hexB),
	}, nil)
	if err == nil {
		t.Fatalf("reduction accepted a duplicate entry path")
	}
}
