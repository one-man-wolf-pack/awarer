package config

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Config is the validated, domain-safe configuration for a project. Each
// section mirrors a TOML table, but every field is a parsed value object or a
// closed enum — never a raw string awaiting interpretation.
type Config struct {
	Scope      Scope
	History    History
	Hashing    Hashing
	Checkpoint Checkpoint
	Run        Run
	GC         GC
	Diff       Diff
	Locks      Locks
	UI         UI
}

// Clone returns a deep copy of the config: every slice field is copied so the
// result shares no backing array with the original. It lets a value that must stay
// immutable (see ResolvedConfig) hand out and hold configs no consumer can mutate
// through a shared slice.
func (c Config) Clone() Config {
	c.Scope.Include = slices.Clone(c.Scope.Include)
	c.Scope.ExtraExcludes = slices.Clone(c.Scope.ExtraExcludes)
	c.History.ExtraExcludes = slices.Clone(c.History.ExtraExcludes)
	c.Run.DefaultScope = slices.Clone(c.Run.DefaultScope)
	c.Run.ExtraExcludes = slices.Clone(c.Run.ExtraExcludes)
	c.Run.EnvAllowlist = slices.Clone(c.Run.EnvAllowlist)
	c.Run.WatchedEffectRoots = slices.Clone(c.Run.WatchedEffectRoots)
	return c
}

// UI controls human-facing presentation. Time selects how timestamps render in
// human output (log, run subcommands); it never affects JSON, which always uses
// machine timestamps.
type UI struct {
	Time TimeDisplay
}

// Scope is the common scan boundary shared by the history/checkpoint family and
// (through ExtraExcludes) the run input scan. It holds only user-supplied values:
// ExtraExcludes is the additive common exclude list, and the product baseline and
// protected defaults live in code (see baselineExcludes/protectedExcludes), never
// here. There is no stored effective exclude list — the layered excludes are
// composed on demand by Config.HistoryScanScope, the single source of truth — so
// no derived field can drift out of sync with these inputs. UseGitignore and
// UseAwaignore govern the history/checkpoint family.
type Scope struct {
	Include                []string
	ExtraExcludes          []string
	UseGitignore           bool
	UseAwaignore           bool
	FollowSymlinks         bool
	SymlinkMaxDepth        int
	AllowSymlinkRootEscape bool
}

// History carries the history-only additive excludes. They affect awa checkpoint,
// awa changes, and awa diff, but never the run input scan, so a generated
// artifact can stay out of local history while remaining a real command input.
// The history/checkpoint family's gitignore/awaignore policy comes from Scope,
// the common base; this section only adds excludes.
type History struct {
	ExtraExcludes []string
}

// CanonicalInclude returns the effective include scope in a stable canonical
// form: each path cleaned and slash-separated, duplicates removed, paths already
// covered by an included ancestor dropped, and the result sorted. Because "." (or
// any path that cleans to it) includes the whole tree, a canonical list containing
// it collapses to exactly ["."].
//
// It is the single source of truth for the include scope, shared by the walker
// (which decides what to scan) and the config hash (which identifies the scan
// inputs), so semantically identical scopes can never scan the same tree yet
// record different config hashes. That holds both for spelling differences like
// ["src"] vs ["./src"] and for redundant nesting like ["src"] vs ["src",
// "src/internal"]: a child under an already-included parent scans nothing new, so
// it must not perturb the canonical form.
func CanonicalInclude(include []string) []string {
	for _, p := range include {
		if cleanScopePath(p) == "." {
			return []string{"."}
		}
	}
	seen := make(map[string]bool, len(include))
	cleaned := make([]string, 0, len(include))
	for _, p := range include {
		c := cleanScopePath(p)
		if !seen[c] {
			seen[c] = true
			cleaned = append(cleaned, c)
		}
	}
	sort.Strings(cleaned)
	// Sorting puts every ancestor before its descendants, so an include is
	// redundant exactly when an already-kept include is its ancestor.
	out := make([]string, 0, len(cleaned))
	for _, c := range cleaned {
		covered := false
		for _, kept := range out {
			if c == kept || strings.HasPrefix(c, kept+"/") {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, c)
		}
	}
	return out
}

