package configcmd

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"awarer/internal/app/initcmd"
	"awarer/internal/domain/config"
	"awarer/internal/domain/paths"
)

func initProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if _, err := initcmd.Run(initcmd.Request{Root: root, Profile: initcmd.ProfileDefault}); err != nil {
		t.Fatalf("init: %v", err)
	}
	return paths.New(root).ConfigFile()
}

func TestLoadLayeredValidatesAndComposes(t *testing.T) {
	cfgPath := initProject(t)
	loaded, err := LoadLayered(LayerFiles{Local: cfgPath}, Overrides{})
	if err != nil {
		t.Fatalf("LoadLayered: %v", err)
	}
	// Origins is a complete provenance map: a default-profile project overrides
	// nothing, so every key reads as the default origin.
	if len(loaded.Origins()) == 0 {
		t.Fatal("origins should cover every key, not be empty")
	}
	for key, layer := range loaded.Origins() {
		if layer != config.LayerDefault {
			t.Errorf("origins[%q] = %q, want default for an unconfigured project", key, layer)
		}
	}
	// The layer facts come from the same read: only a local layer was configured, and
	// a default-profile init writes no file, so it is listed but absent.
	layers := loaded.Layers()
	if len(layers) != 1 || layers[0].Layer() != config.LayerLocal || layers[0].Exists() {
		t.Errorf("layers = %+v, want a single absent local layer", layers)
	}
}

func TestLoadLayeredRejectsInvalidLayer(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[hashing]\ntrust_mode = \"paranoid\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLayered(LayerFiles{Local: cfgPath}, Overrides{}); err == nil {
		t.Error("LoadLayered on an invalid config should error")
	}
}

func TestLoadLayeredAppliesTrustModeOverride(t *testing.T) {
	cfgPath := initProject(t)
	strict := config.TrustStrict
	loaded, err := LoadLayered(LayerFiles{Local: cfgPath}, Overrides{TrustMode: &strict})
	if err != nil {
		t.Fatalf("LoadLayered: %v", err)
	}
	if loaded.Config().Hashing.TrustMode != config.TrustStrict {
		t.Errorf("trust_mode = %v, want strict", loaded.Config().Hashing.TrustMode)
	}
	if loaded.Origins()["hashing.trust_mode"] != config.LayerFlag {
		t.Errorf("trust_mode origin = %q, want flag", loaded.Origins()["hashing.trust_mode"])
	}

	// Without override, the composed value (normal) holds and has no flag origin.
	loaded, err = LoadLayered(LayerFiles{Local: cfgPath}, Overrides{})
	if err != nil {
		t.Fatalf("LoadLayered: %v", err)
	}
	if loaded.Config().Hashing.TrustMode != config.TrustNormal {
		t.Errorf("trust_mode = %v, want normal", loaded.Config().Hashing.TrustMode)
	}
}

func TestLoadLayeredRejectsInvalidOverride(t *testing.T) {
	cfgPath := initProject(t)
	bogus := config.TrustMode(999)
	if _, err := LoadLayered(LayerFiles{Local: cfgPath}, Overrides{TrustMode: &bogus}); err == nil {
		t.Error("LoadLayered should reject an out-of-range trust mode override")
	}
}

func TestNewViewRendersStrings(t *testing.T) {
	v := NewView(config.Defaults())
	if v.Hashing.MaxFileSize != "50MiB" {
		t.Errorf("max_file_size view = %q, want 50MiB", v.Hashing.MaxFileSize)
	}
	if v.Run.TTL != "7d" {
		t.Errorf("ttl view = %q, want 7d", v.Run.TTL)
	}
	if v.Diff.Algorithm != "histogram" {
		t.Errorf("diff algorithm view = %q, want histogram", v.Diff.Algorithm)
	}
	// The effective env allowlist folds in the built-in baseline (PATH etc.) on top
	// of the configured names, so a user sees the variables a run actually keys.
	var hasPATH, hasCI bool
	for _, n := range v.Run.EffectiveEnvAllowlist {
		switch n {
		case "PATH":
			hasPATH = true
		case "CI":
			hasCI = true
		}
	}
	if !hasPATH {
		t.Errorf("effective_env_allowlist = %v, want it to include the baseline PATH", v.Run.EffectiveEnvAllowlist)
	}
	if !hasCI {
		t.Errorf("effective_env_allowlist = %v, want it to include the configured CI", v.Run.EffectiveEnvAllowlist)
	}
}

func TestNewViewClonesSlices(t *testing.T) {
	cfg := config.Defaults()
	v := NewView(cfg)

	// Mutating the view's slices must not reach back into the source config. The
	// effective-exclude views are freshly composed (not aliased to any config
	// field), so the leak risk is on the cloned user lists.
	v.Scope.Include[0] = "MUTATED"
	v.Scope.EffectiveExcludes[0] = "MUTATED"
	v.Run.DefaultScope[0] = "MUTATED"
	v.Run.EffectiveExcludes[0] = "MUTATED"
	v.Run.EnvAllowlist[0] = "MUTATED"

	if cfg.Scope.Include[0] == "MUTATED" ||
		cfg.Run.DefaultScope[0] == "MUTATED" ||
		cfg.Run.EnvAllowlist[0] == "MUTATED" {
		t.Error("mutating View slices leaked into the source Config")
	}
}

// `awa config effective` must show the names a run inherits and the facts awa injects as
// two different things. Folding the marker into the effective allowlist would tell a
// reader they can configure something they cannot, and listing it as an inherited name
// would credit the caller's environment with a value awa states itself.
func TestViewSeparatesInheritedFromInjected(t *testing.T) {
	v := NewView(config.Defaults())

	if want := []string{"AWA_RUN=1"}; !slices.Equal(v.Run.InjectedEnv, want) {
		t.Errorf("injected_env = %v, want %v", v.Run.InjectedEnv, want)
	}
	for _, n := range v.Run.EffectiveEnvAllowlist {
		if config.IsReservedEnvName(n) {
			t.Errorf("effective_env_allowlist contains the injected name %q; the two projections must stay disjoint", n)
		}
	}
	for _, n := range v.Run.EnvAllowlist {
		if config.IsReservedEnvName(n) {
			t.Errorf("env_allowlist contains the injected name %q", n)
		}
	}
	// The inherited projection is names only: an ambient value must never appear there.
	for _, n := range v.Run.EffectiveEnvAllowlist {
		if strings.Contains(n, "=") {
			t.Errorf("effective_env_allowlist entry %q carries a value; it projects names, not values", n)
		}
	}
}

// TestViewProjectsTheLocaleBaseline proves the locale family reaches the surface a user
// consults to answer "what does my command actually see?".
func TestViewProjectsTheLocaleBaseline(t *testing.T) {
	v := NewView(config.Defaults())
	for _, name := range []string{"LANG", "LANGUAGE", "LC_ALL", "LC_COLLATE", "LC_CTYPE", "LC_MESSAGES", "LC_MONETARY", "LC_NUMERIC", "LC_TIME"} {
		if !slices.Contains(v.Run.EffectiveEnvAllowlist, name) {
			t.Errorf("effective_env_allowlist is missing %q: %v", name, v.Run.EffectiveEnvAllowlist)
		}
	}
}
