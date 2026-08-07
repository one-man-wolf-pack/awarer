package scanner

import (
	"testing"

	"awarer/internal/domain/config"
	"awarer/internal/infra/blake3hash"
)

// TestConfigHashListEncodingIsInjective proves the length-prefixed encoding does
// not collide between a single comma-containing element and two elements — the
// classic strings.Join(",") ambiguity.
func TestConfigHashListEncodingIsInjective(t *testing.T) {
	h := blake3hash.New()
	a := config.Defaults()
	a.Scope.Include = []string{"a,b"}
	b := config.Defaults()
	b.Scope.Include = []string{"a", "b"}

	if configHash(a, a.HistoryScanScope(), h) == configHash(b, b.HistoryScanScope(), h) {
		t.Errorf("config hash collides between [\"a,b\"] and [\"a\",\"b\"]")
	}
}

// TestConfigHashCanonicalizesIncludeScope proves semantically identical include
// scopes (["src"] vs ["./src"]) produce the same config hash, so the walker and
// the recorded config hash never drift apart.
func TestConfigHashCanonicalizesIncludeScope(t *testing.T) {
	h := blake3hash.New()
	a := config.Defaults()
	a.Scope.Include = []string{"src"}
	b := config.Defaults()
	b.Scope.Include = []string{"./src"}

	if configHash(a, a.HistoryScanScope(), h) != configHash(b, b.HistoryScanScope(), h) {
		t.Errorf("config hash differs for [\"src\"] vs [\"./src\"]")
	}

	// A child include under an already-included parent scans nothing new, so it must
	// not change the hash either.
	d := config.Defaults()
	d.Scope.Include = []string{"src", "src/internal"}
	if configHash(a, a.HistoryScanScope(), h) != configHash(d, d.HistoryScanScope(), h) {
		t.Errorf("config hash differs for [\"src\"] vs [\"src\", \"src/internal\"]")
	}

	// A genuinely different scope still changes the hash.
	c := config.Defaults()
	c.Scope.Include = []string{"docs"}
	if configHash(a, a.HistoryScanScope(), h) == configHash(c, c.HistoryScanScope(), h) {
		t.Errorf("config hash insensitive to a real scope change")
	}
}

// TestConfigHashStableAndSensitive proves the hash is stable for equal configs and
// changes when a relevant field changes.
func TestConfigHashStableAndSensitive(t *testing.T) {
	h := blake3hash.New()
	base := config.Defaults()
	if configHash(base, base.HistoryScanScope(), h) != configHash(config.Defaults(), config.Defaults().HistoryScanScope(), h) {
		t.Errorf("config hash unstable for equal configs")
	}
	changed := config.Defaults()
	changed.Hashing.MaxFileSize = base.Hashing.MaxFileSize + 1
	if configHash(base, base.HistoryScanScope(), h) == configHash(changed, changed.HistoryScanScope(), h) {
		t.Errorf("config hash insensitive to max_file_size change")
	}
}

// TestConfigHashSensitiveToTrustMode proves trust mode is part of the scan config
// hash, so strict, normal, and fast scans of the same worktree get distinct
// scan_config_hash values (their evidence strength differs).
func TestConfigHashSensitiveToTrustMode(t *testing.T) {
	h := blake3hash.New()
	strict := config.Defaults()
	strict.Hashing.TrustMode = config.TrustStrict
	normal := config.Defaults()
	normal.Hashing.TrustMode = config.TrustNormal
	fast := config.Defaults()
	fast.Hashing.TrustMode = config.TrustFast

	hs, hn, hf := configHash(strict, strict.HistoryScanScope(), h), configHash(normal, normal.HistoryScanScope(), h), configHash(fast, fast.HistoryScanScope(), h)
	if hs == hn || hn == hf || hs == hf {
		t.Errorf("config hash must differ across trust modes: strict=%q normal=%q fast=%q", hs, hn, hf)
	}
}
