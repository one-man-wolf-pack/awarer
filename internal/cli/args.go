package cli

import (
	"context"
	"strings"
)

// TrustMode selects how aggressively awa trusts cached state. Parsed from
// --trust-mode/--strict at the boundary.
type TrustMode int

const (
	TrustNormal TrustMode = iota
	TrustStrict
	TrustFast
)

// capability identifies a global option for the purpose of deciding whether a
// command acts on it. A command accepts a fixed set of capabilities; any option
// outside that set that was nonetheless provided is a usage error rather than a
// silently ignored flag (see requireAccepted).
type capability string

const (
	capJSON      capability = "--json"
	capRoot      capability = "--root"
	capConfig    capability = "--config"
	capTrustMode capability = "--trust-mode/--strict"
)

// allCapabilities lists every capability in a stable order, used both for
// deterministic capability checks and to render the global-options help.
var allCapabilities = []capability{capRoot, capConfig, capJSON, capTrustMode}

// capSpelling is one way a global option can be written and whether that spelling
// takes a value. --trust-mode and --strict are two spellings of one capability,
// only the first of which takes a value — so a flat display string cannot model it,
// but this can.
type capSpelling struct {
	flag  string // e.g. "--root", "--strict"
	value string // e.g. "<path>"; "" for a boolean spelling
}

// capInfo is the metadata for a capability: its spellings (structured, so both the
// human --help form and the machine reference derive from one source) and what it
// does. It is the single source the --help text is rendered from, so help can never
// advertise a flag the validator does not recognize.
type capInfo struct {
	spellings []capSpelling
	help      string
}

// display renders the human "--help" form for a capability, e.g. "--root <path>"
// or "--trust-mode <mode> / --strict".
func (i capInfo) display() string {
	parts := make([]string, len(i.spellings))
	for j, s := range i.spellings {
		parts[j] = s.flag
		if s.value != "" {
			parts[j] += " " + s.value
		}
	}
	return strings.Join(parts, " / ")
}

var capabilityInfo = map[capability]capInfo{
	capRoot:      {spellings: []capSpelling{{"--root", "<path>"}}, help: "project root override"},
	capConfig:    {spellings: []capSpelling{{"--config", "<path>"}}, help: "config file override"},
	capJSON:      {spellings: []capSpelling{{"--json", ""}}, help: "emit schema-versioned JSON"},
	capTrustMode: {spellings: []capSpelling{{"--trust-mode", "<mode>"}, {"--strict", ""}}, help: "cache trust level (normal|strict|fast)"},
}

// GlobalOptions holds the options accepted at the CLI boundary. Parsing records
// honest typed values plus the set of options actually provided (see provided),
// so a command can act on the options it supports and reject the rest instead
// of ignoring them.
type GlobalOptions struct {
	Root      string
	Config    string
	JSON      bool
	TrustMode TrustMode
	Help      bool

	// provided records which capabilities were explicitly given, so commands
	// can distinguish an explicit option from a default and reject options
	// they do not act on.
	provided map[capability]bool
}

// has reports whether the capability was explicitly provided.
func (o GlobalOptions) has(c capability) bool { return o.provided[c] }

// mark records that a capability was provided.
func (o *GlobalOptions) mark(c capability) {
	if o.provided == nil {
		o.provided = map[capability]bool{}
	}
	o.provided[c] = true
}

// tailTokenClass classifies one token of a command's argument tail by how the top-level
// parser read it. It is the typed fact a command that owns its tail (run) uses to locate an
// awa global relative to its own operands, without re-parsing argv or re-running the
// global-flag matcher.
type tailTokenClass uint8

const (
	// tailArg is a command-owned token — a bare operand or a command-local flag — collected
	// verbatim into args for the command's own parser.
	tailArg tailTokenClass = iota
	// tailGlobal is an awa global flag the top-level parser consumed into options.
	tailGlobal
)

// tailToken is one classified token of the command's argument tail, in argv order, up to
// (but not including) any "--" terminator — everything after "--" is an operand, never an
// awa flag. name carries the flag spelling for a tailGlobal so a boundary error can name it.
type tailToken struct {
	class tailTokenClass
	name  string
}

// invocation is the parsed form of an argv: the global options, the resolved
// command name (empty when none was given), the leftover command arguments, and
// the operands that followed a "--" terminator. Keeping operands separate
// preserves the contract that nothing after "--" is ever treated as a flag,
// even by a command's own parser.
type invocation struct {
	// ctx is the root cancellable context (SIGINT), set by route after parsing.
	// Handlers use it for all product work — scans, git, GC, store cursors, and
	// child processes — instead of minting a local context.Background(). It is
	// nil only for invocations built directly in tests that do not model
	// cancellation; handlers must tolerate a nil ctx by falling back to a
	// background context via the invCtx helper.
	ctx      context.Context
	options  GlobalOptions
	command  string
	args     []string
	operands []string
	// tail is the top-level parser's classification of the tokens after the command word,
	// in argv order, produced in the single parse pass (each token is recorded as awa's
	// parser read it). It lets a command that owns its tail (run) decide whether an awa
	// global landed inside the wrapped child's region without re-parsing argv. Ordinary
	// commands ignore it; it is generic parser output, not command-specific evidence.
	tail []tailToken
}

// invCtx returns the invocation's root context, or a background context when it
// is nil. route always sets ctx, so the fallback only guards invocations built
// directly in tests that do not model cancellation.
func (inv invocation) invCtx() context.Context {
	if inv.ctx == nil {
		return context.Background() //nolint:forbidigo // documented detached-context fallback for tests that build an invocation without a cancellable ctx
	}
	return inv.ctx
}

