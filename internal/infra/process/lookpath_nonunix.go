//go:build !unix

package process

import (
	"os"
	"strings"
)

// resolveExt returns base, or base with one of the candidate extensions appended,
// when it names an existing regular file. A base that already carries a known
// extension is accepted as-is. With a single empty extension it degenerates to a
// plain existence check.
func resolveExt(base string, exts []string) (string, bool) {
	lower := strings.ToLower(base)
	for _, e := range exts {
		if e != "" && strings.HasSuffix(lower, e) && existsRegular(base) {
			return base, true
		}
	}
	for _, e := range exts {
		candidate := base + e
		if existsRegular(candidate) {
			return candidate, true
		}
	}
	return "", false
}

// envValueCI extracts an environment value by case-insensitive name (last
// assignment wins, as Windows env names are case-insensitive), falling back to the
// awa process environment when env is nil — mirroring inherited execution.
func envValueCI(env []string, key string) string {
	if env == nil {
		return os.Getenv(key)
	}
	val := ""
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			continue
		}
		if strings.EqualFold(kv[:i], key) {
			val = kv[i+1:]
		}
	}
	return val
}

// existsRegular reports whether p names an existing regular file. Non-unix
// platforms detect executables by extension and existence rather than mode bits.
func existsRegular(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}
