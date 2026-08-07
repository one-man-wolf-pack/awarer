// Package paths defines the on-disk layout of an awa project.
//
// It maps a project root to the set of paths under .awa/ that awa owns. It is
// pure: it computes paths from a root and performs no filesystem access, so the
// layout is one source of truth shared by init (which creates the directories)
// and every command that later reads them.
package paths

import (
	"os"
	"path/filepath"
	"strings"
)

// Dir is the project-local state directory name.
const Dir = ".awa"

// DirPerm is the mode awa creates its own directories with: owner-only
// (rwx------). awa-owned state under .awa/ holds local evidence — captured command
// output, manifests, content blobs, run metadata, indexes — that can be sensitive,
// so awa never makes it group- or world-accessible by default. The project root
// itself is the user's directory and is not created with this mode.
const DirPerm os.FileMode = 0o700

// FilePerm is the mode awa creates its own writable files with: owner-only
// (rw-------). It covers config, key pointers, indexes, locks, temp, and spill
// files — anything awa may rewrite in place.
const FilePerm os.FileMode = 0o600

// ReadOnlyFilePerm is the mode for awa-owned files that are immutable once
// published: content blobs and sealed run payloads/metadata. Owner read-only
// (r--------) keeps them private and marks them as never-rewritten-in-place. It is
// deliberately no broader than the source it may mirror, so a 0600 source file can
// never become world-readable through the store.
const ReadOnlyFilePerm os.FileMode = 0o400

// ConfigFileName is the local configuration file name inside Dir. It holds a
// project member's private overrides and is ignored with the rest of .awa/.
const ConfigFileName = "config.toml"

// ConfigRelPath is the local config file path relative to the project root. It
// is the optional private override file; root discovery keys on the Dir marker,
// not this path.
const ConfigRelPath = Dir + "/" + ConfigFileName

// SharedConfigFileName is the optional shared project configuration file at the
// project root. Unlike the local .awa/config.toml it lives outside Dir so it can
// be committed, letting a repository share scan/run policy with every contributor
// and agent while local overrides stay private.
const SharedConfigFileName = "awa.toml"

// GitignoreFileName is the name of the awa-owned guard file inside Dir that keeps
// .awa state from being offered to git. It is awa-owned local state, not a user
// configuration surface.
const GitignoreFileName = ".gitignore"

// Layout resolves the paths awa owns under a single project root.
type Layout struct {
	root string
}

// New returns a Layout rooted at the given project root.
func New(root string) Layout { return Layout{root: root} }

// Root returns the project root directory.
func (l Layout) Root() string { return l.root }

// AwaDir returns the .awa state directory.
func (l Layout) AwaDir() string { return filepath.Join(l.root, Dir) }

// ConfigFile returns the path to the local .awa/config.toml override.
func (l Layout) ConfigFile() string { return filepath.Join(l.root, Dir, ConfigFileName) }

// SharedConfigFile returns the path to the optional shared, committable
// awa.toml at the project root.
func (l Layout) SharedConfigFile() string { return filepath.Join(l.root, SharedConfigFileName) }

// StateGitignore returns the path to the awa-owned .awa/.gitignore guard, the
// file that keeps the state directory out of git by default.
func (l Layout) StateGitignore() string { return filepath.Join(l.root, Dir, GitignoreFileName) }

// StoreDir returns the content-addressed store directory.
func (l Layout) StoreDir() string { return filepath.Join(l.root, Dir, "store") }

// BlobsDir returns the blob storage directory.
func (l Layout) BlobsDir() string { return filepath.Join(l.StoreDir(), "blobs") }

// TmpDir returns the store's temp directory, used for atomic writes.
func (l Layout) TmpDir() string { return filepath.Join(l.StoreDir(), "tmp") }

// CheckpointsDir returns the checkpoint store directory.
func (l Layout) CheckpointsDir() string { return filepath.Join(l.root, Dir, "checkpoints") }

