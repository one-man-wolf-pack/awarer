// Package status implements the durable-facts half of the "awa status" use case:
// where the project is, how many checkpoints exist, the latest checkpoint, and the
// run-cache counts. This is the JSON contract and the
// metadata-only core — it invents no state it cannot honestly observe.
//
// Honesty is the point of this package. A failed durable read is never collapsed into
// "no data exists": every subsystem reports a stable machine `state` token, and any
// degraded/incompatible/corrupt/unreadable/permission condition is also recorded as a
// structured evidence.Degradation (component + token + detail) so an agent never has
// to infer trouble from prose. "Empty" is reserved for a store that was read
// successfully enough to know nothing exists.
//
// Boundedness is the other point. status is the recommended first command, so it must
// stay cheap enough to be habitual: it reads checkpoint headers (header-only) and a
// bounded newest sample of runs, but it does NOT walk the blob store or compute a
// whole-store byte footprint — that deep historical accounting moved to the explicit
// `awa doctor` / `awa gc --dry-run` diagnostics. The blob footprint is reported as
// intentionally unavailable with a pointer to those commands.
//
// Human "awa status" is a review dashboard: on top of these durable facts the CLI
// adds the working-tree dirty summary, live git state, which recent runs are
// reusable, and the commands to run next. That review layer is assembled at the CLI
// edge and is not part of this package's JSON Result.
package status

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/config"
	"awarer/internal/domain/evidence"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/runcache"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/checkpointjson"
	"awarer/internal/infra/projfs"
	"awarer/internal/infra/runstore"
)

// statusRunSampleSize bounds how many of the newest run entries status reads to
// report corrupt/unhealthy counts and the latest run. Status is a cheap diagnostic,
// not doctor: it never verifies every payload in a large run cache.
const statusRunSampleSize = 16

// The two non-degraded state tokens. "healthy" is the one non-degraded, non-empty value,
// for which no evidence token exists because a healthy read produces no degradation;
// "store-empty" is the honestly-empty value. Every degraded state token is derived from its
// evidence.Degradation (see subsystemVerdict), never spelled out here — that is what keeps
// the `state` field and the `degradations` array from drifting.
const (
	stateHealthy    = "healthy"
	stateStoreEmpty = string(evidence.TokenStoreEmpty)
)

// subsystemVerdict couples a subsystem's read-health state token with the structured
// degradation(s) that a degraded state must carry. It is the single typed value that both
// the checkpoint and run-cache builders resolve to, so the two facts a renderer must keep
// in sync — the `state` token and the `degradations` array — are produced together rather
// than by two parallel code paths. Its constructors make the two invalid pairings
// unrepresentable: a degraded state always carries at least one matching degradation, and
// a healthy/empty state carries none. JSON-edge tests pin this invariant as a backstop.
type subsystemVerdict struct {
	state string
	// degradations is empty for a healthy or honestly-empty store, and holds one or more
	// matching-component facts for every degraded state.
	degradations []evidence.Degradation
}

// healthyVerdict is the one non-degraded, non-empty verdict: every record read cleanly, so
// there is no degradation to carry.
func healthyVerdict() subsystemVerdict { return subsystemVerdict{state: stateHealthy} }

// emptyVerdict is the honestly-empty verdict: the store was read successfully and holds
// nothing, so it is non-degraded and carries no degradation.
func emptyVerdict() subsystemVerdict { return subsystemVerdict{state: stateStoreEmpty} }

// degradedVerdict derives the state token from the degradation itself, so the two can never
// disagree: the state IS the degradation's token. It is the common single-fact degraded
// case (store-missing, an access error, a whole-store read failure, a checkpoint store's
// partial/incompatible/corrupt roll-up).
func degradedVerdict(d evidence.Degradation) subsystemVerdict {
	return subsystemVerdict{state: d.Token().String(), degradations: []evidence.Degradation{d}}
}

// degradedAggregate is the degraded verdict whose state token is a derived roll-up distinct
// from the individual degradation tokens — the run cache's bounded/partial sample, where
// the subsystem verdict (read-bounded / read-partial) summarizes per-entry facts that carry
// their own tokens (metadata-corrupt, ...). It still requires at least one degradation, so a
// degraded state without a matching fact stays unrepresentable; an empty set is a
// programming error.
func degradedAggregate(state string, degs []evidence.Degradation) subsystemVerdict {
	if len(degs) == 0 {
		panic("status: degraded aggregate verdict without a matching degradation")
	}
	return subsystemVerdict{state: state, degradations: degs}
}

