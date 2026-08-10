package scanner

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"awarer/internal/domain/config"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/projfs"
	"awarer/internal/infra/worktreefs"
	"awarer/internal/scantest"
)

// retentionFixture is a worktree holding one of every case the retention decision has
// to separate: a directly-reached blob file, a blob file reached through a file
// symlink, a blob file reached below a followed directory symlink, a large file the
// policy records hash-only (directly and through a symlink), an unreadable input the
// scan records as skipped, a broken link recorded as a symlink entry, and the
// directories all of it lives in.
//
// It is built and scanned in this package on purpose: what a mode retains is internal
// state with no production accessor, and adding one just to count it would put a
// scale-sensitive API on Result to serve a test.
type retentionFixture struct {
	proj projfs.Project
	cfg  config.Config
}

// maxFileSize is small enough that the fixture's "large" files exceed it and its
// ordinary ones do not, so the hash-only policy applies without a multi-megabyte
// fixture.
const maxFileSize = 64

func newRetentionFixture(t *testing.T, withSymlinks bool) retentionFixture {
	t.Helper()
	// The fixture's skipped input is a mode-0000 file, which root would read anyway.
	// The guard belongs here, with the file whose meaning depends on it, so no test
	// using this fixture can forget it.
	if os.Geteuid() == 0 {
		t.Skip("the fixture's unreadable input is meaningless as root")
	}
	root := t.TempDir()
	proj := scantest.InitProject(t, root)

	scantest.Write(t, root, "plain.txt", "plain\n")
	scantest.Write(t, root, "real.txt", "real\n")
	scantest.Write(t, root, "dir/inner.txt", "inner\n")
	// Comfortably past maxFileSize, so the large-file policy records it hash-only.
	scantest.Write(t, root, "big.bin", string(make([]byte, maxFileSize*4)))
	scantest.Write(t, root, "bigreal.bin", string(make([]byte, maxFileSize*4)))

	unreadable := filepath.Join(root, "unreadable.txt")
	if err := os.WriteFile(unreadable, []byte("nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })

	cfg := config.Defaults()
	cfg.Hashing.MaxFileSize = maxFileSize
	cfg.Hashing.LargeFilePolicy = config.LargeFileHashOnly

	if withSymlinks {
		scantest.RequireSymlinks(t)
		scantest.Symlink(t, root, "link.txt", "real.txt")
		scantest.Symlink(t, root, "dirlink", "dir")
		scantest.Symlink(t, root, "biglink.bin", "bigreal.bin")
		scantest.Symlink(t, root, "broken", "nowhere.txt")
		cfg.Scope.FollowSymlinks = true
	}
	return retentionFixture{proj: proj, cfg: cfg}
}

// scan runs the fixture under one retention mode and returns the completed result.
func (f retentionFixture) scan(t *testing.T, mode SourceRetention) Result {
	t.Helper()
	svc := New(worktreefs.New(), blake3hash.New(), nil)
	res, err := svc.Scan(context.Background(), f.proj, f.cfg, f.cfg.HistoryScanScope(), Options{
		AllowSkippedInputs: true,
		Sources:            mode,
	})
	if err != nil {
		t.Fatalf("Scan(%v): %v", mode, err)
	}
	t.Cleanup(func() { _ = res.Close() })
	return res
}

// retainedPaths is the exact set of paths whose openers the scan kept, in canonical
// order, read straight off the internal map so the assertion is about retention itself
// and not about what a lookup happens to answer.
func retainedPaths(r Result) []string {
	out := make([]string, 0, len(r.sources))
	for p := range r.sources {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func assertRetained(t *testing.T, r Result, want []string) {
	t.Helper()
	got := retainedPaths(r)
	if len(got) != len(want) {
		t.Fatalf("retained %d sources %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("retained %v, want %v", got, want)
		}
	}
}

func TestRetainNoSourcesKeepsNoRegistry(t *testing.T) {
	f := newRetentionFixture(t, true)
	res := f.scan(t, RetainNoSources)
	if res.sources != nil {
		t.Fatalf("retain-none allocated a source registry holding %v", retainedPaths(res))
	}
}

func TestRetainAllSourcesKeepsEveryBlobIntentRegular(t *testing.T) {
	f := newRetentionFixture(t, true)
	res := f.scan(t, RetainAllSources)
	// Directly reached and followed blob entries alike; the hash-only files (big.bin,
	// bigreal.bin, biglink.bin), the skipped input, the broken link, and every
	// directory are absent because none of them is a blob-intent regular entry.
	assertRetained(t, res, []string{
		"dir/inner.txt",
		"dirlink/inner.txt",
		"link.txt",
		"plain.txt",
		"real.txt",
	})
}

func TestRetainFollowedSourcesKeepsExactlyFollowedBlobEntries(t *testing.T) {
	f := newRetentionFixture(t, true)
	res := f.scan(t, RetainFollowedSources)
	// Only the two entries whose provenance cannot be rebuilt from their own record:
	// the file reached through a file symlink, and the file reached below a followed
	// directory symlink. Retaining any directly-reached source here — the whole cost
	// this mode exists to remove — fails this assertion.
	assertRetained(t, res, []string{
		"dirlink/inner.txt",
		"link.txt",
	})
}

func TestRetainFollowedSourcesAllocatesNoRegistryWithoutFollowedEntries(t *testing.T) {
	f := newRetentionFixture(t, false)
	res := f.scan(t, RetainFollowedSources)
	if res.sources != nil {
		t.Fatalf("followed-only scan of a worktree with no followed entry allocated a registry holding %v", retainedPaths(res))
	}
}

func TestUnknownRetentionModeFailsAtTheScanBoundary(t *testing.T) {
	f := newRetentionFixture(t, false)
	svc := New(worktreefs.New(), blake3hash.New(), nil)
	// The same fixture under a defined mode must scan cleanly, so the rejection below
	// is attributable to the mode rather than to anything else the scan could dislike.
	f.scan(t, RetainNoSources)

	_, err := svc.Scan(context.Background(), f.proj, f.cfg, f.cfg.HistoryScanScope(), Options{
		AllowSkippedInputs: true,
		Sources:            SourceRetention(42),
	})
	if err == nil {
		t.Fatal("an unknown retention mode must fail the scan, not pick a branch by accident")
	}
	if !strings.Contains(err.Error(), "retention mode") {
		t.Fatalf("Scan err = %v, want it to name the unknown retention mode", err)
	}
}
