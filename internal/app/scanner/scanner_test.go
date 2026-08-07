package scanner_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"awarer/internal/app/scanner"
	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/projfs"
	"awarer/internal/infra/worktreefs"
	"awarer/internal/scantest"
)

// countingHasher wraps a hasher and counts HashReader calls so tests can assert
// how many files were actually read and hashed.
type countingHasher struct {
	inner hashing.Hasher
	reads int
}

func (c *countingHasher) HashReader(r io.Reader) (hashing.ContentHash, error) {
	c.reads++
	return c.inner.HashReader(r)
}
func (c *countingHasher) HashBytes(b []byte) hashing.TreeHash { return c.inner.HashBytes(b) }

// run scans proj with cfg using a fresh blake3 hasher and no index.
func run(t *testing.T, proj projfs.Project, cfg config.Config) scanner.Result {
	t.Helper()
	svc := scanner.New(worktreefs.New(), blake3hash.New(), nil)
	res, err := svc.Scan(context.Background(), proj, cfg, cfg.HistoryScanScope(), scanner.Options{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return res
}

func findEntry(res scanner.Result, path string) (worktree.Entry, bool) {
	for _, e := range res.MaterializeEntries() {
		if e.Path.String() == path {
			return e, true
		}
	}
	return worktree.Entry{}, false
}

func findSkipped(res scanner.Result, path string) (worktree.SkippedInput, bool) {
	for _, s := range res.MaterializeSkipped() {
		if s.Path.String() == path {
			return s, true
		}
	}
	return worktree.SkippedInput{}, false
}

func TestNewPanicsOnNilPorts(t *testing.T) {
	cases := map[string]func(){
		"nil walker": func() { scanner.New(nil, blake3hash.New(), nil) },
		"nil hasher": func() { scanner.New(worktreefs.New(), nil, nil) },
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("expected New to panic on %s", name)
				}
			}()
			build()
		})
	}
}

func TestScanStableTreeHash(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.BuildPlainGo(t, root)

	first := run(t, proj, config.Defaults())
	second := run(t, proj, config.Defaults())

	if first.TreeHash().IsZero() {
		t.Fatalf("tree hash is zero")
	}
	if first.TreeHash().String() != second.TreeHash().String() {
		t.Errorf("tree hash unstable across scans: %q != %q", first.TreeHash(), second.TreeHash())
	}
}

func TestScanIgnoredDirsDoNotAffectTreeHash(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.BuildPlainGo(t, root)
	base := run(t, proj, config.Defaults())

	// Add files only inside built-in excluded directories.
	scantest.Write(t, root, "node_modules/dep/index.js", "x")
	scantest.Write(t, root, "dist/bundle.js", "y")
	after := run(t, proj, config.Defaults())

	if base.TreeHash().String() != after.TreeHash().String() {
		t.Errorf("ignored dirs changed tree hash")
	}
}

func TestScanTrackedChangeChangesTreeHash(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.BuildPlainGo(t, root)
	base := run(t, proj, config.Defaults())

	scantest.Write(t, root, "calc/calc.go", "package calc\n\nfunc Add(a, b int) int { return a + b + 1 }\n")
	after := run(t, proj, config.Defaults())

	if base.TreeHash().String() == after.TreeHash().String() {
		t.Errorf("tracked change did not change tree hash")
	}
}

func TestScanIgnoredFileChangeDoesNotChangeTreeHash(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.BuildPlainGo(t, root)
	// .awaignore is the native ignore source and is on by default; .gitignore is
	// off by default, so this uses .awaignore to exercise ignored-file stability.
	scantest.Write(t, root, ".awaignore", "*.log\n")
	base := run(t, proj, config.Defaults())

	scantest.Write(t, root, "debug.log", "noise")
	after := run(t, proj, config.Defaults())

	if base.TreeHash().String() != after.TreeHash().String() {
		t.Errorf("ignored file change altered tree hash")
	}
}

