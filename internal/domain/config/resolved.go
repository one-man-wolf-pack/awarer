package config

import (
	"fmt"
	"maps"
	"slices"
)

// Layer identifies where an effective configuration value came from. It is a typed
// token rather than a raw string so a mistyped or invented layer name cannot reach a
// consumer, and its String form is the stable wire token used in JSON and messages.
type Layer int

const (
	// LayerDefault is the product default: no file or flag set the value.
	LayerDefault Layer = iota
	// LayerShared is the committable root awa.toml.
	LayerShared
	// LayerLocal is the private .awa/config.toml override.
	LayerLocal
	// LayerConfig is an explicit --config invocation override.
	LayerConfig
	// LayerFlag is a CLI flag override (highest precedence).
	LayerFlag
)

// Valid reports whether l is a known layer.
func (l Layer) Valid() bool { return l >= LayerDefault && l <= LayerFlag }

// String returns the stable wire token for the layer.
func (l Layer) String() string {
	switch l {
	case LayerDefault:
		return "default"
	case LayerShared:
		return "shared"
	case LayerLocal:
		return "local"
	case LayerConfig:
		return "config"
	case LayerFlag:
		return "flag"
	default:
		return "unknown"
	}
}

// HasFile reports whether this layer is backed by a config file on disk (so a
// value's source path is meaningful). The product defaults and a CLI flag have no
// file.
func (l Layer) HasFile() bool {
	return l == LayerShared || l == LayerLocal || l == LayerConfig
}

// LayerFact records one config layer's identity, source path, and whether its file
// existed when the config was loaded. It is a validated immutable value: NewLayerFact
// rejects a fact that could not describe a real config file — an unknown or
// non-file-backed layer, or an empty path — so a LayerFact in hand is trustworthy on
// its own, not merely once the aggregate re-checks it.
type LayerFact struct {
	layer  Layer
	path   string
	exists bool
}

// NewLayerFact builds a validated layer fact. It fails rather than returning a
// fabricated value when the layer is not a file-backed config layer or the path is
// empty, so an invalid fact cannot exist. The zero LayerFact is still reachable by a
// caller that ignores this error, so the aggregate re-guards against it.
func NewLayerFact(layer Layer, path string, exists bool) (LayerFact, error) {
	if !layer.Valid() || !layer.HasFile() {
		return LayerFact{}, fmt.Errorf("layer fact: %q is not a file-backed config layer", layer)
	}
	if path == "" {
		return LayerFact{}, fmt.Errorf("layer fact: %s layer has an empty path", layer)
	}
	return LayerFact{layer: layer, path: path, exists: exists}, nil
}

// Layer returns the fact's layer.
func (f LayerFact) Layer() Layer { return f.layer }

// Path returns the layer's config file path.
func (f LayerFact) Path() string { return f.path }

// Exists reports whether the layer's file existed at load time.
func (f LayerFact) Exists() bool { return f.exists }

// keySpec pairs a dotted config key ("section.key") with the predicate deciding
// whether two configs differ at that key. It is the one descriptor from which the
// key namespace (Keys), the origin seed (DefaultOrigins), and the value-agreement
// guard (changedKeys) are all derived, so a key and its comparison can never drift
// into two parallel lists — adding a key here adds it everywhere, with its own
// comparator. A test pins each comparator to exactly its key, and the whole set to
// the decoder's wire tags.
type keySpec struct {
	key     string
	differs func(a, b Config) bool
}

