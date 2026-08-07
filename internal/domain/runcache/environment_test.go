package runcache_test

import (
	"slices"
	"strings"
	"testing"

	"awarer/internal/domain/config"
	"awarer/internal/domain/runcache"
)

// envMap is a controlled ambient environment. Unlike os.LookupEnv it can express
// "unset" for a name the developer's own shell happens to define, which is exactly the
// distinction these tests are about.
type envMap map[string]string

func (m envMap) lookup(name string) (string, bool) {
	v, ok := m[name]
	return v, ok
}

func buildEnv(t testing.TB, ambient envMap, allowlist ...string) runcache.EffectiveEnvironment {
	t.Helper()
	return runcache.BuildEffectiveEnvironment(newHasher(t), "linux", allowlist, ambient.lookup)
}

// presenceOf reports the keyed presence token for name, or "" when the name is not keyed.
func presenceOf(env runcache.Environment, name string) string {
	for _, v := range env.Vars() {
		if v.Name() == name {
			return v.Presence().String()
		}
	}
	return ""
}

// localeNames are the nine selectors the run environment must preserve.
var localeNames = []string{
	"LANG", "LANGUAGE", "LC_ALL", "LC_COLLATE", "LC_CTYPE",
	"LC_MESSAGES", "LC_MONETARY", "LC_NUMERIC", "LC_TIME",
}

// The regression guard: a wrapped command that loses the caller's locale entirely makes
// a reader that decodes UTF-8 directly decode US-ASCII under awa instead. Every selector
// must reach the child.
func TestLocaleSelectorsAreInherited(t *testing.T) {
	ambient := envMap{"PATH": "/usr/bin"}
	for _, n := range localeNames {
		ambient[n] = "en_US.UTF-8"
	}

	child := buildEnv(t, ambient).ChildEnv()
	for _, n := range localeNames {
		if !slices.Contains(child, n+"=en_US.UTF-8") {
			t.Errorf("child environment is missing %s=en_US.UTF-8; the caller's locale must reach the command", n)
		}
	}
}

// TestEnvPresenceStatesAreIndependent proves the three states stay three states, for
// each locale selector on its own: unset is not passed and keys as unset, empty is
// passed as NAME= and keys as empty, and a value is passed byte for byte.
//
// The states are exercised one variable at a time so a bug that collapses, say, LC_TIME
// onto LANG cannot hide behind the others being correct.
func TestEnvPresenceStatesAreIndependent(t *testing.T) {
	for _, name := range localeNames {
		t.Run(name, func(t *testing.T) {
			t.Run("unset", func(t *testing.T) {
				env := buildEnv(t, envMap{"PATH": "/usr/bin"})
				if got := presenceOf(env.Keyed(), name); got != "unset" {
					t.Errorf("presence = %q, want unset", got)
				}
				for _, e := range env.ChildEnv() {
					if strings.HasPrefix(e, name+"=") {
						t.Errorf("child got %q for a variable the caller did not set; awa must never synthesize a locale", e)
					}
				}
			})
			t.Run("empty", func(t *testing.T) {
				env := buildEnv(t, envMap{"PATH": "/usr/bin", name: ""})
				if got := presenceOf(env.Keyed(), name); got != "empty" {
					t.Errorf("presence = %q, want empty", got)
				}
				if !slices.Contains(env.ChildEnv(), name+"=") {
					t.Errorf("child is missing %s= ; present-empty differs from absent and must be passed", name)
				}
			})
			t.Run("set", func(t *testing.T) {
				env := buildEnv(t, envMap{"PATH": "/usr/bin", name: "ru_RU.UTF-8"})
				if got := presenceOf(env.Keyed(), name); got != "set" {
					t.Errorf("presence = %q, want set", got)
				}
				if !slices.Contains(env.ChildEnv(), name+"=ru_RU.UTF-8") {
					t.Errorf("child is missing %s=ru_RU.UTF-8", name)
				}
			})
		})
	}
}

// TestUnsetUmbrellaIsNotSynthesized is the specific "awa does not choose a locale"
// proof: a caller with only LANG set must not cause LC_ALL — the umbrella that
// overrides every category — to appear, and a caller with only one category set must
// not cause the others to appear.
func TestUnsetUmbrellaIsNotSynthesized(t *testing.T) {
	env := buildEnv(t, envMap{"PATH": "/usr/bin", "LANG": "en_US.UTF-8"})

	for _, e := range env.ChildEnv() {
		name, _, _ := strings.Cut(e, "=")
		if name != "LANG" && slices.Contains(localeNames, name) {
			t.Errorf("child got %q; only LANG was set, so no other selector may be materialized", e)
		}
	}
	if got := presenceOf(env.Keyed(), "LC_ALL"); got != "unset" {
		t.Errorf("LC_ALL presence = %q, want unset: an absent umbrella must key as absent, not as a chosen value", got)
	}
}

