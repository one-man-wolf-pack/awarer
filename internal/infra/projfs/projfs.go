// Package projfs is the filesystem boundary for project state.
//
// It walks the directory tree to discover a project root, creates the .awa
// layout idempotently, and writes the config file atomically. Everything here
// takes explicit paths — there is no package-level current-directory state — so
// the behavior is deterministic and testable against real temp directories.
package projfs

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"awarer/internal/domain/paths"
)

// ErrRootNotFound reports that no enclosing directory contained a .awa state
// directory. Callers translate it into the "not a project" exit code. It is
// returned only when the directories were searched successfully and none owned a
// .awa directory; an I/O or permission failure surfaces as its own error so it
// is never misreported as a missing project.
var ErrRootNotFound = errors.New("no .awa directory found in this or any parent directory")

// ErrUnresolvedProject reports that a zero-value Project was used. The zero
// value is unavoidable in Go, so consumers guard against it with IsResolved
// and surface this rather than silently operating on a relative ".awa" path.
var ErrUnresolvedProject = errors.New("project is not resolved; construct it with Open or Find")

// Project is a resolved awa project: an absolute root directory proven to own a
// .awa state directory. It is constructed only by Open or Find, both of which
// verify that the marker exists, so any code holding a resolved Project may rely
// on the project existing without re-checking. This turns the "a root owns its
// .awa directory" invariant into a type guarantee rather than a convention
// enforced by callers. The config file under .awa is optional and not part of
// this marker. The Go zero value is necessarily constructible; it reports
// IsResolved() == false so misuse fails loudly instead of reading a relative path.
type Project struct {
	layout   paths.Layout
	resolved bool
}

// Open returns the Project rooted at root, verifying that root owns a .awa
// state directory. root is normalized to an absolute path so the resolved
// project never depends on the caller's working directory. It returns
// ErrRootNotFound when the directory is not an initialized project, or the
// underlying stat error when ownership cannot be determined (for example a
// permission or I/O failure, or a .awa that exists but is not a directory).
func Open(root string) (Project, error) {
	if root == "" {
		// filepath.Abs("") resolves to the current directory; reject empty
		// input so an unset root never silently becomes "the current project".
		return Project{}, errors.New("project root must not be empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return Project{}, fmt.Errorf("resolving project root: %w", err)
	}
	l := paths.New(abs)
	ok, err := markerExists(l.AwaDir())
	if err != nil {
		return Project{}, err
	}
	if !ok {
		return Project{}, ErrRootNotFound
	}
	return Project{layout: l, resolved: true}, nil
}

// Find discovers the project root by walking upward from startDir and returns
// the resolved Project.
func Find(startDir string) (Project, error) {
	root, err := FindRoot(startDir)
	if err != nil {
		return Project{}, err
	}
	return Project{layout: paths.New(root), resolved: true}, nil
}

// IsResolved reports whether the Project was produced by Open or Find. A zero
// Project is not resolved.
func (p Project) IsResolved() bool { return p.resolved }

// Paths returns the project's path layout. It is the only way to reach a
// Project's paths, and it fails with ErrUnresolvedProject for a zero
// (unconstructed) Project — so the type itself, not caller discipline, prevents
// operating on relative ".awa" paths. A resolved Project always succeeds.
func (p Project) Paths() (paths.Layout, error) {
	if !p.resolved {
		return paths.Layout{}, ErrUnresolvedProject
	}
	return p.layout, nil
}

// FindRoot walks upward from startDir until it finds a directory containing a
// real .awa state directory, and returns that directory. startDir must be
// absolute (or resolvable to absolute); the search stops at the filesystem root.
// A path named .awa that is not a real directory — a file or a symlink — does not
// mark a root: the search treats it like an absent marker and keeps walking, so
// neither an unrelated file nor a symlinked state directory can hijack
// discovery. It returns ErrRootNotFound when no project root exists above
// startDir, or the underlying stat error if a directory cannot be examined.
func FindRoot(startDir string) (string, error) {
	if startDir == "" {
		// As in Open, refuse the empty path rather than let filepath.Abs turn
		// it into the current directory.
		return "", errors.New("start directory must not be empty")
	}
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", fmt.Errorf("resolving start directory: %w", err)
	}
	for {
		awaDir := filepath.Join(dir, paths.Dir)
		ok, err := markerExists(awaDir)
		if err != nil {
			// A .awa that exists but is not a real directory — a stray file or a
			// symlink — is not a project root; keep walking rather than treating it
			// as a fatal error or, worse, adopting a symlinked state directory.
			if errors.Is(err, ErrNotDirectory) {
				ok = false
			} else {
				return "", fmt.Errorf("checking %s: %w", awaDir, err)
			}
		}
		if ok {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached the filesystem root without finding a project.
			return "", ErrRootNotFound
		}
		dir = parent
	}
}

// ConfigExists reports whether the layout's config file exists as a regular
// file. A "not found" outcome is reported as (false, nil); any other stat
// failure (permission, I/O) is returned so callers do not mistake it for a
// missing config.
func ConfigExists(l paths.Layout) (bool, error) {
	return regularFileExists(l.ConfigFile())
}

