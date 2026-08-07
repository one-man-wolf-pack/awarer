// Package docsexport publishes a validated documentation bundle into a
// destination directory.
//
// It accepts an already-built docbundle.Bundle — completeness and internal
// consistency are the bundle's own guarantee — and adds two things: refusing a
// destination that could destroy something the user cares about, and publishing so
// that what a caller is told matches what is on disk.
//
// # Destination contract
//
// The path must not exist. Not "must be empty", not "must be empty unless forced" —
// must not exist. An export therefore never replaces, merges into, or re-permissions
// anything, and the filesystem root, the working directory, an ancestor of it, and
// the home directory need no rules of their own: each exists, so each is refused.
//
// # Invariants
//
//  1. The bundle is built and validated in full before the destination is touched.
//  2. The destination is reserved by an atomic, exclusive mkdir, which on EEXIST has
//     not read or modified whatever holds the name.
//  3. The manifest is installed last, by an operation whose errors all happen before
//     it becomes visible. Its presence is the only thing that makes a directory an
//     authoritative export, so a failed export never leaves one.
//  4. Success is reported only after that install returns, so a Result names a
//     directory holding the whole bundle and nothing else is reported as one.
//
// How each is achieved lives with the code that achieves it; repeating the syscall
// sequence here would create a second specification to keep in step.
//
// # Failure
//
// This is a one-way procedure. An I/O error or a cancellation after the destination
// exists returns, leaves an incomplete directory holding some of the bundle and no
// manifest.json, and names that directory in the error along with the instruction to
// remove it before retrying. awa does not unwind, roll back, or delete anything.
//
// A killed process returns nothing, so what it leaves depends only on where it was
// killed: before the reservation, nothing at all; between the reservation and the
// manifest, the same incomplete directory; after the manifest install, a complete and
// entirely valid export, because that install is atomic and is the last thing the
// export does. There is no fourth state for a crash to produce.
//
// That is why the leftover is recognizable on its own rather than through a message.
// After a crash, look at the destination instead of reasoning about the interruption:
// manifest.json is what makes a directory an export, so a path without one is not
// valid and is removed before retrying, and a path with one is finished.
//
// That is the cheap premise this package is built on rather than a limitation of it.
// A partial export is replaceable generated output, its absence-of-manifest is
// already the marker that makes it non-authoritative, and a retry needs an absent
// path anyway — so recursive removal would buy the convenience of not typing rm -rf
// at the cost of an ownership ledger, destructive race reasoning, and per-platform
// deletion outcomes.
//
// # Threat model
//
// awa is a cooperative, single-user tool, and these guarantees are stated inside that
// boundary. Two ordinary awa runs racing for one destination are resolved by the
// atomic mkdir: one reserves it, the other is refused and changes nothing.
//
// An active malicious process manipulating the destination path mid-export is outside
// the contract, deliberately. Building the deliverable directly at a path someone else
// can name cannot be made robust against one on any operating system. It is named
// rather than papered over: checks there would buy narrower windows and a false
// impression of atomicity, and nothing destructive is conditioned on them anyway.
package docsexport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"awarer/internal/domain/docbundle"
	"awarer/internal/infra/fsx"
)

// ErrUnsafeDestination marks a destination this package refuses to write into. It
// is a property of the argument the user gave, not of the machine's state, so
// callers map it to a usage error rather than a runtime failure.
var ErrUnsafeDestination = errors.New("unsafe export destination")

// ErrIncompleteExport marks every failure that happened after the destination was
// created, and so left a directory the caller has to remove. It is carried alongside
// the original cause rather than replacing it, because both facts matter: what went
// wrong decides how the caller classifies the failure, and this decides whether there
// is anything on disk to act on.
//
// It is exported because the CLI has to separate an interruption that left a
// directory from one that created nothing, while still reporting the cause's own
// classification for both. A cancellation is normally reported tersely, and a terse
// message is the wrong answer for the one that left something behind.
var ErrIncompleteExport = errors.New("an incomplete export was left in place")

