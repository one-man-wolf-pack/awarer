// Package diff implements the "awa diff" use case: the change set of "awa
// changes" plus a content diff for each change where content is available.
//
// Content loading is an explicit step over the resolved states' content access:
// blob bytes for a checkpoint, the scanner's verified opener for "now". A missing
// blob a diff needs is storage corruption and fails the whole diff loudly; a
// current-workspace file that changed under the scan degrades that one file to
// "unavailable" rather than emitting misleading bytes.
package diff

import (
	"context"
	"errors"
	"fmt"

	"awarer/internal/app/state"
	"awarer/internal/domain/compare"
	"awarer/internal/domain/config"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/difftext"
)

// Availability classifies whether and how a change's content can be shown.
type Availability int

const (
	// Text: a unified text diff is available in FileDiff.Text.
	Text Availability = iota
	// Binary: content differs but is binary; no text diff.
	Binary
	// HashOnly: content changed but is not stored, so no content diff.
	HashOnly
	// Skipped: a skipped/unavailable input; no content diff.
	Skipped
	// TypeChanged: the node changed kind; no content diff.
	TypeChanged
	// Metadata: a symlink/dir/rename change shown by metadata only.
	Metadata
	// Unavailable: content could not be read reliably (e.g. changed under scan).
	Unavailable
)

func (a Availability) String() string {
	switch a {
	case Text:
		return "text"
	case Binary:
		return "binary"
	case HashOnly:
		return "hash-only"
	case Skipped:
		return "skipped"
	case TypeChanged:
		return "type-changed"
	case Metadata:
		return "metadata"
	case Unavailable:
		return "unavailable"
	default:
		return "unknown"
	}
}

// FileDiff is a change paired with its rendered content diff (or the reason
// there is none).
type FileDiff struct {
	Change       compare.Change
	Availability Availability
	// Text holds the unified diff when Availability is Text.
	Text string
	// Reason explains a non-text availability for human and JSON output.
	Reason string
}

// Service runs the diff use case. Construct it with New.
type Service struct {
	resolver *state.Resolver
}

// New builds the service from the shared state resolver.
func New(resolver *state.Resolver) *Service {
	if resolver == nil {
		panic("diff.New: resolver must not be nil")
	}
	return &Service{resolver: resolver}
}

// Request is one diff invocation. Algorithm selects the text diff engine; the
// caller resolves it from CLI/config/default before constructing the request, so
// exactly one effective algorithm reaches the engine.
type Request struct {
	Range         state.Range
	Now           state.NowContext
	DetectRenames bool
	PathFilters   []worktree.RelPath
	Context       int
	Algorithm     config.DiffAlgorithm
}

// StreamResult is the resolved range plus a live per-file diff cursor: the caller
// pulls one FileDiff at a time, so only a single file's bounded content diff is held
// in memory rather than the whole change set. It is the sole diff execution surface —
// there is no materializing counterpart that renders every file's diff at once. The
// caller owns Files and must Close it.
type StreamResult struct {
	Left  *state.ResolvedState
	Right *state.ResolvedState
	Files *FileDiffCursor
	// RenameDetection records whether rename pairing was requested and whether the
	// bounded presentation policy applied it, so the renderer can note when renames were
	// not detected (change set over the buffer limit). It is fixed once Stream returns.
	RenameDetection compare.RenameDetection
}

// Close releases everything the StreamResult owns in one call: first the per-file diff
// cursor (which closes the underlying change and manifest cursors and their file
// descriptors), then the resolved states (which remove any temp spill files behind a
// "now" scan). Doing both here — rather than making the caller close the cursor and the
// states separately — means a caller cannot release the cursor yet leak the spill
// directory. Safe to call more than once; every step's Close is idempotent.
func (r StreamResult) Close() error {
	var err error
	if r.Files != nil {
		err = r.Files.Close()
	}
	if e := r.Left.Close(); e != nil && err == nil {
		err = e
	}
	if e := r.Right.Close(); e != nil && err == nil {
		err = e
	}
	return err
}

// FileDiffCursor pulls one change from the underlying change stream and renders its
// content diff on demand, so a large diff never materializes every file at once.
// Each rendered FileDiff holds at most one file's diff, itself bounded by the text
// engine's per-file size cap. Its contract mirrors the manifest and change cursors:
// Next advances and reports availability, FileDiff is valid only after Next returned
// true, Err is sticky and tells a clean end of stream from a failure (including a
// corrupt-store error surfaced mid-stream), and Close releases the underlying
// change cursor and is safe to call more than once.
type FileDiffCursor struct {
	svc          *Service
	changes      compare.ChangeCursor
	left, right  *state.ResolvedState
	contextLines int
	algorithm    config.DiffAlgorithm

	cur FileDiff
	err error
}

