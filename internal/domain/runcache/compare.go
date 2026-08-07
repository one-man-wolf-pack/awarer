package runcache

import (
	"fmt"
	"slices"
	"strings"
)

// DiffCode is a stable, machine-readable token naming one kind of difference
// between two key inputs. The tokens are part of explain's contract, so consumers
// (agents, tests) can switch on them; they never change spelling once shipped.
type DiffCode string

const (
	DiffInvocationChanged      DiffCode = "invocation-changed"
	DiffCommandChanged         DiffCode = "command-changed"
	DiffExecutableChanged      DiffCode = "executable-changed"
	DiffCWDChanged             DiffCode = "cwd-changed"
	DiffInputTreeChanged       DiffCode = "input-tree-changed"
	DiffEffectStateChanged     DiffCode = "effect-state-changed"
	DiffEffectStateUnavailable DiffCode = "effect-state-unavailable"
	DiffScopeChanged           DiffCode = "scope-changed"
	DiffTrustChanged           DiffCode = "trust-changed"
	DiffRunConfigChanged       DiffCode = "run-config-changed"
	DiffEnvChanged             DiffCode = "env-changed"
	DiffPlatformChanged        DiffCode = "platform-changed"
	DiffStdinChanged           DiffCode = "stdin-changed"
	DiffTTYChanged             DiffCode = "tty-changed"
	DiffSkippedChanged         DiffCode = "skipped-changed"
	DiffSchemaChanged          DiffCode = "schema-changed"
	DiffVersionChanged         DiffCode = "awa-version-changed"
)

// EnvChange names how one environment variable differs between two checkpoints,
// preserving the unset/empty distinction that the key itself preserves: a
// variable set to "" keys differently from one that is unset.
type EnvChange string

// EnvAdded says only that the name is keyed on one side and not the other. The reason
// can be that the allowlist gained it, that the built-in baseline did, or that awa
// started injecting it — the comparison sees names, not origins, and must not assert a
// cause it cannot observe. A surface that reports origin derives it from the name.
const (
	EnvAdded   EnvChange = "added"   // keyed in new, absent from old
	EnvRemoved EnvChange = "removed" // keyed in old, absent from new
	EnvSet     EnvChange = "set"     // was unset, now present
	EnvUnset   EnvChange = "unset"   // was present, now unset
	EnvValue   EnvChange = "value"   // present in both, value changed
)

// Difference is one typed difference between two key inputs. It records the stable
// code, the field name, and rendered old/new values. For an environment variable
// the EnvName and EnvChange detail which variable changed and how; the rendered
// Old/New are non-secret markers ("<unset>", "<empty>", "<set:blake3:hex>")
// rather than the raw value, so a difference never leaks an environment secret.
type Difference struct {
	Code      DiffCode
	Field     string
	Old       string
	New       string
	EnvName   string
	EnvChange EnvChange
}

// KeyComparison is the typed set of differences between two key inputs, in a
// deterministic field order. An empty Differences slice means the two inputs are
// identical and would compute the same key. Because only genuine differences are
// appended, a single field can never appear as both "same" and "different".
type KeyComparison struct {
	Differences []Difference
}

// Equal reports whether the two compared inputs are identical.
func (c KeyComparison) Equal() bool { return len(c.Differences) == 0 }

// PrimaryReason returns the most explanatory difference code, or the empty string
// when the inputs are identical. Every keyed difference forces a miss equally, so
// "most explanatory" is a fixed, deterministic priority: structural facts
// (command, executable, cwd) rank above scope/config/env, which rank above content
// (input tree) and platform/policy, so a human-facing one-line reason names the
// most actionable change first.
func (c KeyComparison) PrimaryReason() DiffCode {
	priority := []DiffCode{
		DiffSchemaChanged,
		DiffVersionChanged,
		DiffInvocationChanged,
		DiffCommandChanged,
		DiffExecutableChanged,
		DiffCWDChanged,
		DiffScopeChanged,
		DiffTrustChanged,
		DiffRunConfigChanged,
		DiffEnvChanged,
		DiffStdinChanged,
		DiffTTYChanged,
		DiffSkippedChanged,
		DiffPlatformChanged,
		DiffEffectStateUnavailable,
		DiffEffectStateChanged,
		DiffInputTreeChanged,
	}
	present := make(map[DiffCode]bool, len(c.Differences))
	for _, d := range c.Differences {
		present[d.Code] = true
	}
	for _, code := range priority {
		if present[code] {
			return code
		}
	}
	if len(c.Differences) > 0 {
		return c.Differences[0].Code
	}
	return ""
}