// cleanScopePath normalizes a scope path to its cleaned, slash-separated form. An
// empty path cleans to ".".
func cleanScopePath(p string) string {
	return filepath.ToSlash(filepath.Clean(p))
}

// Hashing controls content hashing and index trust. The hash primitive itself is
// fixed product behavior (BLAKE3) and is deliberately absent: only the policy that
// governs how it is applied is configurable.
type Hashing struct {
	TrustMode       TrustMode
	MaxFileSize     ByteSize
	LargeFilePolicy LargeFilePolicy
}

// Checkpoint controls checkpoint recording behavior.
type Checkpoint struct {
	StoreFileContents bool
	DiffContext       int
	RenameDetection   bool
}

// Run controls the run cache. ExtraExcludes holds only the user's additive
// run-only excludes; the effective run input exclude list — protected + baseline
// + scope and run ExtraExcludes, with the history-only defaults deliberately
// omitted so a build artifact stays a real command input — is composed on demand
// (see EffectiveRunExcludes), not stored. UseGitignore and UseAwaignore govern
// the run input scan and feed the run cache key.
type Run struct {
	DefaultScope    []string
	ExtraExcludes   []string
	UseGitignore    bool
	UseAwaignore    bool
	EnvAllowlist    []string
	TTL             Duration
	MaxStdoutSize   ByteSize
	MaxStderrSize   ByteSize
	CaptureOutput   bool
	CacheFailedRuns bool
	// WatchedEffectRoots holds only the user's additive watched generated-output
	// selectors ([run].extra_effect_roots). The effective watch list — the built-in
	// effectRootDefaults plus these — is composed on demand (see EffectiveEffectRoots),
	// not stored, so it cannot drift from the additive value. The list is additive only,
	// since silencing a built-in would reintroduce the false-hit class effect
	// observation exists to close.
	WatchedEffectRoots []string
}

// ScanScope is the fully-resolved boundary a single scan walks: the include list,
// an effective exclude list already composed from the protected/baseline/history
// layers and the user's additive excludes (no further derivation), the
// ignore-source toggles, and the symlink policy. It is produced only by
// Config.HistoryScanScope and the run package's scope builder, the single places
// the layered excludes are composed, so the walker and the config hash consume a
// ready value instead of a derived Config field that could drift out of sync with
// the additive ExtraExcludes.
type ScanScope struct {
	Include                []string
	Exclude                []string
	UseGitignore           bool
	UseAwaignore           bool
	FollowSymlinks         bool
	SymlinkMaxDepth        int
	AllowSymlinkRootEscape bool
}

// CanonicalInclude returns the scope's canonical include list (see the package
// CanonicalInclude function).
func (s ScanScope) CanonicalInclude() []string { return CanonicalInclude(s.Include) }

// HistoryScanScope composes the boundary the history/checkpoint family (awa checkpoint,
// changes, diff) scans under: the configured include, the effective history
// excludes (protected + baseline + history defaults + the common and history-only
// additive excludes), and the scope's ignore-source and symlink policy.
func (c Config) HistoryScanScope() ScanScope {
	return ScanScope{
		Include:                c.Scope.Include,
		Exclude:                EffectiveHistoryExcludes(c.Scope.ExtraExcludes, c.History.ExtraExcludes),
		UseGitignore:           c.Scope.UseGitignore,
		UseAwaignore:           c.Scope.UseAwaignore,
		FollowSymlinks:         c.Scope.FollowSymlinks,
		SymlinkMaxDepth:        c.Scope.SymlinkMaxDepth,
		AllowSymlinkRootEscape: c.Scope.AllowSymlinkRootEscape,
	}
}

