package config

import (
	"sort"
	"testing"
)

// mustResolve builds a ResolvedConfig or fails the test; the aggregate contract is
// exercised separately by TestNewResolvedConfigRejectsInconsistentAggregate. Origins
// are folded onto a complete DefaultOrigins seed so a test only names the keys it
// overrides, mirroring how the loader composes provenance.
func mustResolve(t *testing.T, cfg Config, overrides map[string]Layer, layers []LayerFact) ResolvedConfig {
	t.Helper()
	origins := DefaultOrigins()
	for k, l := range overrides {
		origins[k] = l
	}
	r, err := NewResolvedConfig(cfg, origins, layers)
	if err != nil {
		t.Fatalf("NewResolvedConfig: %v", err)
	}
	return r
}

// mustFact builds a valid LayerFact or fails the test.
func mustFact(t *testing.T, layer Layer, path string, exists bool) LayerFact {
	t.Helper()
	f, err := NewLayerFact(layer, path, exists)
	if err != nil {
		t.Fatalf("NewLayerFact(%v, %q): %v", layer, path, err)
	}
	return f
}

func TestResolvedConfigOriginAndSourcePath(t *testing.T) {
	origins := map[string]Layer{
		"run.env_allowlist":              LayerShared,
		"checkpoint.store_file_contents": LayerLocal,
		"hashing.trust_mode":             LayerFlag,
	}
	layers := []LayerFact{
		mustFact(t, LayerShared, "/repo/awa.toml", true),
		mustFact(t, LayerLocal, "/repo/.awa/config.toml", true),
	}
	r := mustResolve(t, Defaults(), origins, layers)

	// OriginOf reflects the recorded layer; an unset key reads as default.
	if got := r.OriginOf("run.env_allowlist"); got != LayerShared {
		t.Errorf("OriginOf(run.env_allowlist) = %v, want shared", got)
	}
	if got := r.OriginOf("diff.algorithm"); got != LayerDefault {
		t.Errorf("OriginOf(unset key) = %v, want default", got)
	}

	// SourcePathOf resolves the layer's file; a shared value names awa.toml, a local
	// value names .awa/config.toml.
	if got := r.SourcePathOf("run.env_allowlist"); got != "/repo/awa.toml" {
		t.Errorf("SourcePathOf(run.env_allowlist) = %q, want the shared awa.toml", got)
	}
	if got := r.SourcePathOf("checkpoint.store_file_contents"); got != "/repo/.awa/config.toml" {
		t.Errorf("SourcePathOf(checkpoint.store_file_contents) = %q, want the local config.toml", got)
	}
	// A default or flag value has no source file.
	if got := r.SourcePathOf("diff.algorithm"); got != "" {
		t.Errorf("SourcePathOf(default) = %q, want empty", got)
	}
	if got := r.SourcePathOf("hashing.trust_mode"); got != "" {
		t.Errorf("SourcePathOf(flag) = %q, want empty (a flag has no file)", got)
	}
}

func TestResolvedConfigAccessorsReturnCopies(t *testing.T) {
	// Pass the maps/slices to the constructor directly (not via a helper that copies
	// first) so the mutations below actually test NewResolvedConfig's defensive copy.
	origins := DefaultOrigins()
	origins["run.ttl"] = LayerLocal
	layers := []LayerFact{mustFact(t, LayerLocal, "/p/.awa/config.toml", true)}
	r, err := NewResolvedConfig(Defaults(), origins, layers)
	if err != nil {
		t.Fatalf("NewResolvedConfig: %v", err)
	}

	// Mutating the constructor inputs must not reach into the value (defensive copy).
	origins["run.ttl"] = LayerShared
	layers[0] = mustFact(t, LayerShared, "MUTATED", false)
	if r.OriginOf("run.ttl") != LayerLocal {
		t.Error("mutating the origins input leaked into ResolvedConfig")
	}
	if got := r.Layers(); len(got) != 1 || got[0].Path() != "/p/.awa/config.toml" {
		t.Errorf("mutating the layers input leaked into ResolvedConfig: %+v", got)
	}

	// Mutating a returned copy must not reach back either.
	got := r.Origins()
	got["run.ttl"] = LayerShared
	if r.OriginOf("run.ttl") != LayerLocal {
		t.Error("mutating the returned origins copy leaked into ResolvedConfig")
	}
}

