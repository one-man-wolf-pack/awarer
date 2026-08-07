package config

import "testing"

func TestDefaults(t *testing.T) {
	c := Defaults()
	if c.Hashing.TrustMode != TrustNormal {
		t.Errorf("trust_mode = %v, want normal", c.Hashing.TrustMode)
	}
	if c.Hashing.MaxFileSize != 50*miB {
		t.Errorf("max_file_size = %v, want 50MiB", c.Hashing.MaxFileSize)
	}
	if c.Hashing.LargeFilePolicy != LargeFileHashOnly {
		t.Errorf("large_file_policy = %v, want hash-only", c.Hashing.LargeFilePolicy)
	}
	if c.Run.TTL.String() != "7d" {
		t.Errorf("run.ttl = %v, want 7d", c.Run.TTL)
	}
	if c.GC.KeepRunsFor.String() != "14d" {
		t.Errorf("gc.keep_runs_for = %v, want 14d", c.GC.KeepRunsFor)
	}
	if c.Locks.Timeout.String() != "5s" {
		t.Errorf("locks.timeout = %v, want 5s", c.Locks.Timeout)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Defaults() should be valid: %v", err)
	}
}

func TestStrictDiffersOnlyInTrustMode(t *testing.T) {
	s := Strict()
	if s.Hashing.TrustMode != TrustStrict {
		t.Errorf("strict trust_mode = %v, want strict", s.Hashing.TrustMode)
	}
	// Everything else must match the defaults.
	d := Defaults()
	d.Hashing.TrustMode = TrustStrict
	if !equalConfig(d, s) {
		t.Error("Strict() should differ from Defaults() only in trust_mode")
	}
	if err := s.Validate(); err != nil {
		t.Errorf("Strict() should be valid: %v", err)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"empty include", func(c *Config) { c.Scope.Include = nil }},
		{"empty run default scope", func(c *Config) { c.Run.DefaultScope = nil }},
		{"empty include element", func(c *Config) { c.Scope.Include = []string{""} }},
		{"absolute include element", func(c *Config) { c.Scope.Include = []string{"/etc"} }},
		{"escaping include element", func(c *Config) { c.Scope.Include = []string{"../outside"} }},
		{"escaping nested include element", func(c *Config) { c.Scope.Include = []string{"a/../../b"} }},
		{"absolute default scope element", func(c *Config) { c.Run.DefaultScope = []string{"/var"} }},
		{"empty scope extra exclude element", func(c *Config) { c.Scope.ExtraExcludes = []string{"foo", ""} }},
		{"empty history extra exclude element", func(c *Config) { c.History.ExtraExcludes = []string{""} }},
		{"empty run extra exclude element", func(c *Config) { c.Run.ExtraExcludes = []string{""} }},
		{"negative symlink depth", func(c *Config) { c.Scope.SymlinkMaxDepth = -1 }},
		{"zero max file size", func(c *Config) { c.Hashing.MaxFileSize = 0 }},
		{"negative diff context", func(c *Config) { c.Checkpoint.DiffContext = -1 }},
		{"zero stdout size", func(c *Config) { c.Run.MaxStdoutSize = 0 }},
		{"negative keep checkpoints", func(c *Config) { c.GC.KeepLastCheckpoints = -1 }},
		{"negative locks timeout", func(c *Config) { c.Locks.Timeout = -1 }},
		{"out-of-range trust mode", func(c *Config) { c.Hashing.TrustMode = TrustMode(999) }},
		{"out-of-range large file policy", func(c *Config) { c.Hashing.LargeFilePolicy = LargeFilePolicy(999) }},
		{"empty env allowlist name", func(c *Config) { c.Run.EnvAllowlist = []string{"CI", ""} }},
		{"env allowlist name with equals", func(c *Config) { c.Run.EnvAllowlist = []string{"CI=true"} }},
		{"duplicate env allowlist name", func(c *Config) { c.Run.EnvAllowlist = []string{"CI", "CI"} }},
		{"case-insensitive duplicate env name", func(c *Config) { c.Run.EnvAllowlist = []string{"FOO", "foo"} }},
		{"env name collides with baseline", func(c *Config) { c.Run.EnvAllowlist = []string{"Path"} }},
		{"env name is a baseline name", func(c *Config) { c.Run.EnvAllowlist = []string{"HOME"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Defaults()
			tt.mutate(&c)
			if err := c.Validate(); err == nil {
				t.Errorf("Validate() = nil, want error for %s", tt.name)
			}
		})
	}
}

