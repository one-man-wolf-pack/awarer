// Package configfile is the config-file boundary for awa configuration: it owns
// turning config files on disk into a validated, layered domain.Config, and back.
//
// It generates the config.toml that awa init writes, decodes bytes into a
// validated domain.Config, reads and composes the layered config stack (shared
// awa.toml, local .awa/config.toml, explicit --config) via ReadOverlays/Compose,
// and reports per-key origins. TOML-specific types never leave this package:
// decoding produces a private wire struct (overlayWire), and every value is
// converted through the domain ParseX constructors so an invalid enum, size, or
// duration is a decode error rather than a silently-accepted default. The config
// use cases (configcmd) depend on this one boundary rather than hand-rolling the
// read/decode/compose loop; other consumers such as the status read-model take the
// already-composed aggregate from configcmd instead of touching this package.
package configfile

import (
	"awarer/internal/domain/config"
)

// Decode parses a single config file's bytes into a validated domain.Config by
// overlaying its present keys onto the product defaults. It is the one-shot form of
// DecodeOverlay + Compose, kept for callers that load exactly one file (init's
// written config, unit tests), so there is a single wire definition (overlayWire)
// and a single parse path — no parallel decoder to keep in sync.
func Decode(data []byte) (config.Config, error) {
	overlay, err := DecodeOverlay(data)
	if err != nil {
		return config.Config{}, err
	}
	cfg, _, err := Compose([]NamedOverlay{{Layer: config.LayerLocal, Overlay: overlay}})
	return cfg, err
}
