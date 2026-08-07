package run

import (
	"testing"

	"awarer/internal/domain/config"
	"awarer/internal/infra/blake3hash"
)

func TestEffectiveScanScope(t *testing.T) {
	cfg := config.Defaults()
	cfg.Run.DefaultScope = []string{"."}
	cfg.Run.ExtraExcludes = []string{"fixtures"}
	cfg.Run.UseGitignore = false

	// No overrides: run scope comes from the run section.
	eff := effectiveScanScope(cfg, ScopeOverrides{})
	if len(eff.Include) != 1 || eff.Include[0] != "." {
		t.Errorf("include = %v, want [.]", eff.Include)
	}
	if eff.UseGitignore {
		t.Error("use_gitignore should come from run config (false)")
	}

	// --scope replaces; --include and --exclude append.
	eff2 := effectiveScanScope(cfg, ScopeOverrides{
		ScopeReplace: []string{"src"},
		Include:      []string{"tools"},
		Exclude:      []string{"testdata"},
	})
	if got := eff2.Include; len(got) != 2 || got[0] != "src" || got[1] != "tools" {
		t.Errorf("include = %v, want [src tools]", got)
	}
	// The effective run excludes are baseline + the run extra_excludes + any
	// --exclude, with the history-only defaults (dist/build/coverage) omitted.
	got := eff2.Exclude
	if got[len(got)-1] != "testdata" {
		t.Errorf("exclude should end with the --exclude override, got %v", got)
	}
	has := func(v string) bool {
		for _, e := range got {
			if e == v {
				return true
			}
		}
		return false
	}
	if !has("fixtures") || !has("node_modules") {
		t.Errorf("exclude missing run extra/baseline: %v", got)
	}
	if has("dist") {
		t.Errorf("history-only default leaked into run exclude: %v", got)
	}
}

// TestRunScopeIgnoresScopeGitignoreFlags locks the cross-family isolation in the
// run direction: the run input scan reads its .gitignore/.awaignore policy from the
// [run] section only. Toggling the [scope] flags (which drive checkpoint/changes/diff) must
// not change what awa run scans, so scope.use_gitignore=true cannot silently hide a
// real command input from the cache key.
func TestRunScopeIgnoresScopeGitignoreFlags(t *testing.T) {
	cfg := config.Defaults()
	// Flip the [scope] flags to the opposite of the run defaults. If the run scope
	// mistakenly derived from [scope], these values would leak through.
	cfg.Scope.UseGitignore = true
	cfg.Scope.UseAwaignore = false
	// Leave [run] at its defaults (gitignore off, awaignore on).

	eff := effectiveScanScope(cfg, ScopeOverrides{})
	if eff.UseGitignore != cfg.Run.UseGitignore {
		t.Errorf("run scope UseGitignore = %v, want %v (from [run], not [scope])", eff.UseGitignore, cfg.Run.UseGitignore)
	}
	if eff.UseAwaignore != cfg.Run.UseAwaignore {
		t.Errorf("run scope UseAwaignore = %v, want %v (from [run], not [scope])", eff.UseAwaignore, cfg.Run.UseAwaignore)
	}
}

func TestRunConfigHashSensitivity(t *testing.T) {
	h := blake3hash.New()
	base := config.Defaults()
	baseHash := runConfigHash(base, h).String()

	cases := []struct {
		name   string
		mutate func(*config.Config)
	}{
		{"capture output", func(c *config.Config) { c.Run.CaptureOutput = false }},
		{"cache failed", func(c *config.Config) { c.Run.CacheFailedRuns = false }},
		{"max stdout", func(c *config.Config) { c.Run.MaxStdoutSize = 1 }},
		{"max stderr", func(c *config.Config) { c.Run.MaxStderrSize = 1 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.Defaults()
			c.mutate(&cfg)
			if runConfigHash(cfg, h).String() == baseHash {
				t.Errorf("mutating %s did not change run config hash", c.name)
			}
		})
	}

	// Scope, ignore flags, and the env allowlist are keyed in their effective form
	// by KeyInput, not here, so changing them must not move the run config hash. In
	// particular an unused run.default_scope (replaced by --scope) must not cause a
	// spurious miss through this hash.
	keyedElsewhere := []struct {
		name   string
		mutate func(*config.Config)
	}{
		{"default scope", func(c *config.Config) { c.Run.DefaultScope = []string{"src"} }},
		{"run extra excludes", func(c *config.Config) { c.Run.ExtraExcludes = []string{"vendor"} }},
		{"use gitignore", func(c *config.Config) { c.Run.UseGitignore = !c.Run.UseGitignore }},
		{"use awaignore", func(c *config.Config) { c.Run.UseAwaignore = !c.Run.UseAwaignore }},
		{"env allowlist", func(c *config.Config) { c.Run.EnvAllowlist = []string{"FOO"} }},
	}
	for _, c := range keyedElsewhere {
		t.Run(c.name+" not in run config hash", func(t *testing.T) {
			cfg := config.Defaults()
			c.mutate(&cfg)
			if runConfigHash(cfg, h).String() != baseHash {
				t.Errorf("mutating %s changed the run config hash; it is keyed by KeyInput, not here", c.name)
			}
		})
	}

	// Stable for the same config.
	if runConfigHash(config.Defaults(), h).String() != baseHash {
		t.Error("run config hash not stable for identical config")
	}

	// TTL is a lookup-time freshness window, not part of a result's identity, so it
	// must not change the key.
	ttlChanged := config.Defaults()
	ttlChanged.Run.TTL = config.Duration(99 * 24 * 60 * 60 * 1e9)
	if runConfigHash(ttlChanged, h).String() != baseHash {
		t.Error("changing run.ttl must not change the run config hash")
	}
}