// Subsystem reports the state of a store subsystem. State is the derived read health
// as a stable token, so a consumer never mistakes an unreadable store for an empty
// one. Recorded is the number of records that read back cleanly; Unreadable and
// Incompatible break down the rest. Populated is whether any readable records exist — but
// it is advisory: State is authoritative, and a corrupt/incompatible/unreadable store has
// State != store-empty even when Recorded is 0. Latest is the canonical latest
// checkpoint (the one a "latest" reference resolves to), populated only when the whole
// store is readable; on a degraded store the canonical latest is unresolvable, so it
// is omitted rather than reporting a newest-readable a follow-up "latest..now" would
// reject. Latest applies only to the checkpoints subsystem.
type Subsystem struct {
	State        string          `json:"state"`
	Recorded     int             `json:"recorded"`
	Populated    bool            `json:"populated"`
	Unreadable   int             `json:"unreadable,omitempty"`
	Incompatible int             `json:"incompatible,omitempty"`
	Latest       *CheckpointInfo `json:"latest,omitempty"`
}

// Degraded reports whether the subsystem is in a read-troubled state. Only healthy and
// an honestly-empty store are non-degraded; everything else — including store-missing,
// which for an initialized project means a subsystem directory that should exist but
// does not — is degraded, so an unreadable or missing store is never presented as
// "none yet". Pre-init absence of the whole .awa root is modeled outside Subsystem, by
// project initialization / root resolution, not by this state.
func (s Subsystem) Degraded() bool {
	switch s.State {
	case stateHealthy, stateStoreEmpty:
		return false
	default:
		return true
	}
}

// CheckpointInfo summarizes the latest checkpoint for status.
type CheckpointInfo struct {
	ID        string `json:"id"`
	ShortID   string `json:"short_id"`
	CreatedAt string `json:"created_at"`
	Skipped   int    `json:"skipped"`
}

// StoreFootprint reports whether an exact blob-store footprint (count and bytes) is
// available in this dashboard. It is deliberately unavailable in ordinary status:
// computing it walks the entire blob store, which scales with total store size, so it
// was moved to the explicit `awa doctor` / `awa gc --dry-run` diagnostics. Reason is a
// stable token (currently always "bounded") explaining why status omits it, so an
// agent reads a structured fact rather than assuming the store is empty.
type StoreFootprint struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// Store reports the on-disk store location and the bounded footprint fact.
type Store struct {
	Path      string         `json:"path"`
	Footprint StoreFootprint `json:"footprint"`
}

// DegradationView is the wire projection of an evidence.Degradation: the affected
// component, its stable token, an optional bounded sample, and the human detail. The
// domain value object enforces that these are well-formed; this struct only serializes
// them. It replaces the old prose Warnings []string so JSON carries structured facts.
type DegradationView struct {
	Component string            `json:"component"`
	Token     string            `json:"token"`
	Detail    string            `json:"detail,omitempty"`
	Sample    *BoundedCountView `json:"sample,omitempty"`
}

// BoundedCountView is the wire projection of an evidence.BoundedCount.
type BoundedCountView struct {
	Shown    int  `json:"shown"`
	Total    *int `json:"total,omitempty"`
	Complete bool `json:"complete"`
	Limit    int  `json:"limit,omitempty"`
}

// Advisory reports whether this degradation is a calm advisory (a "note") rather than
// a correctness warning, so the human renderer picks the right diagnostic prefix
// without re-deriving the classification from the token string.
func (d DegradationView) Advisory() bool {
	return evidence.DiagnosticToken(d.Token).Advisory()
}

func degradationView(d evidence.Degradation) DegradationView {
	v := DegradationView{
		Component: d.Component().String(),
		Token:     d.Token().String(),
		Detail:    d.Detail(),
	}
	if s, ok := d.Sample(); ok {
		bc := BoundedCountView{Shown: s.Shown(), Complete: s.Complete(), Limit: s.Limit()}
		if total, known := s.Total(); known {
			t := total
			bc.Total = &t
		}
		v.Sample = &bc
	}
	return v
}

