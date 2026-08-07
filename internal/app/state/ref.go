// Package state parses state references and resolves them to comparable states.
//
// A reference names either the current workspace ("now") or a stored checkpoint
// ("latest", "@-N", or a checkpoint id prefix). A range pairs a left (old) and
// right (new) reference. Parsing is pure and deterministic and lives here, and it
// owns syntax: a token that cannot be a reference is rejected here, before any
// stored state is touched. Resolution — which touches the checkpoint repository,
// the scanner, and the blob store — lives in the Resolver. The CLI hands this layer
// a single range/shortcut token and gets back two resolved states ready for the
// pure comparison in internal/domain/compare.
package state

import (
	"fmt"
	"strconv"
	"strings"

	"awarer/internal/domain/checkpoint"
)

// RefKind classifies a parsed reference.
type RefKind int

const (
	// RefNow is the current workspace state (an ephemeral scan).
	RefNow RefKind = iota
	// RefLatest is the newest checkpoint.
	RefLatest
	// RefAtN is the Nth checkpoint from newest, one-based (@-1 is newest).
	RefAtN
	// RefCheckpointPrefix is a syntactically valid checkpoint id or id prefix. The
	// parser has already proved its syntax; only the resolver can say whether it
	// matches exactly one stored checkpoint.
	RefCheckpointPrefix
	// RefRunObservation is a recorded run's pre/post observed state, named
	// "run:<id>:before" or "run:<id>:after". The resolver opens the run's stored
	// observation manifest.
	RefRunObservation
	// RefRestoreObservation is an applied restore's pre-restore recovery
	// observation, named "restore:<id>:before". It is the system-owned evidence
	// restore publishes before its first worktree write, so it is addressable for
	// inspection and for restoring the state a restore overwrote. There is no
	// "after": the state after a restore is the worktree itself, which "now"
	// already names.
	RefRestoreObservation
)

// RunObservationSel selects which observation of a recorded run a run reference
// names: the pre-run or the post-run observed state.
type RunObservationSel int

const (
	// RunBefore is the pre-run observed state.
	RunBefore RunObservationSel = iota
	// RunAfter is the post-run observed state.
	RunAfter
)

// String returns the selector token used in a run reference.
func (s RunObservationSel) String() string {
	if s == RunAfter {
		return "after"
	}
	return "before"
}

// Ref is a parsed, not-yet-resolved reference.
type Ref struct {
	Kind RefKind
	// Display is the reference as it should appear to the user.
	Display string
	// N is the one-based index for RefAtN.
	N int
	// Raw is the literal token for RefCheckpointPrefix (a full id or id prefix).
	Raw string
	// RunRef is the run id or id prefix for RefRunObservation; RunSel selects which
	// observation it names.
	RunRef string
	RunSel RunObservationSel
	// RestoreRef is the restore operation id or id prefix for
	// RefRestoreObservation. It needs no selector: only "before" exists.
	RestoreRef string
}

// Range is a parsed left..right pair.
type Range struct {
	Left  Ref
	Right Ref
}

// ParseRange parses the single range/shortcut token a command accepts. An empty
// token means the default range latest..now. A "-N" token is the numeric
// shortcut @-N..now. Any other token must contain exactly one "..": both sides
// are parsed as references, and an empty side is a usage error.
func ParseRange(token string) (Range, error) {
	if token == "" {
		return Range{Left: Ref{Kind: RefLatest, Display: "latest"}, Right: nowRef()}, nil
	}

	// Numeric shortcut "-N" expands to @-N..now. "-1" is the common shortcut for
	// latest..now (and @-1 resolves to the newest checkpoint, so it is equivalent).
	if n, ok := numericShortcut(token); ok {
		if n < 1 {
			return Range{}, fmt.Errorf("invalid shortcut %q: N must be positive", token)
		}
		return Range{Left: atN(n), Right: nowRef()}, nil
	}

	if !strings.Contains(token, "..") {
		return Range{}, fmt.Errorf("invalid range %q: expected A..B (or use -N as a shortcut for @-N..now)", token)
	}
	parts := strings.Split(token, "..")
	if len(parts) != 2 {
		return Range{}, fmt.Errorf("invalid range %q: a range contains exactly one '..'", token)
	}
	left, right := parts[0], parts[1]
	if left == "" || right == "" {
		return Range{}, fmt.Errorf("invalid range %q: both sides of '..' are required", token)
	}
	lref, err := parseRef(left)
	if err != nil {
		return Range{}, err
	}
	rref, err := parseRef(right)
	if err != nil {
		return Range{}, err
	}
	return Range{Left: lref, Right: rref}, nil
}

