//go:build windows

package process

import (
	"path/filepath"
	"strings"
)

// lookExecutable resolves an executable on Windows from the execution directory
// dir using the command's own env. It applies PATHEXT, treats env names
// case-insensitively, recognizes both '/' and '\\' and drive-qualified paths, and
// — like the Windows loader — searches the current directory (here, dir) before
// %PATH% for a bare command. Relative PATH entries are anchored at dir so
// resolution matches where the command runs.
func lookExecutable(name, dir string, env []string) (string, bool) {
	exts := pathExts(env)
	if strings.ContainsAny(name, `/\`) || filepath.VolumeName(name) != "" {
		base := name
		if !filepath.IsAbs(base) {
			base = filepath.Join(dir, base)
		}
		return resolveExt(base, exts)
	}
	// Windows searches the current directory before PATH for a bare command.
	if p, ok := resolveExt(filepath.Join(dir, name), exts); ok {
		return p, true
	}
	for _, entry := range filepath.SplitList(envValueCI(env, "PATH")) {
		if entry == "" {
			continue
		}
		if !filepath.IsAbs(entry) {
			entry = filepath.Join(dir, entry)
		}
		if p, ok := resolveExt(filepath.Join(entry, name), exts); ok {
			return p, true
		}
	}
	return "", false
}

// pathExts returns the executable extensions to try, derived from PATHEXT (or the
// conventional default), each lowercased and dot-prefixed.
func pathExts(env []string) []string {
	pathext := envValueCI(env, "PATHEXT")
	if pathext == "" {
		pathext = `.COM;.EXE;.BAT;.CMD`
	}
	var exts []string
	for _, e := range strings.Split(pathext, ";") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		exts = append(exts, strings.ToLower(e))
	}
	if len(exts) == 0 {
		return []string{""}
	}
	return exts
}