// Validate checks a resolved scan scope: a non-empty include whose entries stay
// within the project root, and non-empty exclude entries. It is the scope-level
// counterpart to Config.Validate for a scope assembled outside decode (notably a
// run scope built from CLI overrides).
func (s ScanScope) Validate() error {
	var p []string
	if len(s.Include) == 0 {
		p = append(p, "scan scope include must list at least one path")
	}
	p = append(p, pathProblems("include", s.Include, mustStayInRoot)...)
	p = append(p, pathProblems("exclude", s.Exclude, nonEmptyOnly)...)
	if len(p) > 0 {
		return &ValidationError{Problems: p}
	}
	return nil
}

// Diff controls content-diff rendering. The algorithm is a closed enum (Myers
// or Histogram) parsed from the [diff] section; Histogram is the default, with
// Myers remaining explicit and the local fallback for unsuitable regions.
type Diff struct {
	Algorithm DiffAlgorithm
}

// GC controls garbage collection retention.
//
// KeepRestoresFor is deliberately independent of KeepRunsFor. A recovery
// observation is the only evidence that can undo a restore awa performed, so how
// long it is kept is a decision about worktree safety, not about how long a
// command's cached output stays useful; tying them together would let a shorter
// run-history window silently shorten the undo window too.
type GC struct {
	KeepLastCheckpoints int
	KeepRunsFor         Duration
	KeepRestoresFor     Duration
}

// Locks controls writer/collector lock acquisition. Timeout bounds every wait in the
// interlock: a collector (awa gc, awa doctor --repair) waiting for the exclusive
// collector lock, doctor --repair then waiting in-flight writers out, and a writer
// (checkpoint, run, run rm) standing down for an active collector. Any, on expiry, gives up
// with the lock-timeout exit code. Presence locks still never wait on one another. A
// zero timeout means fail fast (do not wait at all); a negative timeout is a config
// error.
type Locks struct {
	Timeout Duration
}

// The product default excludes are layered (baseline and history excludes).
// They are code constants, never serialized into a project's config:
//
//   - protectedExcludes are awa's own state and VCS metadata. They apply to every
//     scan and cannot be brought back by ordinary config (only a future explicit
//     unsafe option could).
//   - baselineExcludes are common dependency/cache directories. They apply to both
//     the history family and the run input scan.
//   - historyDefaultExcludes are generated-artifact directories. They apply to the
//     history family only; the run input scan keeps them visible so a build or test
//     that depends on them still keys its inputs correctly.
//
// Each is unexported and only ever copied (see the Effective* helpers and
// Defaults), so the shared default sets cannot be mutated by callers.
var (
	protectedExcludes      = []string{".git", ".awa"}
	baselineExcludes       = []string{"node_modules", ".next", ".nuxt", ".turbo", ".cache", "__pycache__", ".pytest_cache", ".mypy_cache", ".ruff_cache", ".venv", "venv", "target"}
	historyDefaultExcludes = []string{"dist", "build", "coverage"}
)

// effectRootExtra are watched generated-output directory names beyond the
// run-input-blind baseline excludes. build/dist/out/coverage are visible to the run
// input scan today (they are not baseline excludes), but are watched too so that a
// user who excludes them (via extra_excludes) still has them covered by effect
// observation rather than falling into the false-hit class.
var effectRootExtra = []string{"build", "dist", "out", "coverage"}

// effectRootDefaults is the built-in set of watched generated-output directories the
// run cache observes with a bounded stat signature. It is DERIVED from baselineExcludes
// plus effectRootExtra so the correctness invariant is structural,
// not a hand-maintained parallel list: every directory the run input scan is blind to
// (every baseline exclude) is watched, so nothing hidden from the input scan can be
// invisible to cache correctness. Deleting or changing one of these after a reusable
// run misses rather than falsely hits. The protected excludes (.git/.awa) are the one
// documented exception — VCS/awa state is outside awa's guarantee and the effect
// walker never observes it. The set is composed once and only ever copied.
var effectRootDefaults = buildEffectRootDefaults()