// Codes returns the distinct difference codes present, preserving the
// deterministic field order in which they were recorded.
func (c KeyComparison) Codes() []DiffCode {
	var out []DiffCode
	for _, d := range c.Differences {
		if !slices.Contains(out, d.Code) {
			out = append(out, d.Code)
		}
	}
	return out
}

// CompareKeyInputs reports the typed differences between an older key input (e.g. a
// stored run's) and a newer one (e.g. the current invocation's), comparing the
// typed fields directly rather than the canonical key bytes — so a miss can be
// explained field by field. Differences are appended in a fixed field order, so
// the result is deterministic for a given pair.
func CompareKeyInputs(old, new KeyInput) KeyComparison {
	var c KeyComparison

	if old.CacheSchemaVersion != new.CacheSchemaVersion {
		c.add(DiffSchemaChanged, "cache_schema_version", fmt.Sprint(old.CacheSchemaVersion), fmt.Sprint(new.CacheSchemaVersion))
	}
	if old.AwaVersion != new.AwaVersion {
		c.add(DiffVersionChanged, "awa_version", old.AwaVersion, new.AwaVersion)
	}
	if old.InvocationMode != new.InvocationMode {
		c.add(DiffInvocationChanged, "invocation_mode", old.InvocationMode, new.InvocationMode)
	}
	// The raw executable is argv[0] for any valid key input, but CompareKeyInputs does
	// not assume its inputs were validated, so it is compared explicitly rather than
	// trusted to track argv — keeping Equal() honest: no two inputs that compute
	// different keys can compare equal.
	if !slices.Equal(old.Command.Argv, new.Command.Argv) || old.Command.RawExecutable != new.Command.RawExecutable {
		c.add(DiffCommandChanged, "command", commandString(old.Command), commandString(new.Command))
	}
	// The raw executable is folded into the command diff above; only a change in the
	// resolved path or stat (the binary moved or its contents/mode changed under the
	// same argv[0]) is an independent executable difference.
	if old.Command.ResolvedPath != new.Command.ResolvedPath || old.Command.ResolvedStat != new.Command.ResolvedStat {
		c.add(DiffExecutableChanged, "executable", resolvedString(old.Command), resolvedString(new.Command))
	}
	if old.CWD.String() != new.CWD.String() {
		c.add(DiffCWDChanged, "cwd", old.CWD.String(), new.CWD.String())
	}
	if old.InputTreeHash.String() != new.InputTreeHash.String() {
		c.add(DiffInputTreeChanged, "input_tree_hash", old.InputTreeHash.String(), new.InputTreeHash.String())
	}
	// The effect observation is a distinct identity from the input tree: it covers the
	// watched generated-output roots excluded from the input scan. A change here — a
	// deleted or rewritten build/dist/target/node_modules — is its own reason
	// (effect-state-differs), never collapsed into input-tree-differs. When the current
	// effect state is unavailable (a watched root could not be observed safely), the
	// honest diagnosis is unavailability, not "the output differs" — so it maps to its
	// own reason rather than implying a changed result.
	if !old.Effect.Equal(new.Effect) {
		if new.Effect.Status() == EffectUnavailable {
			c.add(DiffEffectStateUnavailable, "effect_observation", old.Effect.effectDisplay(), new.Effect.effectDisplay())
		} else {
			c.add(DiffEffectStateChanged, "effect_observation", old.Effect.effectDisplay(), new.Effect.effectDisplay())
		}
	}
	if !slices.Equal(old.IncludeScope, new.IncludeScope) ||
		!slices.Equal(old.ExcludeScope, new.ExcludeScope) ||
		old.UseGitignore != new.UseGitignore ||
		old.UseAwaignore != new.UseAwaignore {
		c.add(DiffScopeChanged, "scope", scopeString(old), scopeString(new))
	}
	if old.TrustMode != new.TrustMode {
		c.add(DiffTrustChanged, "trust_mode", old.TrustMode.String(), new.TrustMode.String())
	}
	if old.RunConfigHash.String() != new.RunConfigHash.String() {
		c.add(DiffRunConfigChanged, "run_config_hash", old.RunConfigHash.String(), new.RunConfigHash.String())
	}
	c.addEnv(old.Env, new.Env)
	if old.Platform != new.Platform {
		c.add(DiffPlatformChanged, "platform", platformString(old.Platform), platformString(new.Platform))
	}
	if old.StdinMode != new.StdinMode {
		c.add(DiffStdinChanged, "stdin_mode", old.StdinMode.String(), new.StdinMode.String())
	}
	if old.TTYAllowed != new.TTYAllowed {
		c.add(DiffTTYChanged, "tty_allowed", fmt.Sprint(old.TTYAllowed), fmt.Sprint(new.TTYAllowed))
	}
	if old.AllowSkippedInputs != new.AllowSkippedInputs {
		c.add(DiffSkippedChanged, "allow_skipped_inputs", fmt.Sprint(old.AllowSkippedInputs), fmt.Sprint(new.AllowSkippedInputs))
	}
	return c
}

