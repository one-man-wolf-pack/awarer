package worktreefs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"awarer/internal/domain/config"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/worktree"
)

// ClassifyRootFile answers what a full scan under scope would record for one
// root-level entry, without walking the tree. It applies the same decisions in the
// same order as Walk (protected names, the include filter and its precedence over
// excludes, the root's enabled ignore files, symlink policy, and file
// classification), so a caller asking whether one file would be observed reaches the
// scan's own answer rather than a second evaluator's.
//
// It reads metadata only: the root's ignore files and the entry's lstat (and, with
// follow_symlinks, its link chain). It never opens the entry's content, and an entry
// resolving to a directory is not descended. visit receives the node the scan would
// record; an excluded, absent, or directory entry yields nothing. A lookup or
// ignore-file read failure is returned, never treated as exclusion.
func (w *Walker) ClassifyRootFile(ctx context.Context, layout paths.Layout, scope config.ScanScope, name string, visit func(worktree.Node) error) error {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return fmt.Errorf("classify: %q is not a root-level name", name)
	}
	root := layout.Root()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root
	}
	st := &walk{
		ctx:        ctx,
		root:       root,
		realRoot:   realRoot,
		eng:        newIgnoreEngine(scope.Exclude, scope.UseGitignore, scope.UseAwaignore),
		inc:        newIncludeFilter(scope.CanonicalInclude()),
		protected:  protectedNames(),
		scope:      scope,
		visit:      visit,
		leavesOnly: true,
	}
	if err := st.eng.loadDirAt(root, ""); err != nil {
		return fmt.Errorf("reading root ignore files: %w", err)
	}
	abs := filepath.Join(root, name)
	info, err := os.Lstat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("lstat %s: %w", abs, err)
	}
	if info.IsDir() {
		return nil
	}
	return st.base(abs, fs.FileInfoToDirEntry(info), nil)
}
