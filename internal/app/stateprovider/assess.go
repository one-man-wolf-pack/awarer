// Package stateprovider is the application assessor behind the awa-state/v1 provider
// surface. It drives the shared state resolver and the canonical streaming comparison,
// then projects their results into the closed provider domain model
// (internal/domain/provider): a resolved identity or a stable unavailable reason for
// `state resolve`, and an equal/different/incomparable/unavailable verdict for
// `state compare`.
//
// Its defining rule is that expected evidence degradation is a *successful assessment*,
// never a failure: a missing, corrupt, incompatible, permission-denied, or GC-removed
// reference resolves to an unavailable outcome with a machine reason, not a non-zero
// error. The only faults it propagates are interruption (context cancellation) and an
// impossible internal state — one this package raised itself, either by returning it
// directly (a resolved state the domain constructors reject) or by marking it
// errInternalFault where it passes through the availability classifier. An internal state
// is recognized by that explicit marking, never by an error's absence from the
// availability allowlist. It never classifies through the changes/diff exit-code mapping,
// whose store sentinels mean the opposite there.
package stateprovider

import (
	"context"
	"errors"
	"fmt"
	"os"

	"awarer/internal/app/state"
	"awarer/internal/domain/blob"
	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/provider"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/worktree"
)

// Assessor turns parsed references into provider assessments and comparisons over the
// shared state resolver.
type Assessor struct {
	resolver *state.Resolver
}

// NewAssessor builds an assessor over an already-wired state resolver (the same
// composition root changes/diff use).
func NewAssessor(resolver *state.Resolver) *Assessor { return &Assessor{resolver: resolver} }

// candidate is a resolved-but-not-yet-verified operand: the open ResolvedState plus the
// provider.Identity it *would* publish. That identity carries its metadata (tree hash,
// scan identity, and a reconstructable claim for a stored state) from the header alone —
// enough to classify a pair — but must not be published until the manifest is confirmed
// readable, since publishing asserts a reconstructability the manifest must back. It is
// the typed boundary between a resolution candidate and a verified, publishable identity.
type candidate struct {
	rs *state.ResolvedState
	id provider.Identity
}

// operandOutcome is one operand's final availability verdict as a closed two-state
// value: either a verified, publishable identity, or the closed reason it is
// unavailable. Its zero value is not built directly — both states come from the
// constructors below, so "an identity together with a reason" and "neither" have no
// representation and callers coordinate no nullable tuple. A genuine fault
// (interruption) is not an outcome: finalize returns it separately as an error, never
// folded in here. This is the type that makes the OC-11-class defect — a helper
// silently dropping one arm of the verdict — unrepresentable.
type operandOutcome struct {
	id     *provider.Identity
	reason provider.Reason
}

// verifiedOperand is the outcome of an operand whose manifest verified readable: the
// identity it may publish.
func verifiedOperand(id provider.Identity) operandOutcome { return operandOutcome{id: &id} }

// unavailableOperand is the outcome of an operand that did not resolve, or whose
// manifest is unreadable: the closed reason why. The reason is a validated availability
// reason from reasonFor, so it is never empty.
func unavailableOperand(reason provider.Reason) operandOutcome {
	return operandOutcome{reason: reason}
}

// identity returns the publishable identity and true when the operand verified readable,
// or (nil, false) when it is unavailable — the two arms are mutually exclusive by
// construction.
func (o operandOutcome) identity() (*provider.Identity, bool) { return o.id, o.id != nil }

// unavailableReason returns the unavailability reason and true when the operand is
// unavailable, or ("", false) when it verified readable.
func (o operandOutcome) unavailableReason() (provider.Reason, bool) {
	if o.id != nil {
		return "", false
	}
	return o.reason, true
}