// publishBytes is the file-publishing primitive for document bodies. It is a
// variable solely so a test can inject a failure partway through an export: "a
// failure after the destination exists keeps its classification, names the residue,
// and leaves no manifest" is the guarantee that matters most here, and it is
// unprovable if the only reachable failures are the ones that happen before the
// first byte is written.
var publishBytes = fsx.PublishBytesAt

// Exported documentation is ordinary readable user content, not the owner-private
// evidence under .awa/, so it is published with the permissions of ordinary files
// the user creates rather than the 0600/0700 of the private store. The process
// umask applies to the directories, as it does for any tool that creates them.
const (
	docsDirPerm  os.FileMode = 0o755
	docsFilePerm os.FileMode = 0o644
)

// Result reports where a successful export landed. It deliberately carries only
// destination facts: what the bundle contains is the bundle's to answer, and the
// caller already holds it, so re-exporting those fields here would be a second
// copy to keep in step.
type Result struct {
	Output       string // absolute destination directory
	ManifestPath string // absolute path of the published manifest
}

// Publish writes the bundle into output. output must name a path that does not
// exist yet, inside an existing real directory; there is no force mode, because
// overwriting or filling in something a user named by mistake is not a
// recoverable error.
//
// A cancelled ctx aborts the export at the next file. Cancellation is not a special
// outcome here: before the destination exists it returns context.Canceled and creates
// nothing, and after it exists it is an ordinary post-reservation failure, which means
// the returned error still resolves as context.Canceled and additionally carries the
// residue and the action. What a caller does with those two facts is the caller's.
func Publish(ctx context.Context, bundle docbundle.Bundle, output string) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	dest, err := resolveDestination(output)
	if err != nil {
		return Result{}, err
	}

	// Rendered before the destination is reserved, so a manifest that cannot be
	// produced fails with nothing created.
	manifest, err := bundle.ManifestJSON()
	if err != nil {
		return Result{}, fmt.Errorf("rendering the manifest: %w", err)
	}

	// The mkdir is the transition: before it a failure has created nothing, after it
	// every failure leaves the directory behind.
	if err := reserve(dest); err != nil {
		return Result{}, err
	}
	// The handle is a write anchor and confers nothing else — nothing is deleted or
	// reclaimed here, so there is no ownership for it to establish. It keeps the
	// bundle's relative paths resolving under the directory this export created
	// rather than through a pathname re-resolved once per file.
	dir, err := fsx.OpenDirNoFollow(filepath.Dir(dest), filepath.Base(dest))
	if err != nil {
		return Result{}, withResidueAction(dest, fmt.Errorf("opening %s: %w", dest, err))
	}
	defer func() { _ = dir.Close() }()

	if err := fill(ctx, dir, bundle, manifest); err != nil {
		return Result{}, withResidueAction(dest, err)
	}
	return Result{
		Output:       dest,
		ManifestPath: filepath.Join(dest, docbundle.ManifestName),
	}, nil
}

// reserve claims the destination by creating it. The create IS the check: mkdir
// decides "free" and "taken" atomically, so unlike a stat-then-create pair it cannot
// be raced, and on EEXIST it has neither read nor modified whatever holds the name.
func reserve(dest string) error {
	err := os.Mkdir(dest, docsDirPerm)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, os.ErrExist):
		return fmt.Errorf("%w: %s already exists; there is no force mode, so name a directory that does not exist yet. "+
			"(A failed export leaves its directory in place with no manifest.json — that is not a valid export; inspect it and remove it yourself.)",
			ErrUnsafeDestination, dest)
	default:
		return fmt.Errorf("creating %s: %w", dest, err)
	}
}

// fill writes the bundle into the reserved directory, manifest last.
func fill(ctx context.Context, dir *os.File, bundle docbundle.Bundle, manifest []byte) error {
	for _, doc := range bundle.Documents() {
		if err := write(ctx, dir, doc.Path(), doc.Body()); err != nil {
			return err
		}
	}
	ref := bundle.MachineReference()
	if err := write(ctx, dir, ref.Path(), ref.Body()); err != nil {
		return err
	}
	return installManifest(ctx, dir, manifest)
}