func TestScanLargeFilePolicies(t *testing.T) {
	const limit = 4096
	cases := []struct {
		policy      config.LargeFilePolicy
		wantStorage worktree.ContentStorageIntent
		wantSkipped bool
	}{
		{config.LargeFileStore, worktree.StorageBlob, false},
		{config.LargeFileHashOnly, worktree.StorageHashOnly, false},
		{config.LargeFileSkip, worktree.StorageNone, true},
	}
	for _, c := range cases {
		t.Run(c.policy.String(), func(t *testing.T) {
			root := t.TempDir()
			proj := scantest.InitProject(t, root)
			scantest.BuildLargeFiles(t, root, limit)

			cfg := config.Defaults()
			cfg.Hashing.MaxFileSize = config.ByteSize(limit)
			cfg.Hashing.LargeFilePolicy = c.policy
			res := run(t, proj, cfg)

			if c.wantSkipped {
				s, ok := findSkipped(res, "over-limit.bin")
				if !ok {
					t.Fatalf("over-limit.bin not skipped")
				}
				if s.Reason != worktree.ReasonLargeFileSkipPolicy {
					t.Errorf("reason = %v, want large-file-skip", s.Reason)
				}
				if _, ok := findEntry(res, "over-limit.bin"); ok {
					t.Errorf("skipped large file should not be an entry")
				}
				return
			}

			e, ok := findEntry(res, "over-limit.bin")
			if !ok {
				t.Fatalf("over-limit.bin missing from entries")
			}
			if e.Storage != c.wantStorage {
				t.Errorf("storage = %v, want %v", e.Storage, c.wantStorage)
			}
			if e.Content.IsZero() {
				t.Errorf("large file should still be hashed")
			}
			// The small under-limit file is always a stored blob.
			small, ok := findEntry(res, "small.txt")
			if !ok || small.Storage != worktree.StorageBlob {
				t.Errorf("small.txt storage = %v, want blob", small.Storage)
			}
		})
	}
}

func TestScanReadErrorFailsFastByDefault(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Write(t, root, "secret.txt", "classified")
	unreadable := filepath.Join(root, "secret.txt")
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })
	if f, err := os.Open(unreadable); err == nil {
		f.Close()
		t.Skip("file still readable (running as root?); cannot test read-error")
	}

	svc := scanner.New(worktreefs.New(), blake3hash.New(), nil)
	_, err := svc.Scan(context.Background(), proj, config.Defaults(), config.Defaults().HistoryScanScope(), scanner.Options{})
	if err == nil {
		t.Fatalf("expected fail-fast error on unreadable file")
	}
}

func TestScanReadErrorOptInSkipTaints(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Write(t, root, "secret.txt", "classified")
	scantest.Write(t, root, "ok.txt", "fine")
	unreadable := filepath.Join(root, "secret.txt")
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })
	if f, err := os.Open(unreadable); err == nil {
		f.Close()
		t.Skip("file still readable (running as root?); cannot test read-error")
	}

	svc := scanner.New(worktreefs.New(), blake3hash.New(), nil)
	res, err := svc.Scan(context.Background(), proj, config.Defaults(), config.Defaults().HistoryScanScope(), scanner.Options{AllowSkippedInputs: true})
	if err != nil {
		t.Fatalf("opt-in skip should not fail: %v", err)
	}
	if !res.Incomplete() {
		t.Errorf("result should be tainted (Incomplete) after a skipped read")
	}
	s, ok := findSkipped(res, "secret.txt")
	if !ok {
		t.Fatalf("unreadable file not recorded as skipped")
	}
	if s.Reason != worktree.ReasonReadError || s.OSError != "permission-denied" {
		t.Errorf("skipped = %+v, want read-error/permission-denied", s)
	}
	if _, ok := findEntry(res, "ok.txt"); !ok {
		t.Errorf("readable files should still be scanned")
	}
}

func TestScanStrictAlwaysHashesWithoutIndex(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.BuildPlainGo(t, root)

	ch := &countingHasher{inner: blake3hash.New()}
	cfg := config.Defaults()
	cfg.Hashing.TrustMode = config.TrustStrict
	svc := scanner.New(worktreefs.New(), ch, nil)
	res, err := svc.Scan(context.Background(), proj, cfg, cfg.HistoryScanScope(), scanner.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Meta().TrustMode != config.TrustStrict {
		t.Errorf("meta trust mode = %v, want strict", res.Meta().TrustMode)
	}
	// Five regular files in the plain-go fixture must each be hashed.
	if ch.reads != 5 {
		t.Errorf("hashed %d files, want 5", ch.reads)
	}
}

func TestScanUsesSingleInjectedClock(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.BuildPlainGo(t, root)

	fixed := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)
	svc := scanner.New(worktreefs.New(), blake3hash.New(), nil)
	res, err := svc.Scan(context.Background(), proj, config.Defaults(), config.Defaults().HistoryScanScope(), scanner.Options{Now: fixed})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Meta().StartedAt.Equal(fixed) || !res.Meta().CompletedAt.Equal(fixed) {
		t.Errorf("injected clock not honored: started=%v completed=%v", res.Meta().StartedAt, res.Meta().CompletedAt)
	}
}

func TestScanFastModeFlagsWeakSignature(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.BuildPlainGo(t, root)

	cfg := config.Defaults()
	cfg.Hashing.TrustMode = config.TrustFast
	res := run(t, proj, cfg)
	if !res.Meta().FastModeWeakSignature {
		t.Errorf("fast mode should flag weak signature in metadata")
	}
}