// RunCacheStatus reports the run-cache state. Recorded counts every stored run from
// the directory refs; the sample fields report what a bounded scan of the newest
// entries found, so status stays fast on a large cache. State is the authoritative
// token, so a whole-store read failure (permission/io/corrupt) is never presented as
// an empty cache. Corrupt/incompatible/unhealthy counts are diagnostics, not repairs.
type RunCacheStatus struct {
	State                string   `json:"state"`
	Recorded             int      `json:"recorded"`
	Populated            bool     `json:"populated"`
	SampleSize           int      `json:"sample_size"`
	CorruptInSample      int      `json:"corrupt_in_sample"`
	IncompatibleInSample int      `json:"incompatible_in_sample"`
	UnhealthyInSample    int      `json:"unhealthy_in_sample"`
	Latest               *RunInfo `json:"latest,omitempty"`
}

// Degraded reports whether the run cache is in a read-troubled state — anything other
// than healthy or an honestly-empty store. Renderers use it so an unreadable, missing,
// or permission-denied run store is never presented as "none yet". It mirrors
// Subsystem.Degraded so both subsystems render degradation the same way.
func (r RunCacheStatus) Degraded() bool {
	switch r.State {
	case stateHealthy, stateStoreEmpty:
		return false
	default:
		return true
	}
}

// RunInfo summarizes the newest readable run for status.
type RunInfo struct {
	ID        string   `json:"id"`
	ShortID   string   `json:"short_id"`
	Command   []string `json:"command"`
	ExitCode  int      `json:"exit_code"`
	StartedAt string   `json:"started_at"`
	Healthy   bool     `json:"healthy"`
}

// Result is the status of a project.
type Result struct {
	Root string `json:"root"`
	// ConfigPath names the config file to look at: the highest-precedence present
	// layer (local over shared), or the conventional local location when none exists.
	// ConfigPresent reports whether any layer exists. ConfigLayers is the full,
	// ordered breakdown (shared then local) with each layer's path and existence, so
	// a machine consumer never has to infer the layered config from the singular
	// fields. An absent config is valid — the project runs on the built-in defaults.
	ConfigPath    string            `json:"config_path"`
	ConfigPresent bool              `json:"config_present"`
	ConfigLayers  []ConfigLayerFact `json:"config_layers"`
	Initialized   bool              `json:"initialized"`
	Checkpoints   Subsystem         `json:"checkpoints"`
	RunCache      RunCacheStatus    `json:"run_cache"`
	Store         Store             `json:"store"`
	// Degradations carries every structured durable-read fact (corrupt/incompatible/
	// unreadable/permission/partial) across subsystems. It is the machine-authoritative
	// signal a human warning line is projected from, so the two never drift.
	Degradations []DegradationView `json:"degradations"`
}

// ConfigLayerFact is the JSON projection of one config layer's provenance for the
// status Result: the layer token, its path, and whether the file exists. It mirrors
// the domain config.LayerFact through a stable machine shape status owns.
type ConfigLayerFact struct {
	Layer  string `json:"layer"`
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
}

// Degraded reports whether any subsystem is in a read-troubled state, so the review
// dashboard and human renderer can flag it once.
func (r Result) Degraded() bool {
	return len(r.Degradations) > 0
}

// Degradation returns the first structured degradation for a component, so a renderer
// can source one message from the same fact the JSON carries rather than re-deriving
// it. ok is false when the component has no degradation.
func (r Result) Degradation(component string) (DegradationView, bool) {
	for _, d := range r.Degradations {
		if d.Component == component {
			return d, true
		}
	}
	return DegradationView{}, false
}

