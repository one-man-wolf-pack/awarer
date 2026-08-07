package run

import (
	"bytes"
	"encoding/binary"

	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
)

// ScopeOverrides are the CLI overrides for the run input scan, already resolved
// to clean project-root-relative slash paths. ScopeReplace, when non-nil,
// replaces run.default_scope as the full include list (one or more --scope
// values); Include and Exclude append to the effective include and exclude lists.
type ScopeOverrides struct {
	ScopeReplace []string
	Include      []string
	Exclude      []string
}

// effectiveScanScope builds the ScanScope the run input scan walks. The include
// list comes from --scope or run.default_scope plus any --include; the exclude
// list is the effective run excludes (baseline plus the common and run-only
// extra_excludes, with the history-only defaults deliberately omitted, composed
// fresh from the config inputs) plus any --exclude; the gitignore/awaignore flags
// come from the run section. The symlink policy is inherited from the common
// scope. Because the effective excludes are composed here from the additive
// config fields — not read from a stored, derivable field — the run scope cannot
// drift out of sync with the config.
func effectiveScanScope(cfg config.Config, ov ScopeOverrides) config.ScanScope {
	include := cfg.Run.DefaultScope
	if ov.ScopeReplace != nil {
		include = ov.ScopeReplace
	}
	merged := make([]string, 0, len(include)+len(ov.Include))
	merged = append(merged, include...)
	merged = append(merged, ov.Include...)

	runExcludes := config.EffectiveRunExcludes(cfg.Scope.ExtraExcludes, cfg.Run.ExtraExcludes)
	exclude := make([]string, 0, len(runExcludes)+len(ov.Exclude))
	exclude = append(exclude, runExcludes...)
	exclude = append(exclude, ov.Exclude...)

	return config.ScanScope{
		Include:                merged,
		Exclude:                exclude,
		UseGitignore:           cfg.Run.UseGitignore,
		UseAwaignore:           cfg.Run.UseAwaignore,
		FollowSymlinks:         cfg.Scope.FollowSymlinks,
		SymlinkMaxDepth:        cfg.Scope.SymlinkMaxDepth,
		AllowSymlinkRootEscape: cfg.Scope.AllowSymlinkRootEscape,
	}
}

// runConfigHash computes a stable identity for the run output and caching policy —
// the part of the run configuration that shapes a stored result but is not already
// recorded elsewhere in the key. It folds those fields through the project hasher
// with the same length-prefixed, injective encoding the scanner uses for its
// config hash, so two semantically identical policies hash identically and any
// meaningful change produces a distinct key.
//
// Scope, ignore flags, and the env allowlist are deliberately excluded: they are
// already keyed in their effective form by KeyInput (IncludeScope, ExcludeScope,
// UseGitignore, UseAwaignore, and the env checkpoint). Hashing the raw run-section
// copies here too would both duplicate that identity and, because --scope replaces
// run.default_scope without rewriting it, make an unused run.default_scope change
// produce a spurious miss. TTL is excluded as well: it is a freshness window
// applied at lookup, not part of a result's identity, so changing it must not
// orphan otherwise-valid entries.
func runConfigHash(cfg config.Config, h hashing.Hasher) hashing.ConfigHash {
	var b bytes.Buffer
	r := cfg.Run
	putHashUint(&b, uint64(r.MaxStdoutSize.Bytes()))
	putHashUint(&b, uint64(r.MaxStderrSize.Bytes()))
	putHashBool(&b, r.CaptureOutput)
	putHashBool(&b, r.CacheFailedRuns)
	return hashing.ConfigHashFromTree(h.HashBytes(b.Bytes()))
}

func putHashUint(b *bytes.Buffer, v uint64) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], v)
	b.Write(n[:])
}

func putHashBool(b *bytes.Buffer, v bool) {
	if v {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
}
