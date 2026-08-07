package restore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
)

// Record store errors. They are sentinels so the application can branch on them
// — and map each to the right exit code — without matching error strings.
var (
	// ErrRecoveryNotFound reports that no recovery observation matched a lookup.
	ErrRecoveryNotFound = errors.New("restore recovery observation not found")
	// ErrRecoveryAmbiguousPrefix reports that a short id matched more than one
	// recovery observation.
	ErrRecoveryAmbiguousPrefix = errors.New("restore operation id prefix is ambiguous")
	// ErrCorruptRecoveryStore reports that durable recovery-observation state on
	// disk is corrupt: a record that cannot be decoded or validated, an id that
	// disagrees with its directory name, or a non-regular node where a payload
	// belongs. It is distinct from ErrRecoveryNotFound (absent, normal) so the CLI
	// can signal state-action-required rather than an ordinary failure, and so GC
	// can fail closed on blob reachability instead of guessing.
	ErrCorruptRecoveryStore = errors.New("corrupt restore recovery store")
	// ErrRecoveryIDCollision reports that a recovery observation id already exists.
	// The store never overwrites an immutable record, so a collision is surfaced.
	ErrRecoveryIDCollision = errors.New("restore recovery observation id already exists")
)

// ManifestRef is the durable pointer to a recovery observation's manifest
// payload: which file holds it, the tree hash the records reduce to, and how many
// records it must contain. The bytes are the source of truth and these derived
// fields are re-checked on read, so a tampered manifest (records swapped while
// the count is preserved) fails loudly rather than driving a wrong restore.
type ManifestRef struct {
	File        string
	TreeHash    hashing.TreeHash
	RecordCount int
}

// ScopeRef is the durable pointer to a recovery observation's covered-scope
// payload: the sorted set of paths the restore was going to mutate.
//
// The scope is what makes the observation a complete undo. The manifest alone
// records the entries that existed; the scope additionally records the paths the
// restore was going to *create*, which had no current entry to capture. A path
// inside the scope but absent from the manifest is therefore proved to have been
// absent — which is exactly what an inverse restore needs in order to delete it
// again.
type ScopeRef struct {
	File      string
	PathCount int
}

// RecoveryObservation is the validated durable metadata of one pre-restore
// recovery observation: the immutable record awa publishes before the first
// worktree write of an applied restore, addressable as `restore:<id>:before`.
//
// It is deliberately not a checkpoint. It never enters checkpoint history, never
// moves `latest`, and is never selected by a checkpoint-relative reference. It is
// one immutable evidence record per applied restore under its own retention
// policy, not a mutable undo stack.
type RecoveryObservation struct {
	id              OperationID
	createdAt       time.Time
	awaVersion      string
	scanConfigHash  hashing.ConfigHash
	source          Source
	selection       Selection
	manifest        ManifestRef
	scope           ScopeRef
	contentComplete bool
}

// RecoveryBuild carries the non-derived facts a recovery observation is
// published from. The manifest's tree hash and record count are derived by the
// store from the records it streams, never supplied here, so they cannot disagree
// with the bytes on disk.
type RecoveryBuild struct {
	ID             OperationID
	CreatedAt      time.Time
	AwaVersion     string
	ScanConfigHash hashing.ConfigHash
	Source         Source
	Selection      Selection
}