// TestResolvedConfigDeepImmutable proves the effective Config is deep-copied at both
// construction and access, so no slice field shares a backing array with the
// aggregate — mutating a returned config's slice cannot silently change the
// effective config while its provenance stays put.
func TestResolvedConfigDeepImmutable(t *testing.T) {
	cfg := Defaults()
	cfg.Run.EnvAllowlist = []string{"CI"}
	// The changed value is honestly attributed to a local file, so the constructor's
	// value/origin agreement holds.
	r := mustResolve(t, cfg,
		map[string]Layer{"run.env_allowlist": LayerLocal},
		[]LayerFact{mustFact(t, LayerLocal, "/p/.awa/config.toml", true)})

	// Mutating the constructor input's slice must not reach into the value.
	cfg.Run.EnvAllowlist[0] = "MUTATED"
	if got := r.Config().Run.EnvAllowlist[0]; got != "CI" {
		t.Errorf("mutating the constructor input leaked in: env_allowlist[0] = %q", got)
	}
	// Mutating a returned config's slice must not reach back into the aggregate.
	out := r.Config()
	out.Run.EnvAllowlist[0] = "MUTATED"
	if got := r.Config().Run.EnvAllowlist[0]; got != "CI" {
		t.Errorf("mutating a returned Config leaked back: env_allowlist[0] = %q", got)
	}
}

// TestNewLayerFactRejectsInvalidInput proves the layer fact is valid by construction:
// a non-file-backed layer or an empty path fails rather than yielding a fabricated
// value the aggregate would have to catch later.
func TestNewLayerFactRejectsInvalidInput(t *testing.T) {
	bad := []struct {
		name  string
		layer Layer
		path  string
	}{
		{"unknown layer", Layer(99), "/p"},
		{"non-file layer (flag)", LayerFlag, "/p"},
		{"non-file layer (default)", LayerDefault, "/p"},
		{"empty path", LayerShared, ""},
	}
	for _, tc := range bad {
		if _, err := NewLayerFact(tc.layer, tc.path, true); err == nil {
			t.Errorf("%s: NewLayerFact should reject the invalid input", tc.name)
		}
	}
	if _, err := NewLayerFact(LayerShared, "/repo/awa.toml", true); err != nil {
		t.Errorf("a file-backed layer with a path should be accepted, got %v", err)
	}
}

// TestNewResolvedConfigRejectsInconsistentAggregate proves the constructor enforces
// the aggregate contract rather than accepting any combination — illegal states are
// unrepresentable. Provenance must be complete and schema-identical: every key, only
// real keys, each attributed to a layer whose file was actually present.
func TestNewResolvedConfigRejectsInconsistentAggregate(t *testing.T) {
	good := []LayerFact{mustFact(t, LayerShared, "/repo/awa.toml", true)}
	// full is a complete, all-default provenance map that each case perturbs.
	full := func(mut func(map[string]Layer)) map[string]Layer {
		o := DefaultOrigins()
		if mut != nil {
			mut(o)
		}
		return o
	}
	// changedTTL holds a non-default value, so attributing it to the default layer is
	// a semantic lie the constructor must reject.
	changedTTL := Defaults()
	changedTTL.Run.TTL = Duration(1)
	cases := []struct {
		name    string
		cfg     Config
		origins map[string]Layer
		layers  []LayerFact
	}{
		{"invalid config", Config{}, DefaultOrigins(), nil},
		{"default origin for changed value", changedTTL, DefaultOrigins(), nil},
		{"out-of-order layers", Defaults(), DefaultOrigins(), []LayerFact{mustFact(t, LayerLocal, "/l", true), mustFact(t, LayerShared, "/s", true)}},
		{"duplicate layer", Defaults(), DefaultOrigins(), []LayerFact{mustFact(t, LayerShared, "/a", true), mustFact(t, LayerShared, "/b", true)}},
		{"incomplete provenance", Defaults(), map[string]Layer{"run.ttl": LayerDefault}, nil},
		{"unknown key", Defaults(), full(func(o map[string]Layer) { o["run.no_such_key"] = LayerDefault }), nil},
		{"invalid origin", Defaults(), full(func(o map[string]Layer) { o["run.ttl"] = Layer(99) }), nil},
		{"origin without present fact", Defaults(), full(func(o map[string]Layer) { o["run.ttl"] = LayerShared }), nil},
		{"origin of absent file", Defaults(), full(func(o map[string]Layer) { o["run.ttl"] = LayerShared }), []LayerFact{mustFact(t, LayerShared, "/s", false)}},
	}
	for _, tc := range cases {
		if _, err := NewResolvedConfig(tc.cfg, tc.origins, tc.layers); err == nil {
			t.Errorf("%s: NewResolvedConfig should reject the inconsistent aggregate", tc.name)
		}
	}

	// A consistent aggregate is accepted.
	if _, err := NewResolvedConfig(Defaults(), full(func(o map[string]Layer) { o["run.ttl"] = LayerShared }), good); err != nil {
		t.Errorf("a consistent aggregate should be accepted, got %v", err)
	}
}

