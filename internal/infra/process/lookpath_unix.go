//go:build unix

package process

import (
	"os"
	"path/filepath"
	"strings"
)

// lookExecutable resolves an executable on unix from the execution directory dir
// using the command's own env. A name containing a path separator is taken
// relative to dir (or used as-is when absolute). A bare name is searched on the
// env's PATH, with relative PATH entries anchored at dir rather than awa's own
// working directory — so resolution matches where the command runs. Executable
// detection uses the unix execute bit. It returns the first match, or ok=false
// when none is found.
func lookExecutable(name, dir string, env []string) (string, bool) {
	if strings.ContainsRune(name, os.PathSeparator) {
		p := name
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		return p, isExecutableFile(p)
	}
	for _, entry := range filepath.SplitList(pathFromEnv(env)) {
		if entry == "" {
			entry = "."
		}
		if !filepath.IsAbs(entry) {
			entry = filepath.Join(dir, entry)
		}
		candidate := filepath.Join(entry, name)
		if isExecutableFile(candidate) {
			return candidate, true
		}
	}
	return "", false
}

// pathFromEnv extracts PATH from an environment slice (last assignment wins,
// matching exec semantics), falling back to the awa process PATH when env is nil —
// so resolution uses the same PATH the command runs with. Unix env names are
// case-sensitive, so the match is exact.
func pathFromEnv(env []string) string {
	if env == nil {
		return os.Getenv("PATH")
	}
	path := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	return path
}

// isExecutableFile reports whether p is a regular file with an execute bit set.
func isExecutableFile(p string) bool {
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}
