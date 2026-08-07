package runcache

import (
	"fmt"
	"strings"

	"awarer/internal/domain/hashing"
)

// EnvPresence is the closed classification of an allowlisted environment
// variable's state at capture time. It is a first-class part of the run key
// because the three states can each change a command's behavior, so they must
// key — and persist — distinctly. Crucially, it lets the durable record carry the
// unset/empty/set distinction without carrying the raw value: unset and empty need
// no value at all, and a set value is represented by its redacted identity.
type EnvPresence int

const (
	// PresenceUnset means the variable was not present in the child environment
	// (os.LookupEnv reported not-ok). It carries no value identity.
	PresenceUnset EnvPresence = iota
	// PresenceEmpty means the variable was present and set to the empty string. It
	// carries no value identity — "" needs no fingerprint and storing one would only
	// leak that the value was empty, which the presence token already says.
	PresenceEmpty
	// PresenceSet means the variable was present with a non-empty value. It carries a
	// non-zero EnvValueIdentity — the redacted fingerprint that lets the key and the
	// diff distinguish one set value from another without persisting the value.
	PresenceSet
)

// Valid reports whether p is a known presence class.
func (p EnvPresence) Valid() bool {
	return p == PresenceUnset || p == PresenceEmpty || p == PresenceSet
}

// String returns the stable persisted token.
func (p EnvPresence) String() string {
	switch p {
	case PresenceUnset:
		return "unset"
	case PresenceEmpty:
		return "empty"
	case PresenceSet:
		return "set"
	default:
		return "unknown"
	}
}

// ParseEnvPresence resolves a persisted presence token.
func ParseEnvPresence(s string) (EnvPresence, error) {
	switch s {
	case "unset":
		return PresenceUnset, nil
	case "empty":
		return PresenceEmpty, nil
	case "set":
		return PresenceSet, nil
	default:
		return 0, fmt.Errorf("invalid env presence %q: want unset, empty, or set", s)
	}
}

// EnvValueIdentity is the redacted identity of a non-empty environment value: the
// digest of the raw value, never the value itself. It is a distinct type from the
// input TreeHash and EffectHash so a value fingerprint can never be assigned where
// a tree or effect identity belongs, even though all three share the validated
// "blake3:hex" persisted form. The zero value means "no set value" (an unset or
// empty variable); IsZero reports it.
//
// This is the load-bearing privacy boundary: a set environment value contributes
// to cache identity through this fingerprint in memory and on disk, so a run's
// durable metadata can prove "the same value" or "a different value" without ever
// persisting the value — which is often a secret.
type EnvValueIdentity struct {
	th hashing.TreeHash
}

// NewEnvValueIdentity fingerprints a non-empty raw environment value through the
// project hasher. The caller must only invoke it for a set (non-empty) value;
// hashing "" or an unset variable would create a misleading non-zero identity for a
// state whose presence token already carries the full meaning.
func NewEnvValueIdentity(h hashing.Hasher, value string) EnvValueIdentity {
	return EnvValueIdentity{th: h.HashBytes([]byte(value))}
}

// ParseEnvValueIdentity parses a persisted "blake3:hex" env value identity.
func ParseEnvValueIdentity(s string) (EnvValueIdentity, error) {
	th, err := hashing.ParseTreeHash(s)
	if err != nil {
		return EnvValueIdentity{}, fmt.Errorf("invalid env value identity: %w", err)
	}
	return EnvValueIdentity{th: th}, nil
}

// IsZero reports whether the identity is unset (no set value fingerprinted).
func (i EnvValueIdentity) IsZero() bool { return i.th.IsZero() }

// String renders the canonical "blake3:hex" form; the zero value renders as the
// empty string.
func (i EnvValueIdentity) String() string { return i.th.String() }

// Short renders a compact "blake3:hex[:8]" marker for human display, e.g.
// "blake3:abcd1234". It is derived from the fingerprint, never the value, so it is
// safe to print. The zero value renders empty.
func (i EnvValueIdentity) Short() string {
	hex := i.th.Hex()
	if hex == "" {
		return ""
	}
	if len(hex) > 8 {
		hex = hex[:8]
	}
	return hashing.Namespace + ":" + hex
}