// parse splits argv into global options, a command, and command arguments.
//
// Recognized global flags are consumed wherever they appear, so both
// "awa --json version" and "awa version --json" are valid. The first bare token
// is the command; everything after it that is not a global flag — positional
// arguments and command-specific flags alike — is collected verbatim for the
// command's own parser. An unknown flag before any command is named is a usage
// error, as are bad global flag values.
func parse(argv []string) (invocation, error) {
	inv := invocation{options: GlobalOptions{TrustMode: TrustNormal, provided: map[capability]bool{}}}

	i := 0
	for i < len(argv) {
		tok := argv[i]
		i++

		if tok == "--" {
			// Everything after "--" is a literal operand, never a flag — kept
			// separate from args so no command parser can re-interpret it.
			inv.operands = append(inv.operands, argv[i:]...)
			break
		}

		if !strings.HasPrefix(tok, "-") || tok == "-" {
			// First bare token is the command; the rest are its arguments, each also
			// recorded in the typed tail so the run wrapper can locate a global stolen from
			// a wrapped child without re-parsing argv.
			if inv.command == "" {
				inv.command = tok
			} else {
				inv.args = append(inv.args, tok)
				inv.tail = append(inv.tail, tailToken{class: tailArg})
			}
			continue
		}

		name, value, hasValue := splitFlag(tok)
		consumed, err := applyFlag(&inv.options, name, value, hasValue, argv, &i)
		if err != nil {
			return invocation{}, err
		}
		if consumed {
			// Record the global's position in the command's tail (pre-command globals are
			// always awa's, so they are never part of a command tail).
			if inv.command != "" {
				inv.tail = append(inv.tail, tailToken{class: tailGlobal, name: name})
			}
			continue
		}

		// Not a global flag. Before a command is named, an unknown flag has no
		// owner and is a usage error. After the command, the flag belongs to
		// that command: collect it verbatim for the command's own parser. This
		// keeps the boundary clean — the global parser does not own command
		// flags such as "awa diff -1" or "awa log -n 5".
		if inv.command == "" {
			return invocation{}, usageErrorf("unknown flag %q", name)
		}
		inv.args = append(inv.args, tok)
		inv.tail = append(inv.tail, tailToken{class: tailArg})
	}

	return inv, nil
}

// splitFlag normalizes a flag token into its name and optional inline value.
// It accepts both "--flag" and "--flag=value"; the leading dashes are kept on
// the name so error messages echo what the user typed.
func splitFlag(tok string) (name, value string, hasValue bool) {
	if eq := strings.IndexByte(tok, '='); eq >= 0 {
		return tok[:eq], tok[eq+1:], true
	}
	return tok, "", false
}

// requireValue resolves the value of a flag that needs a non-empty argument,
// accepting both --flag=value and --flag value. When the value is a separate
// token it reads args[*i] and advances i. It is shared by the global parser and
// command-local flag parsers so the "required, non-empty value" rule lives in
// one place. Callers must position i at the token following the flag.
func requireValue(name, inlineValue string, hasInline bool, args []string, i *int) (string, error) {
	v := inlineValue
	if !hasInline {
		if *i >= len(args) {
			return "", usageErrorf("flag %q requires a value", name)
		}
		v = args[*i]
		*i++
	}
	// An empty value (--flag= or --flag "") is a mistake, not an unset flag:
	// reject it rather than silently falling back to a default.
	if v == "" {
		return "", usageErrorf("flag %q requires a non-empty value", name)
	}
	return v, nil
}

// noValue rejects an inline value on a boolean flag, so "--json=false" is a loud
// usage error rather than being silently read as true. It is the boolean
// counterpart to requireValue, shared by the global and command-local parsers.
func noValue(name string, hasValue bool) error {
	if hasValue {
		return usageErrorf("flag %q does not take a value", name)
	}
	return nil
}

// applyFlag updates opts for a single recognized global flag. For flags that
// take a value without "=", it reads the next argv token and advances i.
// It returns false when the flag name is not a known global flag.
func applyFlag(opts *GlobalOptions, name, value string, hasValue bool, argv []string, i *int) (bool, error) {
	takeValue := func() (string, error) {
		return requireValue(name, value, hasValue, argv, i)
	}
	boolFlag := func() error {
		return noValue(name, hasValue)
	}

	switch name {
	case "-h", "--help":
		if err := boolFlag(); err != nil {
			return false, err
		}
		opts.Help = true
	case "--json":
		if err := boolFlag(); err != nil {
			return false, err
		}
		opts.JSON = true
		opts.mark(capJSON)
	case "--strict":
		if err := boolFlag(); err != nil {
			return false, err
		}
		opts.TrustMode = TrustStrict
		opts.mark(capTrustMode)
	case "--root":
		v, err := takeValue()
		if err != nil {
			return false, err
		}
		opts.Root = v
		opts.mark(capRoot)
	case "--config":
		v, err := takeValue()
		if err != nil {
			return false, err
		}
		opts.Config = v
		opts.mark(capConfig)
	case "--trust-mode":
		v, err := takeValue()
		if err != nil {
			return false, err
		}
		m, err := parseTrustMode(v)
		if err != nil {
			return false, err
		}
		opts.TrustMode = m
		opts.mark(capTrustMode)
	default:
		return false, nil
	}
	return true, nil
}

func parseTrustMode(v string) (TrustMode, error) {
	switch v {
	case "normal":
		return TrustNormal, nil
	case "strict":
		return TrustStrict, nil
	case "fast":
		return TrustFast, nil
	default:
		return 0, usageErrorf("invalid --trust-mode value %q: want strict, normal, or fast", v)
	}
}