// Resolve produces the provider assessment for one parsed reference. inputRef is the
// exact actor-supplied expression, echoed back verbatim. A genuine fault (interruption,
// or an impossible internal state) is returned as a non-nil error; every expected
// degradation is a complete unavailable assessment with a nil error.
func (a *Assessor) Resolve(ctx context.Context, ref state.Ref, inputRef string, now state.NowContext) (provider.Assessment, error) {
	c, reason, fault := a.resolveCandidate(ctx, ref, now)
	if fault != nil {
		return provider.Assessment{}, fault
	}
	if c != nil {
		defer func() { _ = c.rs.Close() }()
	}
	// finalize folds resolution and the one manifest-verification pass into a single
	// verdict: Resolve reads the manifest nowhere else, so the identity is published only
	// when that verdict is a verified identity.
	outcome, fault := a.finalize(ctx, c, reason)
	if fault != nil {
		return provider.Assessment{}, fault
	}
	if id, ok := outcome.identity(); ok {
		return provider.Resolved(inputRef, *id)
	}
	r, _ := outcome.unavailableReason()
	return provider.Unavailable(inputRef, r)
}

// resolveCandidate resolves one reference to a candidate. On success it returns the
// candidate (caller Closes its rs) and an empty reason; on an expected availability
// failure a nil candidate with a reason; on a genuine fault a non-nil error.
//
// It deliberately does not verify the stored manifest: the caller verifies at the point
// it reads the manifest — a merge for a difference, an explicit verifyReadable otherwise —
// so a manifest is never read twice on a success path. The candidate's identity is never
// published without that verification.
func (a *Assessor) resolveCandidate(ctx context.Context, ref state.Ref, now state.NowContext) (*candidate, provider.Reason, error) {
	// The provider owns the point-in-time invariant for a "now" side: a stable observation
	// is required no matter how the caller built the NowContext. Set here — the single
	// point every ref reaches the resolver — so no assessor caller can forget it and turn
	// a moving worktree into a silently partial snapshot instead of observation-unstable.
	now.RequireStableObservation = true
	if ref.Kind == state.RefNow {
		return a.resolveStableNow(ctx, ref, now)
	}
	return a.resolveOnce(ctx, ref, now)
}

// resolveOnce resolves a reference through a single resolver pass. A "now" reference means
// a single scan here; its whole-scope stability is proven by resolveStableNow, which drives
// two of these passes.
func (a *Assessor) resolveOnce(ctx context.Context, ref state.Ref, now state.NowContext) (*candidate, provider.Reason, error) {
	rs, err := a.resolver.Resolve(ctx, ref, now)
	if err != nil {
		reason, isAvailability := reasonFor(err)
		if !isAvailability {
			return nil, "", err
		}
		return nil, reason, nil
	}
	id, err := identityFrom(rs)
	if err != nil {
		_ = rs.Close()
		return nil, "", fmt.Errorf("state provider: building resolved identity: %w", err)
	}
	return &candidate{rs: rs, id: id}, "", nil
}

// resolveStableNow resolves a "now" reference twice and confirms both independent
// observations agree on the whole tree identity — the same tree hash under the same
// scan/policy identity. Two eager full scans under the same immutable effective config are
// the provider's fixed, un-retried cost for proving a consistent point-in-time
// observation: a whole-scope change between them — an add, delete, or modify of an
// already-visited path, or one surfaced by index reuse — makes them disagree, which the
// narrow mid-scan guard (still active, so a moving single file aborts the first pass early)
// cannot see on its own. On disagreement it is a successful observation-unstable
// assessment with no identity; on agreement it publishes the second observation and closes
// the first at once. A scan fault or interruption in either pass propagates unchanged.
func (a *Assessor) resolveStableNow(ctx context.Context, ref state.Ref, now state.NowContext) (*candidate, provider.Reason, error) {
	first, reason, fault := a.resolveOnce(ctx, ref, now)
	if first == nil {
		// A mid-scan guard trip (observation-unstable reason) or a fault short-circuits here,
		// before a second scan is even attempted.
		return nil, reason, fault
	}
	second, reason, fault := a.resolveOnce(ctx, ref, now)
	if second == nil {
		_ = first.rs.Close()
		return nil, reason, fault
	}
	if !sameObservation(first.rs, second.rs) {
		_ = first.rs.Close()
		_ = second.rs.Close()
		return nil, provider.ReasonObservationUnstable, nil
	}
	// The two observations coincide: publish the second, release the first immediately.
	_ = first.rs.Close()
	return second, "", nil
}