func buildEffectRootDefaults() []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(baselineExcludes)+len(effectRootExtra))
	for _, n := range append(append([]string(nil), baselineExcludes...), effectRootExtra...) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// generatedOutputExtras are generated-artifact directory names that are advisory only
// (they drive run-cache hints), beyond the observed effectRootDefaults. Kept here so
// the one authoritative GeneratedOutputDirs set replaces the formerly separate list in
// the runcache package.
var generatedOutputExtras = []string{"obj", "bin", ".gradle"}

// EffectRootDefaults returns a fresh copy of the built-in watched effect-root set.
func EffectRootDefaults() []string { return append([]string(nil), effectRootDefaults...) }

// EffectiveEffectRoots returns the watch list the run cache observes: the built-in
// effectRootDefaults plus the user's additive run-only roots. The result is a fresh
// slice. It is additive only — there is no way to drop a built-in root — because a
// silenced watched root reintroduces the false-hit class effect observation closes.
func EffectiveEffectRoots(runExtra []string) []string {
	out := make([]string, 0, len(effectRootDefaults)+len(runExtra))
	out = append(out, effectRootDefaults...)
	out = append(out, runExtra...)
	return out
}

// GeneratedOutputDirs returns the set of directory basenames awa treats as
// generated output, as a lookup map. It is the union of the watched effect roots
// and the advisory extras, and is the single source of truth the run-cache advisory
// hints consult (superseding the formerly separate list in the runcache package),
// so the two can never drift.
func GeneratedOutputDirs() map[string]bool {
	out := make(map[string]bool, len(effectRootDefaults)+len(generatedOutputExtras))
	for _, d := range effectRootDefaults {
		out[d] = true
	}
	for _, d := range generatedOutputExtras {
		out[d] = true
	}
	return out
}

// ProtectedExcludes returns a fresh copy of the protected default exclude layer
// (.git, .awa), which applies to every scan and cannot be re-included by config.
func ProtectedExcludes() []string { return append([]string(nil), protectedExcludes...) }

// BaselineExcludes returns a fresh copy of the baseline default exclude layer
// (dependency/cache directories), which applies to both history and run scans.
func BaselineExcludes() []string { return append([]string(nil), baselineExcludes...) }

// HistoryDefaultExcludes returns a fresh copy of the history-only default exclude
// layer (generated-artifact directories), which applies to the history family
// but never to the run input scan.
func HistoryDefaultExcludes() []string { return append([]string(nil), historyDefaultExcludes...) }

// EffectiveHistoryExcludes returns the exclude list the history/checkpoint
// family scans under: protected + baseline + history defaults, then the user's
// additive common (scope) and history-only excludes. The result is a fresh slice.
func EffectiveHistoryExcludes(scopeExtra, historyExtra []string) []string {
	out := make([]string, 0, len(protectedExcludes)+len(baselineExcludes)+len(historyDefaultExcludes)+len(scopeExtra)+len(historyExtra))
	out = append(out, protectedExcludes...)
	out = append(out, baselineExcludes...)
	out = append(out, historyDefaultExcludes...)
	out = append(out, scopeExtra...)
	out = append(out, historyExtra...)
	return out
}

// EffectiveRunExcludes returns the exclude list the run input scan uses:
// protected + baseline, then the user's additive common (scope) and run-only
// excludes. The history-only defaults are deliberately omitted. The result is a
// fresh slice.
func EffectiveRunExcludes(scopeExtra, runExtra []string) []string {
	out := make([]string, 0, len(protectedExcludes)+len(baselineExcludes)+len(scopeExtra)+len(runExtra))
	out = append(out, protectedExcludes...)
	out = append(out, baselineExcludes...)
	out = append(out, scopeExtra...)
	out = append(out, runExtra...)
	return out
}