// TestScanScopeValidate covers the scope-level validation seam: a scan scope
// assembled outside decode (notably a run scope built from CLI overrides) is
// checked for a non-empty, in-root include and non-empty exclude entries, since
// it is no longer a derived Config field that Config.Validate could vouch for.
func TestScanScopeValidate(t *testing.T) {
	if err := Defaults().HistoryScanScope().Validate(); err != nil {
		t.Errorf("default history scan scope should be valid: %v", err)
	}
	bad := []struct {
		name  string
		scope ScanScope
	}{
		{"empty include", ScanScope{Include: nil}},
		{"escaping include", ScanScope{Include: []string{"../oops"}}},
		{"absolute include", ScanScope{Include: []string{"/etc"}}},
		{"empty exclude element", ScanScope{Include: []string{"."}, Exclude: []string{""}}},
	}
	for _, b := range bad {
		t.Run(b.name, func(t *testing.T) {
			if err := b.scope.Validate(); err == nil {
				t.Errorf("ScanScope.Validate() = nil, want error for %s", b.name)
			}
		})
	}
}

func TestBuiltinExcludesPresent(t *testing.T) {
	c := Defaults()
	want := map[string]bool{".git": false, ".awa": false, "node_modules": false, "target": false}
	for _, e := range c.HistoryScanScope().Exclude {
		if _, ok := want[e]; ok {
			want[e] = true
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("default scope.exclude missing %q", k)
		}
	}
}

// TestHistoryOnlyExcludesAreNotInRunDefaults locks the core layering split: the
// generated-artifact defaults (dist/build/coverage) hide from the history family
// but must remain visible to the run input scan, while the baseline and protected
// excludes apply to both.
func TestHistoryOnlyExcludesAreNotInRunDefaults(t *testing.T) {
	c := Defaults()
	has := func(list []string, v string) bool {
		for _, e := range list {
			if e == v {
				return true
			}
		}
		return false
	}
	histExcludes := c.HistoryScanScope().Exclude
	runExcludes := EffectiveRunExcludes(c.Scope.ExtraExcludes, c.Run.ExtraExcludes)
	for _, h := range HistoryDefaultExcludes() {
		if !has(histExcludes, h) {
			t.Errorf("history default %q missing from effective history excludes", h)
		}
		if has(runExcludes, h) {
			t.Errorf("history-only default %q leaked into run excludes", h)
		}
	}
	for _, b := range append(ProtectedExcludes(), BaselineExcludes()...) {
		if !has(histExcludes, b) {
			t.Errorf("baseline/protected %q missing from history excludes", b)
		}
		if !has(runExcludes, b) {
			t.Errorf("baseline/protected %q missing from run excludes", b)
		}
	}
}

// TestEffectiveExcludesAppendUserExtras confirms the user's additive excludes are
// merged in the documented order: common (scope) extras reach both families, the
// history extras reach only history, and the run extras reach only run.
func TestEffectiveExcludesAppendUserExtras(t *testing.T) {
	hist := EffectiveHistoryExcludes([]string{"common"}, []string{"histonly"})
	run := EffectiveRunExcludes([]string{"common"}, []string{"runonly"})
	contains := func(list []string, v string) bool {
		for _, e := range list {
			if e == v {
				return true
			}
		}
		return false
	}
	if !contains(hist, "common") || !contains(hist, "histonly") {
		t.Errorf("history excludes missing user extras: %v", hist)
	}
	if contains(hist, "runonly") {
		t.Errorf("run-only extra leaked into history excludes: %v", hist)
	}
	if !contains(run, "common") || !contains(run, "runonly") {
		t.Errorf("run excludes missing user extras: %v", run)
	}
	if contains(run, "histonly") {
		t.Errorf("history-only extra leaked into run excludes: %v", run)
	}
}

