package runcache_test

import (
	"strings"
	"testing"

	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/runcache"
	"awarer/internal/infra/blake3hash"
)

func newHasher(t testing.TB) hashing.Hasher {
	t.Helper()
	h := blake3hash.New()
	return h
}

func treeHash(t testing.TB, h hashing.Hasher, s string) hashing.TreeHash {
	t.Helper()
	return h.HashBytes([]byte(s))
}

// ev builds a redacted env var from a raw value and presence, fingerprinting a
// non-empty value through the hasher — the same path envCheckpoint uses at capture.
func ev(h hashing.Hasher, name, value string, present bool) runcache.EnvVar {
	return runcache.EnvVarFromValue(h, name, value, present)
}

func configHash(t testing.TB, h hashing.Hasher, s string) hashing.ConfigHash {
	t.Helper()
	return hashing.ConfigHashFromTree(h.HashBytes([]byte(s)))
}

func baseline(t testing.TB, h hashing.Hasher) runcache.KeyInput {
	t.Helper()
	cwd, err := runcache.NewExecutionCWD(".")
	if err != nil {
		t.Fatalf("NewExecutionCWD: %v", err)
	}
	return runcache.KeyInput{
		CacheSchemaVersion: runcache.CacheSchemaVersion,
		AwaVersion:         "1.2.3",
		InvocationMode:     runcache.InvocationArgv,
		Command: runcache.Command{
			Argv:          []string{"go", "test", "./..."},
			RawExecutable: "go",
			ResolvedPath:  "/usr/bin/go",
			ResolvedStat:  "size=1 mode=755 mtime=0",
		},
		CWD:           cwd,
		InputTreeHash: treeHash(t, h, "tree"),
		Effect:        observedEffect(t, h, "effect"),
		IncludeScope:  []string{"."},
		ExcludeScope:  []string{"node_modules"},
		UseGitignore:  true,
		UseAwaignore:  true,
		TrustMode:     config.TrustNormal,
		RunConfigHash: configHash(t, h, "runcfg"),
		Env: runcache.NewEnvironment([]runcache.EnvVar{
			ev(h, "CI", "true", true),
		}),
		Platform:           runcache.Platform{GOOS: "linux", GOARCH: "amd64"},
		StdinMode:          runcache.StdinNull,
		TTYAllowed:         false,
		AllowSkippedInputs: false,
	}
}

func TestKeyStableForSameInput(t *testing.T) {
	h := newHasher(t)
	a := baseline(t, h).Compute(h)
	b := baseline(t, h).Compute(h)
	if a.String() != b.String() {
		t.Fatalf("identical inputs produced different keys:\n%s\n%s", a, b)
	}
	if a.IsZero() {
		t.Fatal("key is zero")
	}
}

func TestKeyEnvOrderIndependent(t *testing.T) {
	h := newHasher(t)
	in1 := baseline(t, h)
	in1.Env = runcache.NewEnvironment([]runcache.EnvVar{
		ev(h, "CI", "1", true),
		ev(h, "NODE_ENV", "test", true),
	})
	in2 := baseline(t, h)
	in2.Env = runcache.NewEnvironment([]runcache.EnvVar{
		ev(h, "NODE_ENV", "test", true),
		ev(h, "CI", "1", true),
	})
	if in1.Compute(h).String() != in2.Compute(h).String() {
		t.Fatal("env allowlist order changed the key")
	}
}

func TestKeyUnsetVsEmptyEnvDiffer(t *testing.T) {
	h := newHasher(t)
	unset := baseline(t, h)
	unset.Env = runcache.NewEnvironment([]runcache.EnvVar{ev(h, "X", "", false)})
	empty := baseline(t, h)
	empty.Env = runcache.NewEnvironment([]runcache.EnvVar{ev(h, "X", "", true)})
	if unset.Compute(h).String() == empty.Compute(h).String() {
		t.Fatal("unset and empty env produced the same key")
	}
}

