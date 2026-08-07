// Package configcmd implements the "awa config" use cases: path, show,
// effective, validate, template, and init. It loads, layers, and merges
// configuration but does not format output or read the process current directory.
package configcmd

import (
	"awarer/internal/domain/config"
	"awarer/internal/infra/configfile"
	"awarer/internal/infra/projfs"
)

// LayerFiles names the config files that may participate in a layered load, in
// precedence order (lowest first). An empty path means the layer is not present.
// Shared and Local are optional: an absent file simply contributes nothing.
// Override (an explicit --config) is required to exist when provided; the CLI
// verifies that before calling, and LoadLayered fails loudly if it is missing.
type LayerFiles struct {
	Shared   string
	Local    string
	Override string
}

// LoadLayered composes the effective configuration from the product defaults and
// the present config layers (shared awa.toml, local .awa/config.toml, explicit
// --config), then applies the CLI flag overrides as the top layer. It returns an
// immutable config.ResolvedConfig — the effective config, per-key origins, and
// per-layer existence facts, all from one read — so an invalid layer fails loudly
// naming the layer and key, and provenance can never disagree with the config it
// describes. It is the only constructor of a ResolvedConfig for a real project.
func LoadLayered(files LayerFiles, ov Overrides) (config.ResolvedConfig, error) {
	// Shared and local are optional (skip-absent). An explicit --config override is
	// required to exist when provided: a named file that is missing is a hard error,
	// never a silent fall-through to defaults, so it is read on its own path.
	overlays, _, err := configfile.ReadOverlays([]configfile.LayerFile{
		{Layer: config.LayerShared, Path: files.Shared},
		{Layer: config.LayerLocal, Path: files.Local},
	})
	if err != nil {
		return config.ResolvedConfig{}, err
	}
	if files.Override != "" {
		o, err := configfile.ReadOverlay(config.LayerConfig, files.Override)
		if err != nil {
			return config.ResolvedConfig{}, err
		}
		overlays = append(overlays, o)
	}

	cfg, origins, err := configfile.Compose(overlays)
	if err != nil {
		return config.ResolvedConfig{}, err
	}
	if ov.TrustMode != nil {
		cfg.Hashing.TrustMode = *ov.TrustMode
		origins["hashing.trust_mode"] = config.LayerFlag
	}
	facts, err := layerFacts(files, overlays)
	if err != nil {
		return config.ResolvedConfig{}, err
	}
	resolved, err := config.NewResolvedConfig(cfg, origins, facts)
	if err != nil {
		return config.ResolvedConfig{}, err
	}
	return resolved, nil
}

// layerFacts derives each configured layer's existence from the overlays actually
// read: a layer is present exactly when its file was read into an overlay, so the
// facts come from the same read as the config rather than a separate stat.
func layerFacts(files LayerFiles, overlays []configfile.NamedOverlay) ([]config.LayerFact, error) {
	read := make(map[config.Layer]bool, len(overlays))
	for _, o := range overlays {
		read[o.Layer] = true
	}
	var out []config.LayerFact
	add := func(layer config.Layer, path string) error {
		if path == "" {
			return nil
		}
		f, err := config.NewLayerFact(layer, path, read[layer])
		if err != nil {
			return err
		}
		out = append(out, f)
		return nil
	}
	for _, lf := range []struct {
		layer config.Layer
		path  string
	}{
		{config.LayerShared, files.Shared},
		{config.LayerLocal, files.Local},
		{config.LayerConfig, files.Override},
	} {
		if err := add(lf.layer, lf.path); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Show returns the raw bytes of a config file for `awa config show`. The caller
// selects the layer's path.
func Show(path string) ([]byte, error) {
	return projfs.ReadFile(path)
}

// Overrides carries the CLI overrides that affect the effective config. Only trust
// mode is overridable (via --trust-mode/--strict). It is applied as the top layer by
// LoadLayered.
type Overrides struct {
	TrustMode *config.TrustMode
}
