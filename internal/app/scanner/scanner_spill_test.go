package scanner_test

import (
	"context"
	"fmt"
	"io"
	"testing"

	"awarer/internal/app/scanner"
	"awarer/internal/domain/config"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/worktreefs"
	"awarer/internal/scantest"
)

// TestScanSpillMatchesInMemory proves the scanner's bounded external sort produces the
// exact same tree hash and canonical manifest whether it sorted in memory or spilled
// to disk: a tiny BufferRecords forces the spill path on a modest fixture, and the
// tree hash must match the default in-memory scan of the same worktree.
func TestScanSpillMatchesInMemory(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	// A spread of nested paths so canonical order differs from directory-walk order.
	for i := 0; i < 40; i++ {
		scantest.Write(t, root, fmt.Sprintf("dir%02d/file%02d.txt", i%7, i), fmt.Sprintf("content-%d\n", i))
	}
	cfg := config.Defaults()
	svc := scanner.New(worktreefs.New(), blake3hash.New(), nil)

	inMem, err := svc.Scan(context.Background(), proj, cfg, cfg.HistoryScanScope(), scanner.Options{})
	if err != nil {
		t.Fatalf("in-memory Scan: %v", err)
	}
	defer func() { _ = inMem.Close() }()

	spilled, err := svc.Scan(context.Background(), proj, cfg, cfg.HistoryScanScope(), scanner.Options{BufferRecords: 4})
	if err != nil {
		t.Fatalf("spilling Scan: %v", err)
	}
	defer func() { _ = spilled.Close() }()

	if inMem.TreeHash() != spilled.TreeHash() {
		t.Fatalf("spilled tree hash %s != in-memory %s", spilled.TreeHash(), inMem.TreeHash())
	}
	// The manifests must be byte-for-byte the same canonical record sequence.
	inOrder := manifestPaths(t, inMem.Manifest())
	spillOrder := manifestPaths(t, spilled.Manifest())
	if len(inOrder) != len(spillOrder) {
		t.Fatalf("spilled manifest has %d records, in-memory %d", len(spillOrder), len(inOrder))
	}
	for i := range inOrder {
		if inOrder[i] != spillOrder[i] {
			t.Fatalf("manifest order differs at %d: %q vs %q", i, spillOrder[i], inOrder[i])
		}
	}
	if inMem.Stats() != spilled.Stats() {
		t.Errorf("spilled stats %+v != in-memory %+v", spilled.Stats(), inMem.Stats())
	}
}

// TestScanSpillContentSourcesWork proves that when a spilling scan retains every
// content source, the verified openers still resolve and read the right bytes — the
// content path is unaffected by whether the manifest spilled.
func TestScanSpillContentSourcesWork(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	for i := 0; i < 20; i++ {
		scantest.Write(t, root, fmt.Sprintf("d%02d/f.txt", i), fmt.Sprintf("bytes-%02d\n", i))
	}
	cfg := config.Defaults()
	svc := scanner.New(worktreefs.New(), blake3hash.New(), nil)

	res, err := svc.Scan(context.Background(), proj, cfg, cfg.HistoryScanScope(), scanner.Options{
		BufferRecords: 3,
		Sources:       scanner.RetainAllSources,
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	defer func() { _ = res.Close() }()

	p, err := worktree.ParseRelPath("d05/f.txt")
	if err != nil {
		t.Fatal(err)
	}
	open, ok := res.Source(p)
	if !ok {
		t.Fatal("content source missing for a blob-intent entry after a spilling scan")
	}
	rc, err := open()
	if err != nil {
		t.Fatalf("open content: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read content: %v", err)
	}
	if string(got) != "bytes-05\n" {
		t.Errorf("content = %q, want %q", got, "bytes-05\n")
	}
}

// TestSpillFollowedOnlySourcesSurviveAndOrdinaryHasNone proves the followed-only
// retention mode over a real worktree that spills: after the sort has gone to disk, the
// two entries whose provenance cannot be rebuilt — a file reached through a file
// symlink, and a file reached below a followed directory symlink — still open and read
// their bytes through their retained openers, while the ordinary file next to them has
// no retained source at all. Spilling changes what backs the manifest, never who owns
// the content sources.
func TestSpillFollowedOnlySourcesSurviveAndOrdinaryHasNone(t *testing.T) {
	scantest.RequireSymlinks(t)
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Write(t, root, "plain.txt", "plain bytes\n")
	scantest.Write(t, root, "real.txt", "followed file bytes\n")
	scantest.Write(t, root, "dir/inner.txt", "followed descendant bytes\n")
	scantest.Symlink(t, root, "link.txt", "real.txt")
	scantest.Symlink(t, root, "dirlink", "dir")
	// Enough filler to make a tiny sorter buffer actually spill.
	for i := 0; i < 20; i++ {
		scantest.Write(t, root, fmt.Sprintf("filler/f%02d.txt", i), fmt.Sprintf("filler-%02d\n", i))
	}

	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true
	svc := scanner.New(worktreefs.New(), blake3hash.New(), nil)
	res, err := svc.Scan(context.Background(), proj, cfg, cfg.HistoryScanScope(), scanner.Options{
		BufferRecords: 3,
		Sources:       scanner.RetainFollowedSources,
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	defer func() { _ = res.Close() }()

	for _, tc := range []struct{ path, want string }{
		{"link.txt", "followed file bytes\n"},
		{"dirlink/inner.txt", "followed descendant bytes\n"},
	} {
		p, err := worktree.ParseRelPath(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		open, ok := res.Source(p)
		if !ok {
			t.Fatalf("followed entry %s lost its retained source after a spilling scan", tc.path)
		}
		rc, err := open()
		if err != nil {
			t.Fatalf("open %s: %v", tc.path, err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read %s: %v", tc.path, err)
		}
		if string(got) != tc.want {
			t.Errorf("%s content = %q, want %q", tc.path, got, tc.want)
		}
	}

	for _, ordinary := range []string{"plain.txt", "real.txt", "dir/inner.txt"} {
		p, err := worktree.ParseRelPath(ordinary)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := res.Source(p); ok {
			t.Errorf("followed-only scan retained a source for the directly-reached %s", ordinary)
		}
	}
}

// TestIdentityScanRetainsNoSources proves an identity-only scan (the default, used by
// run/status/changes) never retains a content opener, so it does not accumulate an
// opener per file.
func TestIdentityScanRetainsNoSources(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Write(t, root, "a.txt", "x\n")
	cfg := config.Defaults()
	svc := scanner.New(worktreefs.New(), blake3hash.New(), nil)
	res, err := svc.Scan(context.Background(), proj, cfg, cfg.HistoryScanScope(), scanner.Options{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	defer func() { _ = res.Close() }()
	p, _ := worktree.ParseRelPath("a.txt")
	if _, ok := res.Source(p); ok {
		t.Error("identity-only scan must not retain a content source")
	}
}

func manifestPaths(t *testing.T, s worktree.ManifestStream) []string {
	t.Helper()
	cur, err := s.Open(context.Background())
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	defer func() { _ = cur.Close() }()
	var out []string
	for cur.Next() {
		out = append(out, cur.Record().Path().String())
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("manifest cursor: %v", err)
	}
	return out
}