func TestKeySensitivity(t *testing.T) {
	h := newHasher(t)
	base := baseline(t, h).Compute(h).String()

	cwdOther, _ := runcache.NewExecutionCWD("sub")

	cases := []struct {
		name   string
		mutate func(*runcache.KeyInput)
	}{
		{"arg order", func(in *runcache.KeyInput) {
			in.Command.Argv = []string{"go", "./...", "test"}
		}},
		{"arg duplicate", func(in *runcache.KeyInput) {
			in.Command.Argv = []string{"go", "test", "test", "./..."}
		}},
		{"raw executable", func(in *runcache.KeyInput) { in.Command.RawExecutable = "gofmt" }},
		{"resolved path", func(in *runcache.KeyInput) { in.Command.ResolvedPath = "/opt/go" }},
		{"resolved stat", func(in *runcache.KeyInput) { in.Command.ResolvedStat = "size=2 mode=755 mtime=9" }},
		{"cwd", func(in *runcache.KeyInput) { in.CWD = cwdOther }},
		{"tree hash", func(in *runcache.KeyInput) { in.InputTreeHash = treeHash(t, h, "other") }},
		{"effect", func(in *runcache.KeyInput) { in.Effect, _ = runcache.UnavailableEffect(3) }},
		{"include scope", func(in *runcache.KeyInput) { in.IncludeScope = []string{"src"} }},
		{"exclude scope", func(in *runcache.KeyInput) { in.ExcludeScope = []string{"dist"} }},
		{"use gitignore", func(in *runcache.KeyInput) { in.UseGitignore = false }},
		{"use awaignore", func(in *runcache.KeyInput) { in.UseAwaignore = false }},
		{"trust mode", func(in *runcache.KeyInput) { in.TrustMode = config.TrustStrict }},
		{"run config hash", func(in *runcache.KeyInput) { in.RunConfigHash = configHash(t, h, "other") }},
		{"env value", func(in *runcache.KeyInput) {
			in.Env = runcache.NewEnvironment([]runcache.EnvVar{ev(h, "CI", "false", true)})
		}},
		{"platform goos", func(in *runcache.KeyInput) { in.Platform.GOOS = "darwin" }},
		{"platform goarch", func(in *runcache.KeyInput) { in.Platform.GOARCH = "arm64" }},
		{"stdin mode", func(in *runcache.KeyInput) { in.StdinMode = runcache.StdinTTY }},
		{"tty allowed", func(in *runcache.KeyInput) { in.TTYAllowed = true }},
		{"allow skipped", func(in *runcache.KeyInput) { in.AllowSkippedInputs = true }},
		{"cache schema", func(in *runcache.KeyInput) { in.CacheSchemaVersion = runcache.CacheSchemaVersion + 5 }},
		{"awa version", func(in *runcache.KeyInput) { in.AwaVersion = "9.9.9" }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := baseline(t, h)
			c.mutate(&in)
			if got := in.Compute(h).String(); got == base {
				t.Errorf("mutating %s did not change the key", c.name)
			}
		})
	}
}

