package docsexport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"awarer/internal/domain/docbundle"
)

// testBundle builds a small but structurally complete bundle: documents in two
// subdirectories plus the machine reference, so publication exercises directory
// creation, nested paths, and the manifest-last ordering.
func testBundle(t *testing.T) docbundle.Bundle {
	t.Helper()
	doc := func(slug, path, body string, kind docbundle.Kind) docbundle.Document {
		s, err := docbundle.NewSlug(slug)
		if err != nil {
			t.Fatalf("NewSlug(%q): %v", slug, err)
		}
		p, err := docbundle.NewDocPath(path)
		if err != nil {
			t.Fatalf("NewDocPath(%q): %v", path, err)
		}
		d, err := docbundle.NewDocument(s, "T "+slug, "s "+slug, kind, p, nil, []byte(body))
		if err != nil {
			t.Fatalf("NewDocument(%q): %v", slug, err)
		}
		return d
	}
	refPath, err := docbundle.NewDocPath("reference/cli.json")
	if err != nil {
		t.Fatalf("NewDocPath: %v", err)
	}
	machine, err := docbundle.NewMachineRef(refPath, 1, []byte("{\"schema_version\":1}\n"))
	if err != nil {
		t.Fatalf("NewMachineRef: %v", err)
	}
	b, err := docbundle.NewBundle([]docbundle.Document{
		doc("alpha", "topics/alpha.md", "# T alpha\n", docbundle.KindTopic),
		doc("beta", "topics/beta.md", "# T beta\n", docbundle.KindTopic),
		doc("commands", "commands/index.md", "# T commands\n", docbundle.KindCommand),
	}, machine, docbundle.Provenance{Version: "1.2.3", Commit: "abc1234"})
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}
	return b
}

// relFiles lists every regular file under root, as sorted slash-relative paths.
func relFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

// mustSymlink creates a symlink or ends the test — fatally where symlink coverage is
// required, with a named skip otherwise.
//
// Windows grants the privilege only in developer mode or to an elevated process, so a
// symlink case really can be unavailable rather than broken. But the Windows lane exists
// to prove this package's behavior on that platform and sets AWA_REQUIRE_SYMLINK_TESTS to
// say so: there, an unavailable privilege must fail the job rather than silently remove
// the cases it was configured to run.
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

// residuePhrases are the two things a post-reservation failure must say beyond naming
// the destination: that the directory is still there, and what the caller does about
// it. They are checked together because either alone is useless — a leftover nobody is
// told to remove, or an instruction with nothing to act on.
var residuePhrases = []string{"was left in place", "remove it before retrying"}

// requireResidueAction asserts the failure named the destination and told the caller to
// remove it, and that no manifest survives to make the leftover read as an export.
func requireResidueAction(t *testing.T, err error, dest string) {
	t.Helper()
	if err == nil {
		t.Fatal("Publish = nil error, want the injected failure")
	}
	if !strings.Contains(err.Error(), dest) {
		t.Errorf("Publish error = %v, want it to name %s", err, dest)
	}
	for _, want := range residuePhrases {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Publish error = %v, want it to say %q", err, want)
		}
	}
	// The same fact has to be answerable without reading the prose: the CLI decides
	// whether an interruption may keep its terse message by asking this, so an error
	// that says "left in place" but does not resolve to it would print nothing.
	if !errors.Is(err, ErrIncompleteExport) {
		t.Errorf("Publish error = %v, want it to resolve as ErrIncompleteExport", err)
	}
	if _, serr := os.Stat(filepath.Join(dest, docbundle.ManifestName)); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("a failed export left a manifest at %s: %v", dest, serr)
	}
}

// requireNoResidueClaim is the assertion for every failure BEFORE the destination is
// created: sending a user to remove a directory this invocation never made is the one
// way the fail-loud contract can misfire. It reads the same phrase list as the positive
// assertion so the two cannot drift into checking different sentences.
func requireNoResidueClaim(t *testing.T, err error) {
	t.Helper()
	for _, unwanted := range residuePhrases {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("Publish error = %v, want no residue claim (%q): this failure created nothing", err, unwanted)
		}
	}
	if errors.Is(err, ErrIncompleteExport) {
		t.Errorf("Publish error = %v, want it not to resolve as ErrIncompleteExport: this failure created nothing", err)
	}
}