// installManifest performs the one act that makes a directory an authoritative
// export, and it is the last thing this package does.
//
// Which primitive installs it matters more than the bytes. fsx.ReplaceFileAt writes
// and syncs a temp file, chmods it, closes it, and only then renames it into place,
// returning immediately afterwards — so every error it can report happens while the
// manifest is still invisible. fsx.PublishBytesAt, which the bodies use, fsyncs the
// directory AFTER the link that makes the file visible, and so can return an error
// with a valid manifest already on disk: the one state this package deliberately has
// no answer for, since answering it means taking the manifest back off again. A
// directory fsync is not owed to replaceable documentation output, and buying it here
// would cost exactly that withdrawal protocol.
func installManifest(ctx context.Context, dir *os.File, manifest []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := fsx.ReplaceFileAt(dir, docbundle.ManifestName, docsFilePerm, func(w io.Writer) error {
		_, werr := w.Write(manifest)
		return werr
	}, nil)
	if err != nil {
		return fmt.Errorf("writing %s: %w", docbundle.ManifestName, err)
	}
	return nil
}

// write publishes one bundle-relative file through the handle opened when the
// directory was created, so the bytes stay under the destination the caller named.
// Cancellation is observed here, which — with the check in installManifest — means
// every file including the manifest sits behind one.
func write(ctx context.Context, dir *os.File, rel docbundle.DocPath, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	into := dir
	if sub := rel.Dir(); sub != "" {
		// Created at the documentation permission rather than the private store
		// permission the publish helper would default to.
		handle, err := fsx.MkdirAllAt(dir, sub, docsDirPerm)
		if err != nil {
			return fmt.Errorf("creating %s: %w", sub, err)
		}
		defer func() { _ = handle.Close() }()
		into = handle
	}
	if err := publishBytes(into, rel.Base(), body, docsFilePerm); err != nil {
		return fmt.Errorf("writing %s: %w", rel, err)
	}
	return nil
}

// withResidueAction is the whole failure path after the destination exists. It adds
// one fact and one action to the cause without replacing it — the wrapped error still
// classifies, so an interruption is still an interruption to the caller that maps it,
// and it additionally answers to ErrIncompleteExport so a caller can ask about the
// residue without reading the prose.
//
// The claim is unconditional rather than probed: the manifest is installed by an
// operation that cannot fail once it is visible, so an export that failed at any point
// has no manifest at that path. Reading the directory back to find that out would be
// a second opinion about something this package already knows.
func withResidueAction(dest string, cause error) error {
	return fmt.Errorf("%w (%w at %s: it holds no manifest.json, "+
		"so it is not a valid export — remove it before retrying)", cause, ErrIncompleteExport, dest)
}

// resolveDestination turns the user's path into an absolute target and checks the
// two things that cannot be left to the reservation: that a path was given at all,
// and that the parent is a real directory the export can anchor its writes at.
// Whether the destination itself is free is deliberately NOT decided here — mkdir
// decides that, atomically, at the moment it matters.
func resolveDestination(output string) (string, error) {
	if strings.TrimSpace(output) == "" {
		return "", fmt.Errorf("%w: no destination given", ErrUnsafeDestination)
	}
	abs, err := filepath.Abs(output)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", output, err)
	}
	abs = filepath.Clean(abs)

	// A root has no parent to anchor at, and mkdir on one reports different errors
	// on different platforms; naming it precisely beats passing that through.
	if filepath.Dir(abs) == abs {
		return "", fmt.Errorf("%w: %s is a filesystem root", ErrUnsafeDestination, abs)
	}
	if err := requireRealParent(abs); err != nil {
		return "", err
	}
	return abs, nil
}

// requireRealParent enforces that the destination can be created directly: its
// parent must already exist as a real directory. Only the final directory is
// created, so a typo cannot silently materialize a chain of directories, and the
// writes have one unambiguous anchor.
func requireRealParent(abs string) error {
	parent := filepath.Dir(abs)
	info, err := os.Lstat(parent)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("%w: parent directory %s does not exist", ErrUnsafeDestination, parent)
	case err != nil:
		return fmt.Errorf("inspecting %s: %w", parent, err)
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%w: parent directory %s is a symlink", ErrUnsafeDestination, parent)
	case !info.IsDir():
		return fmt.Errorf("%w: parent %s is not a directory", ErrUnsafeDestination, parent)
	}
	return nil
}