// EnvVar is one allowlisted environment variable's redacted keyed identity: its
// name, its presence class, and — only when set to a non-empty value — the value's
// identity fingerprint. It deliberately carries no raw value: the value flows to
// the child process separately at execution time and is never stored on this type,
// so neither the cache key nor the persisted record can express a raw secret.
//
// The fields are unexported and reachable only through EnvVarFromValue (capture) and
// NewEnvVar (read path), so an impossible presence/identity pairing — a set value
// without a fingerprint, or an unset/empty variable that carries one — is
// unrepresentable rather than merely rejected after the fact.
type EnvVar struct {
	name     string
	presence EnvPresence
	identity EnvValueIdentity
}

// Name returns the variable name.
func (v EnvVar) Name() string { return v.name }

// Presence returns the variable's presence class (unset/empty/set).
func (v EnvVar) Presence() EnvPresence { return v.presence }

// Identity returns the redacted value fingerprint, zero unless the variable is set
// to a non-empty value.
func (v EnvVar) Identity() EnvValueIdentity { return v.identity }

// EnvVarFromValue builds the redacted identity of an allowlisted variable from its
// looked-up state (value and presence, as from os.LookupEnv). It classifies the
// presence and, for a non-empty value, fingerprints it through the project hasher.
// The raw value is used only to derive the fingerprint and is not retained. The
// pairing it produces is always valid by construction.
func EnvVarFromValue(h hashing.Hasher, name, value string, present bool) EnvVar {
	switch {
	case !present:
		return EnvVar{name: name, presence: PresenceUnset}
	case value == "":
		return EnvVar{name: name, presence: PresenceEmpty}
	default:
		return EnvVar{name: name, presence: PresenceSet, identity: NewEnvValueIdentity(h, value)}
	}
}

// NewEnvVar rebuilds an EnvVar from persisted fields, rejecting an impossible
// presence/identity pairing so a hand-edited record cannot decode into a state a
// real capture could never produce: only a set value carries an identity, and a set
// value must carry one.
func NewEnvVar(name string, presence EnvPresence, identity EnvValueIdentity) (EnvVar, error) {
	if name == "" {
		return EnvVar{}, fmt.Errorf("environment variable has no name")
	}
	if strings.ContainsRune(name, '=') {
		return EnvVar{}, fmt.Errorf("environment variable has invalid name %q", name)
	}
	if !presence.Valid() {
		return EnvVar{}, fmt.Errorf("environment variable %q has invalid presence %v", name, presence)
	}
	switch presence {
	case PresenceSet:
		if identity.IsZero() {
			return EnvVar{}, fmt.Errorf("environment variable %q is set but carries no value identity", name)
		}
	default:
		if !identity.IsZero() {
			return EnvVar{}, fmt.Errorf("environment variable %q is %s but carries a value identity", name, presence)
		}
	}
	return EnvVar{name: name, presence: presence, identity: identity}, nil
}

// Present reports whether the variable was present in the child environment (set to
// a value or to the empty string), as opposed to unset.
func (v EnvVar) Present() bool { return v.presence != PresenceUnset }

// validate re-checks one variable's internal consistency at a boundary. Because the
// fields are unexported, the only invalid value that can reach here is the zero
// EnvVar (blank name) a decoder might leave in a slice; the constructors already
// make every other invalid pairing unrepresentable.
func (v EnvVar) validate() error {
	_, err := NewEnvVar(v.name, v.presence, v.identity)
	return err
}

// marker renders a non-secret marker for this variable's keyed state: "<unset>"
// when absent, "<empty>" when present but empty, and "<set:blake3:hex>" (the
// short value fingerprint) when set. It never returns the value itself, so a
// difference or diagnostic cannot leak a secret.
func (v EnvVar) marker() string {
	switch v.presence {
	case PresenceUnset:
		return "<unset>"
	case PresenceEmpty:
		return "<empty>"
	case PresenceSet:
		return "<set:" + v.identity.Short() + ">"
	default:
		return "<unknown>"
	}
}
