package configfile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	toml "github.com/pelletier/go-toml/v2"

	"awarer/internal/domain/config"
	"awarer/internal/infra/projfs"
)

// The layer identity of a config value is the typed config.Layer, defined in the
// domain package so it is shared by the loader, the config use cases, the status
// read-model, and doctor without any of them depending on each other. This package
// records origins as config.Layer and composes them into a config.ResolvedConfig.

// overlayWire mirrors the TOML document with every field a pointer, so an absent
// key decodes to nil and is distinguishable from a key present at its zero value.
// Composing overlays in precedence order is what makes the layered stack work:
// only present keys overwrite lower layers.
type overlayWire struct {
	Scope      *overlayScope      `toml:"scope"`
	History    *overlayHistory    `toml:"history"`
	Hashing    *overlayHashing    `toml:"hashing"`
	Checkpoint *overlayCheckpoint `toml:"checkpoint"`
	Run        *overlayRun        `toml:"run"`
	GC         *overlayGC         `toml:"gc"`
	Diff       *overlayDiff       `toml:"diff"`
	Locks      *overlayLocks      `toml:"locks"`
	UI         *overlayUI         `toml:"ui"`
}

type overlayScope struct {
	Include                *[]string `toml:"include"`
	ExtraExcludes          *[]string `toml:"extra_excludes"`
	UseGitignore           *bool     `toml:"use_gitignore"`
	UseAwaignore           *bool     `toml:"use_awaignore"`
	FollowSymlinks         *bool     `toml:"follow_symlinks"`
	SymlinkMaxDepth        *int      `toml:"symlink_max_depth"`
	AllowSymlinkRootEscape *bool     `toml:"allow_symlink_root_escape"`
}

type overlayHistory struct {
	ExtraExcludes *[]string `toml:"extra_excludes"`
}

type overlayHashing struct {
	TrustMode       *string `toml:"trust_mode"`
	MaxFileSize     *string `toml:"max_file_size"`
	LargeFilePolicy *string `toml:"large_file_policy"`
}

type overlayCheckpoint struct {
	StoreFileContents *bool `toml:"store_file_contents"`
	DiffContext       *int  `toml:"diff_context"`
	RenameDetection   *bool `toml:"rename_detection"`
}

type overlayRun struct {
	DefaultScope     *[]string `toml:"default_scope"`
	ExtraExcludes    *[]string `toml:"extra_excludes"`
	UseGitignore     *bool     `toml:"use_gitignore"`
	UseAwaignore     *bool     `toml:"use_awaignore"`
	EnvAllowlist     *[]string `toml:"env_allowlist"`
	TTL              *string   `toml:"ttl"`
	MaxStdoutSize    *string   `toml:"max_stdout_size"`
	MaxStderrSize    *string   `toml:"max_stderr_size"`
	CaptureOutput    *bool     `toml:"capture_output"`
	CacheFailedRuns  *bool     `toml:"cache_failed_runs"`
	ExtraEffectRoots *[]string `toml:"extra_effect_roots"`
}

type overlayGC struct {
	KeepLastCheckpoints *int    `toml:"keep_last_checkpoints"`
	KeepRunsFor         *string `toml:"keep_runs_for"`
	KeepRestoresFor     *string `toml:"keep_restores_for"`
}

type overlayDiff struct {
	Algorithm *string `toml:"algorithm"`
}

type overlayLocks struct {
	Timeout *string `toml:"timeout"`
}

type overlayUI struct {
	Time *string `toml:"time"`
}

// Overlay is one decoded config layer: the present keys of a single file. It is a
// value object — invalid enums/sizes/durations are caught when the overlay is
// applied, attributed to the layer, so app logic receives an already-composed,
// validated config plus origin metadata.
type Overlay struct {
	wire overlayWire
}

// DecodeOverlay parses one config file's bytes into an Overlay, capturing which
// keys are present without folding in defaults. It rejects unknown fields, the same
// as Decode, but defers enum/size/duration parsing to Compose so a bad value is
// attributed to its layer. Empty input is a valid overlay that sets nothing.
//
// The config schema is current-only. Every section or key the current shape does not
// define — a typo, a key from another tool, or a spelling this project used before —
// travels this one unknown-key path and is named, not translated: there is no table
// of retired names and no replacement hint, because a parser that recognizes a
// spelling it no longer accepts is carrying compatibility it does not honor.
func DecodeOverlay(data []byte) (Overlay, error) {
	var wc overlayWire
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wc); err != nil {
		return Overlay{}, fmt.Errorf("parsing config: %w", unknownKeyError(err))
	}
	return Overlay{wire: wc}, nil
}

