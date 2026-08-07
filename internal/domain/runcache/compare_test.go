package runcache_test

import (
	"testing"

	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/runcache"
	"awarer/internal/infra/blake3hash"
)

// baseKeyInput builds a minimal, internally consistent key input for comparison
// tests. Compare does not validate, so the exact hash values only need to be
// distinct where a test changes them.
func baseKeyInput(t *testing.T) runcache.KeyInput {
	t.Helper()
	h := blake3hash.New()
	cwd, err := runcache.NewExecutionCWD(".")
	if err != nil {
		t.Fatalf("cwd: %v", err)
	}
	return runcache.KeyInput{
		CacheSchemaVersion: runcache.CacheSchemaVersion,
		AwaVersion:         "test",
		InvocationMode:     runcache.InvocationArgv,
		Command:            runcache.Command{Argv: []string{"go", "test"}, RawExecutable: "go"},
		CWD:                cwd,
		InputTreeHash:      h.HashBytes([]byte("tree-a")),
		IncludeScope:       []string{"."},
		TrustMode:          config.TrustNormal,
		RunConfigHash:      hashing.ConfigHashFromTree(h.HashBytes([]byte("cfg-a"))),
		Env:                runcache.NewEnvironment([]runcache.EnvVar{evh(t, "CI", "x", true)}),
		Platform:           runcache.Platform{GOOS: "linux", GOARCH: "amd64"},
		StdinMode:          runcache.StdinNull,
	}
}

func hashOf(t *testing.T, s string) hashing.TreeHash {
	t.Helper()
	h := blake3hash.New()
	return h.HashBytes([]byte(s))
}

// evh builds a redacted env var through a fresh hasher, fingerprinting a non-empty
// value the same way capture does.
func evh(t *testing.T, name, value string, present bool) runcache.EnvVar {
	t.Helper()
	h := blake3hash.New()
	return runcache.EnvVarFromValue(h, name, value, present)
}

func TestCompareEqual(t *testing.T) {
	a := baseKeyInput(t)
	b := baseKeyInput(t)
	cmp := runcache.CompareKeyInputs(a, b)
	if !cmp.Equal() {
		t.Errorf("identical inputs differ: %+v", cmp.Differences)
	}
	if cmp.PrimaryReason() != "" {
		t.Errorf("PrimaryReason on equal = %q, want empty", cmp.PrimaryReason())
	}
}

func TestCompareScalarFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*runcache.KeyInput)
		want   runcache.DiffCode
	}{
		{"invocation", func(k *runcache.KeyInput) { k.InvocationMode = "shell" }, runcache.DiffInvocationChanged},
		{"command", func(k *runcache.KeyInput) { k.Command.Argv = []string{"go", "build"} }, runcache.DiffCommandChanged},
		{"raw-executable", func(k *runcache.KeyInput) { k.Command.RawExecutable = "gofmt" }, runcache.DiffCommandChanged},
		{"executable", func(k *runcache.KeyInput) {
			k.Command.ResolvedPath = "/usr/bin/go"
			k.Command.ResolvedStat = "stat"
		}, runcache.DiffExecutableChanged},
		{"cwd", func(k *runcache.KeyInput) { c, _ := runcache.NewExecutionCWD("sub"); k.CWD = c }, runcache.DiffCWDChanged},
		{"tree", func(k *runcache.KeyInput) { k.InputTreeHash = hashOf(t, "tree-b") }, runcache.DiffInputTreeChanged},
		{"scope", func(k *runcache.KeyInput) { k.IncludeScope = []string{"pkg"} }, runcache.DiffScopeChanged},
		{"trust", func(k *runcache.KeyInput) { k.TrustMode = config.TrustFast }, runcache.DiffTrustChanged},
		{"config", func(k *runcache.KeyInput) { k.RunConfigHash = hashing.ConfigHashFromTree(hashOf(t, "cfg-b")) }, runcache.DiffRunConfigChanged},
		{"platform", func(k *runcache.KeyInput) { k.Platform = runcache.Platform{GOOS: "darwin", GOARCH: "arm64"} }, runcache.DiffPlatformChanged},
		{"stdin", func(k *runcache.KeyInput) { k.StdinMode = runcache.StdinPipe }, runcache.DiffStdinChanged},
		{"tty", func(k *runcache.KeyInput) { k.TTYAllowed = true }, runcache.DiffTTYChanged},
		{"skipped", func(k *runcache.KeyInput) { k.AllowSkippedInputs = true }, runcache.DiffSkippedChanged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := baseKeyInput(t)
			b := baseKeyInput(t)
			tc.mutate(&b)
			cmp := runcache.CompareKeyInputs(a, b)
			if !hasCode(cmp, tc.want) {
				t.Errorf("mutate %s produced %v, want code %s", tc.name, cmp.Codes(), tc.want)
			}
		})
	}
}

func TestCompareEnvUnsetVsEmpty(t *testing.T) {
	set := func(v runcache.EnvVar) runcache.KeyInput {
		k := baseKeyInput(t)
		k.Env = runcache.NewEnvironment([]runcache.EnvVar{v})
		return k
	}
	present := set(evh(t, "CI", "x", true))
	empty := set(evh(t, "CI", "", true))
	unset := set(evh(t, "CI", "", false))

	// present -> empty is a value change, not unset.
	if c := envChange(t, present, empty); c != runcache.EnvValue {
		t.Errorf("present->empty = %s, want value", c)
	}
	// present -> unset distinguishes unset from empty.
	if c := envChange(t, present, unset); c != runcache.EnvUnset {
		t.Errorf("present->unset = %s, want unset", c)
	}
	// unset -> empty is a distinct, keyed transition (set as empty).
	if c := envChange(t, unset, empty); c != runcache.EnvSet {
		t.Errorf("unset->empty = %s, want set", c)
	}
}

func TestComparePrimaryReasonPriority(t *testing.T) {
	a := baseKeyInput(t)
	b := baseKeyInput(t)
	// Both the command and the input tree differ; the command outranks the tree.
	b.Command.Argv = []string{"go", "build"}
	b.InputTreeHash = hashOf(t, "tree-b")
	cmp := runcache.CompareKeyInputs(a, b)
	if got := cmp.PrimaryReason(); got != runcache.DiffCommandChanged {
		t.Errorf("PrimaryReason = %s, want command-changed", got)
	}
}

func hasCode(c runcache.KeyComparison, code runcache.DiffCode) bool {
	for _, d := range c.Differences {
		if d.Code == code {
			return true
		}
	}
	return false
}

func envChange(t *testing.T, a, b runcache.KeyInput) runcache.EnvChange {
	t.Helper()
	cmp := runcache.CompareKeyInputs(a, b)
	for _, d := range cmp.Differences {
		if d.Code == runcache.DiffEnvChanged && d.EnvName == "CI" {
			return d.EnvChange
		}
	}
	t.Fatalf("no env difference for CI: %+v", cmp.Differences)
	return ""
}
