// Package provider holds the closed domain model for awa's external state
// provider surface (the awa-state/v1 contract behind `awa state resolve` and
// `awa state compare`).
//
// It is a pure wire projection: it makes the resolved-identity, provider-assessment,
// and state-comparison shapes unrepresentable in an invalid state, and it owns the
// closed reason catalog and the provider-contract token. It knows nothing about
// JSON, the filesystem, `.awa/` layouts, or any consumer. Record ids are carried as
// full strings — a resolved identity never shortens an id — so the model imports only
// leaf domain packages (hashing, config, evidence) and never the resolver or the
// checkpoint/run repositories.
package provider

import "awarer/internal/domain/evidence"

// Contract is the versioned identity-semantics token this provider surface emits,
// independent of the binary SemVer and of the global JSON envelope version. It is a
// closed value, not an arbitrary string: additive diagnostic detail is allowed under
// the same token, but changing identity fields, outcome/reason spelling, or equality
// meaning requires a new contract value.
type Contract string

// ContractStateV1 is the state-resolution/comparison contract token.
const ContractStateV1 Contract = "awa-state/v1"

// Valid reports whether c is a known contract token.
func (c Contract) Valid() bool { return c == ContractStateV1 }

func (c Contract) String() string { return string(c) }

// Reason is a stable, closed, lowercase-kebab token explaining why a state could
// not be resolved (`unavailable`) or why two states could not be honestly compared
// (`incomparable`/`unavailable`). The catalog is closed so a consumer switches on one
// finite set and never parses human stderr.
//
// Four concepts overlap the shared durable-read vocabulary in package evidence
// (metadata-corrupt, metadata-incompatible, permission-denied, io-error); those Reasons
// are declared as the exact evidence token value, so the wire spelling is
// compiler-guaranteed identical without folding provider-only concepts
// (not-found, ambiguous-reference, not-initialized, incompatible-identity-policy)
// into the evidence set, whose closed membership is owned by durable-read health.
type Reason string

const (
	// ReasonNotInitialized: no awa project was found for the invocation, so there is
	// no evidence to resolve. The provider is optional, so this is a complete
	// assessment, not a broken invocation.
	ReasonNotInitialized Reason = "not-initialized"
	// ReasonNotFound: the reference is well-formed but names no existing state — an
	// empty store, an @-N past the oldest checkpoint, an id prefix matching nothing,
	// or a GC-removed record.
	ReasonNotFound Reason = "not-found"
	// ReasonAmbiguousReference: an id prefix (checkpoint or run) matches more than one
	// record. The syntax is valid; the reference is simply not specific enough.
	ReasonAmbiguousReference Reason = "ambiguous-reference"
	// ReasonMetadataIncompatible: a record declares a schema this build cannot read.
	ReasonMetadataIncompatible Reason = Reason(evidence.TokenMetadataIncompatible)
	// ReasonMetadataCorrupt: a record's metadata failed to decode or validate for a
	// reason other than an incompatible declared schema — genuine durable corruption.
	ReasonMetadataCorrupt Reason = Reason(evidence.TokenMetadataCorrupt)
	// ReasonManifestUnavailable: metadata exists and reads, but the manifest needed to
	// interpret the state is absent (a post-scan-failed run's after observation, a
	// missing manifest payload).
	ReasonManifestUnavailable Reason = "manifest-unavailable"
	// ReasonPermissionDenied: a store record could not be read because the platform
	// denied access — an access problem, not data damage.
	ReasonPermissionDenied Reason = Reason(evidence.TokenPermissionDenied)
	// ReasonIOError: a store or scan read failed for an I/O reason other than
	// permission. It is the fail-closed classification for an otherwise unclassified
	// read-path failure — availability is always an assessment, never an empty success.
	ReasonIOError Reason = Reason(evidence.TokenIOError)
	// ReasonObservationUnstable: a current-worktree observation could not be taken
	// consistently because a file changed underneath the scan.
	ReasonObservationUnstable Reason = "observation-unstable"
	// ReasonUnsupportedReference: the reference is a recognized shape the provider
	// cannot resolve in this context (a semantically invalid run id, a run reference
	// with no run reader wired).
	ReasonUnsupportedReference Reason = "unsupported-reference"
	// ReasonIncompatibleIdentityPolicy: two states cannot be compared because their
	// scope/observation policy compatibility is not proven — the operands were
	// observed under differing scan identities. Equal tree-hash bytes alone do not
	// overcome it.
	ReasonIncompatibleIdentityPolicy Reason = "incompatible-identity-policy"
)

// reasonSet is the closed set of known reasons, the single source of truth for Valid.
var reasonSet = map[Reason]bool{
	ReasonNotInitialized:             true,
	ReasonNotFound:                   true,
	ReasonAmbiguousReference:         true,
	ReasonMetadataIncompatible:       true,
	ReasonMetadataCorrupt:            true,
	ReasonManifestUnavailable:        true,
	ReasonPermissionDenied:           true,
	ReasonIOError:                    true,
	ReasonObservationUnstable:        true,
	ReasonUnsupportedReference:       true,
	ReasonIncompatibleIdentityPolicy: true,
}

// Valid reports whether r is a known reason token.
func (r Reason) Valid() bool { return reasonSet[r] }

func (r Reason) String() string { return string(r) }