// TestOriginsFromKeepsValueAndProvenanceInAgreement proves OriginsFrom derives a
// provenance the constructor accepts: a config with an overridden value gets that
// key attributed to a real (file) layer, not falsely to default, while untouched
// keys stay default with no source path.
func TestOriginsFromKeepsValueAndProvenanceInAgreement(t *testing.T) {
	cfg := Defaults()
	cfg.Run.TTL = Duration(1)

	// Attributing the change to the default layer is rejected (the OC-16 lie)...
	if _, err := NewResolvedConfig(cfg, DefaultOrigins(), nil); err == nil {
		t.Fatal("a changed value attributed to default should be rejected")
	}

	// ...while OriginsFrom attributes the change to a local file, which the
	// constructor accepts given a present local fact.
	origins := OriginsFrom(cfg, LayerLocal)
	r, err := NewResolvedConfig(cfg, origins, []LayerFact{mustFact(t, LayerLocal, "/p/.awa/config.toml", true)})
	if err != nil {
		t.Fatalf("OriginsFrom result should be accepted: %v", err)
	}
	if got := r.OriginOf("run.ttl"); got != LayerLocal {
		t.Errorf("OriginOf(run.ttl) = %v, want local", got)
	}
	if got := r.SourcePathOf("run.ttl"); got != "/p/.awa/config.toml" {
		t.Errorf("SourcePathOf(run.ttl) = %q, want the local file", got)
	}
	// An untouched key stays default with no source path.
	if got := r.OriginOf("diff.algorithm"); got != LayerDefault {
		t.Errorf("OriginOf(diff.algorithm) = %v, want default", got)
	}
	if got := r.SourcePathOf("diff.algorithm"); got != "" {
		t.Errorf("SourcePathOf(diff.algorithm) = %q, want empty", got)
	}
}

