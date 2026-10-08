package checkpointcmd

import (
	"awarer/internal/domain/config"
	"awarer/internal/domain/worktree"
)

// PatternCompiler compiles an ordered pattern list into a path selector. The
// composition root supplies the worktree filesystem's compiler, so the checkpoint
// policy reuses the existing ignore-pattern language rather than owning one.
type PatternCompiler func(patterns []string) worktree.PathSelector

// Retention is one checkpoint operation's byte-retention policy: the global
// store_file_contents switch plus the compiled hash_only_patterns selection. It is
// compiled once per operation and is immutable, so every manifest reopen or id
// collision retry applies exactly the same decisions.
type Retention struct {
	store    bool
	hashOnly worktree.PathSelector
}

// NewRetention compiles the effective checkpoint policy. The pattern list arrives
// already resolved by the configuration layers — unspecified lists hold the
// product default — so nothing here depends on where the list came from.
func NewRetention(cfg config.Checkpoint, compile PatternCompiler) Retention {
	return Retention{store: cfg.StoreFileContents, hashOnly: compile(cfg.HashOnlyPatterns)}
}

// Storage is the persisted storage class a checkpoint gives a regular file at path
// whose scan-time storage intent is intent. Only a blob intent can change: it
// becomes hash-only when contents are not stored at all or the path is selected for
// hash-only retention. A hash-only or skipped large-file outcome and every non-blob
// intent pass through, so a selection exception can never re-enable bytes another
// policy declined.
func (r Retention) Storage(path worktree.RelPath, intent worktree.ContentStorageIntent) worktree.ContentStorageIntent {
	if intent != worktree.StorageBlob {
		return intent
	}
	if !r.store || r.hashOnly.Selects(path) {
		return worktree.StorageHashOnly
	}
	return worktree.StorageBlob
}