var keySpecs = []keySpec{
	{"scope.include", func(a, b Config) bool { return !slices.Equal(a.Scope.Include, b.Scope.Include) }},
	{"scope.extra_excludes", func(a, b Config) bool { return !slices.Equal(a.Scope.ExtraExcludes, b.Scope.ExtraExcludes) }},
	{"scope.use_gitignore", func(a, b Config) bool { return a.Scope.UseGitignore != b.Scope.UseGitignore }},
	{"scope.use_awaignore", func(a, b Config) bool { return a.Scope.UseAwaignore != b.Scope.UseAwaignore }},
	{"scope.follow_symlinks", func(a, b Config) bool { return a.Scope.FollowSymlinks != b.Scope.FollowSymlinks }},
	{"scope.symlink_max_depth", func(a, b Config) bool { return a.Scope.SymlinkMaxDepth != b.Scope.SymlinkMaxDepth }},
	{"scope.allow_symlink_root_escape", func(a, b Config) bool { return a.Scope.AllowSymlinkRootEscape != b.Scope.AllowSymlinkRootEscape }},
	{"history.extra_excludes", func(a, b Config) bool { return !slices.Equal(a.History.ExtraExcludes, b.History.ExtraExcludes) }},
	{"hashing.trust_mode", func(a, b Config) bool { return a.Hashing.TrustMode != b.Hashing.TrustMode }},
	{"hashing.max_file_size", func(a, b Config) bool { return a.Hashing.MaxFileSize != b.Hashing.MaxFileSize }},
	{"hashing.large_file_policy", func(a, b Config) bool { return a.Hashing.LargeFilePolicy != b.Hashing.LargeFilePolicy }},
	{"checkpoint.store_file_contents", func(a, b Config) bool { return a.Checkpoint.StoreFileContents != b.Checkpoint.StoreFileContents }},
	{"checkpoint.diff_context", func(a, b Config) bool { return a.Checkpoint.DiffContext != b.Checkpoint.DiffContext }},
	{"checkpoint.rename_detection", func(a, b Config) bool { return a.Checkpoint.RenameDetection != b.Checkpoint.RenameDetection }},
	{"run.default_scope", func(a, b Config) bool { return !slices.Equal(a.Run.DefaultScope, b.Run.DefaultScope) }},
	{"run.extra_excludes", func(a, b Config) bool { return !slices.Equal(a.Run.ExtraExcludes, b.Run.ExtraExcludes) }},
	{"run.use_gitignore", func(a, b Config) bool { return a.Run.UseGitignore != b.Run.UseGitignore }},
	{"run.use_awaignore", func(a, b Config) bool { return a.Run.UseAwaignore != b.Run.UseAwaignore }},
	{"run.env_allowlist", func(a, b Config) bool { return !slices.Equal(a.Run.EnvAllowlist, b.Run.EnvAllowlist) }},
	{"run.ttl", func(a, b Config) bool { return a.Run.TTL != b.Run.TTL }},
	{"run.max_stdout_size", func(a, b Config) bool { return a.Run.MaxStdoutSize != b.Run.MaxStdoutSize }},
	{"run.max_stderr_size", func(a, b Config) bool { return a.Run.MaxStderrSize != b.Run.MaxStderrSize }},
	{"run.capture_output", func(a, b Config) bool { return a.Run.CaptureOutput != b.Run.CaptureOutput }},
	{"run.cache_failed_runs", func(a, b Config) bool { return a.Run.CacheFailedRuns != b.Run.CacheFailedRuns }},
	{"run.extra_effect_roots", func(a, b Config) bool { return !slices.Equal(a.Run.WatchedEffectRoots, b.Run.WatchedEffectRoots) }},
	{"gc.keep_last_checkpoints", func(a, b Config) bool { return a.GC.KeepLastCheckpoints != b.GC.KeepLastCheckpoints }},
	{"gc.keep_runs_for", func(a, b Config) bool { return a.GC.KeepRunsFor != b.GC.KeepRunsFor }},
	{"gc.keep_restores_for", func(a, b Config) bool { return a.GC.KeepRestoresFor != b.GC.KeepRestoresFor }},
	{"diff.algorithm", func(a, b Config) bool { return a.Diff.Algorithm != b.Diff.Algorithm }},
	{"locks.timeout", func(a, b Config) bool { return a.Locks.Timeout != b.Locks.Timeout }},
	{"ui.time", func(a, b Config) bool { return a.UI.Time != b.UI.Time }},
}