// Run reports the status of an already-resolved project. It loads the project's config
// to validate it (a corrupt config surfaces here) and reads the durable checkpoint,
// name, and run state to report real counts and stable state tokens. Reading is
// best-effort per subsystem: a problem reading one becomes a structured degradation
// (and a distinct state) rather than failing the whole status, since status is the
// command a user runs to diagnose trouble.
//
// ctx is the root cancellable context. Status is the fast bounded dashboard, so
// cancellation is checked at the boundaries between subsystems AND threaded into the
// store reads that scale with local history: the checkpoint header walk (StoreHealth)
// and the run enumeration (CountRefs/ListRefsNewest) honor ctx in their per-record loops,
// so a Ctrl+C during one long read returns promptly rather than finishing it. The tiny
// fixed newest-sample decode loop stays ceremony-free; its cost is capped at
// statusRunSampleSize regardless.
func Run(ctx context.Context, p projfs.Project, resolved config.ResolvedConfig) (Result, error) {
	layout, err := p.Paths()
	if err != nil {
		return Result{}, err
	}

	// The immutable resolved config — effective config and per-layer facts — is
	// composed once at the CLI boundary and passed in, so the dashboard and these
	// durable facts describe the same config, and the layer facts came from the same
	// read as the config rather than a separate stat that could disagree under a
	// concurrent edit. Derive presence and an honest primary path from the facts: with
	// a shared-only config, config_path names the shared awa.toml that exists, not the
	// absent local.
	configPresent := false
	configPath := layout.ConfigFile()
	var layerFacts []ConfigLayerFact
	for _, l := range resolved.Layers() {
		layerFacts = append(layerFacts, ConfigLayerFact{Layer: l.Layer().String(), Path: l.Path(), Exists: l.Exists()})
		if l.Exists() {
			configPresent = true
			configPath = l.Path() // higher-precedence present layer wins (local over shared)
		}
	}

	var degradations []evidence.Degradation

	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	checkpointSub, cpDeg := checkpointSubsystem(ctx, layout)
	degradations = append(degradations, cpDeg...)

	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	runCache, runDeg := runCacheStatus(ctx, layout)
	degradations = append(degradations, runDeg...)

	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	// The remaining store subdirectories (blobs, tmp, indexes, locks, logs) are not
	// summarized as subsystems, but a missing or non-directory one is still a fact an
	// agent should see rather than infer, so it becomes a store-level degradation.
	degradations = append(degradations, layoutDegradations(layout)...)

	views := make([]DegradationView, 0, len(degradations))
	for _, d := range degradations {
		views = append(views, degradationView(d))
	}

	return Result{
		Root:          layout.Root(),
		ConfigPath:    configPath,
		ConfigPresent: configPresent,
		ConfigLayers:  layerFacts,
		Initialized:   true,
		Checkpoints:   checkpointSub,
		RunCache:      runCache,
		Store: Store{
			Path:      layout.StoreDir(),
			Footprint: StoreFootprint{Available: false, Reason: "bounded"},
		},
		Degradations: views,
	}, nil
}

// checkpointSubsystem reads the checkpoint store's health (header-only, so its cost
// does not grow with manifest sizes), deriving a stable state token and any
// structured degradations. A structural/permission/io read failure is classified
// into a distinct token rather than collapsed to "corrupt" or "empty", so an access
// problem is never presented as data damage or as an empty store.
func checkpointSubsystem(ctx context.Context, layout paths.Layout) (Subsystem, []evidence.Degradation) {
	var sub Subsystem
	// apply couples the subsystem's state token and its degradation(s) from one typed
	// verdict, so a degraded state can never be returned without its matching fact.
	apply := func(v subsystemVerdict) (Subsystem, []evidence.Degradation) {
		sub.State = v.state
		return sub, v.degradations
	}

	// Distinguish a never-created store directory from a present-but-empty one: absent
	// is store-missing (an anomaly for an initialized project), present-and-empty is the
	// honest store-empty. Without this, listIDs treats absent as empty and the two
	// collapse.
	if ok, derr := projfs.DirExists(layout.CheckpointsDir()); derr == nil && !ok {
		return apply(degradedVerdict(mustDegrade(evidence.ComponentCheckpoints, evidence.TokenStoreMissing,
			"checkpoint store directory is missing; run awa doctor to inspect, or "+paths.ResetEvidenceHint())))
	}

	health, err := checkpointjson.NewRepo(layout).StoreHealth(ctx)
	if err != nil {
		token, detail := classifyStoreReadError(err, "checkpoint store")
		return apply(degradedVerdict(mustDegrade(evidence.ComponentCheckpoints, token, detail)))
	}

	sub.Recorded = health.Recorded()
	sub.Populated = health.Recorded() > 0
	sub.Unreadable = health.Unreadable()
	sub.Incompatible = health.Incompatible()

	if health.AnyUnreadable() {
		// A degraded store has no resolvable canonical latest, so it is left omitted (a
		// newest-readable would hand an agent a reference "latest..now" rejects); the
		// degradation carries the reason, and the state token is derived from it.
		token := checkpointDegradationToken(health.State())
		return apply(degradedVerdict(mustDegrade(evidence.ComponentCheckpoints, token, checkpointDegradedDetail(health))))
	}
	if health.State() == checkpoint.StoreEmpty {
		return apply(emptyVerdict())
	}
	if latest, ok := health.Latest(); ok {
		sub.Latest = &CheckpointInfo{
			ID:        latest.ID.String(),
			ShortID:   latest.ID.Short(),
			CreatedAt: latest.CreatedAt.UTC().Format(time.RFC3339Nano),
			Skipped:   latest.Stats.Skipped,
		}
	}
	return apply(healthyVerdict())
}