// CreateMarker creates the .awa marker directory exclusively, so initializing a
// project is an atomic step rather than a check-then-act race: two concurrent
// inits cannot both succeed. It first ensures the (possibly brand-new) root
// exists, then creates .awa with a plain Mkdir, which fails when the path already
// exists. When the existing path is a directory the returned error satisfies
// errors.Is(err, os.ErrExist), so callers map it to "already initialized". When
// it is a non-directory the error satisfies errors.Is(err, ErrNotDirectory)
// instead: a stray .awa file is not a project (discovery does not treat it as one
// either), so it must surface as a real filesystem problem, not "initialized".
func CreateMarker(l paths.Layout) error {
	// The project root is the user's own directory; awa creates it if missing but does
	// not narrow its permissions. The .awa marker and everything under it is awa-owned
	// state and is created owner-private.
	if err := os.MkdirAll(l.Root(), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", l.Root(), err)
	}
	if err := os.Mkdir(l.AwaDir(), paths.DirPerm); err != nil {
		if errors.Is(err, os.ErrExist) {
			// Distinguish an existing project directory from a stray non-directory
			// at .awa, matching discovery's marker semantics. markerExists uses
			// Lstat, so a symlink is rejected (ErrNotDirectory) just like a file: a
			// symlinked .awa is not a project and must not be reported as one. A
			// real directory passes the os.ErrExist error through to mean
			// "initialized".
			if _, derr := markerExists(l.AwaDir()); derr != nil {
				return derr
			}
		}
		return err
	}
	return nil
}

// EnsureDirs creates every directory the layout requires, idempotently. It is
// safe to call on a partially initialized project.
func EnsureDirs(l paths.Layout) error {
	for _, d := range l.RequiredDirs() {
		if err := os.MkdirAll(d, paths.DirPerm); err != nil {
			return fmt.Errorf("creating %s: %w", d, err)
		}
	}
	return nil
}

// WriteNewFileAtomic writes data to path, failing if path already exists. It
// publishes by hard-linking the staged temp file into place: os.Link fails when
// the target exists, so a file that appears between a caller's existence check and
// this publish is never clobbered. Existence and publication are therefore one
// atomic, exclusive step rather than a check-then-write race. A target that already
// exists is reported as an error satisfying errors.Is(err, os.ErrExist).
func WriteNewFileAtomic(path string, data []byte, perm os.FileMode) error {
	return writeFileAtomic(path, data, perm, os.Link)
}

// WriteFileAtomic writes data to path, replacing any existing file, via the same
// staged temp-file publish as WriteNewFileAtomic but with os.Rename so it is not
// exclusive. Use it for a deliberate overwrite (e.g. `config init --force`); use
// WriteNewFileAtomic when the file must not already exist so create-or-fail stays
// one atomic step.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	return replaceFileAtomic(path, data, perm)
}

// replaceFileAtomic writes data to path, replacing any existing file. Unlike
// WriteNewFileAtomic it publishes with os.Rename, so it is not exclusive: it is
// used to repair an existing-but-ineffective, awa-owned guard rather than to create
// a file that must not already exist. The rename replaces the target in one step on
// Unix; Go does not guarantee that atomicity on every platform, but the data is
// always either fully staged or not published, so a reader never sees a partial
// write.
func replaceFileAtomic(path string, data []byte, perm os.FileMode) error {
	return writeFileAtomic(path, data, perm, os.Rename)
}

// writeFileAtomic stages data in a temp file in path's directory — written,
// synced, and chmod'd — then hands it to publish to move it to path. The only
// difference between the exclusive create and the replacing variant is publish
// (os.Link vs os.Rename), so both share this body. The temp file is removed on any
// failure, and after a successful publish that left it behind (os.Link keeps the
// source; os.Rename consumes it, making the cleanup a harmless no-op).
func writeFileAtomic(path string, data []byte, perm os.FileMode, publish func(oldpath, newpath string) error) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, paths.DirPerm); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing temp file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err = os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("setting permissions: %w", err)
	}
	if err = publish(tmpName, path); err != nil {
		// Wrapped so errors.Is(err, os.ErrExist) holds for an existing target under
		// the exclusive (os.Link) publish.
		return fmt.Errorf("publishing %s: %w", path, err)
	}
	_ = os.Remove(tmpName)
	return nil
}

// ReadFile reads the entire contents of path.
func ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// StateGitignoreContent is the canonical body of the awa-owned .awa/.gitignore
// guard. The lone "*" makes git ignore everything under .awa — including the
// guard file itself, which is local state, not a repository artifact. This is the
// single source of truth for the guard so init, doctor repair, and tests never
// duplicate the literal.
const StateGitignoreContent = "# awa local state; never commit this directory.\n*\n"

// StateGitignore classifies the .awa/.gitignore guard's protection state.
type StateGitignore int