// configKeys is the ordered dotted-key namespace, projected from keySpecs so it
// cannot drift from the comparators.
var configKeys = func() []string {
	keys := make([]string, len(keySpecs))
	for i, s := range keySpecs {
		keys[i] = s.key
	}
	return keys
}()

// configKeySet is the membership index for configKeys, built once for O(1) lookups.
var configKeySet = func() map[string]bool {
	set := make(map[string]bool, len(configKeys))
	for _, k := range configKeys {
		set[k] = true
	}
	return set
}()

func isConfigKey(key string) bool { return configKeySet[key] }

// Keys returns the closed, ordered set of dotted config keys. It is the single
// authoritative namespace for provenance: an origin map keys against exactly these,
// so a typo'd or invented key is a construction error, not a silent no-op.
func Keys() []string { return slices.Clone(configKeys) }

// DefaultOrigins returns a complete origin map attributing every config key to the
// product default layer. It is the seed the layered loader overwrites per present
// key so provenance is always complete, and the honest starting point for any
// consumer (or test) that wraps a config without full layer tracking.
func DefaultOrigins() map[string]Layer {
	origins := make(map[string]Layer, len(configKeys))
	for _, k := range configKeys {
		origins[k] = LayerDefault
	}
	return origins
}

// OriginsFrom derives a complete, value-consistent provenance map for a config that
// was assembled outside the layered loader (a config.Strict() profile, a test
// fixture): every key whose value differs from the product default is attributed to
// layer, the rest stay default. This keeps a hand-built config from tripping
// NewResolvedConfig's value/origin agreement check by falsely claiming an overridden
// value came from the default layer. When layer is file-backed the caller must still
// supply a present LayerFact for it.
func OriginsFrom(c Config, layer Layer) map[string]Layer {
	origins := DefaultOrigins()
	for k := range changedKeys(c) {
		origins[k] = layer
	}
	return origins
}

// changedKeys reports which config keys hold a value that differs from the product
// default, running each keySpec's comparator against Defaults(). It is the guard
// behind the resolved-config constructor's value/origin agreement check and
// OriginsFrom. A test pins each comparator to exactly its key.
func changedKeys(c Config) map[string]bool {
	d := Defaults()
	out := map[string]bool{}
	for _, s := range keySpecs {
		if s.differs(c, d) {
			out[s.key] = true
		}
	}
	return out
}

// ResolvedConfig is the immutable result of composing the configuration layers: the
// effective Config plus its provenance — the layer that set each key and the facts
// (path, existence) of the layers that participated, all from one read. It is the
// shared boundary consumers pass around instead of a config and a separately
// gathered, possibly-contradicting provenance. Its fields are closed, its inputs are
// deep-copied and validated at construction, and its accessors return copies, so no
// consumer can reorder the layers, mutate the config or origins, or pair the config
// with a provenance the constructor would have rejected.
type ResolvedConfig struct {
	cfg     Config
	origins map[string]Layer
	layers  []LayerFact
}

