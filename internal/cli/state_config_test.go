package cli

import (
	"errors"
	"testing"

	"awarer/internal/app/configcmd"
	domainconfig "awarer/internal/domain/config"
	"awarer/internal/domain/paths"
)

// TestStateConfigLazyLoad pins the provider's lazy-config contract at its seam
// (stateConfig): a stored-only operation must resolve without ever invoking the layered
// loader — so existing evidence stays reachable while a current config is malformed — and
// a "now" operation must invoke it exactly once and surface a malformed required config as
// a config error (exit 4).
//
// Mutation proof: drop the `if !needNow` early return in stateConfig so it always loads —
// the "stored-only never reads config" subtest goes red (calls == 1, and its failing
// loader turns the resolve into an error).
func TestStateConfigLazyLoad(t *testing.T) {
	loadErr := errors.New("malformed awa.toml")
	failing := func(count *int) layeredConfigLoader {
		return func(configcmd.LayerFiles, configcmd.Overrides) (domainconfig.ResolvedConfig, error) {
			*count++
			return domainconfig.ResolvedConfig{}, loadErr
		}
	}
	succeeding := func(count *int) layeredConfigLoader {
		return func(configcmd.LayerFiles, configcmd.Overrides) (domainconfig.ResolvedConfig, error) {
			*count++
			return domainconfig.ResolvedConfig{}, nil
		}
	}

	t.Run("stored-only never reads config", func(t *testing.T) {
		calls := 0
		cfg, err := stateConfig(false, paths.Layout{}, GlobalOptions{}, failing(&calls))
		if err != nil {
			t.Fatalf("stored-only stateConfig: %v", err)
		}
		if calls != 0 {
			t.Errorf("stored-only read config %d times, want 0", calls)
		}
		// It returns the product defaults, not a zero config, so the resolver still has a
		// working default scan policy.
		if cfg.Hashing.TrustMode != domainconfig.Defaults().Hashing.TrustMode {
			t.Errorf("stored-only config trust mode = %v, want product defaults", cfg.Hashing.TrustMode)
		}
	})

	t.Run("now loads exactly once", func(t *testing.T) {
		calls := 0
		if _, err := stateConfig(true, paths.Layout{}, GlobalOptions{}, succeeding(&calls)); err != nil {
			t.Fatalf("now stateConfig: %v", err)
		}
		if calls != 1 {
			t.Errorf("now read config %d times, want exactly 1", calls)
		}
	})

	t.Run("now malformed config is a config error", func(t *testing.T) {
		calls := 0
		_, err := stateConfig(true, paths.Layout{}, GlobalOptions{}, failing(&calls))
		if calls != 1 {
			t.Errorf("now read config %d times, want exactly 1", calls)
		}
		var ce *codedError
		if !errors.As(err, &ce) || ce.Code() != ExitConfigError {
			t.Fatalf("now malformed config: err = %v, want exit %d (config error)", err, ExitConfigError)
		}
	})
}
