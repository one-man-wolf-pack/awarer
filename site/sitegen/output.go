package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The generated directory is a replaceable artifact and is authoritative only
// after a successful exit. Deployment starts after that exit, so a failure
// part-way needs no transaction: what it leaves behind is a partial tree with no
// publication, ownership, or recovery meaning, and the answer to it is an
// ordinary removal and another run. The recipe that owns the repository's
// site/dist does exactly that; this tool owns no removal at all.

// routeToPath maps a route to the file that serves it. A route ending in "/" is
// a directory page and is served by its index.html; anything else is served by
// the file it names.
func routeToPath(route string) (string, error) {
	if !strings.HasPrefix(route, "/") {
		return "", fmt.Errorf("route %q is not rooted", route)
	}
	rel := strings.TrimPrefix(route, "/")
	if rel == "" || strings.HasSuffix(rel, "/") {
		return rel + "index.html", nil
	}
	return rel, nil
}

// writeSite writes the built site into a directory that must not exist.
//
// Lstat rather than Stat: a symbolic link at the output path counts as
// something already there and is refused by the same rule, without this file
// carrying a branch about links.
func writeSite(ctx context.Context, root string, files []outputFile) error {
	switch _, err := os.Lstat(root); {
	case err == nil:
		return fmt.Errorf("output %s already exists; sitegen writes only into a path that does not exist", root)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("looking at the output path: %w", err)
	}

	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := routeToPath(f.route)
		if err != nil {
			return err
		}
		dest := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dest, f.data, 0o644); err != nil { //nolint:gosec // public documentation, served as static files
			return err
		}
	}
	return nil
}