// baselineEnvCommon and baselineEnvWindows are the built-in environment allowlist,
// always folded into a run's effective environment on top of run.env_allowlist. Like
// the baseline excludes they are a built-in default, and like the configured allowlist
// every baseline variable is captured into the cache key.
//
// Two rationales admit a name, and both are deliberate:
//
//   - execution: the variables a command needs to run at all — executable lookup, home
//     and temp directories, the shell, and the Windows equivalents;
//   - locale: the caller's selection of encoding, collation, formatting, and message
//     language. Dropping these does not merely lose a preference, it changes program
//     semantics — a reader that decodes UTF-8 under the caller's locale decodes
//     US-ASCII without it. awa passes the exact present/unset/empty state it found and
//     never forces or synthesizes a locale, so the child observes what the caller
//     observes and every value participates in the key.
//
// Growth is conservative. A candidate must be broadly relevant to ordinary command
// semantics, safe to expose to the same-user child, free of credential/capability or
// code-injection authority, keyable without durable raw storage, portable enough for
// the claimed surface, and worth its false-miss cost. Toolchain flags, external
// config/cache roots, proxy/credential variables, auth sockets, and loader/shell
// injection are opt-in through run.env_allowlist or never-default; see the audit guard
// in env_audit_test.go, which fails when this list changes without review.
//
// NLSPATH is deliberately absent: POSIX warns that overriding it changes message
// catalog lookup and can yield undefined utility behavior, so it is never-default.
var (
	baselineEnvCommon = []string{
		"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TEMP", "TMP",
		"LANG", "LANGUAGE", "LC_ALL", "LC_COLLATE", "LC_CTYPE", "LC_MESSAGES",
		"LC_MONETARY", "LC_NUMERIC", "LC_TIME",
	}
	baselineEnvWindows = []string{"SystemRoot", "WINDIR", "COMSPEC", "PATHEXT"}
)

// BaselineEnvNames returns the built-in environment allowlist for goos: the common
// set plus the platform-specific additions on Windows.
//
// The locale names are in the common set on every platform, including Windows, because
// membership means "inherit whatever the parent has". A Windows process usually has
// none of them, so they are keyed unset and nothing is passed; a Windows process that
// does define one (a POSIX-flavored shell, a cross-platform toolchain) has it passed
// through unchanged. Inventing a POSIX locale value on a platform that does not use one
// would be a different and unacceptable behavior.
func BaselineEnvNames(goos string) []string {
	out := append([]string(nil), baselineEnvCommon...)
	if goos == "windows" {
		out = append(out, baselineEnvWindows...)
	}
	return out
}

// WrapperMarkerName and WrapperMarkerValue are the fixed advisory fact awa injects into
// every actually executed child, letting a cooperative command detect that it is
// running under the wrapper.
//
// It is advisory and nothing more: any process can set it, so it authenticates nothing,
// grants no permission, proves no run record exists, and can never weaken a safety
// policy. It is fixed on purpose — no run id, checkpoint id, timestamp, store path, or
// cache state travels through the environment, because those are unstable, local, and
// would turn a debugging aid into a forgeable evidence channel.
//
// The name is awa-owned: run.env_allowlist rejects it (a user cannot redirect or
// silence it), the built-in baseline never inherits it, and the injected fact therefore
// always wins over an ambient value of the same name in the direct child.
const (
	WrapperMarkerName  = "AWA_RUN"
	WrapperMarkerValue = "1"
)

// IsReservedEnvName reports whether name is awa-owned and therefore not configurable.
// The comparison is case-insensitive on every platform, not only on the platform where
// environment names are case-insensitive, so one shared config is valid everywhere.
func IsReservedEnvName(name string) bool {
	return strings.EqualFold(name, WrapperMarkerName)
}