// ParseRef parses a single state reference for a command that names exactly one
// state (state resolve). Unlike ParseRange it rejects a range token and the bare
// "-N" range shortcut, which name a pair rather than a single state, and it rejects an
// empty token: resolve requires an explicit reference. It accepts the same single-
// reference vocabulary as one side of a range (now, latest, @-N, a checkpoint id or
// id prefix, and run:<id>:before|after), reusing the one parser so resolve and
// changes/diff can never drift on what a reference means.
func ParseRef(token string) (Ref, error) {
	if token == "" {
		return Ref{}, fmt.Errorf("a state reference is required")
	}
	if strings.Contains(token, "..") {
		return Ref{}, fmt.Errorf("invalid reference %q: expected a single state reference, not a range", token)
	}
	if _, ok := numericShortcut(token); ok {
		return Ref{}, fmt.Errorf("invalid reference %q: -N is a range shortcut; use @-N to name a single checkpoint", token)
	}
	return parseRef(token)
}

// numericShortcut reports whether token is "-<digits>" and returns the N.
func numericShortcut(token string) (int, bool) {
	if len(token) < 2 || token[0] != '-' {
		return 0, false
	}
	n, err := strconv.Atoi(token[1:])
	if err != nil {
		return 0, false
	}
	return n, true
}

// parseRef parses one side of a range. Reserved tokens win first; anything else
// must be a syntactically valid checkpoint id or id prefix, which is decided here
// rather than by the resolver. A token that cannot be a reference is a usage error
// the user fixes by retyping it, not a lookup that could have succeeded against a
// different store, so it never reaches stored state.
func parseRef(s string) (Ref, error) {
	switch s {
	case "now":
		return nowRef(), nil
	case "latest":
		return Ref{Kind: RefLatest, Display: "latest"}, nil
	}
	if strings.HasPrefix(s, runRefPrefix) {
		return parseRunRef(s)
	}
	if strings.HasPrefix(s, restoreRefPrefix) {
		return parseRestoreRef(s)
	}
	if strings.HasPrefix(s, "@-") {
		n, err := strconv.Atoi(s[2:])
		if err != nil || n < 1 {
			return Ref{}, fmt.Errorf("invalid reference %q: expected @-N with N >= 1", s)
		}
		return atN(n), nil
	}
	if strings.HasPrefix(s, "@") {
		return Ref{}, fmt.Errorf("invalid reference %q: only @-N is supported", s)
	}
	if err := checkpoint.ValidateIDPrefix(s); err != nil {
		return Ref{}, fmt.Errorf("invalid reference %q: %v", s, err)
	}
	return Ref{Kind: RefCheckpointPrefix, Display: s, Raw: s}, nil
}

// runRefPrefix marks a recorded-run observation reference.
const runRefPrefix = "run:"

// parseRunRef parses "run:<id>:<before|after>". The selector is the final
// colon-separated token; a run id is lowercase hex and never contains a colon, so
// splitting on the last colon is unambiguous. The resolver validates the id prefix.
func parseRunRef(s string) (Ref, error) {
	rest := s[len(runRefPrefix):]
	i := strings.LastIndex(rest, ":")
	if i < 0 {
		return Ref{}, fmt.Errorf("invalid run reference %q: expected run:<id>:before or run:<id>:after", s)
	}
	id, selTok := rest[:i], rest[i+1:]
	if id == "" {
		return Ref{}, fmt.Errorf("invalid run reference %q: missing run id", s)
	}
	var sel RunObservationSel
	switch selTok {
	case "before":
		sel = RunBefore
	case "after":
		sel = RunAfter
	default:
		return Ref{}, fmt.Errorf("invalid run reference %q: selector must be before or after", s)
	}
	return Ref{Kind: RefRunObservation, Display: s, RunRef: id, RunSel: sel}, nil
}

// restoreRefPrefix marks an applied restore's recovery-observation reference.
const restoreRefPrefix = "restore:"

// parseRestoreRef parses "restore:<id>:before". The selector is the final
// colon-separated token; a restore operation id is lowercase hex and never
// contains a colon, so splitting on the last colon is unambiguous. Only "before"
// exists: a restore's "after" state is the worktree, which "now" already names, so
// naming one is a usage error rather than a reference the resolver could satisfy.
// The resolver validates the id prefix.
func parseRestoreRef(s string) (Ref, error) {
	rest := s[len(restoreRefPrefix):]
	i := strings.LastIndex(rest, ":")
	if i < 0 {
		return Ref{}, fmt.Errorf("invalid restore reference %q: expected restore:<id>:before", s)
	}
	id, selTok := rest[:i], rest[i+1:]
	if id == "" {
		return Ref{}, fmt.Errorf("invalid restore reference %q: missing restore operation id", s)
	}
	if selTok != "before" {
		return Ref{}, fmt.Errorf("invalid restore reference %q: only restore:<id>:before exists (the state after a restore is the current worktree, which \"now\" names)", s)
	}
	return Ref{Kind: RefRestoreObservation, Display: s, RestoreRef: id}, nil
}

func nowRef() Ref { return Ref{Kind: RefNow, Display: "now"} }

func atN(n int) Ref {
	return Ref{Kind: RefAtN, N: n, Display: fmt.Sprintf("@-%d", n)}
}
