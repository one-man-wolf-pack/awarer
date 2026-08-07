package restore

import "errors"

// Mutation errors. They are sentinels so the application can turn a filesystem
// refusal into the right closed Reason — and therefore the right outcome — without
// matching error strings, and so infrastructure can report a refusal without
// deciding what it means for the plan.
var (
	// ErrPreconditionMismatch reports that a destination path is no longer the
	// state the plan proved it was, as observed at the descriptor boundary
	// immediately before mutating it. It is the last-moment guard; it maps to
	// ReasonPreconditionMismatch and a conflict, never to a refreshed plan.
	ErrPreconditionMismatch = errors.New("restore precondition no longer holds")
	// ErrDirectoryNotEmpty reports that a proved directory deletion could not run
	// because the directory still holds an entry restore never observed. Deletion
	// is empty-only and never recursive, so this is a conflict rather than a reason
	// to take the unobserved content with it.
	ErrDirectoryNotEmpty = errors.New("directory is not empty")
	// ErrSymlinkAncestor reports that a component above the destination is a
	// symlink, so writing through it would leave the confined tree.
	ErrSymlinkAncestor = errors.New("path is reached through a symlink")
	// ErrUnsupportedNode reports a destination or desired shape restore does not
	// handle: a device, socket, fifo, or other special file.
	ErrUnsupportedNode = errors.New("unsupported filesystem node kind")
	// ErrStagedContentMismatch reports that bytes read from the source store do not
	// hash back to the identity the source manifest addresses. The store is not
	// serving what it promised, so nothing is staged and apply refuses before
	// mutating: awa never manufactures bytes to fill the gap.
	ErrStagedContentMismatch = errors.New("staged bytes do not match the source content identity")
)

// Reason is a stable machine token for why a restore operation is not ready, or
// why an apply stopped. The set is closed: an operation cannot be built with a
// reason outside it, so JSON consumers and agents get tokens they can branch on
// rather than free-form prose. Each reason declares the availabilities it may
// justify, so "blocked because the precondition changed" is unrepresentable — that
// is an apply-time fact, and planning never learns it.
type Reason string

const (
	// ReasonHashOnlyContent: the source proves this file's identity but stored no
	// bytes for it. A hash proves identity, not recoverability, and awa never
	// manufactures bytes from a current file, another path, Git, or the network.
	ReasonHashOnlyContent Reason = "hash-only-content"
	// ReasonBlobMissing: the source names a blob that is not in the store.
	ReasonBlobMissing Reason = "blob-missing"
	// ReasonBlobCorrupt: a stored blob does not hash to the address it is stored
	// under, so its bytes cannot be trusted as the source's content.
	ReasonBlobCorrupt Reason = "blob-corrupt"
	// ReasonSkippedBoundary: a skipped (unreadable or policy-excluded) input on
	// either side intersects this path, so its comparison is not evidence.
	ReasonSkippedBoundary Reason = "skipped-boundary"
	// ReasonIgnoredBoundary: the path is outside awa's evidence boundary. It is
	// never deleted merely for being absent from a source manifest.
	ReasonIgnoredBoundary Reason = "ignored-boundary"
	// ReasonPolicyIncompatible: the source's persisted scan identity and the
	// current effective scan identity disagree, so absence in the source is not
	// proof of absence in awa scope and deletion cannot be justified.
	ReasonPolicyIncompatible Reason = "policy-incompatible"
	// ReasonObservationUnstable: the current observation could not be taken as a
	// consistent point-in-time snapshot, so its preconditions are not trustworthy.
	ReasonObservationUnstable Reason = "observation-unstable"
	// ReasonPathConflict: two operations, or an operation and an unobserved child,
	// contend for the same path — for example a directory that must be removed
	// while a child it still holds was never observed.
	ReasonPathConflict Reason = "path-conflict"
	// ReasonRootEscape: the path resolves outside the project root.
	ReasonRootEscape Reason = "root-escape"
	// ReasonSymlinkAncestor: a component above the path is a symlink, so writing
	// through it would leave the confined tree.
	ReasonSymlinkAncestor Reason = "symlink-ancestor"
	// ReasonUnsupportedEntryKind: a device, socket, fifo, or other special file.
	// Restore refuses these rather than approximating them.
	ReasonUnsupportedEntryKind Reason = "unsupported-entry-kind"
	// ReasonOutOfProvenScope: the source is a recovery observation whose recorded
	// selection does not cover this path, so it proves nothing about it.
	ReasonOutOfProvenScope Reason = "out-of-proven-scope"

	// ReasonPreconditionMismatch: the current state at this path is no longer what
	// the plan proved. It is discovered while applying — by the re-observation or by
	// the per-path guard — so it describes an apply result, never a planned
	// operation, and it is a conflict rather than a silently refreshed plan.
	ReasonPreconditionMismatch Reason = "precondition-mismatch"
	// ReasonRecoveryIncomplete: the pre-restore recovery observation could not
	// capture every selected current byte or metadata item the operation may
	// destroy, so apply refuses before mutating. There is no --force bypass.
	ReasonRecoveryIncomplete Reason = "recovery-incomplete"
	// ReasonCancelled: the operation was cancelled (Ctrl+C or a cancelled context).
	ReasonCancelled Reason = "cancelled"
	// ReasonIOFailure: a filesystem operation failed while applying.
	ReasonIOFailure Reason = "io-failure"
	// ReasonPartialApply: a multi-path commit stopped after a prefix was applied.
	// Filesystem mutation across several paths is not transactional, and this is
	// the token that says so rather than dressing the result up as success.
	ReasonPartialApply Reason = "partial-apply"
)

