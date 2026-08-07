package worktreemut

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/restore"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
)

type harness struct {
	t       *testing.T
	root    string
	layout  paths.Layout
	hasher  *blake3hash.Hasher
	applier *Applier
}

func setup(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	layout := paths.New(root)
	if err := os.MkdirAll(layout.TmpDir(), paths.DirPerm); err != nil {
		t.Fatalf("mkdir store tmp: %v", err)
	}
	hasher := blake3hash.New()
	id, err := restore.NewOperationID(1, bytes.NewReader([]byte{1, 2, 3, 4, 5, 6, 7, 8}))
	if err != nil {
		t.Fatalf("operation id: %v", err)
	}
	applier, err := New(layout, id, hasher)
	if err != nil {
		t.Fatalf("New applier: %v", err)
	}
	t.Cleanup(func() { _ = applier.Discard() })
	return &harness{t: t, root: root, layout: layout, hasher: hasher, applier: applier}
}

// observedPerm returns the permission bits the running platform will actually
// report for a file created with mode. Windows has no POSIX permission bits: Go
// reports 0666 for any writable file and 0444 for a read-only one. Tests state the
// mode they intend and compare against what the platform can express, so the same
// assertions stay meaningful on every shipped platform instead of being silently
// Unix-only. On darwin and linux this is the identity.
func observedPerm(mode uint32) uint32 {
	if runtime.GOOS != "windows" {
		return mode
	}
	if mode&0o200 != 0 {
		return 0o666
	}
	return 0o444
}

// requireSymlinksEnv turns a missing symlink capability from a skip into a
// failure. The windows-portability lane sets it, because that lane exists to prove
// this package's symlink behavior on Windows: a run that quietly skipped every
// symlink case and reported green would be worse than no lane at all, since it
// would look like evidence. A developer running the suite locally on a machine
// without the privilege still gets a named skip.
const requireSymlinksEnv = "AWA_REQUIRE_SYMLINK_TESTS"

// requireSymlinks proves the platform will create a symlink, or ends the test —
// fatally where symlink coverage is required, with a named skip otherwise. Windows
// grants the privilege only in developer mode or to an elevated process.
func requireSymlinks(t *testing.T) {
	t.Helper()
	err := os.Symlink("target", filepath.Join(t.TempDir(), "link"))
	if err == nil {
		return
	}
	if os.Getenv(requireSymlinksEnv) != "" {
		t.Fatalf("%s is set, so symlink coverage is required, but this platform will not create a symlink: %v",
			requireSymlinksEnv, err)
	}
	t.Skipf("this platform will not create a symlink: %v", err)
}

// TestSymlinkCapability is this package's preflight. The windows-portability lane
// runs it as its own step before the suite, so a runner that cannot create symbolic
// links fails the job on a single, obviously-named test instead of letting every
// symlink case below self-skip.
//
// It proves the whole primitive rather than just the call: that the created entry
// really is a link (not a copy the platform substituted), that its stored target
// reads back verbatim, and that removing it removes the link and not what it points
// at.
func TestSymlinkCapability(t *testing.T) {
	requireSymlinks(t)

	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("target"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink("target.txt", link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat symlink: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("created entry is not a symbolic link (mode %s)", info.Mode())
	}
	if got, rerr := os.Readlink(link); rerr != nil || got != "target.txt" {
		t.Fatalf("readlink = %q (%v), want target.txt", got, rerr)
	}
	if err := os.Remove(link); err != nil {
		t.Fatalf("remove symlink: %v", err)
	}
	if _, serr := os.Lstat(link); !os.IsNotExist(serr) {
		t.Fatalf("symlink survived removal: %v", serr)
	}
	if _, serr := os.Lstat(target); serr != nil {
		t.Fatalf("removing the link removed its target: %v", serr)
	}
}

// supportsExecutableBit reports whether the platform can express an executable
// file at all. Windows cannot, so a test about restoring that bit has nothing to
// prove there.
func supportsExecutableBit() bool { return runtime.GOOS != "windows" }

func (h *harness) abs(rel string) string { return filepath.Join(h.root, filepath.FromSlash(rel)) }

func (h *harness) relPath(p string) worktree.RelPath {
	h.t.Helper()
	rp, err := worktree.ParseRelPath(p)
	if err != nil {
		h.t.Fatalf("rel path %q: %v", p, err)
	}
	return rp
}

func (h *harness) hashOf(content string) hashing.ContentHash {
	h.t.Helper()
	c, err := h.hasher.HashReader(strings.NewReader(content))
	if err != nil {
		h.t.Fatalf("hash: %v", err)
	}
	return c
}

// regularNode builds the proved state of a regular file. The mode is normalized
// through observedPerm because both sides of every comparison here ultimately come
// from the filesystem: a plan built from an observation on this platform would
// carry exactly the bits the platform reports.
func (h *harness) regularNode(content string, mode uint32) restore.NodeState {
	h.t.Helper()
	n, err := restore.RegularNode(h.hashOf(content), observedPerm(mode))
	if err != nil {
		h.t.Fatalf("regular node: %v", err)
	}
	return n
}

func (h *harness) symlinkNode(target string) restore.NodeState {
	h.t.Helper()
	tgt, err := worktree.NewSymlinkTarget(target)
	if err != nil {
		h.t.Fatalf("symlink target: %v", err)
	}
	n, err := restore.SymlinkNode(tgt)
	if err != nil {
		h.t.Fatalf("symlink node: %v", err)
	}
	return n
}

func (h *harness) writeWorktreeFile(rel, content string, mode os.FileMode) {
	h.t.Helper()
	p := h.abs(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		h.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		h.t.Fatalf("write %s: %v", rel, err)
	}
	if err := os.Chmod(p, mode); err != nil {
		h.t.Fatalf("chmod %s: %v", rel, err)
	}
}

// stage verifies content into the applier's staging area and returns the payload.
func (h *harness) stage(content string) restore.StagedPayload {
	h.t.Helper()
	p, err := h.applier.Stage(context.Background(), h.hashOf(content), func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(content)), nil
	})
	if err != nil {
		h.t.Fatalf("Stage: %v", err)
	}
	return p
}

