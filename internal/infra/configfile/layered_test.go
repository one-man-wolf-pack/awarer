package configfile

import (
	"os"
	"strings"
	"testing"

	"awarer/internal/domain/config"
)

func overlay(t *testing.T, toml string) Overlay {
	t.Helper()
	o, err := DecodeOverlay([]byte(toml))
	if err != nil {
		t.Fatalf("DecodeOverlay(%q): %v", toml, err)
	}
	return o
}

func TestReadOverlaysSkipsAbsentAndReportsPresence(t *testing.T) {
	dir := t.TempDir()
	local := dir + "/config.toml"
	if err := os.WriteFile(local, []byte("[run]\nttl = \"2d\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A shared path that does not exist is skipped; the present local layer is read.
	overlays, present, err := ReadOverlays([]LayerFile{
		{Layer: config.LayerShared, Path: dir + "/awa.toml"},
		{Layer: config.LayerLocal, Path: local},
		{Layer: config.LayerConfig, Path: ""}, // empty path is skipped
	})
	if err != nil {
		t.Fatalf("ReadOverlays: %v", err)
	}
	if !present {
		t.Error("present = false, want true (local layer exists)")
	}
	if len(overlays) != 1 || overlays[0].Layer != config.LayerLocal {
		t.Fatalf("overlays = %v, want one local overlay", overlays)
	}
}

func TestReadOverlaysAbsentEverywhereIsNotPresent(t *testing.T) {
	dir := t.TempDir()
	overlays, present, err := ReadOverlays([]LayerFile{
		{Layer: config.LayerShared, Path: dir + "/awa.toml"},
		{Layer: config.LayerLocal, Path: dir + "/config.toml"},
	})
	if err != nil {
		t.Fatalf("ReadOverlays: %v", err)
	}
	if present || len(overlays) != 0 {
		t.Errorf("present=%v overlays=%v, want absent/empty", present, overlays)
	}
}

func TestReadOverlaysMalformedNamesLayerAndPath(t *testing.T) {
	dir := t.TempDir()
	bad := dir + "/awa.toml"
	if err := os.WriteFile(bad, []byte("[hashing]\nnope = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := ReadOverlays([]LayerFile{{Layer: config.LayerShared, Path: bad}})
	if err == nil {
		t.Fatal("ReadOverlays should surface a malformed layer's decode error")
	}
	// The error must name the layer and the path so a user knows which file to fix.
	if !strings.Contains(err.Error(), config.LayerShared.String()) || !strings.Contains(err.Error(), bad) {
		t.Errorf("error %q should name the layer and path", err)
	}
}

// TestComposeValueErrorNamesLayerAndPath proves an invalid VALUE (not a TOML
// syntax error) is attributed to the layer and its source path too, so shared and
// local failures are distinguishable.
func TestComposeValueErrorNamesLayerAndPath(t *testing.T) {
	bad := overlay(t, "[hashing]\ntrust_mode = \"paranoid\"\n")
	_, _, err := Compose([]NamedOverlay{{Layer: config.LayerLocal, Path: "/repo/.awa/config.toml", Overlay: bad}})
	if err == nil {
		t.Fatal("Compose should reject an invalid trust mode")
	}
	msg := err.Error()
	if !strings.Contains(msg, config.LayerLocal.String()) || !strings.Contains(msg, "/repo/.awa/config.toml") || !strings.Contains(msg, "hashing.trust_mode") {
		t.Errorf("error %q should name the layer, path, and key", msg)
	}
}

func TestComposeEmptyIsDefaults(t *testing.T) {
	cfg, origins, err := Compose(nil)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if cfg.Hashing.TrustMode != config.Defaults().Hashing.TrustMode {
		t.Errorf("trust_mode = %v, want default", cfg.Hashing.TrustMode)
	}
	// Origins is complete provenance: with no overlays every key reads as default,
	// and the seed covers exactly the accepted keys.
	if len(origins) != len(config.Keys()) {
		t.Errorf("origins has %d keys, want %d (all keys seeded)", len(origins), len(config.Keys()))
	}
	for k, layer := range origins {
		if layer != config.LayerDefault {
			t.Errorf("origins[%q] = %q, want default", k, layer)
		}
	}
}

func TestComposeMergesLayersAndRecordsOrigins(t *testing.T) {
	shared := overlay(t, "[hashing]\ntrust_mode = \"fast\"\n[run]\nttl = \"3d\"\n")
	local := overlay(t, "[run]\nttl = \"1d\"\n")
	cfg, origins, err := Compose([]NamedOverlay{
		{Layer: config.LayerShared, Overlay: shared},
		{Layer: config.LayerLocal, Overlay: local},
	})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	// Shared set trust_mode; local overrode the shared ttl.
	if cfg.Hashing.TrustMode != config.TrustFast {
		t.Errorf("trust_mode = %v, want fast", cfg.Hashing.TrustMode)
	}
	if got := cfg.Run.TTL.String(); got != "1d" {
		t.Errorf("ttl = %s, want 1d (local overrides shared)", got)
	}
	if origins["hashing.trust_mode"] != config.LayerShared {
		t.Errorf("trust_mode origin = %q, want shared", origins["hashing.trust_mode"])
	}
	if origins["run.ttl"] != config.LayerLocal {
		t.Errorf("ttl origin = %q, want local", origins["run.ttl"])
	}
}

func TestComposeConfigLayerIsAdditiveTop(t *testing.T) {
	shared := overlay(t, "[run]\nttl = \"3d\"\n")
	override := overlay(t, "[run]\nttl = \"9d\"\n")
	cfg, origins, err := Compose([]NamedOverlay{
		{Layer: config.LayerShared, Overlay: shared},
		{Layer: config.LayerConfig, Overlay: override},
	})
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if got := cfg.Run.TTL.String(); got != "9d" {
		t.Errorf("ttl = %s, want 9d (--config overlays shared)", got)
	}
	if origins["run.ttl"] != config.LayerConfig {
		t.Errorf("ttl origin = %q, want config", origins["run.ttl"])
	}
}

func TestComposeInvalidLayerNamesLayerAndKey(t *testing.T) {
	bad := overlay(t, "[hashing]\ntrust_mode = \"paranoid\"\n")
	_, _, err := Compose([]NamedOverlay{{Layer: config.LayerShared, Overlay: bad}})
	if err == nil {
		t.Fatal("Compose should reject an invalid trust mode")
	}
	msg := err.Error()
	if !strings.Contains(msg, config.LayerShared.String()) || !strings.Contains(msg, "hashing.trust_mode") {
		t.Errorf("error %q should name the layer and key", msg)
	}
}

func TestDecodeOverlayRejectsUnknownField(t *testing.T) {
	if _, err := DecodeOverlay([]byte("[hashing]\nnope = true\n")); err == nil {
		t.Error("DecodeOverlay should reject an unknown field")
	}
}

// TestComposeListReplacesWholesale confirms a later layer replaces a list rather
// than appending, so a list field's origin is well-defined.
// TestComposeListReplacesWholesale pins the layering rule for EVERY list-valued key:
// a later layer replaces the whole list rather than merging into it. One key is not
// enough to pin this, because the rule is what makes a local override silently drop a
// shared policy list — the failure mode is the same for excludes, an env allowlist, a
// scope, and the effect roots, and each is decoded by its own branch that could start
// appending. The generated configuration reference states this rule; this is the test
// that keeps the statement true.
func TestComposeListReplacesWholesale(t *testing.T) {
	cases := []struct {
		key    string
		shared string
		local  string
		got    func(config.Config) []string
		want   string
	}{
		{"scope.include", "[scope]\ninclude = [\"src\", \"docs\"]\n", "[scope]\ninclude = [\"lib\"]\n",
			func(c config.Config) []string { return c.Scope.Include }, "lib"},
		{"scope.extra_excludes", "[scope]\nextra_excludes = [\"a\", \"b\"]\n", "[scope]\nextra_excludes = [\"c\"]\n",
			func(c config.Config) []string { return c.Scope.ExtraExcludes }, "c"},
		{"history.extra_excludes", "[history]\nextra_excludes = [\"a\", \"b\"]\n", "[history]\nextra_excludes = [\"c\"]\n",
			func(c config.Config) []string { return c.History.ExtraExcludes }, "c"},
		{"run.default_scope", "[run]\ndefault_scope = [\"src\", \"docs\"]\n", "[run]\ndefault_scope = [\"lib\"]\n",
			func(c config.Config) []string { return c.Run.DefaultScope }, "lib"},
		{"run.extra_excludes", "[run]\nextra_excludes = [\"a\", \"b\"]\n", "[run]\nextra_excludes = [\"c\"]\n",
			func(c config.Config) []string { return c.Run.ExtraExcludes }, "c"},
		// Both spellings are deliberately outside the built-in baseline: a baseline name
		// is rejected by validation, so it could not stand in for "a user's own entry".
		{"run.env_allowlist", "[run]\nenv_allowlist = [\"CI\", \"OLD_TOOL_VAR\"]\n", "[run]\nenv_allowlist = [\"NEW_TOOL_VAR\"]\n",
			func(c config.Config) []string { return c.Run.EnvAllowlist }, "NEW_TOOL_VAR"},
		{"run.extra_effect_roots", "[run]\nextra_effect_roots = [\"gen\", \"out2\"]\n", "[run]\nextra_effect_roots = [\"artifacts\"]\n",
			func(c config.Config) []string { return c.Run.WatchedEffectRoots }, "artifacts"},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			cfg, origins, err := Compose([]NamedOverlay{
				{Layer: config.LayerShared, Overlay: overlay(t, c.shared)},
				{Layer: config.LayerLocal, Overlay: overlay(t, c.local)},
			})
			if err != nil {
				t.Fatalf("Compose: %v", err)
			}
			if got := c.got(cfg); len(got) != 1 || got[0] != c.want {
				t.Errorf("%s = %v, want [%s] — the shared list must be replaced, not merged", c.key, got, c.want)
			}
			if origins[c.key] != config.LayerLocal {
				t.Errorf("%s origin = %q, want local", c.key, origins[c.key])
			}
		})
	}
}
