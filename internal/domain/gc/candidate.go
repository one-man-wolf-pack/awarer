package gc

import (
	"fmt"
	"strings"
)

// GCCandidate records one decision GC made about one item. Its fields are
// unexported so a candidate can only be built through NewCandidate, which enforces
// the invariants: the kind, action, and reason are known; the reason justifies the
// action; the message is human-readable; a checkpoint, run, or blob carries a
// non-empty id; and every candidate carries a path so a report line is locatable.
// This keeps an impossible decision — an unknown reason, a delete justified by a
// retain reason, an empty target — out of every plan.
type GCCandidate struct {
	kind    CandidateKind
	action  CandidateAction
	reason  RetentionReason
	id      string
	path    string
	message string
	bytes   int64
}

// NewCandidate builds a validated candidate. The reason's intrinsic action (see
// reasonAction) must equal action, so the two can never drift. id is required for
// checkpoint, run, and blob candidates (a temp artifact has no domain id, only a
// path); path and message are always required; bytes must be non-negative.
func NewCandidate(kind CandidateKind, reason RetentionReason, id, path, message string, bytes int64) (GCCandidate, error) {
	if !kind.Valid() {
		return GCCandidate{}, fmt.Errorf("invalid candidate kind %d", kind)
	}
	if !reason.Valid() {
		return GCCandidate{}, fmt.Errorf("invalid retention reason %q", reason)
	}
	action := reason.Action()
	if !action.Valid() {
		return GCCandidate{}, fmt.Errorf("retention reason %q has no valid action", reason)
	}
	if !reason.allowsKind(kind) {
		return GCCandidate{}, fmt.Errorf("retention reason %q cannot describe a %s candidate", reason, kind)
	}
	if strings.TrimSpace(path) == "" {
		return GCCandidate{}, fmt.Errorf("candidate %s/%s requires a path", kind, reason)
	}
	if strings.TrimSpace(message) == "" {
		return GCCandidate{}, fmt.Errorf("candidate %s/%s requires a message", kind, reason)
	}
	if kind != KindTemp && kind != KindLock && strings.TrimSpace(id) == "" {
		return GCCandidate{}, fmt.Errorf("candidate %s/%s requires an id", kind, reason)
	}
	if bytes < 0 {
		return GCCandidate{}, fmt.Errorf("candidate %s/%s has negative bytes %d", kind, reason, bytes)
	}
	return GCCandidate{
		kind:    kind,
		action:  action,
		reason:  reason,
		id:      id,
		path:    path,
		message: message,
		bytes:   bytes,
	}, nil
}

func (c GCCandidate) Kind() CandidateKind     { return c.kind }
func (c GCCandidate) Action() CandidateAction { return c.action }
func (c GCCandidate) Reason() RetentionReason { return c.reason }
func (c GCCandidate) ID() string              { return c.id }
func (c GCCandidate) Path() string            { return c.path }
func (c GCCandidate) Message() string         { return c.message }
func (c GCCandidate) Bytes() int64            { return c.bytes }