// TestPublishCreatesTheDestination is the success shape: the declared file set, the
// Result, and — because the manifest is installed through a different primitive from
// the bodies — the manifest's bytes and the bodies' bytes compared against the bundle
// that produced them. Checking the manifest merely exists would leave the one file
// whose contents carry the export's authority unverified at the layer that writes it.
func TestPublishCreatesTheDestination(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out")
	bundle := testBundle(t)

	res, err := Publish(context.Background(), bundle, dest)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.Output != dest {
		t.Errorf("Result.Output = %q, want %q", res.Output, dest)
	}
	want := []string{"commands/index.md", "manifest.json", "reference/cli.json", "topics/alpha.md", "topics/beta.md"}
	if got := relFiles(t, dest); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("published files = %v, want %v", got, want)
	}
	if res.ManifestPath != filepath.Join(dest, "manifest.json") {
		t.Errorf("ManifestPath = %q", res.ManifestPath)
	}

	wantManifest, err := bundle.ManifestJSON()
	if err != nil {
		t.Fatalf("ManifestJSON: %v", err)
	}
	if got := readPublished(t, res.ManifestPath); !bytes.Equal(got, wantManifest) {
		t.Errorf("published manifest = %q, want the bundle's rendering %q", got, wantManifest)
	}
	for _, doc := range bundle.Documents() {
		path := filepath.Join(dest, filepath.FromSlash(doc.Path().String()))
		if got := readPublished(t, path); !bytes.Equal(got, doc.Body()) {
			t.Errorf("published %s = %q, want %q", doc.Path(), got, doc.Body())
		}
	}
	ref := bundle.MachineReference()
	refPath := filepath.Join(dest, filepath.FromSlash(ref.Path().String()))
	if got := readPublished(t, refPath); !bytes.Equal(got, ref.Body()) {
		t.Errorf("published %s = %q, want %q", ref.Path(), got, ref.Body())
	}
}

// readPublished reads one published file or ends the test.
func readPublished(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return body
}

// TestPublishedFilesAreReadableUserContent pins the permission decision: an
// exported bundle is ordinary documentation a site build or another user process
// reads, not the owner-private evidence under .awa/. Directories are compared
// against a control directory created the same way rather than against the literal
// constant, because the process umask applies to both and the export must not
// special-case itself out of it.
func TestPublishedFilesAreReadableUserContent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful here")
	}
	control := filepath.Join(t.TempDir(), "control")
	if err := os.Mkdir(control, docsDirPerm); err != nil {
		t.Fatalf("preparing the control directory: %v", err)
	}
	controlInfo, err := os.Stat(control)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	wantDirPerm := controlInfo.Mode().Perm()

	dest := filepath.Join(t.TempDir(), "out")
	if _, err := Publish(context.Background(), testBundle(t), dest); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	for _, name := range []string{filepath.Join("topics", "alpha.md"), "manifest.json"} {
		info, serr := os.Stat(filepath.Join(dest, name))
		if serr != nil {
			t.Fatalf("stat %s: %v", name, serr)
		}
		if perm := info.Mode().Perm(); perm != docsFilePerm {
			t.Errorf("published %s mode = %04o, want %04o", name, perm, docsFilePerm)
		}
	}
	for _, dir := range []string{dest, filepath.Join(dest, "topics")} {
		dirInfo, serr := os.Stat(dir)
		if serr != nil {
			t.Fatalf("stat %s: %v", dir, serr)
		}
		if perm := dirInfo.Mode().Perm(); perm != wantDirPerm {
			t.Errorf("published directory %s mode = %04o, want %04o", dir, perm, wantDirPerm)
		}
	}
}