// checkpointDegradationToken picks the evidence token for a degraded checkpoint store:
// partial and incompatible map to their own tokens, everything else to metadata-corrupt.
func checkpointDegradationToken(s checkpoint.StoreState) evidence.DiagnosticToken {
	switch s {
	case checkpoint.StorePartial:
		return evidence.TokenReadPartial
	case checkpoint.StoreIncompatible:
		return evidence.TokenMetadataIncompatible
	default:
		return evidence.TokenMetadataCorrupt
	}
}

// checkpointDegradedDetail renders the human detail for a read-troubled checkpoint
// store, breaking down incompatible vs corrupt when per-record counts are known, and
// carrying the recovery hint. It is the single author of this message.
func checkpointDegradedDetail(h checkpoint.CheckpointStoreHealth) string {
	if h.Unreadable() == 0 {
		return "checkpoint store could not be read; " + paths.ResetEvidenceHint()
	}
	parts := make([]string, 0, 2)
	if obs := h.Incompatible(); obs > 0 {
		parts = append(parts, fmt.Sprintf("%d incompatible", obs))
	}
	if corrupt := h.Unreadable() - h.Incompatible(); corrupt > 0 {
		parts = append(parts, fmt.Sprintf("%d corrupt", corrupt))
	}
	return "checkpoint store is partially readable: " + strings.Join(parts, ", ") +
		"; " + paths.ResetEvidenceHint()
}

// runCacheStatus reports the run-cache state by reading the run store. It counts every
// stored run from the directory refs (cheap, no decode), then reads a bounded sample
// of the newest entries to report corrupt metadata, incompatible entries, unhealthy
// payloads, and the latest readable run. A whole-store read failure is classified into
// a distinct state token and a structured degradation, never an empty cache.
func runCacheStatus(ctx context.Context, layout paths.Layout) (RunCacheStatus, []evidence.Degradation) {
	// Distinguish an absent run store directory from a present-but-empty one.
	if ok, derr := projfs.DirExists(layout.RunsDir()); derr == nil && !ok {
		return applyRunVerdict(RunCacheStatus{}, degradedVerdict(mustDegrade(evidence.ComponentRuns, evidence.TokenStoreMissing,
			"run store directory is missing; run awa doctor to inspect, or "+paths.ResetEvidenceHint())))
	}

	repo := runstore.New(layout, blake3hash.New())

	// The total is folded from the store layout in O(1) memory (no id list retained);
	// the sample is the newest statusRunSampleSize ids. status never materializes or
	// sorts the whole ref set, so it stays cheap on a large run cache.
	total, err := repo.CountRefs(ctx)
	if err != nil {
		return applyRunVerdict(RunCacheStatus{}, runReadFailure(err))
	}
	st := RunCacheStatus{Recorded: total, Populated: total > 0}
	if total == 0 {
		return applyRunVerdict(st, emptyVerdict())
	}

	refs, err := repo.ListRefsNewest(ctx, statusRunSampleSize)
	if err != nil {
		// Preserve the counts already known (Recorded/Populated) alongside the failure.
		return applyRunVerdict(st, runReadFailure(err))
	}
	st.SampleSize = len(refs)

	var degradations []evidence.Degradation
	for _, id := range refs {
		entry, err := repo.Get(id)
		if err != nil {
			switch {
			case errors.Is(err, runcache.ErrCorruptStore):
				st.CorruptInSample++
			case errors.Is(err, runcache.ErrNotFound):
				// Raced with a delete; not corruption.
			case errors.Is(err, runcache.ErrIncompatibleEntry):
				// A schema this build cannot read: never reusable, but a real fact — count it
				// as incompatible rather than silently skipping it, so a machine consumer does
				// not read the store as smaller or healthier than it is.
				st.IncompatibleInSample++
			default:
				token, detail := classifyStoreReadError(err, "run "+id.Short())
				degradations = append(degradations, mustDegrade(evidence.ComponentRuns, token, detail))
			}
			continue
		}
		healthy := payloadsHealthy(repo, entry)
		if !healthy {
			st.UnhealthyInSample++
		}
		if st.Latest == nil {
			st.Latest = &RunInfo{
				ID:        entry.ID.String(),
				ShortID:   entry.ID.Short(),
				Command:   entry.KeyInput.Command.Argv,
				ExitCode:  entry.Exit.Code,
				StartedAt: entry.StartedAt.UTC().Format(time.RFC3339Nano),
				Healthy:   healthy,
			}
		}
	}

	return applyRunVerdict(st, runCacheVerdict(st, degradations))
}