// sameObservation reports whether two independent "now" scans observed the same whole-tree
// identity: the same tree hash under the same scan/policy identity. Both scans run under
// the same immutable effective config, so a disagreement means the worktree moved between
// them. It proves stability by the observed policy, not by defending against a deliberate
// retention of every trusted stat field under an index-reuse trust policy.
func sameObservation(a, b *state.ResolvedState) bool {
	if a.TreeHash != b.TreeHash {
		return false
	}
	return a.ScanIdentity() == b.ScanIdentity()
}

// finalize computes one operand's final availability outcome as a closed
// operandOutcome: a verified, publishable identity — or the reason it is unavailable
// because it did not resolve (resolveReason) or its manifest is unreadable (the
// verification reason). Interruption propagates separately as a fault. It is the single
// place resolution and manifest verification are folded into one per-operand verdict, so
// an unavailable comparison can pick its reason honestly.
func (a *Assessor) finalize(ctx context.Context, c *candidate, resolveReason provider.Reason) (operandOutcome, error) {
	if c == nil {
		return unavailableOperand(resolveReason), nil
	}
	r, fault := a.verifyReadable(ctx, c.rs)
	if fault != nil {
		return operandOutcome{}, fault
	}
	if r != "" {
		return unavailableOperand(r), nil
	}
	return verifiedOperand(c.id), nil
}

// verifyReadable drains a stored state's manifest once to confirm it is readable and
// internally consistent, classifying an unreadable one into an availability reason. It
// returns ("", nil) when readable, (reason, nil) for an expected availability failure, and
// ("", fault) for a genuine fault (interruption). It is the confirmation a caller runs
// when it does not otherwise read the manifest (Resolve, and the equal/incomparable
// verdicts, which take no merge).
func (a *Assessor) verifyReadable(ctx context.Context, rs *state.ResolvedState) (provider.Reason, error) {
	if err := verifyManifest(ctx, rs); err != nil {
		reason, isAvailability := reasonFor(err)
		if !isAvailability {
			return "", err
		}
		return reason, nil
	}
	return "", nil
}

// verifyManifest streams a stored state's manifest through its verifying cursor to
// confirm it is readable and internally consistent. A missing manifest payload surfaces
// as manifest-unavailable and corrupt/truncated content as metadata-corrupt, so an
// unreadable manifest yields an unavailable assessment rather than a false resolved. A
// "now" state's manifest is the fresh scan just taken, readable by construction, so it is
// not re-read. The drain is O(1) memory (streaming) and reads no blob content.
func verifyManifest(ctx context.Context, rs *state.ResolvedState) error {
	if rs.Kind == state.KindNow {
		return nil
	}
	cur, err := rs.Manifest(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cur.Close() }()
	for cur.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return cur.Err()
}

// errInternalFault marks an error this package raised because its own program invariant
// was violated, so reasonFor propagates it as a fault instead of classifying it as
// evidence availability. It carries identity only — the classifier needs one bit, and the
// diagnostic context travels in the wrapping. It is attached solely at a call site that
// locally knows the state is impossible; it is never inferred from an error's text or
// from its absence in the availability allowlist below, which would erase the accepted
// io-error boundary for an ordinary unclassified read failure.
var errInternalFault = errors.New("impossible internal state")