// TestPublishRefusesUnsafeDestinations is the destination-safety table. The contract
// is one rule — the destination must not exist — so every case here is a place an
// export must never write, and almost every case is refused for the same stated
// reason. Keeping the dangerous destinations enumerated is the point: if the rule
// were ever relaxed back to "an empty directory is acceptable", the empty cases
// below would start passing and the working directory would become a legal target
// again.
//
// Each refusal is also checked for what it must NOT say. These failures happen before
// anything is created, so a residue claim here would send a user looking for a
// directory this invocation never made.
func TestPublishRefusesUnsafeDestinations(t *testing.T) {
	bundle := testBundle(t)

	// The working directory is moved to an EMPTY temporary directory on purpose.
	// Pointed at the real one, the cwd cases would be refused merely for holding
	// files, and would keep passing even if an empty destination became acceptable
	// — which is exactly the regression they exist to catch.
	emptyCwd := t.TempDir()
	t.Chdir(emptyCwd)

	emptyDir := t.TempDir()

	nonEmpty := t.TempDir()
	if err := os.WriteFile(filepath.Join(nonEmpty, "keep.txt"), []byte("user data\n"), 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	dotfileOnly := t.TempDir()
	if err := os.Mkdir(filepath.Join(dotfileOnly, ".awa"), 0o700); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	regularFile := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(regularFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	cases := []struct {
		name   string
		dest   string
		reason string
	}{
		{"empty argument", "", "no destination given"},
		{"whitespace argument", "   ", "no destination given"},
		{"filesystem root", filepath.VolumeName(emptyCwd) + string(filepath.Separator), "is a filesystem root"},
		{"current directory", emptyCwd, "already exists"},
		{"ancestor of the current directory", filepath.Dir(emptyCwd), "already exists"},
		{"existing empty directory", emptyDir, "already exists"},
		{"non-empty directory", nonEmpty, "already exists"},
		{"directory holding only a project marker", dotfileOnly, "already exists"},
		{"regular file", regularFile, "already exists"},
		{"missing parent", filepath.Join(t.TempDir(), "missing", "out"), "does not exist"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Publish(context.Background(), bundle, c.dest)
			if err == nil {
				t.Fatal("Publish = nil error, want a refusal")
			}
			if !errors.Is(err, ErrUnsafeDestination) {
				t.Errorf("Publish error = %v, want ErrUnsafeDestination", err)
			}
			if !strings.Contains(err.Error(), c.reason) {
				t.Errorf("Publish error = %v, want it refused because it %q", err, c.reason)
			}
			requireNoResidueClaim(t, err)
		})
	}

	// The refusals must be inert: nothing published, nothing of the user's removed.
	if got := relFiles(t, nonEmpty); strings.Join(got, ",") != "keep.txt" {
		t.Errorf("a refused export disturbed the destination: %v", got)
	}
	for _, dir := range []string{emptyCwd, emptyDir} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading %s: %v", dir, err)
		}
		if len(entries) != 0 {
			t.Errorf("a refused export left entries in %s: %v", dir, entries)
		}
	}
}

// TestPublishRefusesSymlinkedDestinations keeps the symlink cases separate so the
// main table still runs where symlinks need a privilege. A symlink is refused
// because it EXISTS — mkdir reports EEXIST for one, dangling or not, which is the
// case a Stat-based existence check would call absent and publish straight through.
func TestPublishRefusesSymlinkedDestinations(t *testing.T) {
	linkDir := t.TempDir()
	linkTarget := t.TempDir()

	symlinked := filepath.Join(linkDir, "link")
	mustSymlink(t, linkTarget, symlinked)
	dangling := filepath.Join(linkDir, "dangling")
	mustSymlink(t, filepath.Join(linkDir, "nothing-here"), dangling)
	linkedParent := filepath.Join(linkDir, "linkparent")
	mustSymlink(t, linkTarget, linkedParent)

	for _, c := range []struct{ name, dest, reason string }{
		{"symlinked directory", symlinked, "already exists"},
		{"dangling symlink", dangling, "already exists"},
		{"destination under a symlinked parent", filepath.Join(linkedParent, "out"), "parent directory"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Publish(context.Background(), testBundle(t), c.dest)
			if !errors.Is(err, ErrUnsafeDestination) {
				t.Fatalf("Publish error = %v, want ErrUnsafeDestination", err)
			}
			if !strings.Contains(err.Error(), c.reason) {
				t.Errorf("Publish error = %v, want it refused because it %q", err, c.reason)
			}
		})
	}
	if got := relFiles(t, linkTarget); len(got) != 0 {
		t.Errorf("a refused export wrote through a symlink: %v", got)
	}
}

// TestPublishRefusesTraversingOutput proves a relative destination that climbs out
// of the working directory is still resolved and judged as an absolute path, so
// "../.." cannot slip past the existence rule by never looking like a real path.
func TestPublishRefusesTraversingOutput(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if _, err := Publish(context.Background(), testBundle(t), filepath.Join("..", "..")); !errors.Is(err, ErrUnsafeDestination) {
		t.Errorf("Publish(../..) error = %v, want ErrUnsafeDestination", err)
	}
}