// Defaults returns the production default configuration. The
// effective exclude lists are composed on demand from the default layers with no
// user additions; .gitignore is off by default (it answers "what not to commit",
// not "what can affect a command") while .awaignore is on.
func Defaults() Config {
	return Config{
		Scope: Scope{
			Include:                []string{"."},
			ExtraExcludes:          nil,
			UseGitignore:           false,
			UseAwaignore:           true,
			FollowSymlinks:         false,
			SymlinkMaxDepth:        16,
			AllowSymlinkRootEscape: false,
		},
		History: History{
			ExtraExcludes: nil,
		},
		Hashing: Hashing{
			TrustMode:       TrustNormal,
			MaxFileSize:     50 * miB,
			LargeFilePolicy: LargeFileHashOnly,
		},
		Checkpoint: Checkpoint{
			StoreFileContents: true,
			DiffContext:       3,
			RenameDetection:   true,
		},
		Run: Run{
			DefaultScope:  []string{"."},
			ExtraExcludes: nil,
			UseGitignore:  false,
			UseAwaignore:  true,
			// The shipped allowlist is deliberately small and was re-assessed against the
			// baseline-admission criteria: both of these commonly change what an ordinary
			// non-interactive command does, carry no credential or capability, and are fully
			// keyed as a redacted identity. They stay configured rather than built-in
			// because they are tool-specific.
			//
			// PYTHONPATH was removed from the default. It is path-valued, and awa can key
			// the path string but cannot prove the importable code behind it is unchanged,
			// so inheriting it for every project would permit a false hit — a reused result
			// from a run whose real inputs moved. It remains a valid explicit opt-in for a
			// project that needs it and accepts that limit.
			EnvAllowlist:       []string{"CI", "NODE_ENV"},
			TTL:                Duration(7 * 24 * 60 * 60 * 1e9), // 7d
			MaxStdoutSize:      100 * miB,
			MaxStderrSize:      100 * miB,
			CaptureOutput:      true,
			CacheFailedRuns:    true,
			WatchedEffectRoots: nil,
		},
		GC: GC{
			KeepLastCheckpoints: 100,
			KeepRunsFor:         Duration(14 * 24 * 60 * 60 * 1e9), // 14d
			KeepRestoresFor:     Duration(14 * 24 * 60 * 60 * 1e9), // 14d
		},
		Diff: Diff{
			Algorithm: DefaultDiffAlgorithm(),
		},
		Locks: Locks{
			Timeout: Duration(5 * 1e9), // 5s
		},
		UI: UI{
			Time: DefaultTimeDisplay(),
		},
	}
}

// Strict returns the default configuration tightened for the strict profile:
// the worktree index is never trusted, so each scoped file is rehashed.
func Strict() Config {
	c := Defaults()
	c.Hashing.TrustMode = TrustStrict
	return c
}

// ValidationError reports one or more invalid configuration fields.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return "invalid config: " + e.Problems[0]
	}
	return fmt.Sprintf("invalid config:\n  - %s", strings.Join(e.Problems, "\n  - "))
}

