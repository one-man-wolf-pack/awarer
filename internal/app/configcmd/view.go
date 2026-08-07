package configcmd

import (
	"runtime"
	"slices"

	"awarer/internal/domain/config"
	"awarer/internal/domain/runcache"
)

// View is a JSON- and human-friendly projection of a domain config. Enums,
// byte sizes, and durations are rendered as their canonical string forms (e.g.
// "normal", "50MiB", "7d") so the effective output reads like a config file.
type View struct {
	Scope      ScopeView      `json:"scope"`
	History    HistoryView    `json:"history"`
	Hashing    HashingView    `json:"hashing"`
	Checkpoint CheckpointView `json:"checkpoint"`
	Run        RunView        `json:"run"`
	GC         GCView         `json:"gc"`
	Diff       DiffView       `json:"diff"`
	Locks      LocksView      `json:"locks"`
	UI         UIView         `json:"ui"`
}

// UIView shows human-presentation policy. Time governs human timestamp rendering
// only; JSON always uses machine timestamps.
type UIView struct {
	Time string `json:"time"`
}

// ScopeView shows both the user's additive ExtraExcludes (what a config file
// would carry) and the EffectiveExcludes the history/checkpoint family actually
// scans under — the product baseline/history defaults plus the scope and history
// additions — so the resolved policy is visible without implying the file should
// repeat the defaults.
type ScopeView struct {
	Include                []string `json:"include"`
	ExtraExcludes          []string `json:"extra_excludes"`
	EffectiveExcludes      []string `json:"effective_excludes"`
	UseGitignore           bool     `json:"use_gitignore"`
	UseAwaignore           bool     `json:"use_awaignore"`
	FollowSymlinks         bool     `json:"follow_symlinks"`
	SymlinkMaxDepth        int      `json:"symlink_max_depth"`
	AllowSymlinkRootEscape bool     `json:"allow_symlink_root_escape"`
}

// HistoryView shows the history-only additive excludes, which affect awa checkpoint,
// awa changes, and awa diff but not the run input scan.
type HistoryView struct {
	ExtraExcludes []string `json:"extra_excludes"`
}

type HashingView struct {
	TrustMode       string `json:"trust_mode"`
	MaxFileSize     string `json:"max_file_size"`
	LargeFilePolicy string `json:"large_file_policy"`
}

type CheckpointView struct {
	StoreFileContents bool `json:"store_file_contents"`
	DiffContext       int  `json:"diff_context"`
	RenameDetection   bool `json:"rename_detection"`
}

type RunView struct {
	DefaultScope []string `json:"default_scope"`
	// ExtraExcludes is the user's additive run-only excludes; EffectiveExcludes
	// is what the run input scan resolves to (baseline plus the common scope and
	// run additions, with the history-only defaults deliberately omitted).
	ExtraExcludes     []string `json:"extra_excludes"`
	EffectiveExcludes []string `json:"effective_excludes"`
	UseGitignore      bool     `json:"use_gitignore"`
	UseAwaignore      bool     `json:"use_awaignore"`
	EnvAllowlist      []string `json:"env_allowlist"`
	// EffectiveEnvAllowlist is the platform effective set of *inherited* env names shown
	// for observability — the names a run reads from the caller, keys, and passes on when
	// present. Computed by runcache.EffectiveEnvNames; the baseline composition lives
	// there. It is names only: no ambient value is ever projected here.
	EffectiveEnvAllowlist []string `json:"effective_env_allowlist"`
	// InjectedEnv is what awa itself states into every executed child, as NAME=VALUE.
	// These are fixed product constants rather than inherited values, so — unlike the
	// inherited names — the value is shown. It is a separate field on purpose:
	// env_allowlist is user configuration, and folding an unconfigurable injected fact
	// into it would tell the user they can change something they cannot.
	InjectedEnv     []string `json:"injected_env"`
	TTL             string   `json:"ttl"`
	MaxStdoutSize   string   `json:"max_stdout_size"`
	MaxStderrSize   string   `json:"max_stderr_size"`
	CaptureOutput   bool     `json:"capture_output"`
	CacheFailedRuns bool     `json:"cache_failed_runs"`
	// ExtraEffectRoots is the user's additive watched generated-output roots;
	// EffectiveEffectRoots is the full watch list (built-in defaults plus the
	// additions) the run cache observes with a bounded stat signature.
	ExtraEffectRoots     []string `json:"extra_effect_roots"`
	EffectiveEffectRoots []string `json:"effective_effect_roots"`
}

