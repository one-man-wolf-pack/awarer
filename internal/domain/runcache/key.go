// Package runcache is the pure domain model for the run cache.
//
// It owns the value objects the run workflow produces and consumes — the run
// cache key and its typed inputs, the run entry aggregate, exit status, captured
// output metadata, and the cache decision — together with the canonical,
// deterministic key encoding. It performs no I/O and imports no os/exec,
// filesystem, or CLI code: the process runner and the run repository are ports
// the application defines and infrastructure implements, so this package stays
// testable and the infrastructure stays swappable.
package runcache

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
)

// InvocationArgv is the only invocation mode: the command is run from
// an exact argv vector, never through a shell. It is part of the key so a future
// shell mode can never collide with an argv run of the same tokens.
const InvocationArgv = "argv"

// CacheSchemaVersion is the version of the run cache key schema. It is folded into
// every key, so a breaking change to the key fields or their encoding bumps it and
// puts the new keys in a disjoint space — a newer awa never reuses or trusts an older
// key whose meaning has changed. KeyInput.Validate accepts only this version, so a
// recomputed-but-stale or future key_input document cannot pass a read.
const CacheSchemaVersion = 1

// StdinMode is how the child process's stdin is wired. It is part of the run key
// because it changes how a command behaves, and it gates cacheability: inherited
// stdin is not keyed, so a run over it is never cached.
type StdinMode int

const (
	// StdinNull connects the child to an empty reader (the platform null device).
	// This is the default for a terminal stdin without --allow-tty, so a cacheable
	// run never blocks waiting for interactive input.
	StdinNull StdinMode = iota
	// StdinTTY inherits a terminal stdin under --allow-tty. The run is never cached
	// because the interactive input is not part of the key.
	StdinTTY
	// StdinPipe inherits a pipe or file stdin. The child receives the data, but the
	// run is never cached because stdin content is not part of the key.
	StdinPipe
)

// ParseStdinMode resolves a persisted token.
func ParseStdinMode(s string) (StdinMode, error) {
	switch s {
	case "null":
		return StdinNull, nil
	case "tty":
		return StdinTTY, nil
	case "pipe":
		return StdinPipe, nil
	default:
		return 0, fmt.Errorf("invalid stdin mode %q: want null, tty, or pipe", s)
	}
}

func (m StdinMode) String() string {
	switch m {
	case StdinNull:
		return "null"
	case StdinTTY:
		return "tty"
	case StdinPipe:
		return "pipe"
	default:
		return "unknown"
	}
}

// Valid reports whether m is a known stdin mode.
func (m StdinMode) Valid() bool { return m == StdinNull || m == StdinTTY || m == StdinPipe }

// Platform is the build target a command ran on. It is part of the key because a
// cached result from one OS/arch must not be replayed on another. It is injected
// rather than read from runtime so the domain stays pure and testable.
type Platform struct {
	GOOS   string
	GOARCH string
}

// envNamesCaseInsensitive reports whether the platform treats environment variable
// names case-insensitively. Windows does (PATH and Path are one variable); Unix
// does not.
func envNamesCaseInsensitive(goos string) bool { return goos == "windows" }

// EffectiveEnvNames merges the built-in baseline allowlist for goos
// (config.BaselineEnvNames) with the configured allowlist, removing duplicates so a
// name in both is keyed once. On a case-insensitive platform (Windows) names
// differing only in case are the same variable, so the first spelling is kept and
// later case-variants are dropped — otherwise PATH and Path would be keyed and
// passed to the child as two distinct variables for what the OS treats as one.
//
// These are the *inherited* names only: what awa reads from the caller. Facts awa
// injects are not names to look up, so they are owned by InjectedEnvFacts and joined
// with these by BuildEffectiveEnvironment. Keeping them apart is what lets a projection
// such as `awa config effective` report inherited names and injected facts as the
// different things they are.
//
// An awa-owned name is therefore never inherited, whatever the allowlist says. Config
// validation rejects one long before this point, but that check must not be the only
// thing holding the invariant: returning a reserved name here would have the assembler
// key the caller's ambient value beside awa's own injected fact, producing a duplicated
// variable that no later validation could repair into a usable run.
func EffectiveEnvNames(goos string, allowlist []string) []string {
	fold := envNamesCaseInsensitive(goos)
	baseline := config.BaselineEnvNames(goos)
	seen := make(map[string]bool, len(baseline)+len(allowlist))
	out := make([]string, 0, len(baseline)+len(allowlist))
	for _, name := range append(baseline, allowlist...) {
		if config.IsReservedEnvName(name) {
			continue
		}
		key := name
		if fold {
			key = strings.ToUpper(name)
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, name)
		}
	}
	return out
}