// Validate checks the configuration for self-consistency. The ParseX
// constructors are the normal entry point, but Go allows any int to be cast to
// an enum type, so Validate re-checks enum membership rather than trusting that
// every value arrived through a parser. It also enforces non-negative counts
// and sizes and a non-empty scope.
func (c Config) Validate() error {
	var p []string
	if len(c.Scope.Include) == 0 {
		p = append(p, "scope.include must list at least one path")
	}
	if len(c.Run.DefaultScope) == 0 {
		p = append(p, "run.default_scope must list at least one path")
	}
	// Include and default_scope decide what awa reads, so their paths must be
	// relative to the project root and must not escape it. Exclude lists only
	// subtract from that set, so an escaping element is harmless — they need
	// only be non-empty.
	p = append(p, pathProblems("scope.include", c.Scope.Include, mustStayInRoot)...)
	p = append(p, pathProblems("run.default_scope", c.Run.DefaultScope, mustStayInRoot)...)
	// Validate the user's additive excludes (not the merged effective lists) so a
	// blank entry is reported at the field and index the user actually wrote.
	p = append(p, pathProblems("scope.extra_excludes", c.Scope.ExtraExcludes, nonEmptyOnly)...)
	p = append(p, pathProblems("history.extra_excludes", c.History.ExtraExcludes, nonEmptyOnly)...)
	p = append(p, pathProblems("run.extra_excludes", c.Run.ExtraExcludes, nonEmptyOnly)...)
	p = append(p, effectRootSelectorProblems(c.Run.WatchedEffectRoots)...)
	p = append(p, envAllowlistProblems(c.Run.EnvAllowlist)...)
	if !c.Hashing.TrustMode.Valid() {
		p = append(p, "hashing.trust_mode is not a known trust mode")
	}
	if !c.Hashing.LargeFilePolicy.Valid() {
		p = append(p, "hashing.large_file_policy is not a known policy")
	}
	if c.Scope.SymlinkMaxDepth < 0 {
		p = append(p, "scope.symlink_max_depth must not be negative")
	}
	if c.Hashing.MaxFileSize <= 0 {
		p = append(p, "hashing.max_file_size must be positive")
	}
	if c.Checkpoint.DiffContext < 0 {
		p = append(p, "checkpoint.diff_context must not be negative")
	}
	if c.Run.MaxStdoutSize <= 0 {
		p = append(p, "run.max_stdout_size must be positive")
	}
	if c.Run.MaxStderrSize <= 0 {
		p = append(p, "run.max_stderr_size must be positive")
	}
	if c.Run.TTL < 0 {
		p = append(p, "run.ttl must not be negative")
	}
	if c.GC.KeepLastCheckpoints < 0 {
		p = append(p, "gc.keep_last_checkpoints must not be negative")
	}
	if c.GC.KeepRunsFor < 0 {
		p = append(p, "gc.keep_runs_for must not be negative")
	}
	if c.GC.KeepRestoresFor < 0 {
		p = append(p, "gc.keep_restores_for must not be negative")
	}
	if !c.Diff.Algorithm.Valid() {
		p = append(p, "diff.algorithm is not a known algorithm")
	}
	if !c.UI.Time.Valid() {
		p = append(p, "ui.time is not a known time display")
	}
	if c.Locks.Timeout < 0 {
		p = append(p, "locks.timeout must not be negative")
	}
	if len(p) > 0 {
		return &ValidationError{Problems: p}
	}
	return nil
}

// pathRule selects how strictly pathProblems checks a list of project paths.
type pathRule bool

const (
	// nonEmptyOnly requires each element to be non-empty.
	nonEmptyOnly pathRule = false
	// mustStayInRoot additionally requires each element to be relative and to
	// stay within the project root.
	mustStayInRoot pathRule = true
)

// pathProblems validates a list of project-relative paths and returns one
// message per offending element. Empty elements are always rejected; under
// mustStayInRoot, absolute paths and paths that escape the root (via "..") are
// rejected too.
func pathProblems(field string, items []string, rule pathRule) []string {
	var probs []string
	for i, it := range items {
		if strings.TrimSpace(it) == "" {
			probs = append(probs, fmt.Sprintf("%s[%d] must not be empty", field, i))
			continue
		}
		if rule == nonEmptyOnly {
			continue
		}
		if filepath.IsAbs(it) {
			probs = append(probs, fmt.Sprintf("%s[%d] %q must be relative to the project root", field, i, it))
			continue
		}
		if escapesRoot(it) {
			probs = append(probs, fmt.Sprintf("%s[%d] %q must not escape the project root", field, i, it))
		}
	}
	return probs
}

func effectRootSelectorProblems(selectors []string) []string {
	var probs []string
	for i, sel := range selectors {
		if p := effectRootSelectorProblem(sel); p != "" {
			probs = append(probs, fmt.Sprintf("run.extra_effect_roots[%d] %s", i, p))
		}
	}
	return probs
}