// NewRecoveryObservation builds a validated recovery observation record. It
// rejects every shape that would make the record unusable as undo evidence: a
// missing identity, an unresolved source, an unbuilt selection, a manifest or
// scope pointer with no payload, a negative count, or — the one that matters most
// — a record that does not claim complete content.
//
// contentComplete is required to be true. The product rule is that apply refuses
// rather than publishing partial recovery evidence, so a record whose content is
// admittedly incomplete must never reach disk to be found later and trusted.
func NewRecoveryObservation(b RecoveryBuild, manifest ManifestRef, scope ScopeRef, contentComplete bool) (RecoveryObservation, error) {
	switch {
	case b.ID.IsZero():
		return RecoveryObservation{}, fmt.Errorf("a recovery observation requires an operation id")
	case b.CreatedAt.IsZero():
		return RecoveryObservation{}, fmt.Errorf("a recovery observation requires a creation time")
	case b.AwaVersion == "":
		return RecoveryObservation{}, fmt.Errorf("a recovery observation requires the awa version that wrote it")
	case b.ScanConfigHash.IsZero():
		return RecoveryObservation{}, fmt.Errorf("a recovery observation requires the scan-config policy identity it was observed under")
	case b.Source.IsZero():
		return RecoveryObservation{}, fmt.Errorf("a recovery observation requires the resolved source the restore was proven from")
	case b.Selection.IsZero():
		return RecoveryObservation{}, fmt.Errorf("a recovery observation requires the restore's normalized selection")
	case manifest.File == "":
		return RecoveryObservation{}, fmt.Errorf("a recovery observation requires a manifest payload")
	case manifest.RecordCount < 0:
		return RecoveryObservation{}, fmt.Errorf("a recovery observation cannot have %d manifest records", manifest.RecordCount)
	case manifest.RecordCount > 0 && manifest.TreeHash.IsZero():
		return RecoveryObservation{}, fmt.Errorf("a recovery observation with records requires a manifest tree hash")
	case scope.File == "":
		return RecoveryObservation{}, fmt.Errorf("a recovery observation requires a covered-scope payload")
	case scope.PathCount < 0:
		return RecoveryObservation{}, fmt.Errorf("a recovery observation cannot cover %d paths", scope.PathCount)
	case scope.PathCount < manifest.RecordCount:
		return RecoveryObservation{}, fmt.Errorf("a recovery observation covers %d path(s) but recorded %d manifest record(s): the covered scope must include every captured entry", scope.PathCount, manifest.RecordCount)
	case !contentComplete:
		return RecoveryObservation{}, fmt.Errorf("a recovery observation must capture complete verified content; apply refuses rather than publishing partial undo evidence")
	}
	return RecoveryObservation{
		id:              b.ID,
		createdAt:       b.CreatedAt.UTC(),
		awaVersion:      b.AwaVersion,
		scanConfigHash:  b.ScanConfigHash,
		source:          b.Source,
		selection:       b.Selection,
		manifest:        manifest,
		scope:           scope,
		contentComplete: contentComplete,
	}, nil
}

// ID returns the operation id this observation belongs to.
func (o RecoveryObservation) ID() OperationID { return o.id }

// CreatedAt returns when the observation was published, in UTC. It is the value
// ordinary age-based retention classifies against.
func (o RecoveryObservation) CreatedAt() time.Time { return o.createdAt }

// AwaVersion returns the awa version that wrote the record.
func (o RecoveryObservation) AwaVersion() string { return o.awaVersion }

// ScanConfigHash returns the scan-policy identity the observation was taken
// under, so a later comparison can prove policy compatibility rather than assume
// it.
func (o RecoveryObservation) ScanConfigHash() hashing.ConfigHash { return o.scanConfigHash }

// Source returns the immutable source the restore that created this observation
// was proven from.
func (o RecoveryObservation) Source() Source { return o.source }

// Selection returns that restore's normalized selection.
func (o RecoveryObservation) Selection() Selection { return o.selection }

// Manifest returns the durable pointer to the captured entries.
func (o RecoveryObservation) Manifest() ManifestRef { return o.manifest }

// Scope returns the durable pointer to the covered path set.
func (o RecoveryObservation) Scope() ScopeRef { return o.scope }

// ContentComplete reports that every captured regular entry has verified bytes
// in the blob store. It is always true for a published record; the accessor
// exists so output can state the fact rather than imply it.
func (o RecoveryObservation) ContentComplete() bool { return o.contentComplete }

// BeforeRef returns the state reference that names this observation.
func (o RecoveryObservation) BeforeRef() string { return o.id.BeforeRef() }

// IsZero reports whether the record was never built through a constructor.
func (o RecoveryObservation) IsZero() bool { return o.id.IsZero() }

// CoveredScopeCursor is a pull iterator over one recovery observation's covered
// paths in canonical (strictly ascending) order. It mirrors the manifest cursor
// contract deliberately: a covered scope can be as large as the restore that
// produced it, so it crosses every boundary as a stream and is merged against
// the manifest rather than held as a set.
//
// Next advances and reports whether a path is available, Path is valid only
// after Next returned true, Err is sticky and tells a clean end of stream from an
// error, and Close releases the underlying resource and is safe to call twice.
type CoveredScopeCursor interface {
	Next() bool
	Path() worktree.RelPath
	Err() error
	Close() error
}

// CoveredScopeStream opens fresh cursors over the same covered path set.
type CoveredScopeStream interface {
	Open(ctx context.Context) (CoveredScopeCursor, error)
}

// RecoveryRead is the constructor-owned result of opening a recovery
// observation for the state-reference resolver and the restore planner. It
// bundles the verified manifest stream with the covered-scope stream and the
// identity facts a consumer needs, so the policy identity always travels with the
// tree hash it interprets and the scope always travels with the manifest it
// bounds. Both payloads are streams: a recovery read never materializes either.
type RecoveryRead struct {
	record   RecoveryObservation
	manifest worktree.ManifestStream
	scope    CoveredScopeStream
}