// unknownKeyError rewrites go-toml's strict-mode rejection into a message that names
// the keys the caller actually wrote. The library's own Error() says only that some
// field is missing from the target struct, which tells a user nothing about which
// line to fix; the structured error carries each offending key path, so the fix is
// mechanical. Any other decode failure passes through unchanged.
func unknownKeyError(err error) error {
	var strict *toml.StrictMissingError
	if !errors.As(err, &strict) || len(strict.Errors) == 0 {
		return err
	}
	keys := make([]string, 0, len(strict.Errors))
	for i := range strict.Errors {
		keys = append(keys, describeKey(strict.Errors[i].Key()))
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	if len(keys) == 1 {
		return fmt.Errorf("unknown key %s", keys[0])
	}
	return fmt.Errorf("unknown keys %s", strings.Join(keys, ", "))
}

// describeKey renders one offending key path the way the user wrote it: the leaf key
// quoted, and its section named when the key sits inside one.
func describeKey(key toml.Key) string {
	switch len(key) {
	case 0:
		return "(unnamed)"
	case 1:
		return strconv.Quote(key[0])
	default:
		return fmt.Sprintf("%s in [%s]", strconv.Quote(key[len(key)-1]), strings.Join(key[:len(key)-1], "."))
	}
}

// NamedOverlay pairs an overlay with the layer it came from and the file it was
// read from, so Compose can record origins and attribute value-parse errors to a
// concrete layer and path. Path is optional (empty for an in-memory overlay).
type NamedOverlay struct {
	Layer   config.Layer
	Path    string
	Overlay Overlay
}

// layerContext renders the prefix that names where a config problem originated, so
// read, decode, and value errors all attribute the same way. The explicit --config
// override reads as "explicit --config" rather than the doubled "config config".
// The path is omitted when unknown (an in-memory overlay).
func layerContext(layer config.Layer, path string) string {
	label := layer.String() + " config"
	if layer == config.LayerConfig {
		label = "explicit --config"
	}
	if path == "" {
		return label
	}
	return label + " " + path
}

// LayerFile pairs a layer with the path that may hold its overlay.
type LayerFile struct {
	Layer config.Layer
	Path  string
}

// ReadOverlays decodes the overlays for the given optional layer files in order,
// skipping any whose file is absent (an optional layer simply contributes nothing).
// It reports whether any file was present, so a caller can tell "no config at all"
// from "config that sets nothing". A malformed layer is an error. It is the single
// place the read+decode-per-layer step lives, shared by the config use cases and the
// status read-model so neither hand-rolls the loop.
func ReadOverlays(files []LayerFile) ([]NamedOverlay, bool, error) {
	var overlays []NamedOverlay
	present := false
	for _, f := range files {
		if f.Path == "" {
			continue
		}
		o, err := readOverlay(f.Layer, f.Path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, false, err
		}
		overlays = append(overlays, o)
		present = true
	}
	return overlays, present, nil
}

// ReadOverlay reads and decodes a single required layer file, attributing read and
// decode errors to the layer and path. Unlike ReadOverlays it does not skip an
// absent file: a missing required layer (an explicit --config override) surfaces as
// the read error rather than silently contributing nothing.
func ReadOverlay(layer config.Layer, path string) (NamedOverlay, error) {
	return readOverlay(layer, path)
}

// readOverlay reads and decodes one layer file, attributing any read or decode
// failure to the layer and path. A not-exist error is returned unwrapped so callers
// can distinguish "absent optional layer" (skip) from "present but malformed"
// (fail). It is the single read+decode step shared by ReadOverlays and the required
// --config layer so both name the layer/path identically.
func readOverlay(layer config.Layer, path string) (NamedOverlay, error) {
	data, err := projfs.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return NamedOverlay{}, err
		}
		return NamedOverlay{}, fmt.Errorf("reading %s: %w", layerContext(layer, path), err)
	}
	o, err := DecodeOverlay(data)
	if err != nil {
		return NamedOverlay{}, fmt.Errorf("%s: %w", layerContext(layer, path), err)
	}
	return NamedOverlay{Layer: layer, Path: path, Overlay: o}, nil
}