// TestChildEnvIsNeverNil pins the invariant the process runner depends on: a nil child
// environment is os/exec's "inherit everything" sentinel, so producing one would leak
// the whole ambient environment past the allowlist. Even with an empty ambient and an
// empty allowlist the injected marker keeps the slice non-empty.
func TestChildEnvIsNeverNil(t *testing.T) {
	child := buildEnv(t, envMap{}).ChildEnv()
	if child == nil {
		t.Fatal("ChildEnv() is nil; the runner treats nil as inherit-everything")
	}
	if len(child) == 0 {
		t.Error("ChildEnv() is empty; the injected marker is always present in an executed child")
	}
}

// TestWrapperMarkerIsInjectedExactlyOnce proves the child sees exactly AWA_RUN=1 no
// matter what the caller's environment says. The ambient spellings here are the
// adversarial ones: absent, empty, a falsey "0", and a value chosen to look like a
// capability grant.
func TestWrapperMarkerIsInjectedExactlyOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ambient envMap
	}{
		{"absent", envMap{"PATH": "/usr/bin"}},
		{"empty", envMap{"PATH": "/usr/bin", "AWA_RUN": ""}},
		{"falsey", envMap{"PATH": "/usr/bin", "AWA_RUN": "0"}},
		{"attacker-chosen", envMap{"PATH": "/usr/bin", "AWA_RUN": "1 trusted=yes"}},
		{"lowercase spelling", envMap{"PATH": "/usr/bin", "awa_run": "spoofed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child := buildEnv(t, tc.ambient).ChildEnv()

			var got []string
			for _, e := range child {
				if name, _, _ := strings.Cut(e, "="); strings.EqualFold(name, config.WrapperMarkerName) {
					got = append(got, e)
				}
			}
			if want := []string{config.WrapperMarkerName + "=" + config.WrapperMarkerValue}; !slices.Equal(got, want) {
				t.Errorf("child marker entries = %v, want %v: the marker is awa's own statement and an ambient value must never survive into it", got, want)
			}
		})
	}
}

// TestInjectedFactIsKeyedButNotInherited proves the model reports origin honestly: the
// marker is keyed (so a pre-marker execution cannot be reused as though it observed it)
// while never being an inherited name, because awa states it rather than looking it up.
//
// The two halves are checked against their owners — EffectiveEnvNames for what is
// inherited, InjectedEnvFacts for what awa states — rather than by asking the assembled
// environment to describe itself.
func TestInjectedFactIsKeyedButNotInherited(t *testing.T) {
	ambient := envMap{"PATH": "/usr/bin", "AWA_RUN": "ambient"}
	env := buildEnv(t, ambient)

	for _, n := range runcache.EffectiveEnvNames("linux", nil) {
		if config.IsReservedEnvName(n) {
			t.Errorf("%q is reported as inherited; it is injected, and saying otherwise would credit the caller with awa's own fact", n)
		}
	}
	if got := presenceOf(env.Keyed(), config.WrapperMarkerName); got != "set" {
		t.Errorf("keyed marker presence = %q, want set: the marker must participate in cache identity", got)
	}
	facts := runcache.InjectedEnvFacts()
	if len(facts) != 1 || facts[0].Name != config.WrapperMarkerName || facts[0].Value != config.WrapperMarkerValue {
		t.Errorf("InjectedEnvFacts() = %v, want exactly the fixed marker", facts)
	}
	if got := facts[0].Assignment(); got != "AWA_RUN=1" {
		t.Errorf("Assignment() = %q, want AWA_RUN=1", got)
	}
}

// TestInjectedFactsCarryNoDynamicValue guards the "advisory, not evidence" boundary from
// the other side: the injected set must stay fixed. A run id, timestamp, or store path
// here would look authoritative while being trivially forgeable, and would make every
// run's environment — and therefore its key — unique.
func TestInjectedFactsCarryNoDynamicValue(t *testing.T) {
	first := runcache.InjectedEnvFacts()
	second := runcache.InjectedEnvFacts()
	if !slices.Equal(first, second) {
		t.Fatalf("InjectedEnvFacts() is not stable: %v then %v", first, second)
	}
	for _, f := range first {
		if f.Value != config.WrapperMarkerValue {
			t.Errorf("injected fact %q carries value %q; only the fixed marker value is allowed", f.Name, f.Value)
		}
	}
}

