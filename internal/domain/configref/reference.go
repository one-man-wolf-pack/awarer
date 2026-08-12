// Package configref is the single source of truth for awa's configuration
// reference: the ordered sections and keys, their value types, product defaults,
// and the "when to change this" guidance that make configuration discoverable
// from the binary.
//
// It exists so the three discovery surfaces — `awa help config`, the annotated
// `awa config template`, and the README/spec reference — render from one model
// instead of three hand-maintained copies that drift. Defaults are pulled from
// config.Defaults() and rendered through the same canonical String() forms the
// rest of awa uses, so a default can never be documented differently from how it
// behaves. A coverage test pins the reference to the decoder's wire tags: a key
// that is implemented but missing here, or documented here but not implemented,
// is a test failure.
//
// The package is pure: it imports only domain/config, performs no I/O, and knows
// nothing about TOML decoding, the filesystem, or the CLI.
package configref

import (
	"strconv"
	"strings"

	"awarer/internal/domain/config"
)

// Field describes one configuration key: its TOML spelling, the product default
// rendered in canonical form, a human type label, a one-line summary of what it
// controls, and the common reason to change it.
type Field struct {
	Key          string
	Default      string
	Type         string
	Summary      string
	WhenToChange string
}

// Section groups the fields of one TOML table.
type Section struct {
	Name   string
	Title  string
	Intro  string
	Fields []Field
}

