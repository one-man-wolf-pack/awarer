package runcache_test

import (
	"strings"
	"testing"

	"awarer/internal/domain/runcache"
	"awarer/internal/infra/blake3hash"
)

func TestEnvPresenceRoundTrip(t *testing.T) {
	for _, p := range []runcache.EnvPresence{runcache.PresenceUnset, runcache.PresenceEmpty, runcache.PresenceSet} {
		got, err := runcache.ParseEnvPresence(p.String())
		if err != nil {
			t.Errorf("ParseEnvPresence(%q): %v", p.String(), err)
		}
		if got != p {
			t.Errorf("round trip %v -> %q -> %v", p, p.String(), got)
		}
		if !p.Valid() {
			t.Errorf("%v reports invalid", p)
		}
	}
	if _, err := runcache.ParseEnvPresence("bogus"); err == nil {
		t.Error("ParseEnvPresence accepted a bogus token")
	}
}

func TestEnvVarFromValueClassifies(t *testing.T) {
	h := blake3hash.New()
	cases := []struct {
		name     string
		value    string
		present  bool
		presence runcache.EnvPresence
		hasID    bool
	}{
		{"unset", "", false, runcache.PresenceUnset, false},
		{"empty", "", true, runcache.PresenceEmpty, false},
		{"set", "x", true, runcache.PresenceSet, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := runcache.EnvVarFromValue(h, "VAR", c.value, c.present)
			if v.Presence() != c.presence {
				t.Errorf("presence = %v, want %v", v.Presence(), c.presence)
			}
			if v.Identity().IsZero() == c.hasID {
				t.Errorf("identity present = %v, want %v", !v.Identity().IsZero(), c.hasID)
			}
			if v.Present() != c.present {
				t.Errorf("Present() = %v, want %v", v.Present(), c.present)
			}
			if err := runcache.NewEnvironment([]runcache.EnvVar{v}).Validate(); err != nil {
				t.Errorf("valid var rejected: %v", err)
			}
		})
	}
}

func TestEnvVarFromValueFingerprintsDistinctValues(t *testing.T) {
	h := blake3hash.New()
	a := runcache.EnvVarFromValue(h, "VAR", "one", true)
	b := runcache.EnvVarFromValue(h, "VAR", "two", true)
	if a.Identity().String() == b.Identity().String() {
		t.Error("distinct values produced the same fingerprint")
	}
	// The fingerprint is stable and never contains the raw value.
	c := runcache.EnvVarFromValue(h, "VAR", "one", true)
	if a.Identity().String() != c.Identity().String() {
		t.Error("identical values produced different fingerprints")
	}
	if strings.Contains(a.Identity().String(), "one") {
		t.Errorf("fingerprint %q leaks the value", a.Identity().String())
	}
}

func TestNewEnvVarRejectsImpossiblePairings(t *testing.T) {
	h := blake3hash.New()
	id := runcache.NewEnvValueIdentity(h, "x")

	cases := []struct {
		name     string
		varName  string
		presence runcache.EnvPresence
		identity runcache.EnvValueIdentity
	}{
		{"blank name", "", runcache.PresenceUnset, runcache.EnvValueIdentity{}},
		{"equals in name", "A=B", runcache.PresenceUnset, runcache.EnvValueIdentity{}},
		{"set without identity", "A", runcache.PresenceSet, runcache.EnvValueIdentity{}},
		{"unset with identity", "A", runcache.PresenceUnset, id},
		{"empty with identity", "A", runcache.PresenceEmpty, id},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := runcache.NewEnvVar(c.varName, c.presence, c.identity); err == nil {
				t.Errorf("NewEnvVar(%q, %v, id?) accepted an impossible pairing", c.varName, c.presence)
			}
		})
	}

	// The consistent pairings are accepted.
	if _, err := runcache.NewEnvVar("A", runcache.PresenceSet, id); err != nil {
		t.Errorf("set-with-identity rejected: %v", err)
	}
	if _, err := runcache.NewEnvVar("A", runcache.PresenceEmpty, runcache.EnvValueIdentity{}); err != nil {
		t.Errorf("empty-without-identity rejected: %v", err)
	}
}