func (c *KeyComparison) add(code DiffCode, field, old, new string) {
	c.Differences = append(c.Differences, Difference{Code: code, Field: field, Old: old, New: new})
}

// addEnv appends one env-changed difference per variable whose keyed identity
// differs, preserving unset vs empty: a name keyed in only one checkpoint is
// added/removed, a name present in only one is set/unset, and a value change
// between two present variables is reported without leaking the value (markers
// distinguish empty from a set value).
func (c *KeyComparison) addEnv(old, new Environment) {
	oldByName := envByName(old)
	newByName := envByName(new)
	names := unionNames(oldByName, newByName)
	for _, name := range names {
		ov, inOld := oldByName[name]
		nv, inNew := newByName[name]
		switch {
		case inOld && !inNew:
			c.addEnvDiff(name, EnvRemoved, ov.marker(), "<absent>")
		case !inOld && inNew:
			c.addEnvDiff(name, EnvAdded, "<absent>", nv.marker())
		case ov.Present() && !nv.Present():
			c.addEnvDiff(name, EnvUnset, ov.marker(), nv.marker())
		case !ov.Present() && nv.Present():
			c.addEnvDiff(name, EnvSet, ov.marker(), nv.marker())
		case ov.Present() && nv.Present() && ov.identity.String() != nv.identity.String():
			c.addEnvDiff(name, EnvValue, ov.marker(), nv.marker())
		}
	}
}

func (c *KeyComparison) addEnvDiff(name string, change EnvChange, old, new string) {
	c.Differences = append(c.Differences, Difference{
		Code:      DiffEnvChanged,
		Field:     "env",
		Old:       old,
		New:       new,
		EnvName:   name,
		EnvChange: change,
	})
}

func envByName(e Environment) map[string]EnvVar {
	out := make(map[string]EnvVar)
	for _, v := range e.Vars() {
		out[v.name] = v
	}
	return out
}

func unionNames(a, b map[string]EnvVar) []string {
	seen := make(map[string]bool, len(a)+len(b))
	var out []string
	for name := range a {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for name := range b {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// commandString renders a command for a difference value: the argv joined, with the
// raw executable appended when it is not argv[0], so a raw-executable-only change is
// visible rather than rendering identically to its argv.
func commandString(cmd Command) string {
	s := strings.Join(cmd.Argv, " ")
	if len(cmd.Argv) == 0 || cmd.RawExecutable != cmd.Argv[0] {
		s += " (exe: " + cmd.RawExecutable + ")"
	}
	return s
}

func resolvedString(cmd Command) string {
	if cmd.ResolvedPath == "" {
		return "<unresolved>"
	}
	return cmd.ResolvedPath + " (" + cmd.ResolvedStat + ")"
}

func scopeString(in KeyInput) string {
	return fmt.Sprintf("include=%v exclude=%v gitignore=%t awaignore=%t",
		in.IncludeScope, in.ExcludeScope, in.UseGitignore, in.UseAwaignore)
}

func platformString(p Platform) string {
	return p.GOOS + "/" + p.GOARCH
}
