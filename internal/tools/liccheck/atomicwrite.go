package main

import (
	"os"
	"path/filepath"
)

// atomicWrite publishes data to path atomically: it writes a temp file in the same
// directory, gives it the existing file's mode (or 0644 for a new file), then
// renames it over path. A failure at any step removes the temp and leaves an
// existing target untouched. This mirrors internal/tools/refgen so a failed or
// interrupted notice update never leaves a partial committed artifact.
func atomicWrite(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".liccheck-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	if err := writeTemp(tmp, data, mode); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// writeTemp writes data to an open temp file, sets its mode, and closes it. It owns
// closing f so the caller can remove the temp on any returned error.
func writeTemp(f *os.File, data []byte, mode os.FileMode) error {
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
