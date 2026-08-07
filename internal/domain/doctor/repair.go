package doctor

import (
	"fmt"
	"path/filepath"
	"strings"
)

// RepairKind names the closed set of safe, mechanical repairs doctor performs.
// Anything not in this set is not a repair doctor will attempt.
type RepairKind int

const (
	// RepairRemoveTemp removes an orphan temp file or directory under a known
	// .awa temp location (store/tmp or runs/tmp).
	RepairRemoveTemp RepairKind = iota + 1
	// RepairRemoveLock removes a recognized, provably-stale lock record under
	// .awa/locks.
	RepairRemoveLock
	// RepairRebuildIndex invalidates the derived SQLite worktree index so the next
	// scan recreates it. The index is acceleration-only state, so dropping it
	// loses nothing durable.
	RepairRebuildIndex
	// RepairWriteStateGitignore writes the awa-owned .awa/.gitignore guard so the
	// state directory is ignored by git. Unlike the other kinds it creates state
	// rather than removing it; the guard is awa-owned local state, never user data.
	RepairWriteStateGitignore
)

func (k RepairKind) String() string {
	switch k {
	case RepairRemoveTemp:
		return "remove-temp"
	case RepairRemoveLock:
		return "remove-lock"
	case RepairRebuildIndex:
		return "rebuild-index"
	case RepairWriteStateGitignore:
		return "write-state-gitignore"
	default:
		return "unknown"
	}
}

// Valid reports whether k is a known repair kind.
func (k RepairKind) Valid() bool {
	return k == RepairRemoveTemp || k == RepairRemoveLock || k == RepairRebuildIndex ||
		k == RepairWriteStateGitignore
}

// RepairAction is one safe, idempotent mutation with a typed target. Its target
// path is proven to lie inside the project's .awa directory at construction, so a
// repair can never be aimed at an arbitrary path.
type RepairAction struct {
	kind   RepairKind
	target string
}

// NewRepairAction builds a repair action whose target is contained within awaDir.
// It fails if the kind is unknown, the justifying finding code is unknown, or the
// target is not a path strictly inside awaDir — the single lexical guard that keeps
// repair from escaping the project's own storage. The descriptor-anchored, no-follow
// deletion at execution time is the second, stronger guard; this one keeps an
// impossible action from ever being planned.
//
// The code is validated but not retained: a repaired finding is what records that its
// action ran, so the action itself carries only what execution acts on.
func NewRepairAction(kind RepairKind, awaDir, target string, code FindingCode) (RepairAction, error) {
	if !kind.Valid() {
		return RepairAction{}, fmt.Errorf("invalid repair kind %d", kind)
	}
	if !code.Valid() {
		return RepairAction{}, fmt.Errorf("invalid finding code %q for repair", code)
	}
	if strings.TrimSpace(awaDir) == "" || strings.TrimSpace(target) == "" {
		return RepairAction{}, fmt.Errorf("repair action requires an awa dir and target")
	}
	if !underDir(awaDir, target) {
		return RepairAction{}, fmt.Errorf("repair target %q is not under %q", target, awaDir)
	}
	return RepairAction{kind: kind, target: filepath.Clean(target)}, nil
}

func (a RepairAction) Kind() RepairKind { return a.kind }
func (a RepairAction) Target() string   { return a.target }

// underDir reports whether target is the same as, or lexically nested inside, dir.
// It is a pure containment check on cleaned paths; it does not touch the
// filesystem and so does not resolve symlinks — that is the deletion helper's job.
func underDir(dir, target string) bool {
	d := filepath.Clean(dir)
	t := filepath.Clean(target)
	if t == d {
		return false // the .awa dir itself is never a repair target
	}
	rel, err := filepath.Rel(d, t)
	if err != nil {
		return false
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}