// TestExistingDestinationIsNotOpenedOrModified is the reservation contract stated as
// an observable fact: a refused destination keeps its content, its modification
// time, and its permissions, because mkdir reports EEXIST without ever touching what
// occupies the name.
func TestExistingDestinationIsNotOpenedOrModified(t *testing.T) {
	dest := t.TempDir()
	keep := filepath.Join(dest, "keep.txt")
	if err := os.WriteFile(keep, []byte("user data\n"), 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	before, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if _, err := Publish(context.Background(), testBundle(t), dest); !errors.Is(err, ErrUnsafeDestination) {
		t.Fatalf("Publish into an existing directory = %v, want ErrUnsafeDestination", err)
	}

	after, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("the refused destination was modified: mode %v->%v, mtime %v->%v",
			before.Mode(), after.Mode(), before.ModTime(), after.ModTime())
	}
	if got := relFiles(t, dest); strings.Join(got, ",") != "keep.txt" {
		t.Errorf("destination = %v, want only the user's file", got)
	}
	if body, rerr := os.ReadFile(keep); rerr != nil || string(body) != "user data\n" {
		t.Errorf("the user's file was disturbed: %q, %v", body, rerr)
	}
}

// TestNoManifestExistsUntilTheExportIsComplete is the authoritativeness property
// stated directly. The destination is visible while it fills — that is the price of
// a publication protocol that uses nothing but mkdir — so what makes a directory an
// export is the manifest, and it must not appear until everything else has.
func TestNoManifestExistsUntilTheExportIsComplete(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out")
	manifest := filepath.Join(dest, "manifest.json")

	original := publishBytes
	seen := 0
	publishBytes = func(dir *os.File, name string, data []byte, perm os.FileMode) error {
		seen++
		if _, err := os.Lstat(manifest); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("manifest exists while %s is still being written: %v", name, err)
		}
		return original(dir, name, data, perm)
	}
	t.Cleanup(func() { publishBytes = original })

	if _, err := Publish(context.Background(), testBundle(t), dest); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if seen == 0 {
		t.Fatal("no documents were observed; the check never ran")
	}
	if _, err := os.Stat(manifest); err != nil {
		t.Errorf("manifest missing after a successful export: %v", err)
	}
}

// failAfter replaces the publish primitive with one that writes normally for n
// calls and then fails, so a partial export can be observed.
func failAfter(t *testing.T, n int) {
	t.Helper()
	original := publishBytes
	calls := 0
	publishBytes = func(dir *os.File, name string, data []byte, perm os.FileMode) error {
		calls++
		if calls > n {
			return fmt.Errorf("injected failure on call %d", calls)
		}
		return original(dir, name, data, perm)
	}
	t.Cleanup(func() { publishBytes = original })
}

// TestFailedExportLeavesTheIncompleteDirectory is the residue contract. Once the
// destination exists, awa performs no recovery: the bodies already written stay, the
// manifest never appears, and the error carries both the original classification and
// the one action the user has to take.
//
// The two failure points are the ones the protocol distinguishes: an early body, and
// the very last write before the manifest install — where "no manifest appeared" is
// the claim that has actually been given a chance to fail.
func TestFailedExportLeavesTheIncompleteDirectory(t *testing.T) {
	bundle := testBundle(t)
	lastBody := bundle.DocumentCount() + 1 // documents + machine reference

	for _, c := range []struct {
		name    string
		written int // bodies that succeed before the injected failure
	}{
		{"at the second body", 1},
		{"at the last body, immediately before the manifest", lastBody - 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			parent := t.TempDir()
			dest := filepath.Join(parent, "out")
			failAfter(t, c.written)

			_, err := Publish(context.Background(), bundle, dest)
			requireResidueAction(t, err, dest)
			if !strings.Contains(err.Error(), "injected failure") {
				t.Errorf("Publish error = %v, want the original cause preserved", err)
			}
			if files := relFiles(t, dest); len(files) != c.written {
				t.Errorf("the incomplete export holds %v, want the %d bodies it managed to write", files, c.written)
			}
			// The residue is confined to the destination: a failure creates no
			// sibling, lock, or staging tree beside it.
			entries, rerr := os.ReadDir(parent)
			if rerr != nil {
				t.Fatalf("reading %s: %v", parent, rerr)
			}
			if len(entries) != 1 || entries[0].Name() != "out" {
				t.Errorf("a failed export left %v beside the destination", entries)
			}
		})
	}
}

