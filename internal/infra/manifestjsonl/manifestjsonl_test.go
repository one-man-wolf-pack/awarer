package manifestjsonl_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/manifestjsonl"
	"awarer/internal/scantest"
)

func hasher(t *testing.T) hashing.Hasher {
	t.Helper()
	h := blake3hash.New()
	return h
}

func regular(t *testing.T, h hashing.Hasher, path, content string) worktree.Entry {
	t.Helper()
	rp, err := worktree.ParseRelPath(path)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := hashing.ParseContentHash(h.HashBytes([]byte(content)).String())
	if err != nil {
		t.Fatal(err)
	}
	omitted, err := worktree.ParseFieldSet([]string{"ctime", "dev", "ino", "nlink"})
	if err != nil {
		t.Fatal(err)
	}
	stat := worktree.StatSignature{Size: int64(len(content)), Mode: 0o644, MtimeNs: 1, Omitted: omitted}
	e, err := worktree.NewRegularEntry(rp, ch, worktree.StorageHashOnly, stat, worktree.TraversalInfo{})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestTeeAndStreamRoundTrip(t *testing.T) {
	h := hasher(t)
	entries := []worktree.Entry{regular(t, h, "a.txt", "alpha"), regular(t, h, "b.txt", "beta")}

	reducer, err := worktree.NewTreeReducer(h)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := manifestjsonl.Tee(&buf, scantest.CanonicalCursor(entries, nil), reducer); err != nil {
		t.Fatalf("Tee: %v", err)
	}
	red := reducer.Finish()
	if red.Count != 2 {
		t.Fatalf("reduced count = %d, want 2", red.Count)
	}

	// Write the manifest to a temp file and stream it back, verifying the count.
	root := t.TempDir()
	abs := filepath.Join(root, "manifest.jsonl")
	if err := os.WriteFile(abs, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	stream := manifestjsonl.Stream{Root: root, Abs: abs, Expected: red.Count, Label: "test"}
	cur, err := stream.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cur.Close() }()
	var got []string
	for cur.Next() {
		got = append(got, cur.Record().Path().String())
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(got) != 2 || got[0] != "a.txt" || got[1] != "b.txt" {
		t.Errorf("streamed records = %v", got)
	}
}

func TestStreamRejectsCountMismatch(t *testing.T) {
	h := hasher(t)
	reducer, _ := worktree.NewTreeReducer(h)
	var buf bytes.Buffer
	if err := manifestjsonl.Tee(&buf, scantest.CanonicalCursor([]worktree.Entry{regular(t, h, "a.txt", "x")}, nil), reducer); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	abs := filepath.Join(root, "m.jsonl")
	if err := os.WriteFile(abs, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	// Declare a wrong expected count; the drain must fail at EOF.
	sentinel := errors.New("corrupt")
	stream := manifestjsonl.Stream{Root: root, Abs: abs, Expected: 5, Label: "test", Corrupt: func(e error) error { return sentinel }}
	cur, err := stream.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cur.Close() }()
	for cur.Next() {
	}
	if !errors.Is(cur.Err(), sentinel) {
		t.Errorf("err = %v, want the corrupt sentinel", cur.Err())
	}
}

func TestDecodeLineRejectsTrailingData(t *testing.T) {
	if _, err := manifestjsonl.DecodeLine([]byte(`{"entry":{"path":"a","kind":"dir","content_storage":"none"}} {}`)); err == nil {
		t.Error("DecodeLine must reject trailing data after the object")
	}
}

func u64ptr(v uint64) *uint64 { return &v }
func i64ptr(v int64) *int64   { return &v }

func TestDecodeEntryRejectsImpossibleShapes(t *testing.T) {
	const ch = "blake3:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		name string
		d    manifestjsonl.EntryDoc
	}{
		{"dir with content hash", manifestjsonl.EntryDoc{Path: "d", Kind: "dir", ContentStorage: "none", ContentHash: ch}},
		{"dir with symlink target", manifestjsonl.EntryDoc{Path: "d", Kind: "dir", ContentStorage: "none", SymlinkTarget: "x"}},
		{"dir with blob storage", manifestjsonl.EntryDoc{Path: "d", Kind: "dir", ContentStorage: "blob"}},
		{"dir with stat size", manifestjsonl.EntryDoc{Path: "d", Kind: "dir", ContentStorage: "none", Size: 1}},
		{"dir with stat mode", manifestjsonl.EntryDoc{Path: "d", Kind: "dir", ContentStorage: "none", Mode: 0o755}},
		{"dir with optional stat field", manifestjsonl.EntryDoc{Path: "d", Kind: "dir", ContentStorage: "none", Ino: u64ptr(5)}},
		{"symlink with content hash", manifestjsonl.EntryDoc{Path: "l", Kind: "symlink", ContentStorage: "inline-symlink-target", SymlinkTarget: "x", ContentHash: ch}},
		{"symlink with blob storage", manifestjsonl.EntryDoc{Path: "l", Kind: "symlink", ContentStorage: "blob", SymlinkTarget: "x"}},
		{"symlink without target", manifestjsonl.EntryDoc{Path: "l", Kind: "symlink", ContentStorage: "inline-symlink-target"}},
		{"file without content hash", manifestjsonl.EntryDoc{Path: "f", Kind: "file", ContentStorage: "blob"}},
		{"file with non-regular storage", manifestjsonl.EntryDoc{Path: "f", Kind: "file", ContentStorage: "none", ContentHash: ch}},
		{"file with negative size", manifestjsonl.EntryDoc{Path: "f", Kind: "file", ContentStorage: "blob", ContentHash: ch, Size: -1, Mode: 0o644, MtimeNs: 1, CtimeNs: i64ptr(1), Dev: u64ptr(1), Ino: u64ptr(1), Nlink: u64ptr(1)}},
		{"skipped masquerading as entry", manifestjsonl.EntryDoc{Path: "s", Kind: "special", ContentStorage: "none"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := manifestjsonl.DecodeEntry(c.d)
			if err == nil {
				t.Fatalf("expected rejection")
			}
			if !strings.Contains(err.Error(), c.d.Path) {
				t.Fatalf("error %q must identify the bad path %q", err, c.d.Path)
			}
		})
	}
}

// TestDecodeSkippedRejectsKindReasonMismatch proves the persisted kind on a skipped
// record must agree with the kind its reason implies, so a hand-edited record is
// rejected rather than silently re-derived from reason.
func TestDecodeSkippedRejectsKindReasonMismatch(t *testing.T) {
	// reason "special-file" implies kind "special"; a recorded kind of "file" is a
	// contradiction that decode must reject.
	d := manifestjsonl.SkippedDoc{Path: "s", Kind: "file", Reason: "special-file"}
	_, err := manifestjsonl.DecodeSkipped(d)
	if err == nil || !strings.Contains(err.Error(), "s") {
		t.Fatalf("decodeSkipped err = %v, want a rejection identifying path %q", err, d.Path)
	}

	// The matching kind decodes cleanly (a special-file skip carries a full stat).
	ok := manifestjsonl.SkippedDoc{Path: "s", Kind: "special", Reason: "special-file", CtimeNs: i64ptr(0), Dev: u64ptr(0), Ino: u64ptr(0), Nlink: u64ptr(0)}
	if _, err := manifestjsonl.DecodeSkipped(ok); err != nil {
		t.Fatalf("decodeSkipped with matching kind/reason err = %v, want success", err)
	}
}

func TestBuildStatRejectsOmissionMismatch(t *testing.T) {
	ctime := int64(5)
	// Field present but marked omitted.
	if _, err := manifestjsonl.BuildStat(1, 0o644, 2, &ctime, nil, nil, nil, []string{"ctime", "dev", "ino", "nlink"}); err == nil {
		t.Fatal("expected error: ctime present but omitted")
	}
	// Field missing but not marked omitted.
	if _, err := manifestjsonl.BuildStat(1, 0o644, 2, nil, nil, nil, nil, nil); err == nil {
		t.Fatal("expected error: ctime missing but not omitted")
	}
	// Consistent: all present, none omitted.
	dev, ino, nlink := uint64(1), uint64(2), uint64(3)
	if _, err := manifestjsonl.BuildStat(1, 0o644, 2, &ctime, &dev, &ino, &nlink, nil); err != nil {
		t.Fatalf("consistent stat rejected: %v", err)
	}
}