// staged builds an executable operation, staging content when the desired state
// is a regular file.
func (h *harness) staged(path string, current, desired restore.NodeState, content string) restore.StagedOperation {
	h.t.Helper()
	planned, err := restore.NewPlannedOperation(h.relPath(path), current, desired)
	if err != nil {
		h.t.Fatalf("planned operation: %v", err)
	}
	var payload restore.StagedPayload
	if planned.RequiresContent() {
		payload = h.stage(content)
	}
	op, err := restore.NewStagedOperation(planned, payload)
	if err != nil {
		h.t.Fatalf("staged operation: %v", err)
	}
	return op
}

// apply installs one operation and returns only the failure, for the many tests whose
// subject is the filesystem result. applyResult is for the ones that assert what the
// applier reported about the destination.
func (h *harness) apply(op restore.StagedOperation) error {
	return h.applier.Apply(context.Background(), op).Err()
}

func (h *harness) applyResult(op restore.StagedOperation) restore.MutationResult {
	return h.applier.Apply(context.Background(), op)
}

func (h *harness) readFile(rel string) string {
	h.t.Helper()
	b, err := os.ReadFile(h.abs(rel)) //nolint:gosec // test fixture path
	if err != nil {
		h.t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// --- staging --------------------------------------------------------------

func TestStageVerifiesBytesAgainstTheIdentityTheSourceProved(t *testing.T) {
	h := setup(t)
	want := h.hashOf("the real bytes")

	// A poison opener that yields bytes which do not hash to the address the
	// manifest names: the store is lying about its own content, so nothing may be
	// staged and apply must refuse before touching the worktree.
	_, err := h.applier.Stage(context.Background(), want, func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("something else entirely")), nil
	})
	if err == nil {
		t.Fatal("Stage accepted bytes that do not hash to the requested identity")
	}
	if h.applier.Staged() != 0 {
		t.Errorf("Staged() = %d after a rejected payload, want 0", h.applier.Staged())
	}

	p, err := h.applier.Stage(context.Background(), want, func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("the real bytes")), nil
	})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if p.Content() != want {
		t.Errorf("staged content = %s, want %s", p.Content(), want)
	}
	if p.Size() != int64(len("the real bytes")) {
		t.Errorf("staged size = %d, want %d", p.Size(), len("the real bytes"))
	}
	if p.Handle() == "" {
		t.Error("staged payload has no handle")
	}
	if h.applier.Staged() != 1 {
		t.Errorf("Staged() = %d, want 1", h.applier.Staged())
	}
}

func TestStageLandsUnderAwaOwnedTempNotBesideTheWorktreePath(t *testing.T) {
	h := setup(t)
	h.stage("bytes")
	// A sibling temp file beside a worktree path would perturb the re-observation
	// the apply protocol takes immediately before committing, so staging must live
	// entirely inside .awa/store/tmp.
	entries, err := os.ReadDir(h.root)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	for _, e := range entries {
		if e.Name() != paths.Dir {
			t.Errorf("staging created %q in the worktree root", e.Name())
		}
	}
}

func TestStageRefusesWithoutAContentHash(t *testing.T) {
	h := setup(t)
	if _, err := h.applier.Stage(context.Background(), hashing.ContentHash{}, func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("x")), nil
	}); err == nil {
		t.Error("Stage accepted a zero content hash")
	}
}

func TestStageHonorsCancellation(t *testing.T) {
	h := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.applier.Stage(ctx, h.hashOf("x"), func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("x")), nil
	}); !errors.Is(err, context.Canceled) {
		t.Errorf("Stage under a cancelled context = %v, want context.Canceled", err)
	}
}

// --- installation ---------------------------------------------------------

func TestApplyCreatesAFileAndItsMissingParents(t *testing.T) {
	h := setup(t)
	op := h.staged("generated/client/openapi.json", restore.AbsentNode(), h.regularNode("{}", 0o644), "{}")
	if err := h.apply(op); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := h.readFile("generated/client/openapi.json"); got != "{}" {
		t.Errorf("created file = %q, want %q", got, "{}")
	}
	info, err := os.Lstat(h.abs("generated/client/openapi.json"))
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if got := uint32(info.Mode().Perm()); got != observedPerm(0o644) {
		t.Errorf("created mode = %04o, want %04o", got, observedPerm(0o644))
	}
}

func TestApplyRestoresTheExecutableBit(t *testing.T) {
	if !supportsExecutableBit() {
		t.Skip("this platform has no executable bit to restore")
	}
	h := setup(t)
	h.writeWorktreeFile("bin/tool.sh", "old", 0o644)
	op := h.staged("bin/tool.sh", h.regularNode("old", 0o644), h.regularNode("#!/bin/sh\n", 0o755), "#!/bin/sh\n")
	if err := h.apply(op); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	info, err := os.Lstat(h.abs("bin/tool.sh"))
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("restored mode = %04o, want 0755", info.Mode().Perm())
	}
	if got := h.readFile("bin/tool.sh"); got != "#!/bin/sh\n" {
		t.Errorf("restored content = %q", got)
	}
}