// NewRecoveryRead builds a validated read. The payload counts are checked by the
// cursors at clean EOF (a truncated or padded payload fails there), so this
// constructor guards only what it can see: a real record and both streams.
func NewRecoveryRead(record RecoveryObservation, manifest worktree.ManifestStream, scope CoveredScopeStream) (RecoveryRead, error) {
	if record.IsZero() {
		return RecoveryRead{}, fmt.Errorf("a recovery read requires a validated record")
	}
	if manifest == nil {
		return RecoveryRead{}, fmt.Errorf("a recovery read requires a manifest stream")
	}
	if scope == nil {
		return RecoveryRead{}, fmt.Errorf("a recovery read requires a covered-scope stream")
	}
	return RecoveryRead{record: record, manifest: manifest, scope: scope}, nil
}

// Record returns the durable metadata.
func (r RecoveryRead) Record() RecoveryObservation { return r.record }

// Manifest returns the verified manifest stream over the captured entries.
func (r RecoveryRead) Manifest() worktree.ManifestStream { return r.manifest }

// Scope returns the covered-path stream that bounds what this observation
// proves. A path inside it but absent from the manifest was proved absent.
func (r RecoveryRead) Scope() CoveredScopeStream { return r.scope }

// RecoveryRepository stores and retrieves immutable pre-restore recovery
// observations. It is defined here, on the consumer side, and implemented by a
// filesystem adapter, so no os/filepath detail enters the application services
// that use it (restore, the state resolver, gc, doctor, and the timeline).
//
// The write contract is stream-first by design: Publish consumes a manifest
// record stream and a covered-path stream and derives the durable tree hash and
// counts from them, so no caller has to own a materialized observation.
type RecoveryRepository interface {
	// Publish writes the manifest, the covered scope, and then the metadata — the
	// metadata is the commit point — and publishes the record atomically. It
	// derives the manifest's tree hash and record count, and the covered path
	// count, from the streams it consumes, so no caller owns a materialized
	// observation. It returns ErrRecoveryIDCollision if the id already exists; an
	// immutable record is never silently replaced.
	Publish(ctx context.Context, build RecoveryBuild, manifest worktree.ManifestStream, scope CoveredScopeStream) (RecoveryObservation, error)
	// Get returns one record's validated metadata without reading its payloads,
	// or ErrRecoveryNotFound.
	Get(id OperationID) (RecoveryObservation, error)
	// Open returns the record plus verified streams over its payloads, or
	// ErrRecoveryNotFound.
	Open(ctx context.Context, id OperationID) (RecoveryRead, error)
	// ResolvePrefix returns the single id a short prefix identifies, or
	// ErrRecoveryNotFound / ErrRecoveryAmbiguousPrefix. It enumerates stored ids,
	// so it takes a context and honors cancellation mid-scan.
	ResolvePrefix(ctx context.Context, prefix string) (OperationID, error)
	// List returns every stored recovery observation's health finding, newest
	// first. Unlike a plain listing it does not collapse when one record is
	// unreadable: GC and doctor need to tell an absent store from a partially
	// corrupt one, and GC must fail closed on blob reachability when any record
	// cannot be read. It reads one record per entry, so it takes a context and
	// checks cancellation between records.
	List(ctx context.Context) ([]RecoveryFinding, error)
	// Delete removes a recovery observation. It is idempotent — removing an absent
	// id is not an error — so GC and a failed publish's rollback share one path.
	Delete(id OperationID) error
}

// RecoveryFinding is one entry of a recovery-store listing: either a readable
// record, or an unreadable one with the reason. It exists so a listing is honest
// about partial damage instead of failing whole or silently omitting records —
// GC treats any unreadable record as a reachability gap and blocks the blob
// sweep, which it can only do if the listing tells it.
type RecoveryFinding struct {
	// ID is the id parsed from the record's directory name. It is set even for an
	// unreadable record when the name itself parsed, so GC can name what blocked it.
	ID OperationID
	// Name is the raw directory name, set when the name did not parse as an id.
	Name string
	// Record is the validated metadata; it is zero when Err is set.
	Record RecoveryObservation
	// Err is the reason the record could not be read, or nil.
	Err error
}

// Readable reports whether the finding carries a usable record.
func (f RecoveryFinding) Readable() bool { return f.Err == nil && !f.Record.IsZero() }