// Environment is the allowlisted environment checkpoint that contributes to the
// key. Construct it with NewEnvironment, which sorts by name so the key is stable
// regardless of allowlist order.
type Environment struct {
	vars []EnvVar
}

// NewEnvironment returns an Environment with its variables sorted by name. The
// input slice is copied, so later mutation by the caller cannot perturb the key.
func NewEnvironment(vars []EnvVar) Environment {
	out := make([]EnvVar, len(vars))
	copy(out, vars)
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return Environment{vars: out}
}

// Vars returns a copy of the sorted variables.
func (e Environment) Vars() []EnvVar {
	out := make([]EnvVar, len(e.vars))
	copy(out, e.vars)
	return out
}

// Validate checks that the environment checkpoint has the same mechanical shape as
// run.env_allowlist: each item is a variable name, not name=value, names are
// unique, and each variable's presence/identity pairing is internally consistent
// (a redacted value identity present exactly when the variable is set to a non-empty
// value). NewEnvironment sorts by name, so duplicates are adjacent here.
//
// An awa-owned name gets one extra check: it is injected, never looked up, so a
// document carrying it as unset or empty describes a run awa could not have performed.
// The check is shape only — Validate has no hasher, so it cannot prove the identity is
// the fingerprint of the marker's value. Absence stays valid, because a record written
// before the marker existed is intact evidence that simply cannot match a current key.
func (e Environment) Validate() error {
	prev := ""
	for i, v := range e.vars {
		if err := v.validate(); err != nil {
			return fmt.Errorf("environment variable %d: %w", i, err)
		}
		if i > 0 && v.name == prev {
			return fmt.Errorf("environment variable %q is duplicated", v.name)
		}
		if config.IsReservedEnvName(v.name) && v.presence != PresenceSet {
			return fmt.Errorf("environment variable %q is reserved and %s: an injected fact is always set", v.name, v.presence)
		}
		prev = v.name
	}
	return nil
}

// ExecutionCWD is the command's execution directory, expressed as a clean
// project-root-relative slash path. Unlike worktree.RelPath it allows "." — the
// canonical name for the project root, which is the default execution directory.
type ExecutionCWD struct {
	rel string
}

// NewExecutionCWD validates and normalizes a project-root-relative execution
// directory. It accepts "." (the root), rejects absolute paths and any path that
// escapes the root, and normalizes to a clean slash-separated form so two
// spellings of the same directory key identically.
func NewExecutionCWD(rel string) (ExecutionCWD, error) {
	if rel == "" {
		return ExecutionCWD{}, fmt.Errorf("execution cwd must not be empty")
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") {
		return ExecutionCWD{}, fmt.Errorf("execution cwd %q must be relative to the project root", rel)
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.ToSlash(rel)))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return ExecutionCWD{}, fmt.Errorf("execution cwd %q must not escape the project root", rel)
	}
	return ExecutionCWD{rel: clean}, nil
}

// String returns the clean slash-separated path ("." for the root).
func (c ExecutionCWD) String() string {
	if c.rel == "" {
		return "."
	}
	return c.rel
}

// Command is the exact invocation a key identifies: the argv tokens (order and
// duplicates preserved), the raw executable token as typed, and the resolved
// executable path and stat signature when the runner could determine them.
type Command struct {
	Argv          []string
	RawExecutable string
	ResolvedPath  string
	ResolvedStat  string
}

// KeyInput is the complete, typed set of fields that determine a run cache key.
// It doubles as the structured key document persisted with a run entry, so
// explain can reconstruct a cache decision without re-deriving anything.
// Build a key from it with Compute.
type KeyInput struct {
	CacheSchemaVersion int
	AwaVersion         string
	InvocationMode     string
	Command            Command
	CWD                ExecutionCWD
	InputTreeHash      hashing.TreeHash
	Effect             EffectObservation
	IncludeScope       []string
	ExcludeScope       []string
	UseGitignore       bool
	UseAwaignore       bool
	TrustMode          config.TrustMode
	RunConfigHash      hashing.ConfigHash
	Env                Environment
	Platform           Platform
	StdinMode          StdinMode
	TTYAllowed         bool
	AllowSkippedInputs bool
}