func TestApplyRestoresASymlinkWithoutFollowingIt(t *testing.T) {
	requireSymlinks(t)
	h := setup(t)
	h.writeWorktreeFile("target.txt", "target contents", 0o644)
	op := h.staged("link", restore.AbsentNode(), h.symlinkNode("target.txt"), "")
	if err := h.apply(op); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	info, err := os.Lstat(h.abs("link"))
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("restored node is not a symlink (%s)", info.Mode())
	}
	target, err := os.Readlink(h.abs("link"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != "target.txt" {
		t.Errorf("restored target = %q, want target.txt", target)
	}
	// Restoring a link must not have written through it into the target.
	if got := h.readFile("target.txt"); got != "target contents" {
		t.Errorf("restoring a symlink modified its target: %q", got)
	}
}

func TestApplyReplacesASymlinkTarget(t *testing.T) {
	requireSymlinks(t)
	h := setup(t)
	if err := os.Symlink("old-target", h.abs("link")); err != nil {
		t.Fatalf("seed symlink: %v", err)
	}
	op := h.staged("link", h.symlinkNode("old-target"), h.symlinkNode("new-target"), "")
	if err := h.apply(op); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	target, err := os.Readlink(h.abs("link"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != "new-target" {
		t.Errorf("target = %q, want new-target", target)
	}
}

// TestApplyDeletesAProvedSymlinkNotItsTarget covers the deletion half of the
// symlink lifecycle at a nested path: unlinking a proved link must remove the link
// and leave what it pointed at untouched.
func TestApplyDeletesAProvedSymlinkNotItsTarget(t *testing.T) {
	requireSymlinks(t)
	h := setup(t)
	h.writeWorktreeFile("generated/target.txt", "target contents", 0o644)
	if err := os.Symlink("target.txt", h.abs("generated/link")); err != nil {
		t.Fatalf("seed symlink: %v", err)
	}

	op := h.staged("generated/link", h.symlinkNode("target.txt"), restore.AbsentNode(), "")
	if err := h.apply(op); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := os.Lstat(h.abs("generated/link")); !os.IsNotExist(err) {
		t.Errorf("symlink survived its deletion: %v", err)
	}
	if got := h.readFile("generated/target.txt"); got != "target contents" {
		t.Errorf("deleting a symlink damaged its target: %q", got)
	}
}

// linkFixture seeds two names for one inode and returns the shared content. Hard
// links are a production requirement, not a probed capability: every blob is
// published with linkat, so a platform that cannot create one cannot store a
// checkpoint at all. A failure here is therefore fatal rather than a skip.
func (h *harness) linkFixture(a, b, content string) {
	h.t.Helper()
	h.writeWorktreeFile(a, content, 0o644)
	if err := os.Link(h.abs(a), h.abs(b)); err != nil {
		h.t.Fatalf("hard link %s -> %s: %v (awa publishes every blob with a hard link, so this platform cannot run awa)", b, a, err)
	}
	ai, err := os.Lstat(h.abs(a))
	if err != nil {
		h.t.Fatalf("lstat %s: %v", a, err)
	}
	bi, err := os.Lstat(h.abs(b))
	if err != nil {
		h.t.Fatalf("lstat %s: %v", b, err)
	}
	if !os.SameFile(ai, bi) {
		h.t.Fatalf("%s and %s are not the same inode, so this fixture proves nothing", a, b)
	}
}

// TestApplyActsOnTheSelectedNameNotTheSharedInode is the hard-link guard. Two names
// share one inode and only one of them is planned, so an in-place write or an
// in-place truncate would change a path with no operation, no precondition proof,
// and no bytes in the recovery observation — the exact failure the whole apply
// protocol exists to make impossible. Replacement goes through a same-directory temp
// and a rename, and deletion goes through unlinkat, so both act on the name.
func TestApplyActsOnTheSelectedNameNotTheSharedInode(t *testing.T) {
	t.Run("replace", func(t *testing.T) {
		h := setup(t)
		h.linkFixture("a.txt", "b.txt", "shared bytes")

		op := h.staged("a.txt", h.regularNode("shared bytes", 0o644), h.regularNode("restored bytes", 0o644), "restored bytes")
		if err := h.apply(op); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := h.readFile("a.txt"); got != "restored bytes" {
			t.Errorf("selected name = %q, want %q", got, "restored bytes")
		}
		if got := h.readFile("b.txt"); got != "shared bytes" {
			t.Errorf("the unselected hard link was rewritten to %q; it must still hold %q", got, "shared bytes")
		}
		ai, err := os.Lstat(h.abs("a.txt"))
		if err != nil {
			t.Fatalf("lstat a.txt: %v", err)
		}
		bi, err := os.Lstat(h.abs("b.txt"))
		if err != nil {
			t.Fatalf("lstat b.txt: %v", err)
		}
		// The rename broke the link rather than writing through it, which is what makes
		// the byte assertion above a structural property and not a coincidence.
		if os.SameFile(ai, bi) {
			t.Error("the replacement wrote through the shared inode instead of replacing the name")
		}
	})

	t.Run("delete", func(t *testing.T) {
		h := setup(t)
		h.linkFixture("a.txt", "b.txt", "shared bytes")

		op := h.staged("a.txt", h.regularNode("shared bytes", 0o644), restore.AbsentNode(), "")
		if err := h.apply(op); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if _, err := os.Lstat(h.abs("a.txt")); !os.IsNotExist(err) {
			t.Errorf("selected name survived its deletion: %v", err)
		}
		if got := h.readFile("b.txt"); got != "shared bytes" {
			t.Errorf("deleting one name changed the other to %q", got)
		}
	})
}

func TestApplyDeletesAProvedFile(t *testing.T) {
	h := setup(t)
	h.writeWorktreeFile("stale.txt", "gone soon", 0o644)
	op := h.staged("stale.txt", h.regularNode("gone soon", 0o644), restore.AbsentNode(), "")
	if err := h.apply(op); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := os.Lstat(h.abs("stale.txt")); !os.IsNotExist(err) {
		t.Errorf("file survived its deletion: %v", err)
	}
}

func TestApplyDeletesOnlyAnEmptyDirectory(t *testing.T) {
	h := setup(t)
	h.writeWorktreeFile("keep/child.txt", "unobserved", 0o644)
	if err := os.MkdirAll(h.abs("empty"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// The empty one goes.
	if err := h.apply(h.staged("empty", restore.DirNode(), restore.AbsentNode(), "")); err != nil {
		t.Fatalf("Apply on an empty directory: %v", err)
	}
	if _, err := os.Lstat(h.abs("empty")); !os.IsNotExist(err) {
		t.Errorf("empty directory survived: %v", err)
	}

	// The one holding an unobserved child must refuse rather than take it along.
	err := h.apply(h.staged("keep", restore.DirNode(), restore.AbsentNode(), ""))
	if !errors.Is(err, restore.ErrDirectoryNotEmpty) {
		t.Fatalf("Apply on a non-empty directory = %v, want ErrDirectoryNotEmpty", err)
	}
	if got := h.readFile("keep/child.txt"); got != "unobserved" {
		t.Errorf("the refused deletion damaged an unobserved child: %q", got)
	}
}

// isNotEmpty is the platform seam behind that refusal, and the errno identity it
// matches differs per OS. Pinning it directly, against an error the running kernel
// actually produced, keeps a wrong mapping a one-line diagnosis instead of an
// Apply-level mystery: a platform whose classification is missing reports the refusal as
// a generic I/O failure, and the only visible symptom is the wrong restore Reason.
func TestNonEmptyRemovalIsClassified(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "holder")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "child.txt"), []byte("unobserved"), 0o644); err != nil {
		t.Fatalf("seeding the child: %v", err)
	}

	err := os.Remove(dir)
	if err == nil {
		t.Fatal("removing a non-empty directory succeeded; this platform cannot host the empty-only deletion contract")
	}
	if !isNotEmpty(err) {
		t.Fatalf("isNotEmpty(%#v) = false, want true; this platform's non-empty refusal is not classified", err)
	}

	// And it must not answer yes to just any refusal, or the classification would carry
	// no information: an absent directory is a different fault with its own branch.
	if missing := os.Remove(filepath.Join(t.TempDir(), "absent")); isNotEmpty(missing) {
		t.Errorf("isNotEmpty(%#v) = true for an absent directory; the classification is too broad", missing)
	}
}

func TestApplyCreatesADirectoryTheSourceProves(t *testing.T) {
	h := setup(t)
	op := h.staged("made/up/dir", restore.AbsentNode(), restore.DirNode(), "")
	if err := h.apply(op); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	info, err := os.Lstat(h.abs("made/up/dir"))
	if err != nil || !info.IsDir() {
		t.Fatalf("directory was not created: %v %v", info, err)
	}
}

func TestApplyTypeChangeFromAFileToADirectory(t *testing.T) {
	h := setup(t)
	h.writeWorktreeFile("thing", "was a file", 0o644)
	op := h.staged("thing", h.regularNode("was a file", 0o644), restore.DirNode(), "")
	if err := h.apply(op); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	info, err := os.Lstat(h.abs("thing"))
	if err != nil || !info.IsDir() {
		t.Fatalf("type change to a directory did not happen: %v %v", info, err)
	}
}

func TestApplyTypeChangeFromAnEmptyDirectoryToAFile(t *testing.T) {
	h := setup(t)
	if err := os.MkdirAll(h.abs("thing"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	op := h.staged("thing", restore.DirNode(), h.regularNode("now a file", 0o644), "now a file")
	if err := h.apply(op); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := h.readFile("thing"); got != "now a file" {
		t.Errorf("type change produced %q", got)
	}
}

// TestApplyTypeChangeToASymlink covers the shape a generator most often destroys:
// it writes a real file (or a directory) over a symlink the project needs. The
// desired shape decides the mechanics, so a type change whose desired state is a
// link must install the link — refusing it as an unsupported node would leave the
// one case restore exists for unrepairable.
func TestApplyTypeChangeToASymlink(t *testing.T) {
	requireSymlinks(t)

	t.Run("over a regular file", func(t *testing.T) {
		h := setup(t)
		h.writeWorktreeFile("link", "a generator wrote this", 0o644)
		op := h.staged("link", h.regularNode("a generator wrote this", 0o644), h.symlinkNode("target.txt"), "")
		if err := h.apply(op); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		info, err := os.Lstat(h.abs("link"))
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("link is not a symlink: %v %v", info, err)
		}
		if target, rerr := os.Readlink(h.abs("link")); rerr != nil || target != "target.txt" {
			t.Errorf("target = %q (%v), want target.txt", target, rerr)
		}
	})

	t.Run("over an empty directory", func(t *testing.T) {
		h := setup(t)
		if err := os.MkdirAll(h.abs("link"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		op := h.staged("link", restore.DirNode(), h.symlinkNode("target.txt"), "")
		if err := h.apply(op); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if target, rerr := os.Readlink(h.abs("link")); rerr != nil || target != "target.txt" {
			t.Errorf("target = %q (%v), want target.txt", target, rerr)
		}
	})

	t.Run("a directory holding unobserved content still refuses", func(t *testing.T) {
		h := setup(t)
		h.writeWorktreeFile("link/unobserved.txt", "not awa's", 0o644)
		op := h.staged("link", restore.DirNode(), h.symlinkNode("target.txt"), "")
		if err := h.apply(op); !errors.Is(err, restore.ErrDirectoryNotEmpty) {
			t.Fatalf("Apply = %v, want ErrDirectoryNotEmpty", err)
		}
		if got := h.readFile("link/unobserved.txt"); got != "not awa's" {
			t.Errorf("the refused type change damaged an unobserved child: %q", got)
		}
	})
}

func TestApplyTypeChangeRefusesWhenTheDirectoryStillHoldsUnobservedContent(t *testing.T) {
	h := setup(t)
	h.writeWorktreeFile("thing/child.txt", "unobserved", 0o644)
	op := h.staged("thing", restore.DirNode(), h.regularNode("now a file", 0o644), "now a file")
	if err := h.apply(op); !errors.Is(err, restore.ErrDirectoryNotEmpty) {
		t.Fatalf("Apply = %v, want ErrDirectoryNotEmpty", err)
	}
	if got := h.readFile("thing/child.txt"); got != "unobserved" {
		t.Errorf("the refused type change damaged an unobserved child: %q", got)
	}
}

// --- last-moment precondition guards --------------------------------------

func TestApplyRefusesWhenTheDestinationMovedOutFromUnderThePlan(t *testing.T) {
	cases := []struct {
		name string
		// needSymlink marks a case whose fixture is a symbolic link, so the shared
		// capability gate decides whether it must run or may be skipped.
		needSymlink bool
		setup       func(h *harness)
		op          func(h *harness) restore.StagedOperation
	}{
		{
			name:  "planned absent, now present",
			setup: func(h *harness) { h.writeWorktreeFile("x.txt", "someone else wrote this", 0o644) },
			op: func(h *harness) restore.StagedOperation {
				return h.staged("x.txt", restore.AbsentNode(), h.regularNode("new", 0o644), "new")
			},
		},
		{
			name:  "planned present, now absent",
			setup: func(h *harness) {},
			op: func(h *harness) restore.StagedOperation {
				return h.staged("x.txt", h.regularNode("old", 0o644), h.regularNode("new", 0o644), "new")
			},
		},
		{
			name:  "planned a file, now a directory",
			setup: func(h *harness) { _ = os.MkdirAll(h.abs("x.txt"), 0o755) },
			op: func(h *harness) restore.StagedOperation {
				return h.staged("x.txt", h.regularNode("old", 0o644), h.regularNode("new", 0o644), "new")
			},
		},
		{
			// 0400 rather than 0600: Windows can only express "writable" or "read-only",
			// so a 0600-vs-0644 pair would be indistinguishable there and the case would
			// prove nothing on a platform awa ships.
			name:  "planned permission bits changed",
			setup: func(h *harness) { h.writeWorktreeFile("x.txt", "old", 0o400) },
			op: func(h *harness) restore.StagedOperation {
				return h.staged("x.txt", h.regularNode("old", 0o644), h.regularNode("new", 0o644), "new")
			},
		},
		{
			name:  "planned a directory, now a file",
			setup: func(h *harness) { h.writeWorktreeFile("d", "not a directory", 0o644) },
			op: func(h *harness) restore.StagedOperation {
				return h.staged("d", restore.DirNode(), restore.AbsentNode(), "")
			},
		},
		{
			name:        "planned symlink target changed",
			needSymlink: true,
			setup: func(h *harness) {
				if err := os.Symlink("elsewhere", h.abs("link")); err != nil {
					h.t.Fatalf("seed symlink: %v", err)
				}
			},
			op: func(h *harness) restore.StagedOperation {
				return h.staged("link", h.symlinkNode("old-target"), h.symlinkNode("new-target"), "")
			},
		},
		{
			name:        "planned a file, now a symlink",
			needSymlink: true,
			setup: func(h *harness) {
				if err := os.Symlink("elsewhere", h.abs("x.txt")); err != nil {
					h.t.Fatalf("seed symlink: %v", err)
				}
			},
			op: func(h *harness) restore.StagedOperation {
				return h.staged("x.txt", h.regularNode("old", 0o644), restore.AbsentNode(), "")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.needSymlink {
				requireSymlinks(t)
			}
			h := setup(t)
			tc.setup(h)
			op := tc.op(h)
			if err := h.apply(op); !errors.Is(err, restore.ErrPreconditionMismatch) {
				t.Fatalf("Apply = %v, want ErrPreconditionMismatch", err)
			}
		})
	}
}

// TestApplyRefusesWhenTheContentChangedBehindAnUnchangedShape is the guard shape
// alone cannot be. A build tool or an editor can write different bytes into a file
// without changing its kind or its permission bits, and every such write lands
// after the observation the plan was built from. Overwriting or deleting on a
// stale content proof would destroy work no evidence describes: the recovery
// observation was recorded before those bytes existed, so it cannot bring them
// back.
//
// The replacement bytes are the SAME length as the planned ones, so a size
// comparison would not notice them — only re-deriving the content identity does.
func TestApplyRefusesWhenTheContentChangedBehindAnUnchangedShape(t *testing.T) {
	cases := []struct {
		name string
		op   func(h *harness) restore.StagedOperation
	}{
		{
			name: "replace",
			op: func(h *harness) restore.StagedOperation {
				return h.staged("x.txt", h.regularNode("old-bytes", 0o644), h.regularNode("new-bytes", 0o644), "new-bytes")
			},
		},
		{
			name: "delete",
			op: func(h *harness) restore.StagedOperation {
				return h.staged("x.txt", h.regularNode("old-bytes", 0o644), restore.AbsentNode(), "")
			},
		},
		{
			name: "type change to a directory",
			op: func(h *harness) restore.StagedOperation {
				return h.staged("x.txt", h.regularNode("old-bytes", 0o644), restore.DirNode(), "")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := setup(t)
			h.writeWorktreeFile("x.txt", "old-bytes", 0o644)
			op := tc.op(h)

			// The swap happens after the operation was planned and staged, which is
			// exactly the window the caller's whole-selection re-observation cannot see.
			h.writeWorktreeFile("x.txt", "HER-BYTES", 0o644)

			if err := h.apply(op); !errors.Is(err, restore.ErrPreconditionMismatch) {
				t.Fatalf("Apply = %v, want ErrPreconditionMismatch", err)
			}
			if got := h.readFile("x.txt"); got != "HER-BYTES" {
				t.Errorf("the destination = %q, want the unobserved bytes left exactly as they were", got)
			}
		})
	}
}

// TestApplyAcceptsAnUnchangedFileWhoseVolatileStatMoved is the other half of the
// content guard: identity is content and permission bits, not mtime or inode, so
// a file that was merely rewritten with the same bytes must still be restorable.
// Without this the guard would turn an ordinary formatter re-run into a conflict.
func TestApplyAcceptsAnUnchangedFileWhoseVolatileStatMoved(t *testing.T) {
	h := setup(t)
	h.writeWorktreeFile("x.txt", "old-bytes", 0o644)
	op := h.staged("x.txt", h.regularNode("old-bytes", 0o644), h.regularNode("new-bytes", 0o644), "new-bytes")

	// Same bytes, new mtime and (very likely) a new inode.
	h.writeWorktreeFile("x.txt", "old-bytes", 0o644)

	if err := h.apply(op); err != nil {
		t.Fatalf("Apply = %v, want success: identical bytes are not a conflict", err)
	}
	if got := h.readFile("x.txt"); got != "new-bytes" {
		t.Errorf("destination = %q, want the restored bytes", got)
	}
}

// TestApplyAtTheProjectRootHasNoParentComponent covers the one path shape whose
// parent is the project root itself. It is called out separately because the
// no-follow descent has no component to walk there, which is where a naive parent
// split yields an empty relative path and fails closed on a perfectly ordinary
// path.
func TestApplyAtTheProjectRootHasNoParentComponent(t *testing.T) {
	h := setup(t)

	// create
	if err := h.apply(h.staged("root.txt", restore.AbsentNode(), h.regularNode("created", 0o644), "created")); err != nil {
		t.Fatalf("create at the root: %v", err)
	}
	if got := h.readFile("root.txt"); got != "created" {
		t.Fatalf("created file = %q", got)
	}

	// replace
	if err := h.apply(h.staged("root.txt", h.regularNode("created", 0o644), h.regularNode("replaced", 0o644), "replaced")); err != nil {
		t.Fatalf("replace at the root: %v", err)
	}
	if got := h.readFile("root.txt"); got != "replaced" {
		t.Fatalf("replaced file = %q", got)
	}

	// precondition conflict
	h.writeWorktreeFile("root.txt", "somebody else", 0o644)
	err := h.apply(h.staged("root.txt", h.regularNode("replaced", 0o644), h.regularNode("planned", 0o644), "planned"))
	if !errors.Is(err, restore.ErrPreconditionMismatch) {
		t.Fatalf("conflicting apply at the root = %v, want ErrPreconditionMismatch", err)
	}
	if got := h.readFile("root.txt"); got != "somebody else" {
		t.Errorf("a refused apply at the root still wrote: %q", got)
	}

	// delete
	if err := h.apply(h.staged("root.txt", h.regularNode("somebody else", 0o644), restore.AbsentNode(), "")); err != nil {
		t.Fatalf("delete at the root: %v", err)
	}
	if _, serr := os.Lstat(h.abs("root.txt")); !os.IsNotExist(serr) {
		t.Errorf("root-level file survived its deletion: %v", serr)
	}

	// an empty directory at the root
	if err := h.apply(h.staged("rootdir", restore.AbsentNode(), restore.DirNode(), "")); err != nil {
		t.Fatalf("create a root-level directory: %v", err)
	}
	if err := h.apply(h.staged("rootdir", restore.DirNode(), restore.AbsentNode(), "")); err != nil {
		t.Fatalf("delete a root-level directory: %v", err)
	}
	if _, serr := os.Lstat(h.abs("rootdir")); !os.IsNotExist(serr) {
		t.Errorf("root-level directory survived its deletion: %v", serr)
	}
}

func TestApplyAtTheProjectRootRestoresAndRemovesASymlink(t *testing.T) {
	requireSymlinks(t)
	h := setup(t)

	if err := h.apply(h.staged("rootlink", restore.AbsentNode(), h.symlinkNode("target.txt"), "")); err != nil {
		t.Fatalf("create a root-level symlink: %v", err)
	}
	target, err := os.Readlink(h.abs("rootlink"))
	if err != nil || target != "target.txt" {
		t.Fatalf("root-level symlink target = %q (%v)", target, err)
	}

	if err := h.apply(h.staged("rootlink", h.symlinkNode("target.txt"), h.symlinkNode("other.txt"), "")); err != nil {
		t.Fatalf("replace a root-level symlink: %v", err)
	}
	if target, err = os.Readlink(h.abs("rootlink")); err != nil || target != "other.txt" {
		t.Fatalf("replaced root-level symlink target = %q (%v)", target, err)
	}

	if err := h.apply(h.staged("rootlink", h.symlinkNode("other.txt"), restore.AbsentNode(), "")); err != nil {
		t.Fatalf("delete a root-level symlink: %v", err)
	}
	if _, serr := os.Lstat(h.abs("rootlink")); !os.IsNotExist(serr) {
		t.Errorf("root-level symlink survived its deletion: %v", serr)
	}
}

func TestApplyRefusesToWriteThroughASymlinkedAncestor(t *testing.T) {
	requireSymlinks(t)
	h := setup(t)
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "client"), 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := os.Symlink(outside, h.abs("generated")); err != nil {
		t.Fatalf("seed symlinked ancestor: %v", err)
	}
	op := h.staged("generated/client/openapi.json", restore.AbsentNode(), h.regularNode("{}", 0o644), "{}")
	err := h.apply(op)
	if !errors.Is(err, restore.ErrSymlinkAncestor) {
		t.Fatalf("Apply through a symlinked ancestor = %v, want ErrSymlinkAncestor", err)
	}
	// And nothing was written outside the project.
	if _, serr := os.Lstat(filepath.Join(outside, "client", "openapi.json")); !os.IsNotExist(serr) {
		t.Errorf("a write escaped the project root through a symlinked ancestor: %v", serr)
	}
}

func TestApplyRefusesToDeleteThroughASymlinkedAncestor(t *testing.T) {
	requireSymlinks(t)
	h := setup(t)
	outside := t.TempDir()
	victim := filepath.Join(outside, "precious.txt")
	if err := os.WriteFile(victim, []byte("not awa's"), 0o600); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	if err := os.Symlink(outside, h.abs("generated")); err != nil {
		t.Fatalf("seed symlinked ancestor: %v", err)
	}
	op := h.staged("generated/precious.txt", h.regularNode("not awa's", 0o600), restore.AbsentNode(), "")
	if err := h.apply(op); !errors.Is(err, restore.ErrSymlinkAncestor) {
		t.Fatalf("Apply = %v, want ErrSymlinkAncestor", err)
	}
	if _, err := os.Lstat(victim); err != nil {
		t.Errorf("a deletion escaped the project root through a symlinked ancestor: %v", err)
	}
}

// --- atomicity ------------------------------------------------------------

func TestReplacementLeavesOldOrNewBytesNeverAHalfWrittenFile(t *testing.T) {
	h := setup(t)
	h.writeWorktreeFile("data.json", "OLD", 0o644)
	op := h.staged("data.json", h.regularNode("OLD", 0o644), h.regularNode("NEWNEWNEW", 0o644), "NEWNEWNEW")

	// Fail after the replacement bytes are written but before the rename installs
	// them: the destination must still hold its old bytes, byte for byte.
	h.applier.fail = func(failStage, worktree.RelPath) error { return errors.New("injected write failure") }
	if err := h.apply(op); err == nil {
		t.Fatal("Apply succeeded despite an injected write failure")
	}
	if got := h.readFile("data.json"); got != "OLD" {
		t.Fatalf("destination = %q after a failed replacement, want the old bytes intact", got)
	}
	// No temp debris beside the destination.
	entries, err := os.ReadDir(h.root)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".awa-tmp-") {
			t.Errorf("a failed replacement left temp debris beside the destination: %q", e.Name())
		}
	}

	// With the fault removed the same operation installs the complete new bytes.
	h.applier.fail = nil
	if err := h.apply(op); err != nil {
		t.Fatalf("Apply after clearing the fault: %v", err)
	}
	if got := h.readFile("data.json"); got != "NEWNEWNEW" {
		t.Errorf("destination = %q, want the new bytes", got)
	}
}

func TestInstalledCopyIsVerifiedBeforeItIsRenamedIntoPlace(t *testing.T) {
	h := setup(t)
	h.writeWorktreeFile("data.json", "OLD", 0o644)

	// Stage the right bytes, then corrupt the staged payload behind the applier's
	// back. The copy verification re-hashes the temp it just wrote, so the
	// corrupted bytes must never reach the destination.
	op := h.staged("data.json", h.regularNode("OLD", 0o644), h.regularNode("NEW", 0o644), "NEW")
	// The staging path is internal, so locate the payload by its opaque handle
	// rather than reconstructing the layout here.
	handle := op.Payload().Handle()
	found := ""
	err := filepath.Walk(h.layout.TmpDir(), func(p string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if !info.IsDir() && filepath.Base(p) == handle {
			found = p
		}
		return nil
	})
	if err != nil || found == "" {
		t.Fatalf("could not locate the staged payload (err=%v)", err)
	}
	if err := os.WriteFile(found, []byte("CORRUPTED"), 0o600); err != nil {
		t.Fatalf("corrupt staged payload: %v", err)
	}

	if err := h.apply(op); err == nil {
		t.Fatal("Apply installed a copy whose bytes do not match the desired content")
	}
	if got := h.readFile("data.json"); got != "OLD" {
		t.Errorf("destination = %q after a failed verification, want the old bytes intact", got)
	}
}

// --- lifecycle ------------------------------------------------------------

func TestDiscardRemovesStagingOnlyAndIsIdempotent(t *testing.T) {
	h := setup(t)
	h.stage("payload bytes")
	h.writeWorktreeFile("keep.txt", "user data", 0o644)
	foreign := filepath.Join(h.layout.TmpDir(), "not-ours.tmp")
	if err := os.WriteFile(foreign, []byte("keep me"), 0o600); err != nil {
		t.Fatalf("write foreign temp: %v", err)
	}

	if err := h.applier.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if err := h.applier.Discard(); err != nil {
		t.Errorf("Discard is not idempotent: %v", err)
	}
	if got := h.readFile("keep.txt"); got != "user data" {
		t.Errorf("Discard touched the worktree: %q", got)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("Discard removed a foreign temp artifact: %v", err)
	}
}

func TestNewRefusesAnIncompleteApplier(t *testing.T) {
	root := t.TempDir()
	layout := paths.New(root)
	if err := os.MkdirAll(layout.TmpDir(), paths.DirPerm); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	hasher := blake3hash.New()
	if _, err := New(layout, restore.OperationID{}, hasher); err == nil {
		t.Error("New accepted an applier with no operation id")
	}
	id, err := restore.NewOperationID(1, bytes.NewReader([]byte{1, 2, 3, 4, 5, 6, 7, 8}))
	if err != nil {
		t.Fatalf("operation id: %v", err)
	}
	if _, err := New(layout, id, nil); err == nil {
		t.Error("New accepted a nil hasher")
	}
}

func TestApplyRejectsAnUnbuiltOperation(t *testing.T) {
	h := setup(t)
	if err := h.apply(restore.StagedOperation{}); err == nil {
		t.Error("Apply accepted an unbuilt operation")
	}
}

// TestTheReportedEffectDistinguishesTheTwoFailureWindows is the oracle for what the
// applier tells its caller about the destination. Two failures, one operation shape,
// and opposite answers:
//
//   - a failure before the rename leaves the destination holding exactly what it held,
//     so the effect is none and the commit is an ordinary stopped one;
//   - a failure after the old node was removed leaves the destination gone, so the
//     effect is partial — the worktree changed while completing nothing, and only the
//     code that removed the node can say so.
//
// Collapsing the two would make the service report a conflict (an outcome whose
// contract says nothing was written) for a worktree it had already changed.
func TestTheReportedEffectDistinguishesTheTwoFailureWindows(t *testing.T) {
	injected := errors.New("injected failure")

	t.Run("before the rename the destination is untouched", func(t *testing.T) {
		h := setup(t)
		h.writeWorktreeFile("a.txt", "old", 0o644)
		h.applier.fail = func(stage failStage, _ worktree.RelPath) error {
			if stage == stageBeforeRename {
				return injected
			}
			return nil
		}
		op := h.staged("a.txt", h.regularNode("old", 0o644), h.regularNode("new", 0o644), "new")
		out := h.applyResult(op)
		if !errors.Is(out.Err(), injected) {
			t.Fatalf("Apply = %v, want the injected failure", out.Err())
		}
		if out.Effect() != restore.EffectNone || out.Done() {
			t.Errorf("effect = %s (done=%v), want an untouched destination", out.Effect(), out.Done())
		}
		if got := h.readFile("a.txt"); got != "old" {
			t.Errorf("destination = %q, want the old bytes", got)
		}
	})

	t.Run("after the removal the destination is gone", func(t *testing.T) {
		h := setup(t)
		// A type change over a directory: the only shape that must remove the old node
		// before it can install the new one.
		if err := os.MkdirAll(h.abs("thing"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		h.applier.fail = func(stage failStage, _ worktree.RelPath) error {
			if stage == stageAfterRemoval {
				return injected
			}
			return nil
		}
		op := h.staged("thing", restore.DirNode(), h.regularNode("now a file", 0o644), "now a file")
		out := h.applyResult(op)
		if !errors.Is(out.Err(), injected) {
			t.Fatalf("Apply = %v, want the injected failure", out.Err())
		}
		if out.Effect() != restore.EffectPartial || out.Done() {
			t.Errorf("effect = %s (done=%v), want an interrupted mutation", out.Effect(), out.Done())
		}
		if _, serr := os.Lstat(h.abs("thing")); !os.IsNotExist(serr) {
			t.Fatalf("the fixture did not actually remove the old node: %v", serr)
		}
	})
}

func TestApplyHonorsCancellation(t *testing.T) {
	h := setup(t)
	op := h.staged("x.txt", restore.AbsentNode(), h.regularNode("new", 0o644), "new")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := h.applier.Apply(ctx, op)
	if !errors.Is(out.Err(), context.Canceled) {
		t.Errorf("Apply under a cancelled context = %v, want context.Canceled", out.Err())
	}
	if out.Effect() != restore.EffectNone {
		t.Errorf("effect = %s, want none: a cancelled apply touched nothing", out.Effect())
	}
	if _, err := os.Lstat(h.abs("x.txt")); !os.IsNotExist(err) {
		t.Errorf("a cancelled apply still wrote: %v", err)
	}
}