func TestKeyInputValidate(t *testing.T) {
	h := newHasher(t)
	if err := baseline(t, h).Validate(); err != nil {
		t.Fatalf("valid key input rejected: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*runcache.KeyInput)
	}{
		{"zero cache schema", func(in *runcache.KeyInput) { in.CacheSchemaVersion = 0 }},
		{"future cache schema", func(in *runcache.KeyInput) { in.CacheSchemaVersion = runcache.CacheSchemaVersion + 1 }},
		{"empty awa version", func(in *runcache.KeyInput) { in.AwaVersion = "" }},
		{"no argv", func(in *runcache.KeyInput) { in.Command.Argv = nil }},
		{"no raw executable", func(in *runcache.KeyInput) { in.Command.RawExecutable = "" }},
		{"raw executable not argv[0]", func(in *runcache.KeyInput) { in.Command.RawExecutable = "gofmt" }},
		{"resolved path without stat", func(in *runcache.KeyInput) { in.Command.ResolvedStat = "" }},
		{"resolved stat without path", func(in *runcache.KeyInput) { in.Command.ResolvedPath = "" }},
		{"unknown invocation mode", func(in *runcache.KeyInput) { in.InvocationMode = "shell" }},
		{"invalid trust mode", func(in *runcache.KeyInput) { in.TrustMode = config.TrustMode(99) }},
		{"invalid stdin mode", func(in *runcache.KeyInput) { in.StdinMode = runcache.StdinMode(99) }},
		{"windows env case duplicate", func(in *runcache.KeyInput) {
			in.Platform.GOOS = "windows"
			in.Env = runcache.NewEnvironment([]runcache.EnvVar{
				ev(h, "PATH", "a", true),
				ev(h, "Path", "b", true),
			})
		}},
		{"blank env name", func(in *runcache.KeyInput) {
			in.Env = runcache.NewEnvironment([]runcache.EnvVar{ev(h, "", "x", true)})
		}},
		{"env name with equals", func(in *runcache.KeyInput) {
			in.Env = runcache.NewEnvironment([]runcache.EnvVar{ev(h, "CI=true", "x", true)})
		}},
		{"duplicate env name", func(in *runcache.KeyInput) {
			in.Env = runcache.NewEnvironment([]runcache.EnvVar{
				ev(h, "CI", "true", true),
				ev(h, "CI", "false", true),
			})
		}},
		{"empty goos", func(in *runcache.KeyInput) { in.Platform.GOOS = "" }},
		{"empty goarch", func(in *runcache.KeyInput) { in.Platform.GOARCH = "" }},
		{"zero tree hash", func(in *runcache.KeyInput) { in.InputTreeHash = hashing.TreeHash{} }},
		{"empty include scope", func(in *runcache.KeyInput) { in.IncludeScope = nil }},
		{"empty include element", func(in *runcache.KeyInput) { in.IncludeScope = []string{"src", ""} }},
		{"absolute include element", func(in *runcache.KeyInput) { in.IncludeScope = []string{"/etc"} }},
		{"escaping include element", func(in *runcache.KeyInput) { in.IncludeScope = []string{"../outside"} }},
		{"non-canonical include", func(in *runcache.KeyInput) { in.IncludeScope = []string{"src", "src/sub"} }},
		{"unsorted include", func(in *runcache.KeyInput) { in.IncludeScope = []string{"b", "a"} }},
		{"empty exclude element", func(in *runcache.KeyInput) { in.ExcludeScope = []string{"node_modules", ""} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := baseline(t, h)
			c.mutate(&in)
			if err := in.Validate(); err == nil {
				t.Errorf("invalid key input (%s) accepted", c.name)
			}
		})
	}
}

func TestEffectiveEnvNames(t *testing.T) {
	// Windows treats names case-insensitively: "Path" duplicates the baseline PATH
	// and is dropped, keeping the first spelling; a distinct name survives.
	win := runcache.EffectiveEnvNames("windows", []string{"Path", "FOO"})
	if !contains(win, "PATH") || contains(win, "Path") {
		t.Errorf("windows merge = %v, want PATH kept and Path dropped", win)
	}
	if !contains(win, "FOO") {
		t.Errorf("windows merge = %v, want FOO kept", win)
	}
	if countFold(win, "path") != 1 {
		t.Errorf("windows merge = %v, want exactly one PATH-like name", win)
	}

	// Unix is case-sensitive: PATH and Path are different variables, both kept.
	unix := runcache.EffectiveEnvNames("linux", []string{"Path", "FOO"})
	if !contains(unix, "PATH") || !contains(unix, "Path") {
		t.Errorf("unix merge = %v, want both PATH and Path", unix)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func countFold(xs []string, x string) int {
	n := 0
	for _, v := range xs {
		if strings.EqualFold(v, x) {
			n++
		}
	}
	return n
}

func TestExecutionCWD(t *testing.T) {
	if _, err := runcache.NewExecutionCWD(""); err == nil {
		t.Error("empty cwd should be rejected")
	}
	if _, err := runcache.NewExecutionCWD("/abs"); err == nil {
		t.Error("absolute cwd should be rejected")
	}
	if _, err := runcache.NewExecutionCWD("../escape"); err == nil {
		t.Error("escaping cwd should be rejected")
	}
	c, err := runcache.NewExecutionCWD("./a/../b")
	if err != nil {
		t.Fatalf("NewExecutionCWD: %v", err)
	}
	if c.String() != "b" {
		t.Errorf("cwd normalized to %q, want %q", c.String(), "b")
	}
	root, _ := runcache.NewExecutionCWD(".")
	if root.String() != "." {
		t.Errorf("root cwd = %q, want .", root.String())
	}
}

func TestParseRunKeyRoundTrip(t *testing.T) {
	h := newHasher(t)
	k := baseline(t, h).Compute(h)
	parsed, err := runcache.ParseRunKey(k.String())
	if err != nil {
		t.Fatalf("ParseRunKey: %v", err)
	}
	if parsed.String() != k.String() || parsed.Hex() != k.Hex() {
		t.Errorf("round trip mismatch: %s vs %s", parsed, k)
	}
	if _, err := runcache.ParseRunKey("not-a-key"); err == nil {
		t.Error("invalid key string should be rejected")
	}
}