func effectRootSelectorProblem(sel string) string {
	switch {
	case strings.TrimSpace(sel) == "":
		return "must not be empty"
	case sel != strings.TrimSpace(sel):
		return fmt.Sprintf("%q must not have leading or trailing whitespace", sel)
	case strings.ContainsAny(sel, `\:`):
		return fmt.Sprintf(`%q must not contain a backslash or a volume separator: "/" is the path separator on every platform`, sel)
	}
	if !strings.Contains(sel, "/") {
		// Every other single name published releases accept stays accepted, including
		// ".git" and ".awa": the path form's protected-component rule deliberately does
		// not reach here, because narrowing this set breaks a valid v0.1.x config.
		if sel == "." || sel == ".." {
			return fmt.Sprintf("%q must be a directory name, not %q", sel, sel)
		}
		return ""
	}
	switch {
	case strings.HasPrefix(sel, "/"):
		return fmt.Sprintf("%q must be relative to the project root", sel)
	case strings.HasSuffix(sel, "/"):
		return fmt.Sprintf("%q must not end in a slash: name the directory itself", sel)
	}
	// No component is trimmed: whitespace inside one names a real directory (" bin"),
	// unlike padding around the whole value, which the name form has always rejected.
	for _, comp := range strings.Split(sel, "/") {
		switch {
		case comp == "":
			return fmt.Sprintf("%q must not have an empty path component", sel)
		case comp == "." || comp == "..":
			return fmt.Sprintf("%q must be a clean project-relative path, without a %q or %q component", sel, ".", "..")
		case slices.Contains(protectedExcludes, comp):
			return fmt.Sprintf("%q must not select protected state: awa never observes a %q component", sel, comp)
		}
	}
	return ""
}

// envAllowlistProblems validates run.env_allowlist. Every entry names one
// environment variable that feeds the run key, so a blank name, a name carrying an
// "=" (the name/value separator, never part of a name), a duplicate, or a name that
// the built-in baseline already covers would produce a meaningless, redundant, or —
// on Windows, where names are case-insensitive — silently dropped key. Names are
// compared case-insensitively so a config is portable: one that is ambiguous on a
// case-insensitive platform is rejected everywhere rather than behaving differently.
func envAllowlistProblems(names []string) []string {
	var probs []string
	baseline := make(map[string]struct{})
	for _, b := range BaselineEnvNames("windows") { // union across platforms
		baseline[strings.ToUpper(b)] = struct{}{}
	}
	seen := make(map[string]struct{}, len(names))
	for i, name := range names {
		if name == "" {
			probs = append(probs, fmt.Sprintf("run.env_allowlist[%d] must not be empty", i))
			continue
		}
		if strings.ContainsRune(name, '=') {
			probs = append(probs, fmt.Sprintf("run.env_allowlist[%d] %q must be a name, not name=value", i, name))
			continue
		}
		// The reserved marker is checked before the baseline and duplicate branches so it
		// always reports its own reason. It is not an inherited baseline name the user
		// should "remove as redundant": it is injected by awa, is not configurable at all,
		// and telling the user otherwise would misdescribe what the run does.
		if IsReservedEnvName(name) {
			probs = append(probs, fmt.Sprintf("run.env_allowlist[%d] %q is reserved by awa: it is injected into every executed child as %s=%s and cannot be configured", i, name, WrapperMarkerName, WrapperMarkerValue))
			continue
		}
		fold := strings.ToUpper(name)
		if _, dup := seen[fold]; dup {
			probs = append(probs, fmt.Sprintf("run.env_allowlist[%d] %q duplicates an earlier name (case-insensitively)", i, name))
			continue
		}
		if _, base := baseline[fold]; base {
			probs = append(probs, fmt.Sprintf("run.env_allowlist[%d] %q is already in the built-in baseline; remove it", i, name))
			continue
		}
		seen[fold] = struct{}{}
	}
	return probs
}

// escapesRoot reports whether a cleaned relative path steps above the project
// root.
func escapesRoot(p string) bool {
	clean := filepath.ToSlash(filepath.Clean(p))
	return clean == ".." || strings.HasPrefix(clean, "../")
}
