//go:build !unix && !windows

package process

import (
	"path/filepath"
	"strings"
)

// lookExecutable is the fallback resolver for platforms that are neither unix nor
// Windows (plan9, js, wasm). It keeps the resolver contract — env's PATH, anchored
// at the execution directory — but detects an executable only by existence, since
// these platforms lack unix execute bits and Windows PATHEXT (and js/wasm cannot
// exec at all). A name with a separator is resolved relative to dir.
func lookExecutable(name, dir string, env []string) (string, bool) {
	exts := []string{""}
	if strings.ContainsRune(name, '/') {
		base := name
		if !filepath.IsAbs(base) {
			base = filepath.Join(dir, base)
		}
		return resolveExt(base, exts)
	}
	for _, entry := range filepath.SplitList(envValueCI(env, "PATH")) {
		if entry == "" {
			entry = "."
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