// applyRunVerdict couples the run-cache state token with its degradation(s) from one typed
// verdict, keeping the counts already gathered on st. It is the single place the run-cache
// `state` token is written, so a degraded state can never be returned without its matching
// structured fact.
func applyRunVerdict(st RunCacheStatus, v subsystemVerdict) (RunCacheStatus, []evidence.Degradation) {
	st.State = v.state
	return st, v.degradations
}

// runCacheState derives the run cache's authoritative state token from the bounded
// sample and the per-entry read facts already gathered. priorDegradations holds any
// per-entry read failures the sample surfaced; their presence is itself sample trouble.
//
// The verdict is honest about what was actually verified. status checks only the newest
// statusRunSampleSize entries, so:
//   - a clean sample of a cache no larger than the sample is healthy;
//   - a clean sample of a LARGER cache is read-bounded, not healthy — the cache is
//     available and the sample sound, but the rest is unverified, so it carries a
//     bounded-count fact and points at doctor for an exhaustive check rather than
//     overclaiming full health;
//   - a sample with corrupt/incompatible/unhealthy/unreadable entries is read-partial, or
//     metadata-incompatible only when nothing readable remains and every failure is a
//     an incompatible schema (mirroring the checkpoint store's verdict).
func runCacheVerdict(st RunCacheStatus, priorDegradations []evidence.Degradation) subsystemVerdict {
	troubled := st.CorruptInSample + st.IncompatibleInSample + st.UnhealthyInSample + len(priorDegradations)
	if troubled == 0 {
		if st.Recorded <= st.SampleSize {
			return healthyVerdict()
		}
		bc := evidence.NewBoundedCount(st.SampleSize, st.Recorded, statusRunSampleSize)
		detail := fmt.Sprintf("run cache health checked the newest %d of %d run(s); run awa doctor for an exhaustive check",
			st.SampleSize, st.Recorded)
		deg, ok := evidence.NewBoundedDegradation(evidence.ComponentRuns, bc, detail)
		if !ok {
			panic("status: invalid run-cache bounded degradation")
		}
		// read-bounded's state token is exactly the bounded degradation's token, so derive
		// one from the other rather than pairing them by hand.
		return degradedVerdict(deg)
	}
	token := evidence.TokenReadPartial
	// Incompatible (not partial) only when no readable run remains, every counted failure is
	// a an incompatible schema, and no other per-entry read error occurred — so a
	// mostly-healthy or mixed-failure cache is never mislabeled as incompatible.
	if st.CorruptInSample == 0 && st.UnhealthyInSample == 0 && st.Latest == nil && len(priorDegradations) == 0 {
		token = evidence.TokenMetadataIncompatible
	}
	// Add one aggregate degradation for the counted sample trouble. When the only trouble
	// was a per-entry read error, priorDegradations already carries the detail, so no
	// empty aggregate line is emitted — but troubled>0 guarantees priorDegradations is
	// non-empty there, so the degraded verdict always carries a matching fact.
	parts := make([]string, 0, 3)
	if st.CorruptInSample > 0 {
		parts = append(parts, fmt.Sprintf("%d corrupt", st.CorruptInSample))
	}
	if st.IncompatibleInSample > 0 {
		parts = append(parts, fmt.Sprintf("%d incompatible", st.IncompatibleInSample))
	}
	if st.UnhealthyInSample > 0 {
		parts = append(parts, fmt.Sprintf("%d unhealthy payload(s)", st.UnhealthyInSample))
	}
	if len(parts) == 0 {
		// The aggregate state token (read-partial) is a roll-up distinct from the per-entry
		// degradation tokens priorDegradations already carries.
		return degradedAggregate(string(token), priorDegradations)
	}
	detail := fmt.Sprintf("run cache newest %d sample: %s", st.SampleSize, strings.Join(parts, ", "))
	return degradedAggregate(string(token), append(priorDegradations, mustDegrade(evidence.ComponentRuns, token, detail)))
}