func equalConfig(a, b Config) bool {
	// Compare slice fields explicitly; the structs hold slices and so are not
	// comparable with ==. Then zero the slices and compare the remaining
	// scalar fields with ==.
	if !equalStrings(a.Scope.Include, b.Scope.Include) ||
		!equalStrings(a.Scope.ExtraExcludes, b.Scope.ExtraExcludes) ||
		!equalStrings(a.History.ExtraExcludes, b.History.ExtraExcludes) ||
		!equalStrings(a.Run.DefaultScope, b.Run.DefaultScope) ||
		!equalStrings(a.Run.ExtraExcludes, b.Run.ExtraExcludes) ||
		!equalStrings(a.Run.EnvAllowlist, b.Run.EnvAllowlist) {
		return false
	}
	scopeScalarsEqual := a.Scope.UseGitignore == b.Scope.UseGitignore &&
		a.Scope.UseAwaignore == b.Scope.UseAwaignore &&
		a.Scope.FollowSymlinks == b.Scope.FollowSymlinks &&
		a.Scope.SymlinkMaxDepth == b.Scope.SymlinkMaxDepth &&
		a.Scope.AllowSymlinkRootEscape == b.Scope.AllowSymlinkRootEscape
	runScalarsEqual := a.Run.UseGitignore == b.Run.UseGitignore &&
		a.Run.UseAwaignore == b.Run.UseAwaignore &&
		a.Run.TTL == b.Run.TTL &&
		a.Run.MaxStdoutSize == b.Run.MaxStdoutSize &&
		a.Run.MaxStderrSize == b.Run.MaxStderrSize &&
		a.Run.CaptureOutput == b.Run.CaptureOutput &&
		a.Run.CacheFailedRuns == b.Run.CacheFailedRuns
	return scopeScalarsEqual &&
		runScalarsEqual &&
		a.Hashing == b.Hashing &&
		a.Checkpoint == b.Checkpoint &&
		a.GC == b.GC &&
		a.Diff == b.Diff &&
		a.Locks == b.Locks
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestScopeCanonicalInclude(t *testing.T) {
	eq := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	cases := []struct {
		in   []string
		want []string
	}{
		{[]string{"src"}, []string{"src"}},
		{[]string{"./src"}, []string{"src"}},                   // ./src and src are the same
		{[]string{"src/"}, []string{"src"}},                    // trailing slash cleaned
		{[]string{"a/../b"}, []string{"b"}},                    // cleaned
		{[]string{"."}, []string{"."}},                         // whole tree
		{[]string{".", "src"}, []string{"."}},                  // "." subsumes everything
		{[]string{""}, []string{"."}},                          // empty cleans to "."
		{[]string{"b", "a", "b"}, []string{"a", "b"}},          // dedup + sort
		{[]string{"src", "src/internal"}, []string{"src"}},     // child under parent collapses
		{[]string{"src/internal", "src"}, []string{"src"}},     // order-independent
		{[]string{"src", "src/a", "src/a/b"}, []string{"src"}}, // deep nesting collapses
		{[]string{"src", "srcfoo"}, []string{"src", "srcfoo"}}, // prefix-but-not-ancestor kept
		{[]string{"a/b", "a/c", "a"}, []string{"a"}},           // siblings collapse under parent
		{[]string{"a/b", "a/c"}, []string{"a/b", "a/c"}},       // no common include parent kept
	}
	for _, c := range cases {
		got := CanonicalInclude(c.in)
		if !eq(got, c.want) {
			t.Errorf("CanonicalInclude(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
