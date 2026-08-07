// Package initcmd implements the "awa init" use case: turning a directory into
// an awa project. It orchestrates the filesystem and config-file boundaries and
// returns a plain result; it does not format output and does not discover the
// project root from the current directory — the caller passes the root, which
// Run canonicalizes to an absolute path.
package initcmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"awarer/internal/domain/paths"
	"awarer/internal/infra/configfile"
	"awarer/internal/infra/projfs"
)

// Profile selects which configuration defaults init writes.
type Profile int

const (
	// ProfileDefault writes the normal production defaults.
	ProfileDefault Profile = iota
	// ProfileStrict writes the stricter profile (trust_mode = "strict").
	ProfileStrict
)

// ParseProfile resolves a --profile value, rejecting unknown names.
func ParseProfile(s string) (Profile, error) {
	switch s {
	case "default":
		return ProfileDefault, nil
	case "strict":
		return ProfileStrict, nil
	default:
		return 0, fmt.Errorf("invalid profile %q: want default or strict", s)
	}
}

// ErrAlreadyInitialized reports that the target already contains a .awa state
// directory. init refuses to re-initialize it rather than disturbing existing
// project state. The marker is the directory, not the optional config file.
var ErrAlreadyInitialized = errors.New("project already initialized")

// Request describes an init invocation.
type Request struct {
	// Root is the absolute directory to initialize.
	Root string
	// Profile selects the config defaults to write.
	Profile Profile
}

// Result describes what init created. ConfigPath is empty when no config file
// was written — the default profile relies on the built-in defaults and writes
// none, so an absent path is the normal, correct outcome, not an omission.
type Result struct {
	Root        string
	ConfigPath  string
	CreatedDirs []string
}

// Run initializes the project at req.Root. It refuses to re-initialize an
// existing project (ErrAlreadyInitialized, keyed on the .awa directory), creates
// the required directory layout, and — only for profiles that carry an override —
// writes a minimal config.toml atomically. The default profile writes no config:
// an absent file means the product defaults apply. req.Root must be non-empty and
// is canonicalized to an absolute path. Initialization is all-or-nothing: if any
// step after the marker is created fails, the freshly created .awa is removed so a
// half-built project is never left looking complete.
func Run(req Request) (res Result, retErr error) {
	// Resolve the profile and root before any filesystem side effect, so an
	// unknown profile or empty root fails cleanly rather than half-initializing.
	// The use case does not assume callers came through the CLI parser. data is
	// nil for profiles that need no config file.
	data, err := profileConfig(req.Profile)
	if err != nil {
		return Result{}, err
	}
	root, err := canonicalRoot(req.Root)
	if err != nil {
		return Result{}, err
	}

	layout := paths.New(root)

	// The .awa directory is the project marker. Create it exclusively so the
	// "already initialized" check and the creation are one atomic step: two
	// concurrent inits cannot both pass a separate existence check and both
	// succeed. An existing marker directory maps to ErrAlreadyInitialized; a
	// non-directory .awa surfaces as its own filesystem error, not "already
	// initialized", since discovery does not treat it as a project either.
	if err := projfs.CreateMarker(layout); err != nil {
		if errors.Is(err, os.ErrExist) {
			return Result{}, ErrAlreadyInitialized
		}
		return Result{}, err
	}
	// We exclusively created the marker, so we own it. If any later step fails,
	// remove the whole .awa we just made: leaving it would make discovery treat a
	// partially built directory as a complete project and a retry would wrongly
	// report ErrAlreadyInitialized. If the cleanup itself fails, join that error so
	// the caller learns the rollback was incomplete rather than silently believing
	// init is all-or-nothing — the leftover .awa is exactly the state needing
	// manual attention.
	defer func() {
		if retErr != nil {
			if rmErr := os.RemoveAll(layout.AwaDir()); rmErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("rolling back %s: %w", layout.AwaDir(), rmErr))
			}
		}
	}()

	if err := projfs.EnsureDirs(layout); err != nil {
		return Result{}, err
	}

	// Write the awa-owned guard that keeps .awa out of git. awa owns this state, so
	// it protects it itself rather than relying on the user editing a root
	// .gitignore — which init never touches. The guard is unconditional and has no
	// opt-out: local evidence must not be able to start accumulating untracked-by-
	// accident rather than untracked-by-construction.
	if _, err := projfs.EnsureStateGitignore(layout); err != nil {
		return Result{}, err
	}

	res = Result{
		Root:        root,
		CreatedDirs: layout.RequiredDirs(),
	}
	if data == nil {
		// Default profile: no config file, so the project runs purely on the
		// built-in defaults until the user chooses to add overrides.
		return res, nil
	}

	if err := projfs.WriteNewFileAtomic(layout.ConfigFile(), data, paths.FilePerm); err != nil {
		return Result{}, err
	}
	res.ConfigPath = layout.ConfigFile()
	return res, nil
}

// canonicalRoot rejects an empty root and resolves the rest to an absolute
// path, so the use case never writes to a relative ".awa" by accident.
func canonicalRoot(root string) (string, error) {
	if root == "" {
		return "", errors.New("project root must not be empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolving project root %q: %w", root, err)
	}
	return abs, nil
}

// profileConfig returns the config.toml bytes for the requested profile, or an
// error for an unknown profile. A nil result means the profile writes no config
// file and relies entirely on the built-in defaults.
func profileConfig(p Profile) ([]byte, error) {
	switch p {
	case ProfileDefault:
		return nil, nil
	case ProfileStrict:
		return configfile.StrictTOML(), nil
	default:
		return nil, fmt.Errorf("unknown profile %d", p)
	}
}