// reasonFor classifies a non-nil resolver/store error. The second result is true when
// the error is an evidence-availability fact — a complete unavailable assessment that
// exits zero; it is false only for a genuine fault — interruption, or an explicitly
// marked impossible internal state — that must propagate as a non-zero error.
// Interruption is checked first so a cancelled read is never reported as an availability
// reason, and it is a fault by cancellation alone, needing no marker. An otherwise
// unclassified read-path error fails closed to io-error: availability is always an
// assessment, never an empty success.
func reasonFor(err error) (provider.Reason, bool) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "", false
	case errors.Is(err, errInternalFault):
		return "", false
	case errors.Is(err, state.ErrNoCheckpoints),
		errors.Is(err, state.ErrOutOfRange),
		errors.Is(err, checkpoint.ErrNotFound),
		errors.Is(err, runcache.ErrNotFound):
		return provider.ReasonNotFound, true
	case errors.Is(err, state.ErrLatestIncompatible),
		errors.Is(err, checkpoint.ErrIncompatibleFormat):
		return provider.ReasonMetadataIncompatible, true
	case errors.Is(err, runcache.ErrObservationUnavailable),
		errors.Is(err, worktree.ErrManifestMissing):
		// An absent manifest payload (a record's metadata reads, but its manifest is
		// gone) is manifest-unavailable, distinct from corrupt content below. Checked
		// before the corrupt cases because a missing manifest is also tagged as store
		// corruption for integrity callers.
		return provider.ReasonManifestUnavailable, true
	case errors.Is(err, state.ErrLatestCorrupt),
		errors.Is(err, checkpoint.ErrCorruptStore),
		errors.Is(err, runcache.ErrCorruptStore),
		errors.Is(err, blob.ErrCorruptBlob):
		return provider.ReasonMetadataCorrupt, true
	case errors.Is(err, state.ErrLatestUnreadable):
		// A bare unreadable-store error with neither refinement: fail closed to corrupt,
		// never the milder incompatible verdict.
		return provider.ReasonMetadataCorrupt, true
	case errors.Is(err, checkpoint.ErrAmbiguousPrefix),
		errors.Is(err, runcache.ErrAmbiguousPrefix):
		return provider.ReasonAmbiguousReference, true
	case errors.Is(err, runcache.ErrInvalidPrefix):
		return provider.ReasonUnsupportedReference, true
	case errors.Is(err, state.ErrObservationUnstable):
		return provider.ReasonObservationUnstable, true
	case errors.Is(err, os.ErrPermission):
		return provider.ReasonPermissionDenied, true
	case errors.Is(err, os.ErrNotExist):
		return provider.ReasonNotFound, true
	default:
		return provider.ReasonIOError, true
	}
}

// identityFrom projects a resolved state into the closed provider identity. It reads
// only bounded metadata already loaded by the resolver — it never drains the manifest or
// reads blob content — so a resolve retains O(1) memory.
func identityFrom(rs *state.ResolvedState) (provider.Identity, error) {
	scan, err := scanIdentityFrom(rs)
	if err != nil {
		return provider.Identity{}, err
	}
	bnd, err := provider.NewEvidenceBoundary(rs.Skipped())
	if err != nil {
		return provider.Identity{}, err
	}
	// A "now" observation reports the fresh scan's completion time and a checkpoint its
	// recorded creation time; a run observation reports no time (hasObservedAt false).
	observedAt, _ := rs.ObservedAt()
	switch rs.Kind {
	case state.KindNow:
		return provider.NewCurrentWorktreeIdentity(rs.TreeHash, scan, observedAt, bnd)
	case state.KindCheckpoint:
		id, ok := rs.CheckpointID()
		if !ok || rs.Header == nil {
			return provider.Identity{}, fmt.Errorf("resolved checkpoint state without an id or header")
		}
		// The resolver already carries the typed checkpoint id; the constructor derives the
		// canonical ref from it.
		return provider.NewCheckpointIdentity(id, rs.TreeHash, scan, observedAt, bnd)
	case state.KindRunObservation:
		runID, sel, _, _, ok := rs.RunObservation()
		if !ok {
			return provider.Identity{}, fmt.Errorf("resolved run state without an observation id")
		}
		// The run id crosses the resolver boundary as a string; parse it back to the typed
		// id here so the provider identity carries a validated id, never a raw string.
		rid, err := runcache.ParseRunID(runID)
		if err != nil {
			return provider.Identity{}, fmt.Errorf("state provider: parsing run id %q: %w", runID, err)
		}
		return provider.NewRunObservationIdentity(sel == state.RunBefore, rid, rs.TreeHash, scan, observedAt, false, bnd)
	default:
		return provider.Identity{}, fmt.Errorf("unknown resolved state kind %d", rs.Kind)
	}
}

// scanIdentityFrom builds the provider scan identity from the resolved state's
// scan/policy identity. Every resolvable state — checkpoint, current worktree, and run
// observation — carries a complete policy identity, so NewScanIdentity validates it:
// a zero scan-config (the unreachable default kind) is rejected as an invalid identity
// rather than modeled as a weakened partial projection.
func scanIdentityFrom(rs *state.ResolvedState) (provider.ScanIdentity, error) {
	return provider.NewScanIdentity(rs.ScanIdentity())
}