// TestCancelledContextIsRefusedBeforeAnyWrite proves an export that starts already
// cancelled touches nothing at all — not even reserving the destination — and does
// not tell the user to go and remove something.
func TestCancelledContextIsRefusedBeforeAnyWrite(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "out")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Publish(ctx, testBundle(t), dest)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Publish with a cancelled context = %v, want context.Canceled", err)
	}
	requireNoResidueClaim(t, err)
	if entries, rerr := os.ReadDir(parent); rerr != nil || len(entries) != 0 {
		t.Errorf("a cancelled export created %v (err=%v)", entries, rerr)
	}
}

// TestCancellationMidExportLeavesResidueAndStillClassifies is the interruption
// contract. Ctrl+C partway through is an ordinary post-reservation failure: the
// incomplete directory stays and is named, and the error must STILL resolve as
// context.Canceled so the CLI reports the interruption exit rather than a generic
// failure. Wrapping that loses the classification is the regression this catches.
//
// The second case closes the window at the very end of an export: every body is
// written, so only the check in front of the manifest install can stop an interrupted
// export from becoming authoritative.
func TestCancellationMidExportLeavesResidueAndStillClassifies(t *testing.T) {
	bundle := testBundle(t)
	lastBody := bundle.DocumentCount() + 1

	for _, c := range []struct {
		name string
		at   int
	}{
		{"during the bodies", 2},
		{"after the last body", lastBody},
	} {
		t.Run(c.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "out")
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			original := publishBytes
			calls := 0
			publishBytes = func(dir *os.File, name string, data []byte, perm os.FileMode) error {
				if err := original(dir, name, data, perm); err != nil {
					return err
				}
				calls++
				if calls == c.at {
					cancel()
				}
				return nil
			}
			t.Cleanup(func() { publishBytes = original })

			_, err := Publish(ctx, bundle, dest)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Publish after cancellation = %v, want it to still resolve as context.Canceled", err)
			}
			requireResidueAction(t, err, dest)
		})
	}
}

// TestManifestLessResidueIsRefusedByARetry is the other half of failing loud: what
// the export leaves behind must be dealt with by the user, not by the next run. A
// second invocation at the same path is refused exactly like any other existing
// destination — it does not fill, resume, replace, or clean the residue — and it says
// what makes that directory not an export.
func TestManifestLessResidueIsRefusedByARetry(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out")
	failAfter(t, 2)
	if _, err := Publish(context.Background(), testBundle(t), dest); err == nil {
		t.Fatal("Publish = nil error, want the injected failure")
	}
	residue := relFiles(t, dest)
	if len(residue) == 0 {
		t.Fatal("the failed export left nothing; there is no residue to retry onto")
	}

	_, err := Publish(context.Background(), testBundle(t), dest)
	if !errors.Is(err, ErrUnsafeDestination) {
		t.Fatalf("Publish onto a manifest-less directory = %v, want ErrUnsafeDestination", err)
	}
	if !strings.Contains(err.Error(), "manifest.json") {
		t.Errorf("Publish error = %v, want it to explain that a manifest-less directory is not an export", err)
	}
	if got := relFiles(t, dest); strings.Join(got, ",") != strings.Join(residue, ",") {
		t.Errorf("the retry changed the residue: %v, want %v", got, residue)
	}
	if _, serr := os.Stat(filepath.Join(dest, "manifest.json")); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("the retry stamped a manifest onto a directory it did not fill: %v", serr)
	}
}

// TestExportCreatesNothingBesideTheDestination proves publication is confined to the
// path the user named: no temporary sibling, no lock file, no leftover of any kind
// in the parent directory.
func TestExportCreatesNothingBesideTheDestination(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "out")
	if _, err := Publish(context.Background(), testBundle(t), dest); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("reading %s: %v", parent, err)
	}
	if len(entries) != 1 || entries[0].Name() != "out" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("parent holds %v, want only the destination", names)
	}
}
