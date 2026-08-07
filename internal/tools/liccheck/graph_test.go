package main

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// TestGoEnvPinsHermeticPolicyOverAmbient proves the hermetic values win over a hostile
// ambient environment and that the narrow signature offers no way to override the policy.
//
// Every pinned variable is poisoned, not a representative sample: each one is pinned
// because it can change what the child produces, so a pin that quietly stopped applying
// is exactly the failure this test exists to catch. The poison values are also chosen to
// be visibly wrong — an instruction set the release matrix does not target, an experiment
// that is off by default, a FIPS mode the project does not ship — so a leak is a
// mismatch rather than a coincidence.
func TestGoEnvPinsHermeticPolicyOverAmbient(t *testing.T) {
	t.Setenv("GOWORK", "/tmp/evil.work")
	t.Setenv("GOFLAGS", "-mod=mod -tags=evil")
	t.Setenv("GOPROXY", "https://evil.example.com")
	t.Setenv("GOENV", "/tmp/evil.env")
	t.Setenv("GOTOOLCHAIN", "go9.9.9")
	t.Setenv("CGO_ENABLED", "1")
	t.Setenv("GOOS", "plan9")
	t.Setenv("GOARCH", "386")
	t.Setenv("GOAMD64", "v3")
	t.Setenv("GOARM64", "v9.0")
	t.Setenv("GOEXPERIMENT", "cgocheck2")
	t.Setenv("GOFIPS140", "latest")

	env, err := goEnv("linux", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			got[k] = v
		}
	}
	want := map[string]string{
		"GOWORK":       "off",
		"GOFLAGS":      "-mod=readonly",
		"GOENV":        "off",
		"GOTOOLCHAIN":  "local",
		"GOPROXY":      "off",
		"CGO_ENABLED":  "0",
		"GOAMD64":      "v1",
		"GOARM64":      "v8.0",
		"GOEXPERIMENT": "",
		"GOFIPS140":    "off",
		"GOOS":         "linux",
		"GOARCH":       "arm64",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("goEnv[%s] = %q, want %q (ambient value must not leak through)", k, got[k], v)
		}
	}

	// Pinned-empty is not the same as absent. GOEXPERIMENT must be handed to the child as
	// a present, empty entry: dropping it would let the value the parent inherited stand,
	// which is the leak this pin exists to close.
	if !slices.Contains(env, "GOEXPERIMENT=") {
		t.Errorf("goEnv does not carry an explicit empty GOEXPERIMENT entry: %v", env)
	}

	// Each key appears exactly once (no duplicate that could let the child pick the
	// ambient value).
	counts := map[string]int{}
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok {
			counts[k]++
		}
	}
	for k, n := range counts {
		if n != 1 {
			t.Errorf("key %s appears %d times, want exactly 1", k, n)
		}
	}
}

// TestGoEnvRejectsInvalidTargets proves a caller cannot smuggle a blank or corrupt
// target through the only inputs the API accepts.
func TestGoEnvRejectsInvalidTargets(t *testing.T) {
	for _, tc := range []struct{ goos, goarch string }{
		{"", "arm64"},
		{"linux", ""},
		{"li nux", "arm64"},
		{"linux", "arm=64"},
	} {
		if _, err := goEnv(tc.goos, tc.goarch); err == nil {
			t.Errorf("goEnv(%q,%q): want error", tc.goos, tc.goarch)
		}
	}
}

// TestIdentityOfRejectsUnreproducibleReplacements covers the graph side of the
// replacement rule. The manifest side is enforced when a row is parsed; this is the
// other half — what happens when `go list` itself reports a replacement.
//
// A local `replace => ../somewhere` has no upstream version and resolves differently
// on every machine, so accepting it would mean auditing whatever bytes happened to sit
// on the auditor's disk and calling the result a release fact. It must fail closed,
// and the module@version form must still be carried through with both halves intact.
func TestIdentityOfRejectsUnreproducibleReplacements(t *testing.T) {
	t.Run("local replacement refused", func(t *testing.T) {
		_, err := identityOf(&listedModule{
			Path: "example.com/a", Version: "v1.0.0",
			Replace: &listedModule{Path: "../local", Dir: "/home/someone/local"},
		})
		if err == nil {
			t.Fatal("a local filesystem replacement must be refused, not audited")
		}
		if !strings.Contains(err.Error(), "local replacements are not supported") {
			t.Errorf("the diagnostic does not name the refusal: %v", err)
		}
	})

	t.Run("module@version replacement carried", func(t *testing.T) {
		id, err := identityOf(&listedModule{
			Path: "example.com/a", Version: "v1.0.0",
			Replace: &listedModule{Path: "example.com/fork", Version: "v2.3.4"},
		})
		if err != nil {
			t.Fatal(err)
		}
		// The string form keys the evidence index, so it has to carry both halves: the
		// original path a manifest row is matched by, and the identity whose bytes are read.
		if got, want := id.String(), "example.com/a@v1.0.0=>example.com/fork@v2.3.4"; got != want {
			t.Errorf("identity = %q, want %q", got, want)
		}
	})

	t.Run("unreplaced module", func(t *testing.T) {
		id, err := identityOf(&listedModule{Path: "example.com/a", Version: "v1.0.0"})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := id.String(), "example.com/a@v1.0.0"; got != want {
			t.Errorf("identity = %q, want %q", got, want)
		}
	})
}

// TestUnionPreservesPerTargetReachability traces one real module through the union.
//
// github.com/google/uuid is linked into five of the six release targets and not into
// windows/amd64, so it is the module that would disappear from a union that keyed only
// on the host, and the one whose target set would be wrong if per-target collection
// silently reused another target's graph. A module selected on a single target is still
// a production obligation, which is the property this protects.
func TestUnionPreservesPerTargetReachability(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live go list under -short")
	}
	root := testRoot(t)
	union, err := collectProduction(context.Background(), root, releaseTargets)
	if err != nil {
		t.Fatal(err)
	}

	om, ok := union["github.com/google/uuid"]
	if !ok {
		t.Fatalf("the union lost github.com/google/uuid; it is production on five targets")
	}
	want := []string{"darwin/amd64", "darwin/arm64", "freebsd/amd64", "linux/amd64", "linux/arm64"}
	if !slices.Equal(om.targets, want) {
		t.Errorf("github.com/google/uuid targets = %v, want %v", om.targets, want)
	}

	// The same module read from one target alone must carry only that target, so the
	// union above is a fold of per-target evidence rather than a single broad query.
	mods, err := productionModules(context.Background(), root, "windows/amd64")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range mods {
		if m.path == "github.com/google/uuid" {
			t.Error("windows/amd64 selects github.com/google/uuid; the per-target graph is not target-specific")
		}
	}
}