// Compose folds the given overlays onto the product defaults in the order given
// (lowest precedence first) and returns the effective config plus per-key origins.
// A later layer's present key overwrites an earlier one and claims the origin. Any
// invalid value is reported with its layer, and the composed config is validated as
// a final guard so no overlay combination can produce an invalid domain object.
func Compose(overlays []NamedOverlay) (config.Config, map[string]config.Layer, error) {
	cfg := config.Defaults()
	// Seed every key with the default origin, then let each layer overwrite the keys
	// it sets, so the origin map is complete provenance: config effective can explain
	// every value, and a key with no override honestly reads as the default layer.
	origins := config.DefaultOrigins()
	for _, no := range overlays {
		if err := applyOverlay(&cfg, origins, no.Layer, no.Path, no.Overlay.wire); err != nil {
			return config.Config{}, nil, err
		}
	}
	if err := cfg.Validate(); err != nil {
		return config.Config{}, nil, err
	}
	return cfg, origins, nil
}

// applyOverlay sets each present key of one layer onto cfg and records its origin.
// Typed values (enums, sizes, durations) are parsed here, with errors attributed to
// the layer and key so an invalid value names where it came from.
func applyOverlay(cfg *config.Config, origins map[string]config.Layer, layer config.Layer, path string, w overlayWire) error {
	if s := w.Scope; s != nil {
		if s.Include != nil {
			cfg.Scope.Include = *s.Include
			origins["scope.include"] = layer
		}
		if s.ExtraExcludes != nil {
			cfg.Scope.ExtraExcludes = *s.ExtraExcludes
			origins["scope.extra_excludes"] = layer
		}
		if s.UseGitignore != nil {
			cfg.Scope.UseGitignore = *s.UseGitignore
			origins["scope.use_gitignore"] = layer
		}
		if s.UseAwaignore != nil {
			cfg.Scope.UseAwaignore = *s.UseAwaignore
			origins["scope.use_awaignore"] = layer
		}
		if s.FollowSymlinks != nil {
			cfg.Scope.FollowSymlinks = *s.FollowSymlinks
			origins["scope.follow_symlinks"] = layer
		}
		if s.SymlinkMaxDepth != nil {
			cfg.Scope.SymlinkMaxDepth = *s.SymlinkMaxDepth
			origins["scope.symlink_max_depth"] = layer
		}
		if s.AllowSymlinkRootEscape != nil {
			cfg.Scope.AllowSymlinkRootEscape = *s.AllowSymlinkRootEscape
			origins["scope.allow_symlink_root_escape"] = layer
		}
	}

	if h := w.History; h != nil {
		if h.ExtraExcludes != nil {
			cfg.History.ExtraExcludes = *h.ExtraExcludes
			origins["history.extra_excludes"] = layer
		}
	}

	if h := w.Hashing; h != nil {
		if h.TrustMode != nil {
			v, err := config.ParseTrustMode(*h.TrustMode)
			if err != nil {
				return layerErr(layer, path, "hashing.trust_mode", err)
			}
			cfg.Hashing.TrustMode = v
			origins["hashing.trust_mode"] = layer
		}
		if h.MaxFileSize != nil {
			v, err := config.ParseByteSize(*h.MaxFileSize)
			if err != nil {
				return layerErr(layer, path, "hashing.max_file_size", err)
			}
			cfg.Hashing.MaxFileSize = v
			origins["hashing.max_file_size"] = layer
		}
		if h.LargeFilePolicy != nil {
			v, err := config.ParseLargeFilePolicy(*h.LargeFilePolicy)
			if err != nil {
				return layerErr(layer, path, "hashing.large_file_policy", err)
			}
			cfg.Hashing.LargeFilePolicy = v
			origins["hashing.large_file_policy"] = layer
		}
	}

	if c := w.Checkpoint; c != nil {
		if c.StoreFileContents != nil {
			cfg.Checkpoint.StoreFileContents = *c.StoreFileContents
			origins["checkpoint.store_file_contents"] = layer
		}
		if c.DiffContext != nil {
			cfg.Checkpoint.DiffContext = *c.DiffContext
			origins["checkpoint.diff_context"] = layer
		}
		if c.RenameDetection != nil {
			cfg.Checkpoint.RenameDetection = *c.RenameDetection
			origins["checkpoint.rename_detection"] = layer
		}
	}

	if r := w.Run; r != nil {
		if r.DefaultScope != nil {
			cfg.Run.DefaultScope = *r.DefaultScope
			origins["run.default_scope"] = layer
		}
		if r.ExtraExcludes != nil {
			cfg.Run.ExtraExcludes = *r.ExtraExcludes
			origins["run.extra_excludes"] = layer
		}
		if r.UseGitignore != nil {
			cfg.Run.UseGitignore = *r.UseGitignore
			origins["run.use_gitignore"] = layer
		}
		if r.UseAwaignore != nil {
			cfg.Run.UseAwaignore = *r.UseAwaignore
			origins["run.use_awaignore"] = layer
		}
		if r.EnvAllowlist != nil {
			cfg.Run.EnvAllowlist = *r.EnvAllowlist
			origins["run.env_allowlist"] = layer
		}
		if r.TTL != nil {
			v, err := config.ParseDuration(*r.TTL)
			if err != nil {
				return layerErr(layer, path, "run.ttl", err)
			}
			cfg.Run.TTL = v
			origins["run.ttl"] = layer
		}
		if r.MaxStdoutSize != nil {
			v, err := config.ParseByteSize(*r.MaxStdoutSize)
			if err != nil {
				return layerErr(layer, path, "run.max_stdout_size", err)
			}
			cfg.Run.MaxStdoutSize = v
			origins["run.max_stdout_size"] = layer
		}
		if r.MaxStderrSize != nil {
			v, err := config.ParseByteSize(*r.MaxStderrSize)
			if err != nil {
				return layerErr(layer, path, "run.max_stderr_size", err)
			}
			cfg.Run.MaxStderrSize = v
			origins["run.max_stderr_size"] = layer
		}
		if r.CaptureOutput != nil {
			cfg.Run.CaptureOutput = *r.CaptureOutput
			origins["run.capture_output"] = layer
		}
		if r.CacheFailedRuns != nil {
			cfg.Run.CacheFailedRuns = *r.CacheFailedRuns
			origins["run.cache_failed_runs"] = layer
		}
		if r.ExtraEffectRoots != nil {
			cfg.Run.WatchedEffectRoots = *r.ExtraEffectRoots
			origins["run.extra_effect_roots"] = layer
		}
	}

	if g := w.GC; g != nil {
		if g.KeepLastCheckpoints != nil {
			cfg.GC.KeepLastCheckpoints = *g.KeepLastCheckpoints
			origins["gc.keep_last_checkpoints"] = layer
		}
		if g.KeepRunsFor != nil {
			v, err := config.ParseDuration(*g.KeepRunsFor)
			if err != nil {
				return layerErr(layer, path, "gc.keep_runs_for", err)
			}
			cfg.GC.KeepRunsFor = v
			origins["gc.keep_runs_for"] = layer
		}
		if g.KeepRestoresFor != nil {
			v, err := config.ParseDuration(*g.KeepRestoresFor)
			if err != nil {
				return layerErr(layer, path, "gc.keep_restores_for", err)
			}
			cfg.GC.KeepRestoresFor = v
			origins["gc.keep_restores_for"] = layer
		}
	}

	if d := w.Diff; d != nil {
		if d.Algorithm != nil {
			v, err := config.ParseDiffAlgorithm(*d.Algorithm)
			if err != nil {
				return layerErr(layer, path, "diff.algorithm", err)
			}
			cfg.Diff.Algorithm = v
			origins["diff.algorithm"] = layer
		}
	}

	if l := w.Locks; l != nil {
		if l.Timeout != nil {
			v, err := config.ParseDuration(*l.Timeout)
			if err != nil {
				return layerErr(layer, path, "locks.timeout", err)
			}
			cfg.Locks.Timeout = v
			origins["locks.timeout"] = layer
		}
	}

	if u := w.UI; u != nil {
		if u.Time != nil {
			v, err := config.ParseTimeDisplay(*u.Time)
			if err != nil {
				return layerErr(layer, path, "ui.time", err)
			}
			cfg.UI.Time = v
			origins["ui.time"] = layer
		}
	}

	return nil
}

// layerErr wraps a value parse error with the layer, path, and key it came from,
// so an invalid override names where to look
// ("shared config /repo/awa.toml: hashing.trust_mode: ...").
func layerErr(layer config.Layer, path, key string, err error) error {
	return fmt.Errorf("%s: %s: %w", layerContext(layer, path), key, err)
}