// reasonSpec fixes what the domain knows about one reason: which availabilities
// it may justify, and whether it is an outcome-level fact rather than a
// per-operation one.
type reasonSpec struct {
	reason Reason
	// avail lists the operation availabilities this reason may describe. An
	// outcome-only reason lists none.
	avail []Availability
}

// reasonCatalog is the canonical, closed enumeration and the single source of
// truth about it: Valid, allowsAvailability, and Reasons all read it, so
// membership, the availabilities a reason justifies, and the published order can
// never disagree.
//
// The order is semantic: evidence gaps first (why the source cannot prove this),
// then boundary/safety refusals, then the apply-time facts. That is the order
// preview output reads in and the order documentation covering this vocabulary
// presents it in. Consumers may rely on it being stable.
var reasonCatalog = []reasonSpec{
	// Evidence gaps: the source does not prove a recoverable desired state.
	{ReasonHashOnlyContent, []Availability{Blocked}},
	{ReasonBlobMissing, []Availability{Blocked}},
	{ReasonBlobCorrupt, []Availability{Blocked}},
	{ReasonSkippedBoundary, []Availability{Blocked}},
	{ReasonIgnoredBoundary, []Availability{Blocked}},
	{ReasonPolicyIncompatible, []Availability{Blocked}},
	{ReasonObservationUnstable, []Availability{Blocked}},
	{ReasonOutOfProvenScope, []Availability{Blocked}},

	// Boundary and safety refusals.
	{ReasonPathConflict, []Availability{Blocked}},
	{ReasonRootEscape, []Availability{Blocked}},
	{ReasonSymlinkAncestor, []Availability{Blocked}},
	{ReasonUnsupportedEntryKind, []Availability{Blocked}},

	// Apply-time facts. They describe what happened while committing, so they
	// justify no planned operation: a plan is decided before the first mutation and
	// cannot know that a destination will move, that a write will fail, or that the
	// operation will be cancelled. They reach output as an apply result's reasons and
	// its bounded per-path failures.
	{ReasonPreconditionMismatch, nil},
	{ReasonRecoveryIncomplete, nil},
	{ReasonCancelled, nil},
	{ReasonIOFailure, nil},
	{ReasonPartialApply, nil},
}

// Reasons returns the closed reason vocabulary in canonical order. The result is
// a copy: the catalog is package state, and a consumer that walks it must not be
// able to reorder or truncate what every other consumer sees.
func Reasons() []Reason {
	out := make([]Reason, 0, len(reasonCatalog))
	for _, s := range reasonCatalog {
		out = append(out, s.reason)
	}
	return out
}

// reasonAvail is the lookup view of reasonCatalog, built once so the hot paths
// stay map lookups without becoming a second hand-maintained list.
var reasonAvail = func() map[Reason]map[Availability]bool {
	m := make(map[Reason]map[Availability]bool, len(reasonCatalog))
	for _, s := range reasonCatalog {
		set := make(map[Availability]bool, len(s.avail))
		for _, a := range s.avail {
			set[a] = true
		}
		m[s.reason] = set
	}
	return m
}()

// Valid reports whether r is a known reason.
func (r Reason) Valid() bool {
	_, ok := reasonAvail[r]
	return ok
}

// allowsAvailability reports whether r may describe an operation with the given
// availability.
func (r Reason) allowsAvailability(a Availability) bool { return reasonAvail[r][a] }

func (r Reason) String() string { return string(r) }