// Next advances to the next change and renders its file diff. A corrupt-store error
// (a missing/corrupt blob a content diff needs) is sticky and stops iteration,
// so the caller sees it through Err rather than a silent short stream.
func (c *FileDiffCursor) Next() bool {
	if c.err != nil {
		return false
	}
	if !c.changes.Next() {
		c.err = c.changes.Err()
		return false
	}
	fd, err := c.svc.fileDiff(c.left, c.right, c.changes.Change(), c.contextLines, c.algorithm)
	if err != nil {
		c.err = err
		return false
	}
	c.cur = fd
	return true
}

// FileDiff returns the diff rendered by the last successful Next.
func (c *FileDiffCursor) FileDiff() FileDiff { return c.cur }

// Err returns the first error encountered (comparison or storage corruption).
func (c *FileDiffCursor) Err() error { return c.err }

// Close releases the underlying change cursor (and its manifest cursors).
func (c *FileDiffCursor) Close() error { return c.changes.Close() }

// ChangeStreamResult is the resolved range plus a change-only cursor: the changes
// without any file content diff. It backs the --stat summary path, which needs the
// change set's counts but not its content, so it must not pay for reading blobs or
// running the text diff engine. The caller owns Changes and must Close it.
type ChangeStreamResult struct {
	Left            *state.ResolvedState
	Right           *state.ResolvedState
	Changes         compare.ChangeCursor
	RenameDetection compare.RenameDetection
}

// Close releases the change cursor (and its manifest cursors) then the resolved states,
// in one call. Safe to call more than once; every step's Close is idempotent.
func (r ChangeStreamResult) Close() error {
	var err error
	if r.Changes != nil {
		err = r.Changes.Close()
	}
	if e := r.Left.Close(); e != nil && err == nil {
		err = e
	}
	if e := r.Right.Close(); e != nil && err == nil {
		err = e
	}
	return err
}