// TestChildEnvIsACopy proves a consumer cannot reach back into the value the key
// describes: the child environment handed to resolution and execution must stay exactly
// what the key says it is.
func TestChildEnvIsACopy(t *testing.T) {
	env := buildEnv(t, envMap{"PATH": "/usr/bin"})

	child := env.ChildEnv()
	child[0] = "PATH=/tmp/evil"
	if got := env.ChildEnv(); got[0] == "PATH=/tmp/evil" {
		t.Error("ChildEnv() shares its backing array; a consumer could change the environment the key claims")
	}
}

// TestKeyedEnvironmentHoldsNoRawValue is the privacy boundary at the assembly seam: the
// raw value flows to the child and nowhere else. Nothing reachable from the keyed
// environment may render it.
func TestKeyedEnvironmentHoldsNoRawValue(t *testing.T) {
	const sentinel = "ru_RU.UTF-8-sentinel-3f7a2b91"
	env := buildEnv(t, envMap{"PATH": "/usr/bin", "LC_CTYPE": sentinel})

	if !slices.Contains(env.ChildEnv(), "LC_CTYPE="+sentinel) {
		t.Fatal("the child must receive the real value; otherwise this test proves nothing about redaction")
	}
	for _, v := range env.Keyed().Vars() {
		if strings.Contains(v.Identity().String(), sentinel) || strings.Contains(v.Identity().Short(), sentinel) {
			t.Errorf("keyed variable %q renders the raw value", v.Name())
		}
	}
}

// TestLocaleFactsAreKeyedIndependently is the cache-identity half: changing any one
// locale fact must change the key, and restoring it must restore the key. A shared or
// collapsed representation would let a genuinely different run reuse a stale result.
func TestLocaleFactsAreKeyedIndependently(t *testing.T) {
	h := newHasher(t)
	base := envMap{"PATH": "/usr/bin"}
	for _, n := range localeNames {
		base[n] = "en_US.UTF-8"
	}

	keyFor := func(ambient envMap) runcache.RunKey {
		in := baseline(t, h)
		in.Env = runcache.BuildEffectiveEnvironment(h, "linux", nil, ambient.lookup).Keyed()
		return in.Compute(h)
	}

	original := keyFor(base)
	for _, name := range localeNames {
		t.Run(name, func(t *testing.T) {
			for _, mutation := range []struct {
				what  string
				apply func(envMap)
			}{
				{"changed value", func(m envMap) { m[name] = "ru_RU.UTF-8" }},
				{"emptied", func(m envMap) { m[name] = "" }},
				{"unset", func(m envMap) { delete(m, name) }},
			} {
				mutated := envMap{}
				for k, v := range base {
					mutated[k] = v
				}
				mutation.apply(mutated)
				if keyFor(mutated) == original {
					t.Errorf("%s did not change the key; a run under a different locale would reuse this result", mutation.what)
				}
			}
			// Restoring the exact state restores the exact key: the key must be a function
			// of the observed facts, not of the order or history of observation.
			restored := envMap{}
			for k, v := range base {
				restored[k] = v
			}
			if keyFor(restored) != original {
				t.Error("restoring the locale did not restore the key; identity must be reproducible")
			}
		})
	}
}

// TestMarkerIsPartOfCacheIdentity proves an execution that observed the marker cannot
// reuse a record written before the marker existed. That record is intact evidence — it
// simply describes a differently-shaped run — so it must miss rather than be replayed as
// though the child had seen the wrapper.
func TestMarkerIsPartOfCacheIdentity(t *testing.T) {
	h := newHasher(t)
	ambient := envMap{"PATH": "/usr/bin"}

	current := baseline(t, h)
	current.Env = runcache.BuildEffectiveEnvironment(h, "linux", nil, ambient.lookup).Keyed()

	// A pre-marker record: the same inherited names, without the injected fact.
	var preMarker []runcache.EnvVar
	for _, v := range current.Env.Vars() {
		if !config.IsReservedEnvName(v.Name()) {
			preMarker = append(preMarker, v)
		}
	}
	old := baseline(t, h)
	old.Env = runcache.NewEnvironment(preMarker)

	if old.Compute(h) == current.Compute(h) {
		t.Fatal("a pre-marker key input computes the current key; the marker must participate in identity")
	}
	// The old document is still a valid document. Refusing to validate it would turn
	// intact evidence into corruption instead of an honest miss.
	if err := old.Validate(); err != nil {
		t.Errorf("pre-marker key input no longer validates: %v", err)
	}
	// And it is a real difference the comparison reports as an environment change.
	cmp := runcache.CompareKeyInputs(old, current)
	if cmp.PrimaryReason() != runcache.DiffEnvChanged {
		t.Errorf("primary reason = %q, want %q", cmp.PrimaryReason(), runcache.DiffEnvChanged)
	}
}