// layoutDegradations reports required store subdirectories that are missing or not a
// directory as structured facts. It skips checkpoints and runs, which their own
// subsystem builders already classify, and maps each remaining dir to its component
// (blobs/index/locks) or, lacking one, to the store as a whole.
func layoutDegradations(layout paths.Layout) []evidence.Degradation {
	component := func(path string) evidence.Component {
		switch path {
		case layout.BlobsDir():
			return evidence.ComponentBlobs
		case layout.IndexesDir():
			return evidence.ComponentIndex
		case layout.LocksDir():
			return evidence.ComponentLocks
		default:
			return evidence.ComponentStore
		}
	}
	skip := map[string]bool{layout.CheckpointsDir(): true, layout.RunsDir(): true}
	var out []evidence.Degradation
	for _, path := range layout.RequiredDirs() {
		if skip[path] {
			continue
		}
		comp := component(path)
		ok, err := projfs.DirExists(path)
		switch {
		case errors.Is(err, projfs.ErrNotDirectory):
			out = append(out, mustDegrade(comp, evidence.TokenMetadataCorrupt, "not a directory: "+path))
		case err != nil:
			token, detail := classifyStoreReadError(err, path)
			out = append(out, mustDegrade(comp, token, detail))
		case !ok:
			out = append(out, mustDegrade(comp, evidence.TokenStoreMissing, "missing directory: "+path))
		}
	}
	return out
}

// classifyStoreReadError maps a store read failure to a stable token and a human
// detail, distinguishing an access problem (permission) and a generic I/O failure from
// genuine metadata corruption. subject names the store for the detail line.
func classifyStoreReadError(err error, subject string) (evidence.DiagnosticToken, string) {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return evidence.TokenPermissionDenied, "cannot read " + subject + " (permission denied): " + err.Error()
	case errors.Is(err, checkpoint.ErrCorruptStore), errors.Is(err, runcache.ErrCorruptStore):
		return evidence.TokenMetadataCorrupt, "cannot read " + subject + " (corrupt): " + err.Error()
	default:
		return evidence.TokenIOError, "cannot read " + subject + ": " + err.Error()
	}
}

// runReadFailure builds the degraded verdict for a run-store read that failed, classifying
// the error into a stable token so an access or I/O problem is never presented as an empty
// cache. The caller applies it over whatever counts it already knew (Recorded/Populated
// survive a mid-scan failure).
func runReadFailure(err error) subsystemVerdict {
	token, detail := classifyStoreReadError(err, "run cache")
	return degradedVerdict(mustDegrade(evidence.ComponentRuns, token, detail))
}

// mustDegrade builds a non-bounded degradation, panicking only on a programming error
// (an invalid component/token constant), which is unreachable with the package's own
// constants. It keeps call sites terse without dropping the ok check.
func mustDegrade(component evidence.Component, token evidence.DiagnosticToken, detail string) evidence.Degradation {
	d, ok := evidence.NewDegradation(component, token, detail)
	if !ok {
		panic(fmt.Sprintf("status: invalid degradation (%s, %s)", component, token))
	}
	return d
}

// payloadsHealthy reports whether both of an entry's stored payloads verify, by
// opening each through the store (which verifies against the recorded hash) and
// closing it. It never repairs — that is doctor's job.
func payloadsHealthy(repo *runstore.Repo, entry runcache.RunEntry) bool {
	so, err := repo.OpenStdout(entry.ID)
	if err != nil {
		return false
	}
	_ = so.Close()
	se, err := repo.OpenStderr(entry.ID)
	if err != nil {
		return false
	}
	_ = se.Close()
	return true
}