type GCView struct {
	KeepLastCheckpoints int    `json:"keep_last_checkpoints"`
	KeepRunsFor         string `json:"keep_runs_for"`
	KeepRestoresFor     string `json:"keep_restores_for"`
}

type DiffView struct {
	Algorithm string `json:"algorithm"`
}

type LocksView struct {
	Timeout string `json:"timeout"`
}

// NewView projects a domain config into its string-rendered view. Slice fields
// are cloned so the view is a fully independent DTO: mutating it can never
// reach back into the source config's backing arrays.
func NewView(c config.Config) View {
	return View{
		Scope: ScopeView{
			Include:                slices.Clone(c.Scope.Include),
			ExtraExcludes:          slices.Clone(c.Scope.ExtraExcludes),
			EffectiveExcludes:      c.HistoryScanScope().Exclude,
			UseGitignore:           c.Scope.UseGitignore,
			UseAwaignore:           c.Scope.UseAwaignore,
			FollowSymlinks:         c.Scope.FollowSymlinks,
			SymlinkMaxDepth:        c.Scope.SymlinkMaxDepth,
			AllowSymlinkRootEscape: c.Scope.AllowSymlinkRootEscape,
		},
		History: HistoryView{
			ExtraExcludes: slices.Clone(c.History.ExtraExcludes),
		},
		Hashing: HashingView{
			TrustMode:       c.Hashing.TrustMode.String(),
			MaxFileSize:     c.Hashing.MaxFileSize.String(),
			LargeFilePolicy: c.Hashing.LargeFilePolicy.String(),
		},
		Checkpoint: CheckpointView{
			StoreFileContents: c.Checkpoint.StoreFileContents,
			DiffContext:       c.Checkpoint.DiffContext,
			RenameDetection:   c.Checkpoint.RenameDetection,
		},
		Run: RunView{
			DefaultScope:          slices.Clone(c.Run.DefaultScope),
			ExtraExcludes:         slices.Clone(c.Run.ExtraExcludes),
			EffectiveExcludes:     config.EffectiveRunExcludes(c.Scope.ExtraExcludes, c.Run.ExtraExcludes),
			UseGitignore:          c.Run.UseGitignore,
			UseAwaignore:          c.Run.UseAwaignore,
			EnvAllowlist:          slices.Clone(c.Run.EnvAllowlist),
			EffectiveEnvAllowlist: runcache.EffectiveEnvNames(runtime.GOOS, c.Run.EnvAllowlist),
			InjectedEnv:           injectedEnvAssignments(),
			TTL:                   c.Run.TTL.String(),
			MaxStdoutSize:         c.Run.MaxStdoutSize.String(),
			MaxStderrSize:         c.Run.MaxStderrSize.String(),
			CaptureOutput:         c.Run.CaptureOutput,
			CacheFailedRuns:       c.Run.CacheFailedRuns,
			ExtraEffectRoots:      slices.Clone(c.Run.WatchedEffectRoots),
			EffectiveEffectRoots:  config.EffectiveEffectRoots(c.Run.WatchedEffectRoots),
		},
		GC: GCView{
			KeepLastCheckpoints: c.GC.KeepLastCheckpoints,
			KeepRunsFor:         c.GC.KeepRunsFor.String(),
			KeepRestoresFor:     c.GC.KeepRestoresFor.String(),
		},
		Diff: DiffView{
			Algorithm: c.Diff.Algorithm.String(),
		},
		Locks: LocksView{
			Timeout: c.Locks.Timeout.String(),
		},
		UI: UIView{
			Time: c.UI.Time.String(),
		},
	}
}

// injectedEnvAssignments renders the fixed facts awa injects into an executed child in
// NAME=VALUE form, from the same domain owner the run path uses. It is derived, never
// configured, so it carries no origin layer.
func injectedEnvAssignments() []string {
	facts := runcache.InjectedEnvFacts()
	out := make([]string, 0, len(facts))
	for _, f := range facts {
		out = append(out, f.Assignment())
	}
	return out
}