// openCompare is the shared front half of Stream and StreamChanges: it resolves the
// range, opens both manifests, and starts the merged change stream. On any error the
// resolved states and manifest cursors are closed; on success ownership of the change
// cursor (and thus the manifest cursors) and the resolved states passes to the caller.
func (s *Service) openCompare(ctx context.Context, req Request) (*state.ResolvedState, *state.ResolvedState, compare.ChangeCursor, error) {
	left, right, err := s.resolver.ResolveRange(ctx, req.Range, req.Now)
	if err != nil {
		return nil, nil, nil, err
	}
	// Own the resolved states until ownership passes to the caller: close them on any
	// error path, disarm on the successful transfer.
	transferred := false
	defer func() {
		if !transferred {
			_ = left.Close()
			_ = right.Close()
		}
	}()

	lc, err := left.Manifest(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	rc, err := right.Manifest(ctx)
	if err != nil {
		_ = lc.Close()
		return nil, nil, nil, err
	}
	cur, err := compare.CompareStream(lc, rc, compare.Options{DetectRenames: req.DetectRenames, PathFilters: req.PathFilters})
	if err != nil {
		_ = lc.Close()
		_ = rc.Close()
		return nil, nil, nil, err
	}
	transferred = true
	return left, right, cur, nil
}

// StreamChanges resolves the range and returns a change-only cursor — the change set
// without any content diff — for the --stat summary path. It renders no file content, so
// it never reads a blob or runs the text diff engine just to count changes.
func (s *Service) StreamChanges(ctx context.Context, req Request) (ChangeStreamResult, error) {
	left, right, cur, err := s.openCompare(ctx, req)
	if err != nil {
		return ChangeStreamResult{}, err
	}
	return ChangeStreamResult{Left: left, Right: right, Changes: cur, RenameDetection: compare.RenameDetectionOf(cur)}, nil
}

// Stream resolves the range and returns a per-file diff cursor over the merged
// manifest streams. Unlike Run it does not render every file up front: each file's
// content diff is produced only as the caller pulls it, so peak memory is one file's
// bounded diff rather than the whole Files slice. Ownership of the manifest cursors
// passes to the returned cursor; the caller's Close releases them.
func (s *Service) Stream(ctx context.Context, req Request) (StreamResult, error) {
	if !req.Algorithm.Valid() {
		return StreamResult{}, fmt.Errorf("unknown diff algorithm %q", req.Algorithm)
	}
	left, right, cur, err := s.openCompare(ctx, req)
	if err != nil {
		return StreamResult{}, err
	}
	return StreamResult{
		Left:  left,
		Right: right,
		Files: &FileDiffCursor{
			svc:          s,
			changes:      cur,
			left:         left,
			right:        right,
			contextLines: req.Context,
			algorithm:    req.Algorithm,
		},
		RenameDetection: compare.RenameDetectionOf(cur),
	}, nil
}

// fileDiff renders one change. It returns an error only for storage corruption
// (a missing/corrupt blob a content diff needs); every other "no diff" case is
// expressed as an Availability, not an error.
func (s *Service) fileDiff(left, right *state.ResolvedState, c compare.Change, contextLines int, algorithm config.DiffAlgorithm) (FileDiff, error) {
	fd := FileDiff{Change: c}

	switch c.Status {
	case compare.Skipped:
		fd.Availability = Skipped
		fd.Reason = c.Note
		return fd, nil
	case compare.TypeChanged:
		fd.Availability = TypeChanged
		fd.Reason = fmt.Sprintf("type changed: %s -> %s", c.OldKind, c.NewKind)
		return fd, nil
	case compare.Renamed:
		fd.Availability = Metadata
		fd.Reason = "renamed (content unchanged)"
		return fd, nil
	}

	// Non-regular changes (symlink, dir) carry no content diff.
	kind := c.NewKind
	if c.Status == compare.Deleted {
		kind = c.OldKind
	}
	if kind != worktree.KindRegular {
		fd.Availability = Metadata
		if kind == worktree.KindSymlink {
			fd.Reason = fmt.Sprintf("symlink target: %s -> %s", c.OldSymlink, c.NewSymlink)
		} else {
			fd.Reason = "no content diff"
		}
		return fd, nil
	}

	// A regular modify with an unchanged content hash is a metadata-only change
	// (mode or traversal): there are no differing bytes to diff, so report what
	// changed rather than rendering an empty body.
	if c.OldContent == c.NewContent {
		fd.Availability = Metadata
		fd.Reason = metadataReason(c)
		return fd, nil
	}

	// A recorded run's observation stores only a manifest, no file content, so a
	// content diff against it is not available: report the change at hash-only
	// granularity rather than aborting. This degrade is typed (ContentAvailable),
	// not a string sniff, and is the reason a run side never reaches the byte reads
	// below.
	if !left.ContentAvailable() || !right.ContentAvailable() {
		fd.Availability = HashOnly
		fd.Reason = "run observation stores no content"
		return fd, nil
	}

	if !c.ContentDiffAvailable {
		fd.Availability = HashOnly
		fd.Reason = "hash-only content unavailable"
		return fd, nil
	}

	var oldBytes, newBytes []byte
	if c.Status != compare.Added {
		b, err := left.Content(c.OldPath, c.OldContent)
		if err != nil {
			if errors.Is(err, state.ErrContentChanged) {
				fd.Availability = Unavailable
				fd.Reason = "old content changed during read"
				return fd, nil
			}
			return FileDiff{}, err
		}
		oldBytes = b
	}
	if c.Status != compare.Deleted {
		b, err := right.Content(c.NewPath, c.NewContent)
		if err != nil {
			if errors.Is(err, state.ErrContentChanged) {
				fd.Availability = Unavailable
				fd.Reason = "new content changed during read"
				return fd, nil
			}
			return FileDiff{}, err
		}
		newBytes = b
	}

	if difftext.IsBinary(oldBytes) || difftext.IsBinary(newBytes) {
		fd.Availability = Binary
		fd.Reason = "binary content changed"
		return fd, nil
	}

	oldName := label(c.OldPath, c.Status == compare.Added)
	newName := label(c.NewPath, c.Status == compare.Deleted)
	text, err := renderText(algorithm, oldName, newName, oldBytes, newBytes, contextLines)
	if err != nil {
		if errors.Is(err, difftext.ErrTooLarge) {
			fd.Availability = Unavailable
			fd.Reason = "diff too large"
			return fd, nil
		}
		return FileDiff{}, err
	}
	fd.Availability = Text
	fd.Text = text
	return fd, nil
}

// renderText dispatches to the selected text diff engine. This is the single
// selection point between the two engines: the rest of fileDiff — the binary,
// hash-only, size, and content-availability gates — is identical regardless of
// algorithm. An unknown algorithm fails loudly rather than silently rendering
// Myers: the chosen engine is part of the diff's evidence, so a wiring error
// must not be masked by a default.
func renderText(algorithm config.DiffAlgorithm, oldName, newName string, old, new []byte, contextLines int) (string, error) {
	switch algorithm {
	case config.DiffMyers:
		return difftext.Unified(oldName, newName, old, new, contextLines)
	case config.DiffHistogram:
		return difftext.UnifiedHistogram(oldName, newName, old, new, contextLines)
	default:
		return "", fmt.Errorf("unknown diff algorithm %q", algorithm)
	}
}

// metadataReason describes a metadata-only change: a mode change (carried
// structurally) takes precedence, then any traversal note the comparison
// recorded, falling back to a generic phrase.
func metadataReason(c compare.Change) string {
	if c.ModeChanged() {
		return fmt.Sprintf("mode changed %04o -> %04o", c.OldMode, c.NewMode)
	}
	if c.Note != "" {
		return c.Note
	}
	return "metadata changed"
}

// label names a diff side, using /dev/null for an absent side.
func label(p worktree.RelPath, absent bool) string {
	if absent || p.IsZero() {
		return "/dev/null"
	}
	return p.String()
}