// RunsDir returns the run cache directory.
func (l Layout) RunsDir() string { return filepath.Join(l.root, Dir, "runs") }

// RestoresDir returns the restore recovery-observation directory: one immutable
// record per applied restore, holding the pre-restore state that restore is
// about to overwrite. It is created on demand by the first applied restore
// rather than by init, so an existing project does not start reporting a missing
// directory, and a project that never restores never grows one.
func (l Layout) RestoresDir() string { return filepath.Join(l.root, Dir, "restores") }

// IndexesDir returns the worktree index directory. The SQLite file itself is
// created lazily on the first scan that opens the index; init creates only the
// directory.
func (l Layout) IndexesDir() string { return filepath.Join(l.root, Dir, "indexes") }

// LocksDir returns the lock directory.
func (l Layout) LocksDir() string { return filepath.Join(l.root, Dir, "locks") }

// LogsDir returns the logs directory.
func (l Layout) LogsDir() string { return filepath.Join(l.root, Dir, "logs") }

// RequiredDirs returns the directories awa init must create, in a stable order.
// It does not include AwaDir itself, since creating any child creates it.
func (l Layout) RequiredDirs() []string {
	return []string{
		l.BlobsDir(),
		l.TmpDir(),
		l.CheckpointsDir(),
		l.RunsDir(),
		l.IndexesDir(),
		l.LocksDir(),
		l.LogsDir(),
	}
}

// evidenceDirNames are the awa-owned directories that hold recreatable local
// evidence and runtime state, relative to the project root and in a stable order.
// They are exactly what a reset removes.
//
// The set is deliberately expressed as the directories to REMOVE rather than as
// ".awa", because .awa also holds state a reset must not touch: the private
// config.toml layer and the awa-owned .gitignore guard. Telling a user to delete
// .awa is telling them to delete their configuration, which is why every recovery
// hint is built from this list instead of naming the parent.
var evidenceDirNames = []string{
	Dir + "/checkpoints",
	Dir + "/runs",
	Dir + "/restores",
	Dir + "/store",
	Dir + "/indexes",
	Dir + "/locks",
	Dir + "/logs",
}

// EvidenceDirNames returns the root-relative directories a reset of local evidence
// removes, in a stable order. The result is a copy.
func EvidenceDirNames() []string {
	return append([]string(nil), evidenceDirNames...)
}

// EvidenceDirs returns the absolute directories a reset of local evidence removes.
func (l Layout) EvidenceDirs() []string {
	out := make([]string, 0, len(evidenceDirNames))
	for _, rel := range evidenceDirNames {
		out = append(out, filepath.Join(l.root, filepath.FromSlash(rel)))
	}
	return out
}

// PreservedResetFiles returns the root-relative files a reset of local evidence must
// leave byte-for-byte intact: shared and private configuration, ignore policy, and
// the guard that keeps local evidence out of Git.
func PreservedResetFiles() []string {
	return []string{
		SharedConfigFileName,
		".awaignore",
		ConfigRelPath,
		Dir + "/" + GitignoreFileName,
	}
}

// ResetEvidenceHint is the one recovery sentence every surface uses when local
// evidence cannot be read. It is written to be followed literally, which is what
// decides its contents: the paths are project-relative, so it says where to stand;
// the locks directory is in the removal set, so it says to stop other awa processes
// first, since deleting a live collector's or writer's lock invites a half-recreated
// store; and it names the configuration that survives, so nobody reaches for the
// parent directory. It stays one bounded line — the full procedure lives in
// 'awa help install'.
//
// SharedConfigFileName is deliberately not mentioned: it sits outside Dir, so this
// instruction cannot reach it and saying so would only pad the line.
func ResetEvidenceHint() string {
	return "to start fresh: stop other awa processes, then from the project root delete " +
		strings.Join(EvidenceDirNames(), " ") +
		" and run 'awa init' (keeps " + ConfigRelPath + ")"
}
