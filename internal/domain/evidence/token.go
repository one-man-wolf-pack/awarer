// Package evidence holds the shared, wire-facing vocabulary for durable-read health:
// the stable machine tokens and component names that agent-facing JSON uses to
// describe degraded evidence, plus the small value objects that carry those facts.
//
// It exists so that "there is no evidence" is never confused with "evidence exists
// but could not be read, trusted, or fully summarized." A failed durable read must
// surface as a structured Degradation with a stable DiagnosticToken, not as an empty
// count or a prose warning an agent has to parse.
//
// The per-subsystem read-health aggregates (checkpoint.CheckpointStoreHealth, the
// run-cache twin) keep their own richer domain types; they map their derived verdict
// into these tokens at the edge. This package is deliberately the narrow, shared
// bottom of that funnel — one closed token set, not a second parallel health model.
package evidence

// Component names the store subsystem a piece of evidence belongs to. The set is
// closed: a Degradation cannot be built for a component outside it, so a consumer
// filtering by component never meets a free-form string.
type Component string

const (
	// ComponentCheckpoints is the checkpoint store (.awa/checkpoints).
	ComponentCheckpoints Component = "checkpoints"
	// ComponentRuns is the run cache (.awa/store/runs).
	ComponentRuns Component = "runs"
	// ComponentBlobs is the content-addressed blob store (.awa/store/blobs).
	ComponentBlobs Component = "blobs"
	// ComponentIndex is the worktree index (SQLite).
	ComponentIndex Component = "index"
	// ComponentLocks is the lock directory.
	ComponentLocks Component = "locks"
	// ComponentConfig is the project config file.
	ComponentConfig Component = "config"
	// ComponentStore is the store as a whole (used for store-wide facts such as the
	// blob footprint that is bounded out of the ordinary dashboard).
	ComponentStore Component = "store"
)

// componentSet is the closed set of known components, the single source of truth for
// Valid.
var componentSet = map[Component]bool{
	ComponentCheckpoints: true,
	ComponentRuns:        true,
	ComponentBlobs:       true,
	ComponentIndex:       true,
	ComponentLocks:       true,
	ComponentConfig:      true,
	ComponentStore:       true,
}

// Valid reports whether c is a known component.
func (c Component) Valid() bool { return componentSet[c] }

func (c Component) String() string { return string(c) }

// DiagnosticToken is a stable, lowercase-kebab machine token classifying one
// durable-read condition. The set is closed so agent-facing JSON never carries a
// renderer-local phrase where a token is possible. Prefer these few tokens with clear
// semantics over many one-off strings.
type DiagnosticToken string

const (
	// TokenStoreMissing means the store directory does not exist (e.g. a subsystem dir
	// absent under an initialized project). Distinct from store-empty.
	TokenStoreMissing DiagnosticToken = "store-missing"
	// TokenStoreEmpty means the store was read successfully and holds no entries. This
	// is the only token that means "none yet"; it is allowed only after a successful
	// enough read to know nothing exists.
	TokenStoreEmpty DiagnosticToken = "store-empty"
	// TokenReadPartial means some entries read cleanly and at least one did not.
	TokenReadPartial DiagnosticToken = "read-partial"
	// TokenReadBounded means the scan intentionally stopped at a cap, so the reported
	// counts are incomplete. It must carry a BoundedCount.
	TokenReadBounded DiagnosticToken = "read-bounded"
	// TokenMetadataCorrupt means a record's metadata failed to decode or validate for a
	// reason other than an incompatible declared schema — genuine durable corruption.
	TokenMetadataCorrupt DiagnosticToken = "metadata-corrupt"
	// TokenMetadataIncompatible means a record declares a schema this build does not
	// speak. The record is intact; awa has no reader for it and retains it until the
	// user resets local evidence.
	TokenMetadataIncompatible DiagnosticToken = "metadata-incompatible"
	// TokenPayloadMissing means a payload referenced by valid metadata is absent.
	TokenPayloadMissing DiagnosticToken = "payload-missing"
	// TokenPayloadCorrupt means a referenced payload exists but fails hash/size
	// verification.
	TokenPayloadCorrupt DiagnosticToken = "payload-corrupt"
	// TokenPermissionDenied means the store entry could not be read because the
	// platform denied access — an access problem, not data damage.
	TokenPermissionDenied DiagnosticToken = "permission-denied"
	// TokenIOError means a read failed for an I/O reason other than permission.
	TokenIOError DiagnosticToken = "io-error"
	// TokenUnsupportedFormat means a record declares a version this build cannot read.
	TokenUnsupportedFormat DiagnosticToken = "unsupported-format"
	// TokenUnknown is the fail-closed classification: a degradation that could not be
	// classified more precisely. It is never a substitute for a real condition; it
	// exists so an unclassified read never silently becomes "empty".
	TokenUnknown DiagnosticToken = "unknown"
)

// tokenSet is the closed set of known tokens, the single source of truth for Valid.
var tokenSet = map[DiagnosticToken]bool{
	TokenStoreMissing:         true,
	TokenStoreEmpty:           true,
	TokenReadPartial:          true,
	TokenReadBounded:          true,
	TokenMetadataCorrupt:      true,
	TokenMetadataIncompatible: true,
	TokenPayloadMissing:       true,
	TokenPayloadCorrupt:       true,
	TokenPermissionDenied:     true,
	TokenIOError:              true,
	TokenUnsupportedFormat:    true,
	TokenUnknown:              true,
}

// Valid reports whether t is a known diagnostic token.
func (t DiagnosticToken) Valid() bool { return tokenSet[t] }

func (t DiagnosticToken) String() string { return string(t) }

// Note on degradedness: there is deliberately no context-free DiagnosticToken.Degraded()
// helper. Whether a token means "degraded evidence" depends on context a token cannot
// carry — store-missing is a healthy resolution before init but broken evidence for an
// initialized project's subsystem — so collapsing that into a token method invites a
// renderer to re-derive the wrong verdict. Degradedness is owned by the subsystem/result
// health objects that hold that context (e.g. status.Subsystem.Degraded), which decide
// it from the resolved state, not from a token in isolation.

// Advisory reports whether t is a non-correctness advisory (rendered as a calm "note")
// rather than a correctness warning. read-bounded is advisory: the evidence is present
// and the verified sample is clean — it was simply not exhaustively checked, so it must
// not read as an alarm. Every other degraded token is a warning.
func (t DiagnosticToken) Advisory() bool {
	return t == TokenReadBounded
}