// NewResolvedConfig builds a validated, deep-copied resolved config from a composed
// Config, its per-key origin map, and the ordered layer facts. It is the constructor
// the layered loader calls; it returns an error rather than a fabricated value when
// the aggregate is internally inconsistent, so an illegal ResolvedConfig cannot
// exist. It enforces the full aggregate contract:
//
//   - the Config is self-consistent (Config.Validate);
//   - every LayerFact names a valid, file-backed layer with a non-empty path;
//   - the facts are in strict ascending precedence order with no duplicate layer;
//   - every origin names a valid layer, and a file-backed origin has a matching
//     present (exists=true) layer fact — a value cannot come from a file that was
//     not read.
//
// The provenance map must be complete and schema-identical: it names every config
// key (Keys) and only those keys, so a value can never fall through to a falsely
// reported default and a typo'd key cannot masquerade as real provenance.
//
// The Config and every slice/map are deep-copied, so a later mutation of the
// caller's inputs cannot reach into the value.
func NewResolvedConfig(cfg Config, origins map[string]Layer, layers []LayerFact) (ResolvedConfig, error) {
	if err := cfg.Validate(); err != nil {
		return ResolvedConfig{}, fmt.Errorf("resolved config: %w", err)
	}
	present := make(map[Layer]bool, len(layers))
	lastPrec := -1
	for _, f := range layers {
		if !f.layer.Valid() || !f.layer.HasFile() {
			return ResolvedConfig{}, fmt.Errorf("resolved config: layer fact %q is not a file-backed layer", f.layer)
		}
		if f.path == "" {
			return ResolvedConfig{}, fmt.Errorf("resolved config: %s layer fact has an empty path", f.layer)
		}
		if prec := int(f.layer); prec <= lastPrec {
			return ResolvedConfig{}, fmt.Errorf("resolved config: layer facts are not in strict precedence order (%s out of order or duplicated)", f.layer)
		} else {
			lastPrec = prec
		}
		if f.exists {
			present[f.layer] = true
		}
	}
	if len(origins) != len(configKeys) {
		return ResolvedConfig{}, fmt.Errorf("resolved config: provenance is incomplete (%d origins for %d keys); seed from DefaultOrigins", len(origins), len(configKeys))
	}
	changed := changedKeys(cfg)
	for key, origin := range origins {
		if !isConfigKey(key) {
			return ResolvedConfig{}, fmt.Errorf("resolved config: origin names unknown config key %q", key)
		}
		if !origin.Valid() {
			return ResolvedConfig{}, fmt.Errorf("resolved config: origin of %q is an invalid layer", key)
		}
		if origin.HasFile() && !present[origin] {
			return ResolvedConfig{}, fmt.Errorf("resolved config: %q is attributed to the %s layer, which has no present file fact", key, origin)
		}
		// A key attributed to the default layer must actually hold the product
		// default: a complete-but-false provenance (a changed value claiming
		// "default") would let doctor/status report the wrong source with confidence.
		// A file/flag origin may legitimately match the default (an explicit override
		// of the same value), so the check is one-directional.
		if origin == LayerDefault && changed[key] {
			return ResolvedConfig{}, fmt.Errorf("resolved config: %q is attributed to the default layer but its value differs from the product default", key)
		}
	}
	return ResolvedConfig{
		cfg:     cfg.Clone(),
		origins: maps.Clone(origins),
		layers:  slices.Clone(layers),
	}, nil
}

// Config returns a deep copy of the effective configuration, so a consumer mutating
// a slice field of the result cannot reach back into the aggregate.
func (r ResolvedConfig) Config() Config { return r.cfg.Clone() }

// Layers returns a copy of the ordered layer facts (lowest precedence first).
func (r ResolvedConfig) Layers() []LayerFact { return slices.Clone(r.layers) }

// Origins returns a copy of the per-key origin map. A key absent from the map (which
// the loader avoids by seeding every key) reads as the default layer via OriginOf.
func (r ResolvedConfig) Origins() map[string]Layer { return maps.Clone(r.origins) }

// OriginOf returns the layer that set the effective value of a dotted config key
// (e.g. "run.env_allowlist"), or LayerDefault when no layer overrode it.
func (r ResolvedConfig) OriginOf(key string) Layer {
	if l, ok := r.origins[key]; ok {
		return l
	}
	return LayerDefault
}

// SourcePathOf returns the config file path that set a dotted key, or "" when the
// value came from the product defaults or a CLI flag (which have no file). It lets a
// diagnostic point at the exact layer a user must edit — the shared awa.toml, the
// local .awa/config.toml, or an explicit --config file — rather than assuming the
// local path.
func (r ResolvedConfig) SourcePathOf(key string) string {
	origin := r.OriginOf(key)
	if !origin.HasFile() {
		return ""
	}
	for _, f := range r.layers {
		if f.layer == origin {
			return f.path
		}
	}
	return ""
}
