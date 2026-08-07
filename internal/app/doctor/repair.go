package doctor

import (
	"errors"
	"os"
	"path/filepath"

	"awarer/internal/domain/doctor"
	"awarer/internal/domain/paths"
	"awarer/internal/infra/fsx"
	"awarer/internal/infra/projfs"
	"awarer/internal/infra/sqliteindex"
)

// repair derives a repair plan from the repairable findings and executes each safe
// action, marking a successfully repaired finding repaired (which clears it from the
// exit-code and health calculations). When an action cannot be planned safely or its
// execution fails, the original finding is left unrepaired AND an explicit
// repair-failed error finding is recorded, so the user sees that doctor tried and
// why it could not finish — never a silent no-op that still shows [repairable].
//
// The outcome is carried entirely by the findings it mutates: a repaired finding is
// the record that its action ran, so nothing else has to be threaded out.
func (s *Service) repair(layout paths.Layout, rep *report) {
	awaDir := layout.AwaDir()

	// Capture the length first: repair-failed findings are appended below, and they
	// must not themselves be considered repair candidates.
	n := len(rep.findings)
	for i := 0; i < n; i++ {
		f := rep.findings[i]
		if !f.Repairable() || f.Repaired() {
			continue
		}
		kind, ok := repairKindFor(f.Code())
		if !ok {
			continue
		}
		action, err := doctor.NewRepairAction(kind, awaDir, f.Path(), f.Code())
		if err != nil {
			rep.add(repairFailed(f, kind, "cannot plan a safe repair: "+err.Error()))
			continue
		}
		if err := s.execute(layout, action); err != nil {
			rep.add(repairFailed(f, kind, "repair action failed: "+err.Error()))
			continue
		}
		repaired, err := f.MarkRepaired()
		if err != nil {
			rep.add(repairFailed(f, kind, "could not record repair: "+err.Error()))
			continue
		}
		rep.findings[i] = repaired
	}
}

// repairFailed builds the error finding recorded when a repair could not be
// completed. It is non-repairable (re-running --repair would hit the same failure)
// and carries the original finding's location plus the action and reason, so the
// failure is actionable rather than an unexplained lingering [repairable].
func repairFailed(orig doctor.Finding, kind doctor.RepairKind, reason string) doctor.Finding {
	return finding(doctor.CodeRepairFailed, doctor.SeverityError, orig.Subsystem(),
		orig.Path(), orig.Subject(),
		"repair ("+kind.String()+") for "+orig.Code().String()+" failed: "+reason, false)
}

// repairKindFor maps a repairable finding code to the safe action that resolves it.
// Codes without a safe mechanical repair return ok=false and are never acted on.
func repairKindFor(code doctor.FindingCode) (doctor.RepairKind, bool) {
	switch code {
	case doctor.CodeOrphanTemp:
		return doctor.RepairRemoveTemp, true
	case doctor.CodeLockStale:
		return doctor.RepairRemoveLock, true
	case doctor.CodeIndexUnreadable, doctor.CodeIndexStale:
		return doctor.RepairRebuildIndex, true
	case doctor.CodeStateGitignoreMissing, doctor.CodeStateGitignoreIneffective:
		return doctor.RepairWriteStateGitignore, true
	default:
		return 0, false
	}
}

// execute performs one repair action. Most repairs are removals, run as
// descriptor-anchored, no-follow deletions rooted at the project root so a repair
// can never follow a symlink out of the project or be redirected by a swapped
// ancestor; removal is idempotent, so re-running --repair is safe. The one
// exception is RepairWriteStateGitignore, which writes the awa-owned guard rather
// than removing anything. Every action is idempotent on a healthy project.
func (s *Service) execute(layout paths.Layout, action doctor.RepairAction) error {
	root := layout.Root()
	switch action.Kind() {
	case doctor.RepairWriteStateGitignore:
		// The only repair that creates rather than removes state: restore the
		// awa-owned .awa/.gitignore guard. EnsureStateGitignore is idempotent, so a
		// re-run finds the guard healthy and writes nothing.
		_, err := projfs.EnsureStateGitignore(layout)
		return err
	case doctor.RepairRebuildIndex:
		// The index is a file group, not a single file: removing only the database
		// would leave its -wal/-shm sidecars behind to confuse the next open. Remove
		// the whole owned set so the next scan rebuilds a clean index.
		return removeIndexFiles(root, action.Target())
	default:
		return removeUnderRoot(root, filepath.Dir(action.Target()), filepath.Base(action.Target()))
	}
}

// removeIndexFiles removes every file the worktree index owns in the target's
// directory (the database plus its WAL sidecars), each via no-follow deletion.
func removeIndexFiles(root, target string) error {
	dirAbs := filepath.Dir(target)
	relDir, err := fsx.RelUnder(root, dirAbs)
	if err != nil {
		return err
	}
	dir, err := fsx.OpenDirNoFollow(root, relDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // the indexes dir is gone: nothing to remove
		}
		return err
	}
	defer func() { _ = dir.Close() }()
	for _, name := range sqliteindex.OwnedFiles() {
		if err := fsx.RemoveTreeAt(dir, name); err != nil {
			return err
		}
	}
	return nil
}

// removeUnderRoot removes name within dirAbs via a no-follow directory descriptor
// anchored at root, treating an absent parent or target as success.
func removeUnderRoot(root, dirAbs, name string) error {
	relParent, err := fsx.RelUnder(root, dirAbs)
	if err != nil {
		return err
	}
	dir, err := fsx.OpenDirNoFollow(root, relParent)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // the parent is already gone: nothing to remove
		}
		return err
	}
	defer func() { _ = dir.Close() }()
	// RemoveTreeAt unlinks a file or symlink and recursively removes a directory,
	// never following a symlink, and treats an absent name as success.
	return fsx.RemoveTreeAt(dir, name)
}