// Validate checks that a key input is internally consistent — not merely that it
// hashes to some key, but that the document it describes is possible. A key can be
// self-consistently computed over impossible fields (a half-resolved command, an
// incomplete platform, a zero input tree hash); rejecting those keeps log, show, rm,
// and explain from presenting or reasoning over a metadata shape that could
// never have been produced by a real run.
func (in KeyInput) Validate() error {
	if in.CacheSchemaVersion != CacheSchemaVersion {
		return fmt.Errorf("key input cache schema version %d is not the supported %d", in.CacheSchemaVersion, CacheSchemaVersion)
	}
	// The awa version is part of the key, so every real run records one (the build
	// stamps at least a development marker). An empty version means a miswired
	// composition keyed runs without it; reject it rather than persist a key a
	// versioned build can never reproduce.
	if in.AwaVersion == "" {
		return fmt.Errorf("key input has no awa version")
	}
	if len(in.Command.Argv) == 0 {
		return fmt.Errorf("key input has no command argv")
	}
	if in.Command.RawExecutable == "" {
		return fmt.Errorf("key input has no raw executable")
	}
	// An argv invocation runs Argv[0] directly, so the raw executable token is
	// always exactly Argv[0]. A document where they differ describes a run that
	// could never have happened.
	if in.Command.RawExecutable != in.Command.Argv[0] {
		return fmt.Errorf("key input raw executable %q does not match argv[0] %q", in.Command.RawExecutable, in.Command.Argv[0])
	}
	// Resolution yields a path and its stat signature together or not at all, so
	// the two must be both set (resolution succeeded) or both empty (it did not);
	// one without the other is an impossible half-resolved command.
	if (in.Command.ResolvedPath == "") != (in.Command.ResolvedStat == "") {
		return fmt.Errorf("key input has a half-resolved command: path %q, stat %q", in.Command.ResolvedPath, in.Command.ResolvedStat)
	}
	if in.InvocationMode != InvocationArgv {
		return fmt.Errorf("key input has unknown invocation mode %q", in.InvocationMode)
	}
	if !in.TrustMode.Valid() {
		return fmt.Errorf("key input has invalid trust mode %v", in.TrustMode)
	}
	if !in.StdinMode.Valid() {
		return fmt.Errorf("key input has invalid stdin mode %v", in.StdinMode)
	}
	if err := in.Env.Validate(); err != nil {
		return fmt.Errorf("key input env: %w", err)
	}
	// On a case-insensitive platform, env names differing only in case are the same
	// variable. Env.Validate only catches exact duplicates (it has no platform), so
	// reject case-insensitive duplicates here, where the platform is known, to keep
	// the key from describing PATH and Path as two variables.
	if envNamesCaseInsensitive(in.Platform.GOOS) {
		seen := make(map[string]bool)
		for _, v := range in.Env.Vars() {
			key := strings.ToUpper(v.name)
			if seen[key] {
				return fmt.Errorf("key input env has case-insensitive duplicate name %q on %s", v.name, in.Platform.GOOS)
			}
			seen[key] = true
		}
	}
	if in.Platform.GOOS == "" || in.Platform.GOARCH == "" {
		return fmt.Errorf("key input has an incomplete platform")
	}
	if in.InputTreeHash.IsZero() {
		return fmt.Errorf("key input has no input tree hash")
	}
	if err := in.Effect.Validate(); err != nil {
		return fmt.Errorf("key input effect: %w", err)
	}
	if in.RunConfigHash.IsZero() {
		return fmt.Errorf("key input has no run config hash")
	}
	if err := validateScope(in.IncludeScope, in.ExcludeScope); err != nil {
		return fmt.Errorf("key input scope: %w", err)
	}
	return nil
}

// validateScope checks the persisted include/exclude scope has the shape a real
// run would have written: a non-empty include list whose every element is a
// non-empty, relative, non-escaping path, the include list already in the
// canonical form the write path records (effCfg.Scope.CanonicalInclude), and every
// exclude element non-empty. The config and CLI enforce these on the way in;
// re-checking here keeps a recomputed-but-corrupt key_input from surviving a read.
func validateScope(include, exclude []string) error {
	if len(include) == 0 {
		return fmt.Errorf("include scope is empty")
	}
	for i, p := range include {
		if p == "" {
			return fmt.Errorf("include scope[%d] is empty", i)
		}
		if filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
			return fmt.Errorf("include scope[%d] %q must be relative to the project root", i, p)
		}
		clean := filepath.ToSlash(filepath.Clean(filepath.ToSlash(p)))
		if clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("include scope[%d] %q must not escape the project root", i, p)
		}
	}
	if canon := config.CanonicalInclude(include); !slices.Equal(canon, include) {
		return fmt.Errorf("include scope %v is not in canonical form %v", include, canon)
	}
	for i, p := range exclude {
		if p == "" {
			return fmt.Errorf("exclude scope[%d] is empty", i)
		}
	}
	return nil
}