const (
	// StateGitignoreOK means the guard exists and effectively ignores .awa.
	StateGitignoreOK StateGitignore = iota + 1
	// StateGitignoreMissing means no guard file is present.
	StateGitignoreMissing
	// StateGitignoreIneffective means a guard exists but does not ignore the
	// directory's contents — it is empty, only comments, carries a narrowing
	// pattern, or is a symlink git will not honor. It is repairable.
	StateGitignoreIneffective
)

// StateGitignoreEffective reports whether the given .awa/.gitignore body actually
// ignores everything in the directory. It is a deliberately small, portable check
// — no git invocation — that is conservative on purpose: the guard counts as
// effective only when every non-blank, non-comment line is exactly "*" (and there
// is at least one). A later negation or any narrower pattern (for example
// "*\n!keep") re-exposes part of .awa to git, so it is rejected and repaired back
// to the canonical guard rather than trusted.
func StateGitignoreEffective(content []byte) bool {
	sawStar := false
	for _, line := range bytes.Split(content, []byte("\n")) {
		s := bytes.TrimSpace(line)
		if len(s) == 0 || s[0] == '#' {
			continue
		}
		if !bytes.Equal(s, []byte("*")) {
			return false // a negation or narrower pattern breaks full coverage
		}
		sawStar = true
	}
	return sawStar
}

// InspectStateGitignore reads the .awa/.gitignore guard and classifies it without
// touching git, so doctor and tests share one definition of "protected". The guard
// must be a regular file: git does not honor a symlinked .gitignore, so a symlink
// (which os.ReadFile would silently follow) leaves .awa exposed and is classified
// ineffective — repair replaces the link with a real guard. A directory or other
// irregular node at the path, and any genuine read failure other than the file
// being absent, are returned as errors so callers report "unreadable" rather than
// mistaking it for a missing or healthy guard.
func InspectStateGitignore(l paths.Layout) (StateGitignore, error) {
	path := l.StateGitignore()
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return StateGitignoreMissing, nil
		}
		return 0, err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return StateGitignoreIneffective, nil
	case !info.Mode().IsRegular():
		return 0, fmt.Errorf("%s is not a regular file", path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	if StateGitignoreEffective(content) {
		return StateGitignoreOK, nil
	}
	return StateGitignoreIneffective, nil
}

// EnsureStateGitignore makes the awa-owned .awa/.gitignore guard exist and be
// effective, returning whether it had to write anything. A missing guard is
// created from a staged temp file; an existing effective guard is left untouched;
// an existing ineffective guard is replaced the same way. The guard is awa-owned
// local state and not a user configuration surface, so replacing an ineffective one
// to restore protection is safe. It is idempotent: a second call on a healthy
// project writes nothing and reports changed=false.
func EnsureStateGitignore(l paths.Layout) (changed bool, err error) {
	state, err := InspectStateGitignore(l)
	if err != nil {
		return false, err
	}
	switch state {
	case StateGitignoreOK:
		return false, nil
	case StateGitignoreMissing:
		if err := WriteNewFileAtomic(l.StateGitignore(), []byte(StateGitignoreContent), paths.FilePerm); err != nil {
			return false, err
		}
		return true, nil
	default: // StateGitignoreIneffective
		if err := replaceFileAtomic(l.StateGitignore(), []byte(StateGitignoreContent), paths.FilePerm); err != nil {
			return false, err
		}
		return true, nil
	}
}

// ErrNotDirectory reports that a path exists but is not a directory — a
// different problem from the path being absent, and one a caller may want to
// surface distinctly.
var ErrNotDirectory = errors.New("path exists but is not a directory")

// DirExists reports whether path is a directory, distinguishing three
// outcomes rather than collapsing them: a genuine absence is (false, nil); a
// path that exists but is not a directory is (false, ErrNotDirectory); any
// other stat failure is (false, err). A real directory is (true, nil).
func DirExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("%s: %w", path, ErrNotDirectory)
	}
	return true, nil
}

// markerExists reports whether path is a real directory usable as the .awa
// project marker and state root. Unlike DirExists it uses os.Lstat and rejects a
// symlink (even one pointing at a real directory) as (false, ErrNotDirectory):
// awa writes all project state under .awa, so the marker must be a hard boundary
// that cannot redirect those writes outside the project root. It distinguishes a
// genuine absence (false, nil), a non-directory or symlink (false,
// ErrNotDirectory), and any other stat failure (false, err).
func markerExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	// Under Lstat a symlink is not IsDir(), so this rejects both a stray file and
	// a symlink that would otherwise pass an os.Stat directory check.
	if !info.IsDir() {
		return false, fmt.Errorf("%s: %w", path, ErrNotDirectory)
	}
	return true, nil
}

// regularFileExists reports whether path is a regular file. It distinguishes a
// genuine absence ((false, nil)) from a stat failure ((false, err)) so callers
// can surface permission and I/O errors instead of collapsing them into "not
// found".
func regularFileExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return info.Mode().IsRegular(), nil
}