// Sections returns the ordered configuration reference, with defaults rendered
// from config.Defaults() so the reference and the runtime defaults can never
// disagree. The order is the stable configuration-reference order.
func Sections() []Section {
	d := config.Defaults()
	return []Section{
		{
			Name:  "scope",
			Title: "Scan scope (history/checkpoint family and the shared base)",
			Intro: "The common scan boundary shared by awa checkpoint/changes/diff and, through extra_excludes, the run input scan. Only additive user values live here; the product baseline and protected defaults are built in.",
			Fields: []Field{
				{"include", list(d.Scope.Include), "path list", "Directories/files the scan includes; an explicit narrowed scope outranks every configurable and default exclude.", "Narrow to a subtree, or bring back a path a baseline/ignore rule would otherwise drop."},
				{"extra_excludes", list(d.Scope.ExtraExcludes), "path list", "Additive common excludes applied to both the history family and the run input scan, on top of the built-in baseline.", "Exclude a project-specific directory from all scans."},
				{"use_gitignore", boolean(d.Scope.UseGitignore), "bool", "Whether .gitignore participates as an ignore source for the history family. Off by default: git ignores answer \"what not to commit\", not \"what can affect a command\".", "Turn on only if your .gitignore genuinely matches what should not be observed."},
				{"use_awaignore", boolean(d.Scope.UseAwaignore), "bool", "Whether .awaignore participates for the history family. On by default; .awaignore is awa's committable native ignore file.", "Turn off to ignore .awaignore files entirely."},
				{"follow_symlinks", boolean(d.Scope.FollowSymlinks), "bool", "Whether the scanner follows symbolic links.", "Enable when scoped content lives behind symlinks you trust."},
				{"symlink_max_depth", integer(d.Scope.SymlinkMaxDepth), "int", "Maximum symlink chain depth followed when follow_symlinks is on.", "Raise or lower the guard against deep or cyclic link chains."},
				{"allow_symlink_root_escape", boolean(d.Scope.AllowSymlinkRootEscape), "bool", "Whether a followed symlink may resolve outside the project root.", "Enable only when you deliberately depend on out-of-root linked content."},
			},
		},
		{
			Name:  "history",
			Title: "History-only excludes",
			Intro: "Excludes that affect awa checkpoint/changes/diff but never the run input scan, so a generated artifact can stay out of local history while remaining a real command input. The history family inherits its ignore-source policy from [scope].",
			Fields: []Field{
				{"extra_excludes", list(d.History.ExtraExcludes), "path list", "Additive history-only excludes, on top of the built-in history defaults (dist, build, coverage).", "Keep a generated directory out of checkpoints/diffs while runs still key it."},
			},
		},
		{
			Name:  "hashing",
			Title: "Content hashing and index trust",
			Intro: "Local content identity is fixed: awa hashes worktree content with BLAKE3 and persists every local digest with the blake3: prefix followed by lowercase hex. The primitive is product behavior, not a setting — there is no key here to change it, and only the policy governing how it is applied is configurable. The SHA-256 you see in a release checksum file, an exported documentation manifest, or a site asset digest is a separate external integrity convention and takes no part in local evidence identity.",
			Fields: []Field{
				{"trust_mode", d.Hashing.TrustMode.String(), "enum: normal | strict | fast", "How aggressively the worktree index is trusted to skip rehashing unchanged files. fast compares size and mtime only, which can miss a same-size same-mtime rewrite, so a run observed under it is recorded but never published as reusable.", "Use strict for integrity-critical work; use fast only where losing run reuse entirely is an acceptable price for scan speed."},
				{"max_file_size", d.Hashing.MaxFileSize.String(), "byte size (B/KiB/MiB/GiB/TiB)", "Threshold above which large_file_policy applies.", "Raise to store larger blobs, or lower to keep the store lean."},
				{"large_file_policy", d.Hashing.LargeFilePolicy.String(), "enum: hash-only | store | skip", "What awa does with files above max_file_size: hash-only keeps them in tree hashes without storing blobs.", "Use store to keep large blob content, or skip to leave them out of hashing entirely."},
			},
		},
		{
			Name:  "checkpoint",
			Title: "Checkpoint recording",
			Fields: []Field{
				{"store_file_contents", boolean(d.Checkpoint.StoreFileContents), "bool", "Whether checkpoint blobs are stored (affects checkpoint policy identity).", "Turn off to record checkpoint hashes without keeping content."},
				{"diff_context", integer(d.Checkpoint.DiffContext), "int", "Default number of context lines in checkpoint diffs.", "Adjust the default hunk context."},
				{"rename_detection", boolean(d.Checkpoint.RenameDetection), "bool", "Whether diffs detect renames.", "Turn off for pure add/delete diffs."},
			},
		},
		{
			Name:  "run",
			Title: "Cached command execution",
			Intro: "The run cache. extra_excludes is additive run-only; the effective run input excludes omit the history-only defaults so a build artifact stays a real command input. extra_effect_roots watches generated-output directories a command reads but does not produce (see \"Effect roots vs excludes\"). A wrapped command never inherits the full ambient environment: it receives the built-in baseline, whatever env_allowlist adds, and the fixed advisory marker AWA_RUN=1, and every one of those participates in the cache key. Run `awa config effective` to see the resolved inherited names and injected facts.",
			Fields: []Field{
				{"default_scope", list(d.Run.DefaultScope), "path list", "The run input scan scope when --scope is not given.", "Narrow the default set of inputs a run keys on."},
				{"extra_excludes", list(d.Run.ExtraExcludes), "path list", "Additive run-only excludes, on top of the baseline. Excluding a path weakens what the run can observe, so do it intentionally.", "Exclude self-generated output a command writes during the run."},
				{"use_gitignore", boolean(d.Run.UseGitignore), "bool", "Whether .gitignore participates as an ignore source for the run input scan (keyed into the run cache). Off by default.", "Turn on only if .gitignore matches what should not affect a command."},
				{"use_awaignore", boolean(d.Run.UseAwaignore), "bool", "Whether .awaignore participates for the run input scan. On by default.", "Turn off to ignore .awaignore for runs."},
				{"env_allowlist", list(d.Run.EnvAllowlist), "name list", "Environment variable names, on top of the built-in baseline, folded into the run cache key and passed to the child. The baseline already covers execution (PATH, HOME, SHELL, TMPDIR, the Windows equivalents) and the caller's locale (LANG, LANGUAGE, LC_*), so listing one of those is rejected as redundant; AWA_RUN is reserved because awa injects it.", "Add a variable your commands depend on so it keys the cache."},
				{"ttl", d.Run.TTL.String(), "duration (d/h/m/s)", "Freshness window a reusable run entry stays eligible for a hit.", "Shorten or lengthen how long a cached result is served."},
				{"max_stdout_size", d.Run.MaxStdoutSize.String(), "byte size", "Capture limit for stdout; output beyond it is truncated in stored evidence.", "Raise for commands with very large output."},
				{"max_stderr_size", d.Run.MaxStderrSize.String(), "byte size", "Capture limit for stderr.", "Raise for commands with very large error output."},
				{"capture_output", boolean(d.Run.CaptureOutput), "bool", "Whether run output is captured and stored for replay/inspection.", "Turn off to run without recording output."},
				{"cache_failed_runs", boolean(d.Run.CacheFailedRuns), "bool", "Whether non-zero-exit runs are cached.", "Turn off to always re-run failed commands."},
				{"extra_effect_roots", list(d.Run.WatchedEffectRoots), "name/path list", "Additive watched generated-output selectors on top of the built-ins. Each is either a directory NAME matched wherever it appears (\"bin\") or an exact project-relative PATH matched only there (\"artifacts/bin\"). Literal, never globs. Additive only — a built-in cannot be silenced.", "Watch a generated directory a command reads so later changes to it invalidate reuse."},
			},
		},
		{
			Name:  "gc",
			Title: "Garbage collection retention",
			Fields: []Field{
				{"keep_last_checkpoints", integer(d.GC.KeepLastCheckpoints), "int", "How many recent checkpoints gc retains.", "Keep more or fewer checkpoints."},
				{"keep_runs_for", d.GC.KeepRunsFor.String(), "duration", "How long run records are retained by gc.", "Extend or shorten run history retention."},
				{"keep_restores_for", d.GC.KeepRestoresFor.String(), "duration", "How long a restore's pre-restore recovery observation — the evidence an applied restore can be undone from — is retained by gc. Independent of keep_runs_for.", "Keep restore undo evidence longer (or shorter) than run history."},
			},
		},
		{
			Name:  "diff",
			Title: "Content diff rendering",
			Fields: []Field{
				{"algorithm", d.Diff.Algorithm.String(), "enum: histogram | myers", "Text diff engine; histogram anchors on rare lines and falls back to myers locally.", "Force myers for a stable, classic diff."},
			},
		},
		{
			Name:  "locks",
			Title: "Writer/collector lock acquisition",
			Fields: []Field{
				{"timeout", d.Locks.Timeout.String(), "duration", "How long any interlock wait may last before the command fails with the lock-timeout exit code — including gc waiting for the exclusive collector lease another gc holds. A writer's lock is the one case that is not a wait: gc does not wait writers out, it suppresses its whole destructive pass, reports the lock as a blocked candidate, deletes nothing, and exits 0. Zero means fail fast; negative is a config error.", "Raise on busy shared checkouts, or set 0 to never wait."},
			},
		},
		{
			Name:  "ui",
			Title: "Human presentation",
			Intro: "Human output only; JSON always uses machine timestamps and never carries relative prose.",
			Fields: []Field{
				{"time", d.UI.Time.String(), "enum: relative | local | utc", "How human output renders timestamps (log, run history). A --time flag overrides per invocation.", "Prefer absolute local or utc timestamps over relative ages."},
			},
		},
	}
}

// list renders a string slice as a TOML array literal, e.g. ["."] or [].
func list(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	quoted := make([]string, len(items))
	for i, it := range items {
		quoted[i] = strconv.Quote(it)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func boolean(b bool) string { return strconv.FormatBool(b) }

func integer(n int) string { return strconv.Itoa(n) }