// Compute folds the canonical encoding of the key input through the project
// hasher into a RunKey. The same inputs always produce the same key, and the
// length-prefixed encoding is injective, so distinct inputs cannot collide.
func (in KeyInput) Compute(h hashing.Hasher) RunKey {
	return RunKey{th: h.HashBytes(in.canonical())}
}

// canonical renders the key input as a deterministic, injective byte string.
// Every field is length-prefixed so no list element or value can be confused for
// a field boundary; argv order and duplicates are preserved; env names are
// already sorted by NewEnvironment; the include scope is canonicalized by the
// caller and the exclude list is kept in its effective (gitignore) order.
func (in KeyInput) canonical() []byte {
	var b bytes.Buffer
	putUint(&b, uint64(in.CacheSchemaVersion))
	putString(&b, in.AwaVersion)
	putString(&b, in.InvocationMode)
	putList(&b, in.Command.Argv)
	putString(&b, in.Command.RawExecutable)
	putString(&b, in.Command.ResolvedPath)
	putString(&b, in.Command.ResolvedStat)
	putString(&b, in.CWD.String())
	putString(&b, in.InputTreeHash.String())
	putString(&b, in.Effect.Status().String())
	putString(&b, in.Effect.Signature().String())
	putUint(&b, uint64(in.Effect.RootCount()))
	putList(&b, in.IncludeScope)
	putList(&b, in.ExcludeScope)
	putBool(&b, in.UseGitignore)
	putBool(&b, in.UseAwaignore)
	putString(&b, in.TrustMode.String())
	putString(&b, in.RunConfigHash.String())
	// Each variable folds its name, presence class, and — only when set to a non-empty
	// value — the value's redacted identity fingerprint. The raw value is never folded
	// (it is not carried on EnvVar), so the key is computed identically in memory and
	// from the persisted record, and a set value still keys distinctly from a different
	// set value, from empty, and from unset.
	vars := in.Env.Vars()
	putUint(&b, uint64(len(vars)))
	for _, v := range vars {
		putString(&b, v.name)
		putString(&b, v.presence.String())
		putString(&b, v.identity.String())
	}
	putString(&b, in.Platform.GOOS)
	putString(&b, in.Platform.GOARCH)
	putString(&b, in.StdinMode.String())
	putBool(&b, in.TTYAllowed)
	putBool(&b, in.AllowSkippedInputs)
	return b.Bytes()
}

// RunKey is the identity of a cached run: the digest of its canonical key input.
// It wraps a tree hash so it reuses the validated "blake3:hex" representation
// and can never be constructed from an arbitrary string.
type RunKey struct {
	th hashing.TreeHash
}

// ParseRunKey parses a persisted "blake3:hex" run key.
func ParseRunKey(s string) (RunKey, error) {
	th, err := hashing.ParseTreeHash(s)
	if err != nil {
		return RunKey{}, fmt.Errorf("invalid run key: %w", err)
	}
	return RunKey{th: th}, nil
}

// Hex returns the lowercase hex digest without the identity prefix, used to
// shard and name the key pointer file.
func (k RunKey) Hex() string { return k.th.Hex() }

// IsZero reports whether the key is unset.
func (k RunKey) IsZero() bool { return k.th.IsZero() }

// String renders the canonical "blake3:hex" form; the zero value renders as
// the empty string.
func (k RunKey) String() string { return k.th.String() }

// putString writes an 8-byte big-endian length followed by the raw bytes.
func putString(b *bytes.Buffer, s string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(s)))
	b.Write(n[:])
	b.WriteString(s)
}

// putList writes the element count followed by each element, length-prefixed.
func putList(b *bytes.Buffer, items []string) {
	putUint(b, uint64(len(items)))
	for _, it := range items {
		putString(b, it)
	}
}

// putUint writes a fixed 8-byte big-endian number.
func putUint(b *bytes.Buffer, v uint64) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], v)
	b.Write(n[:])
}

// putBool writes a single discriminating byte.
func putBool(b *bytes.Buffer, v bool) {
	if v {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
}