// TestChangedKeysMapsEachFieldToItsKey pins every value-agreement comparator to
// exactly one key: mutating a single field relative to Defaults() must flag that
// field's dotted key and no other. Unlike an all-at-once test, this catches a
// mis-wired comparator (e.g. run.ttl's predicate accidentally reading
// max_stdout_size) — both fields would be equal to their defaults except the one
// mutated, so a wrong comparator flags the wrong key or nothing. The table lists one
// mutation per key; a key present in Keys() but absent here fails the coverage check
// below, so the table cannot silently omit a field either.
func TestChangedKeysMapsEachFieldToItsKey(t *testing.T) {
	if changed := changedKeys(Defaults()); len(changed) != 0 {
		t.Errorf("changedKeys(Defaults()) = %v, want empty", changed)
	}

	d := Defaults()
	// Enum fields use an out-of-range value so the test needs no knowledge of the
	// alternative constants.
	cases := []struct {
		key    string
		mutate func(*Config)
	}{
		{"scope.include", func(c *Config) { c.Scope.Include = []string{"src"} }},
		{"scope.extra_excludes", func(c *Config) { c.Scope.ExtraExcludes = []string{"x"} }},
		{"scope.use_gitignore", func(c *Config) { c.Scope.UseGitignore = !d.Scope.UseGitignore }},
		{"scope.use_awaignore", func(c *Config) { c.Scope.UseAwaignore = !d.Scope.UseAwaignore }},
		{"scope.follow_symlinks", func(c *Config) { c.Scope.FollowSymlinks = !d.Scope.FollowSymlinks }},
		{"scope.symlink_max_depth", func(c *Config) { c.Scope.SymlinkMaxDepth = d.Scope.SymlinkMaxDepth + 1 }},
		{"scope.allow_symlink_root_escape", func(c *Config) { c.Scope.AllowSymlinkRootEscape = !d.Scope.AllowSymlinkRootEscape }},
		{"history.extra_excludes", func(c *Config) { c.History.ExtraExcludes = []string{"h"} }},
		{"hashing.trust_mode", func(c *Config) { c.Hashing.TrustMode = TrustMode(99) }},
		{"hashing.max_file_size", func(c *Config) { c.Hashing.MaxFileSize = d.Hashing.MaxFileSize + 1 }},
		{"hashing.large_file_policy", func(c *Config) { c.Hashing.LargeFilePolicy = LargeFilePolicy(99) }},
		{"checkpoint.store_file_contents", func(c *Config) { c.Checkpoint.StoreFileContents = !d.Checkpoint.StoreFileContents }},
		{"checkpoint.diff_context", func(c *Config) { c.Checkpoint.DiffContext = d.Checkpoint.DiffContext + 1 }},
		{"checkpoint.rename_detection", func(c *Config) { c.Checkpoint.RenameDetection = !d.Checkpoint.RenameDetection }},
		{"run.default_scope", func(c *Config) { c.Run.DefaultScope = []string{"src"} }},
		{"run.extra_excludes", func(c *Config) { c.Run.ExtraExcludes = []string{"y"} }},
		{"run.use_gitignore", func(c *Config) { c.Run.UseGitignore = !d.Run.UseGitignore }},
		{"run.use_awaignore", func(c *Config) { c.Run.UseAwaignore = !d.Run.UseAwaignore }},
		{"run.env_allowlist", func(c *Config) { c.Run.EnvAllowlist = []string{"FOO"} }},
		{"run.ttl", func(c *Config) { c.Run.TTL = d.Run.TTL + 1 }},
		{"run.max_stdout_size", func(c *Config) { c.Run.MaxStdoutSize = d.Run.MaxStdoutSize + 1 }},
		{"run.max_stderr_size", func(c *Config) { c.Run.MaxStderrSize = d.Run.MaxStderrSize + 1 }},
		{"run.capture_output", func(c *Config) { c.Run.CaptureOutput = !d.Run.CaptureOutput }},
		{"run.cache_failed_runs", func(c *Config) { c.Run.CacheFailedRuns = !d.Run.CacheFailedRuns }},
		{"run.extra_effect_roots", func(c *Config) { c.Run.WatchedEffectRoots = []string{"z"} }},
		{"gc.keep_last_checkpoints", func(c *Config) { c.GC.KeepLastCheckpoints = d.GC.KeepLastCheckpoints + 1 }},
		{"gc.keep_runs_for", func(c *Config) { c.GC.KeepRunsFor = d.GC.KeepRunsFor + 1 }},
		{"gc.keep_restores_for", func(c *Config) { c.GC.KeepRestoresFor = d.GC.KeepRestoresFor + 1 }},
		{"diff.algorithm", func(c *Config) { c.Diff.Algorithm = DiffAlgorithm(99) }},
		{"locks.timeout", func(c *Config) { c.Locks.Timeout = d.Locks.Timeout + 1 }},
		{"ui.time", func(c *Config) { c.UI.Time = TimeDisplay(99) }},
	}

	covered := map[string]bool{}
	for _, tc := range cases {
		if covered[tc.key] {
			t.Errorf("table lists %q more than once", tc.key)
		}
		covered[tc.key] = true

		c := Defaults()
		tc.mutate(&c)
		changed := changedKeys(c)
		if !changed[tc.key] {
			t.Errorf("mutating %q did not flag it as changed (comparator missing or mis-wired)", tc.key)
		}
		if len(changed) != 1 {
			t.Errorf("mutating only %q flagged %v, want exactly that one key (a comparator reads the wrong field)", tc.key, keysOf(changed))
		}
	}

	// The table must exercise every key, so no comparator escapes the per-field check.
	for _, k := range Keys() {
		if !covered[k] {
			t.Errorf("key %q is not exercised by the per-field table", k)
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestLayerStringTokens(t *testing.T) {
	cases := map[Layer]string{
		LayerDefault: "default", LayerShared: "shared", LayerLocal: "local",
		LayerConfig: "config", LayerFlag: "flag",
	}
	for l, want := range cases {
		if got := l.String(); got != want {
			t.Errorf("Layer(%d).String() = %q, want %q", l, got, want)
		}
		if !l.Valid() {
			t.Errorf("Layer(%d) should be valid", l)
		}
	}
	if LayerShared.HasFile() != true || LayerFlag.HasFile() != false || LayerDefault.HasFile() != false {
		t.Error("HasFile should be true for file-backed layers only")
	}
}
