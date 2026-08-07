// Package changes implements the "awa changes" use case: resolve a range of
// states and report the change set between them.
//
// It is a thin application service over two collaborators it does not own: the
// state resolver (which knows repositories, the scanner, config, and cwd) and
// the pure comparison model. It loads no content — "changes" summarizes what
// differs; rendering a content diff is the separate diff use case.
package changes

import (
	"context"

	"awarer/internal/app/state"
	"awarer/internal/domain/compare"
	"awarer/internal/domain/worktree"
)

// Service runs the changes use case. Construct it with New.
type Service struct {
	resolver *state.Resolver
}

// New builds the service from the shared state resolver.
func New(resolver *state.Resolver) *Service {
	if resolver == nil {
		panic("changes.New: resolver must not be nil")
	}
	return &Service{resolver: resolver}
}

// Request is one changes invocation: the range to compare, the context needed to
// resolve "now", and the comparison options.
type Request struct {
	Range         state.Range
	Now           state.NowContext
	DetectRenames bool
	PathFilters   []worktree.RelPath
}

// Result is the resolved range plus the change set between the two states.
type Result struct {
	Left      *state.ResolvedState
	Right     *state.ResolvedState
	ChangeSet compare.ChangeSet
}

// Close releases the resolved states' resources (temp spill files behind a "now"
// scan). It is safe to call more than once.
func (r Result) Close() error {
	err := r.Left.Close()
	if e := r.Right.Close(); e != nil && err == nil {
		err = e
	}
	return err
}

// Run resolves both sides of the range and compares them by merging their
// manifest streams — no full manifest is materialized to compare. It performs no
// writes: a "now" side is an ephemeral scan that publishes no checkpoint. The output
// ChangeSet is collected because it is the materialized report contract JSON and
// small callers use; the scale win is the streamed comparison input. Human output
// uses Stream instead, so it never collects the whole change set.
func (s *Service) Run(ctx context.Context, req Request) (Result, error) {
	left, right, err := s.resolver.ResolveRange(ctx, req.Range, req.Now)
	if err != nil {
		return Result{}, err
	}
	// Own the resolved states until they are handed to the caller in the Result: close
	// them on any error path, disarm on the successful transfer.
	transferred := false
	defer func() {
		if !transferred {
			_ = left.Close()
			_ = right.Close()
		}
	}()

	lc, err := left.Manifest(ctx)
	if err != nil {
		return Result{}, err
	}
	rc, err := right.Manifest(ctx)
	if err != nil {
		_ = lc.Close()
		return Result{}, err
	}
	// CompareToChangeSet owns lc and rc and closes them.
	cs, err := compare.CompareToChangeSet(lc, rc, compare.Options{DetectRenames: req.DetectRenames, PathFilters: req.PathFilters})
	if err != nil {
		return Result{}, err
	}
	transferred = true
	return Result{Left: left, Right: right, ChangeSet: cs}, nil
}

// StreamResult is the resolved range plus a live change cursor. It is the
// streaming counterpart of Result: the caller pulls change events one at a time
// (accumulating only bounded stats and the output it prints) rather than holding
// the whole change set, so a large drift never materializes in memory. The caller
// owns Changes and must Close it.
type StreamResult struct {
	Left    *state.ResolvedState
	Right   *state.ResolvedState
	Changes compare.ChangeCursor
	// RenameDetection records whether rename pairing was requested and whether the
	// bounded presentation policy applied it, so the renderer can note when renames were
	// not detected (change set over the buffer limit). It is fixed once Stream returns.
	RenameDetection compare.RenameDetection
}

// Close releases everything the StreamResult owns in one call: first the change cursor
// (which closes the underlying manifest cursors and their file descriptors), then the
// resolved states (which remove any temp spill files behind a "now" scan). Doing both
// here — rather than making the caller close the cursor and the states separately —
// means a caller cannot release the cursor yet leak the spill directory. Safe to call
// more than once; every step's Close is idempotent.
func (r StreamResult) Close() error {
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

// Stream resolves both sides of the range and returns a change cursor over their
// merged manifest streams. Unlike Run it does not collect the changes: the caller
// drives the cursor, so the human renderer prints each change as it is pulled and
// keeps only bounded summary counts. Ownership of the underlying manifest cursors
// passes to the returned cursor; the caller's Close releases them.
func (s *Service) Stream(ctx context.Context, req Request) (StreamResult, error) {
	left, right, err := s.resolver.ResolveRange(ctx, req.Range, req.Now)
	if err != nil {
		return StreamResult{}, err
	}
	// Own the resolved states until they are handed to the caller in the StreamResult:
	// close them on any error path, disarm on the successful transfer.
	transferred := false
	defer func() {
		if !transferred {
			_ = left.Close()
			_ = right.Close()
		}
	}()

	lc, err := left.Manifest(ctx)
	if err != nil {
		return StreamResult{}, err
	}
	rc, err := right.Manifest(ctx)
	if err != nil {
		_ = lc.Close()
		return StreamResult{}, err
	}
	cur, err := compare.CompareStream(lc, rc, compare.Options{DetectRenames: req.DetectRenames, PathFilters: req.PathFilters})
	if err != nil {
		// CompareStream's rename path drains and closes the inputs on error; for the
		// base path the only error is a nil cursor, which cannot happen here. Close both
		// defensively — the underlying cursors' Close is idempotent — so a stray error
		// never leaks a manifest file descriptor.
		_ = lc.Close()
		_ = rc.Close()
		return StreamResult{}, err
	}
	transferred = true
	return StreamResult{Left: left, Right: right, Changes: cur, RenameDetection: compare.RenameDetectionOf(cur)}, nil
}