// TestReservedNameWithImpossiblePresenceIsRejected covers the read-path shape invariant:
// an injected fact is always set, so a hand-edited record claiming otherwise describes a
// run awa could not have performed.
func TestReservedNameWithImpossiblePresenceIsRejected(t *testing.T) {
	h := newHasher(t)
	for _, presence := range []struct {
		name string
		v    runcache.EnvVar
	}{
		{"unset", runcache.EnvVarFromValue(h, config.WrapperMarkerName, "", false)},
		{"empty", runcache.EnvVarFromValue(h, config.WrapperMarkerName, "", true)},
	} {
		t.Run(presence.name, func(t *testing.T) {
			in := baseline(t, h)
			in.Env = runcache.NewEnvironment([]runcache.EnvVar{presence.v})
			err := in.Validate()
			if err == nil {
				t.Fatalf("key input with a %s reserved name validated", presence.name)
			}
			if !strings.Contains(err.Error(), "injected fact is always set") {
				t.Errorf("error = %q, want it to name the injected-fact invariant", err)
			}
			// The invariant belongs to the environment itself, so it holds wherever an
			// Environment is validated, not only through a full key input.
			if err := in.Env.Validate(); err == nil {
				t.Error("Environment.Validate accepted a reserved name that is not set")
			}
		})
	}
}

// TestEffectiveEnvNamesExcludesInjectedFacts pins the layering the projections depend on:
// the inherited-name owner must not report awa's own facts, or `awa config effective`
// would present an unconfigurable fact as part of the user's allowlist.
//
// The allowlist cases carry the weight. Config validation rejects a reserved name, so
// only exercising an ordinary allowlist would pass on a build that never enforced the
// rule here at all — and the failure it hides is not cosmetic: an inherited AWA_RUN
// would be keyed beside the injected one, and the duplicate makes the whole key input
// invalid.
func TestEffectiveEnvNamesExcludesInjectedFacts(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "freebsd", "windows"} {
		for _, allowlist := range [][]string{
			{"CI"},
			{"AWA_RUN"},
			{"CI", "awa_run", "NODE_ENV"},
		} {
			names := runcache.EffectiveEnvNames(goos, allowlist)
			for _, n := range names {
				if config.IsReservedEnvName(n) {
					t.Errorf("%s: EffectiveEnvNames(%v) returned the reserved name %q", goos, allowlist, n)
				}
			}
		}
	}
}

// TestReservedAllowlistNameCannotBeInherited is the aggregate-level half of the same
// invariant: even handed an allowlist config validation would have refused, the builder
// must produce one usable environment rather than a duplicated variable that only a
// later Validate would catch.
func TestReservedAllowlistNameCannotBeInherited(t *testing.T) {
	ambient := envMap{"PATH": "/usr/bin", config.WrapperMarkerName: "spoofed"}
	env := buildEnv(t, ambient, config.WrapperMarkerName)

	if err := env.Keyed().Validate(); err != nil {
		t.Errorf("the assembled environment does not validate: %v", err)
	}
	var markers []string
	for _, v := range env.Keyed().Vars() {
		if config.IsReservedEnvName(v.Name()) {
			markers = append(markers, v.Name())
		}
	}
	if len(markers) != 1 {
		t.Errorf("keyed reserved entries = %v, want exactly one", markers)
	}
	if want := []string{config.WrapperMarkerName + "=" + config.WrapperMarkerValue}; !slices.Equal(markerAssignments(env.ChildEnv()), want) {
		t.Errorf("child marker entries = %v, want %v: the caller's value must never be inherited", markerAssignments(env.ChildEnv()), want)
	}
}

// markerAssignments returns the child assignments for reserved names.
func markerAssignments(child []string) []string {
	var out []string
	for _, e := range child {
		if name, _, _ := strings.Cut(e, "="); config.IsReservedEnvName(name) {
			out = append(out, e)
		}
	}
	return out
}
