package runcache

import (
	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
)

// InjectedFact is one fixed, awa-owned name/value pair placed into an executed child's
// environment. It is a distinct type from EnvVar because it has a different origin and a
// different truth: an EnvVar records what awa *found* in the caller's environment and
// keeps only a redacted identity of it, while an InjectedFact is what awa *states*, so
// its value is a product constant and is safe to print.
type InjectedFact struct {
	Name  string
	Value string
}

// Assignment renders the fact in the NAME=VALUE form a child environment uses.
func (f InjectedFact) Assignment() string { return f.Name + "=" + f.Value }

// InjectedEnvFacts returns the closed set of facts awa injects into every actually
// executed child. It is fixed and deliberately tiny: the wrapper marker and nothing
// else. A cache hit executes no child and therefore injects nothing.
//
// Nothing dynamic may join this set. A run id, checkpoint id, timestamp, store path, or
// cache state would be unstable, machine-local, and — because the environment is
// forgeable — an evidence channel that proves nothing while looking authoritative.
func InjectedEnvFacts() []InjectedFact {
	return []InjectedFact{{Name: config.WrapperMarkerName, Value: config.WrapperMarkerValue}}
}

// EffectiveEnvironment is the complete environment one run assembles: the allowlisted
// values inherited from the caller plus the fixed facts awa injects. It exists so the
// two things that must never disagree — what the cache key says the child observed, and
// what the child actually received — are produced together, once, from one pass.
//
// The child receives exactly Keyed()'s variables plus the injected facts, and nothing
// else. There is no second assembly path: key construction, executable resolution, and
// child execution all take their environment from the same value.
//
// It carries only the two results, not the ingredients: which names are inherited is
// owned by EffectiveEnvNames and which facts are injected by InjectedEnvFacts, and the
// projections that report those read them from there. Re-exposing them here would be a
// second answer to the same question, and would let a test ask the value under test to
// describe itself instead of checking it against its owner.
type EffectiveEnvironment struct {
	keyed Environment
	child []string
}

// BuildEffectiveEnvironment assembles the environment for one run.
//
// Inherited names come from EffectiveEnvNames (the built-in baseline merged with the
// configured allowlist) and are looked up through lookup, which reports a value and
// whether the variable was present. Unset, present-empty, and present-non-empty are
// three distinct states: an unset variable is keyed as unset and is *not* placed in the
// child environment, so awa never synthesizes a value the caller did not have; an empty
// one is passed as NAME= and keyed as empty; a non-empty one is passed byte-for-byte and
// keyed only as its redacted identity.
//
// Injected facts are appended last. They are never looked up, and a reserved name can
// never arrive as an inherited one either: EffectiveEnvNames filters it out whatever the
// allowlist says, so the caller's ambient AWA_RUN cannot be keyed beside awa's own fact.
// Appending last also makes awa's value win under the portable last-assignment-wins rule
// that the child's environment follows.
func BuildEffectiveEnvironment(h hashing.Hasher, goos string, allowlist []string, lookup func(name string) (string, bool)) EffectiveEnvironment {
	names := EffectiveEnvNames(goos, allowlist)
	injected := InjectedEnvFacts()

	vars := make([]EnvVar, 0, len(names)+len(injected))
	// Never nil: the process runner rejects a nil child environment because that is
	// os/exec's "inherit everything" sentinel, which would be an unkeyed leak.
	child := make([]string, 0, len(names)+len(injected))

	for _, name := range names {
		value, present := lookup(name)
		vars = append(vars, EnvVarFromValue(h, name, value, present))
		if present {
			child = append(child, name+"="+value)
		}
	}
	for _, fact := range injected {
		// The fact is keyed like any other variable so an execution that observed the
		// marker can never reuse a record written before it existed. Its origin stays
		// honest because it is built from the product constant here, not from lookup, and
		// because the reserved name identifies it on every surface that reports origin.
		vars = append(vars, EnvVarFromValue(h, fact.Name, fact.Value, true))
		child = append(child, fact.Assignment())
	}

	return EffectiveEnvironment{keyed: NewEnvironment(vars), child: child}
}

// Keyed returns the redacted environment checkpoint that participates in the run cache
// key. It covers both inherited values and injected facts, because both change what the
// child observed.
func (e EffectiveEnvironment) Keyed() Environment { return e.keyed }

// ChildEnv returns the exact environment the child process receives, in assembly order.
// It is never nil, and the caller hands the one slice it takes to both executable
// resolution and process execution, so the binary that gets keyed is the binary that
// gets executed. The copy keeps a consumer from perturbing the value the key describes.
func (e EffectiveEnvironment) ChildEnv() []string {
	out := make([]string, len(e.child))
	copy(out, e.child)
	return out
}
