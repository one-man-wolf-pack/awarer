// Package runstore implements the runcache.Store port over the filesystem.
//
// A completed run is an immutable entry directory holding a versioned JSON
// metadata file and two output payload files, addressed by a time-sortable run
// id and reached for a cache hit through a key pointer file. Entries are
// published atomically — built in a temp directory and renamed into place, with
// the key pointer updated last — so an interrupted write can never be observed as
// a hit. Encoding and decoding go through the domain value-object constructors, so
// a round-trip re-validates every invariant and a corrupt or hand-edited file is
// rejected at decode rather than trusted.
package runstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/runcache"
)

// decodeStrict parses exactly one JSON document into v, rejecting any field the
// target struct does not declare (at the top level or nested) and any trailing
// data after the object. A persisted store record has an exact schema shape, so an
// unknown, typo'd, or future-version field is corruption to be surfaced, not
// silently dropped while the record is treated as the current schema.
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	// Require the stream to end exactly at the first document. A second Decode must
	// report io.EOF; anything else — a stray "}" or "]", a second object — is
	// trailing data that violates the one-document contract. dec.More() is not enough:
	// it answers "is there another array/object element", not "is the stream at EOF".
	var rest json.RawMessage
	if err := dec.Decode(&rest); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing data after JSON document")
		}
		return fmt.Errorf("unexpected trailing data after JSON document: %w", err)
	}
	return nil
}

// metaSchemaVersion is the run metadata document version this build reads and
// writes. There is exactly one: a document declaring any other version is
// incompatible (ErrIncompatibleEntry) and is never decoded through a second shape,
// migrated, or guessed. The store owns this boundary and names both versions in the
// error text, so a doctor or gc diagnostic can report which record is unreadable
// without any consumer restating a number that moves.
const metaSchemaVersion = 1

// pointerSchemaVersion is the key pointer document version, independent of the
// metadata version: the pointer is an index into the entries, not a copy of one.
// Reusability is a property of the target entry, enforced at the lookup read
// boundary, not asserted on the pointer where it could drift.
const pointerSchemaVersion = 1

// metaDoc is the run metadata document: the run's identity and timing, its exit and
// cache decision, its key input, its captured output, its typed reuse state, its
// mutation and effect-guard outcomes, and its pre/post observations. Each observation
// carries the scanner's scan_config_hash alongside its manifest ref, so a stored
// observation holds the complete scan policy identity that interprets its tree hash.
type metaDoc struct {
	SchemaVersion int             `json:"schema_version"`
	ID            string          `json:"id"`
	Key           string          `json:"key"`
	StartedAt     string          `json:"started_at"`
	FinishedAt    string          `json:"finished_at"`
	DurationMs    int64           `json:"duration_ms"`
	Exit          exitDoc         `json:"exit"`
	Decision      decisionDoc     `json:"cache_decision"`
	Skipped       skippedDoc      `json:"skipped"`
	KeyInput      keyInputDoc     `json:"key_input"`
	Stdout        outputDoc       `json:"stdout"`
	Stderr        outputDoc       `json:"stderr"`
	Reuse         reuseDoc        `json:"reuse"`
	Mutation      string          `json:"mutation"`
	EffectGuard   string          `json:"effect_guard"`
	Observations  observationsDoc `json:"observations"`
}

type reuseDoc struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason,omitempty"`
}

type observationsDoc struct {
	Before *observationDoc `json:"before"`
	After  *observationDoc `json:"after"`
}

// observationDoc is one persisted pre/post observation: the manifest ref that
// verifies the observed tree hash plus the scanner's scan_config_hash (the complete
// scan policy identity that interprets that tree hash). The two are one identity, so
// scan_config_hash is required — never omitempty.
type observationDoc struct {
	File           string `json:"file"`
	TreeHash       string `json:"tree_hash"`
	RecordCount    int    `json:"record_count"`
	ScanConfigHash string `json:"scan_config_hash"`
}

type exitDoc struct {
	Kind   string `json:"kind"`
	Code   int    `json:"code"`
	Signal string `json:"signal,omitempty"`
}

type decisionDoc struct {
	Cacheable bool   `json:"cacheable"`
	Reason    string `json:"reason,omitempty"`
}

type skippedDoc struct {
	Count   int             `json:"count"`
	Allowed bool            `json:"allowed"`
	Samples []skippedSample `json:"samples,omitempty"`
}

type skippedSample struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type commandDoc struct {
	Argv          []string `json:"argv"`
	RawExecutable string   `json:"raw_executable"`
	ResolvedPath  string   `json:"resolved_path,omitempty"`
	ResolvedStat  string   `json:"resolved_stat,omitempty"`
}

// envVarDoc is the persisted redacted identity of one allowlisted environment
// variable: its name, its presence class (unset/empty/set), and — only when set to
// a non-empty value — the value's identity fingerprint. It deliberately has no
// "value" field: a raw allowlisted value is often a secret, so it is never written
// to durable run metadata. The presence token plus the fingerprint carry everything
// the key and the diagnostics need without persisting the value.
type envVarDoc struct {
	Name     string `json:"name"`
	Presence string `json:"presence"`
	Identity string `json:"identity,omitempty"`
}

type platformDoc struct {
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
}

// effectDoc is the persisted effect-observation identity folded into the key
// document: the observation status, the bounded signature (present only when
// observed), and the number of watched roots covered.
type effectDoc struct {
	Status    string `json:"status"`
	Signature string `json:"signature,omitempty"`
	RootCount int    `json:"root_count"`
}

type keyInputDoc struct {
	CacheSchemaVersion int         `json:"cache_schema_version"`
	AwaVersion         string      `json:"awa_version"`
	InvocationMode     string      `json:"invocation_mode"`
	Command            commandDoc  `json:"command"`
	CWD                string      `json:"cwd"`
	InputTreeHash      string      `json:"input_tree_hash"`
	Effect             effectDoc   `json:"effect"`
	IncludeScope       []string    `json:"include_scope"`
	ExcludeScope       []string    `json:"exclude_scope"`
	UseGitignore       bool        `json:"use_gitignore"`
	UseAwaignore       bool        `json:"use_awaignore"`
	TrustMode          string      `json:"trust_mode"`
	RunConfigHash      string      `json:"run_config_hash"`
	Env                []envVarDoc `json:"env"`
	Platform           platformDoc `json:"platform"`
	StdinMode          string      `json:"stdin_mode"`
	TTYAllowed         bool        `json:"tty_allowed"`
	AllowSkippedInputs bool        `json:"allow_skipped_inputs"`
}

type outputDoc struct {
	OriginalBytes    int64  `json:"original_bytes"`
	StoredBytes      int64  `json:"stored_bytes"`
	Truncated        bool   `json:"truncated"`
	OmittedBytes     int64  `json:"omitted_bytes"`
	TruncationPolicy string `json:"truncation_policy"`
	Hash             string `json:"hash"`
	File             string `json:"file"`
}

// keyPointerDoc is the small file under keys/ that maps a run key to the run id
// that currently satisfies it.
type keyPointerDoc struct {
	SchemaVersion int    `json:"schema_version"`
	RunID         string `json:"run_id"`
}

// encodeMeta renders a run entry as current-schema JSON metadata bytes.
func encodeMeta(e runcache.RunEntry) ([]byte, error) {
	doc := metaDoc{
		SchemaVersion: metaSchemaVersion,
		ID:            e.ID.String(),
		Key:           e.Key.String(),
		StartedAt:     e.StartedAt.UTC().Format(time.RFC3339Nano),
		FinishedAt:    e.FinishedAt.UTC().Format(time.RFC3339Nano),
		DurationMs:    e.DurationMs(),
		Exit:          exitDoc{Kind: e.Exit.Kind.String(), Code: e.Exit.Code, Signal: e.Exit.Signal},
		Decision:      decisionDoc{Cacheable: e.Decision.Cacheable, Reason: e.Decision.Reason},
		Skipped:       encodeSkipped(e.Skipped),
		KeyInput:      encodeKeyInput(e.KeyInput),
		Stdout:        encodeOutput(e.Stdout),
		Stderr:        encodeOutput(e.Stderr),
		Reuse:         reuseDoc{Kind: e.Reuse.Kind().String(), Reason: e.Reuse.Reason().String()},
		Mutation:      e.Mutation.Outcome().String(),
		EffectGuard:   e.EffectGuard.Outcome().String(),
		Observations:  observationsDoc{Before: encodeObservation(e.Before), After: encodeObservation(e.After)},
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding run %s: %w", e.ID, err)
	}
	return append(out, '\n'), nil
}

// encodeObservation renders an observation's manifest ref and scan policy identity,
// or nil when the observation is absent (a post-scan-failed after).
func encodeObservation(o *runcache.Observation) *observationDoc {
	if o == nil {
		return nil
	}
	return &observationDoc{
		File:           o.Manifest.File,
		TreeHash:       o.Manifest.TreeHash.String(),
		RecordCount:    o.Manifest.RecordCount,
		ScanConfigHash: o.ScanConfigHash.String(),
	}
}

func encodeSkipped(s runcache.SkippedSummary) skippedDoc {
	d := skippedDoc{Count: s.Count, Allowed: s.Allowed}
	for _, sm := range s.Samples {
		d.Samples = append(d.Samples, skippedSample{Path: sm.Path, Reason: sm.Reason})
	}
	return d
}

func encodeKeyInput(in runcache.KeyInput) keyInputDoc {
	d := keyInputDoc{
		CacheSchemaVersion: in.CacheSchemaVersion,
		AwaVersion:         in.AwaVersion,
		InvocationMode:     in.InvocationMode,
		Command: commandDoc{
			Argv:          append([]string(nil), in.Command.Argv...),
			RawExecutable: in.Command.RawExecutable,
			ResolvedPath:  in.Command.ResolvedPath,
			ResolvedStat:  in.Command.ResolvedStat,
		},
		CWD:                in.CWD.String(),
		InputTreeHash:      in.InputTreeHash.String(),
		Effect:             effectDoc{Status: in.Effect.Status().String(), Signature: in.Effect.Signature().String(), RootCount: in.Effect.RootCount()},
		IncludeScope:       append([]string(nil), in.IncludeScope...),
		ExcludeScope:       append([]string(nil), in.ExcludeScope...),
		UseGitignore:       in.UseGitignore,
		UseAwaignore:       in.UseAwaignore,
		TrustMode:          in.TrustMode.String(),
		RunConfigHash:      in.RunConfigHash.String(),
		Platform:           platformDoc{GOOS: in.Platform.GOOS, GOARCH: in.Platform.GOARCH},
		StdinMode:          in.StdinMode.String(),
		TTYAllowed:         in.TTYAllowed,
		AllowSkippedInputs: in.AllowSkippedInputs,
	}
	for _, v := range in.Env.Vars() {
		d.Env = append(d.Env, envVarDoc{Name: v.Name(), Presence: v.Presence().String(), Identity: v.Identity().String()})
	}
	return d
}

func encodeOutput(c runcache.OutputCapture) outputDoc {
	return outputDoc{
		OriginalBytes:    c.OriginalBytes,
		StoredBytes:      c.StoredBytes,
		Truncated:        c.Truncated,
		OmittedBytes:     c.OmittedBytes,
		TruncationPolicy: c.TruncationPolicy.String(),
		Hash:             c.Hash.String(),
		File:             c.File,
	}
}

// decodeMeta parses a JSON metadata document back into a validated run entry,
// rebuilding every value object through its constructor.
//
// It draws the incompatible/corrupt line first and only once. A lenient probe reads
// the document's own schema_version: a readable version other than the current one is
// a format this build does not speak (ErrIncompatibleEntry) and is never routed to a
// second decoder, while data that is not even a schema-carrying JSON document is
// corruption — reported with the reason, since a truncated record and a wrong-shaped
// one need different fixes. Only a document claiming the current schema reaches the
// strict reader below, where an unknown field, a missing invariant, or a cross-field
// contradiction is corruption too.
func decodeMeta(data []byte) (runcache.RunEntry, error) {
	var probe struct {
		SchemaVersion *int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return runcache.RunEntry{}, fmt.Errorf("decoding run: %w", err)
	}
	if probe.SchemaVersion == nil {
		return runcache.RunEntry{}, fmt.Errorf("decoding run: no schema_version")
	}
	if *probe.SchemaVersion != metaSchemaVersion {
		return runcache.RunEntry{}, fmt.Errorf("%w: schema version %d, want %d",
			runcache.ErrIncompatibleEntry, *probe.SchemaVersion, metaSchemaVersion)
	}
	return decodeCurrentMeta(data)
}

// decodeCurrentMeta strictly decodes a current-schema record: its typed reuse state,
// mutation outcome, effect-guard outcome, and pre/post observations (each carrying
// the scanner's scan_config_hash alongside its manifest ref). Its env facts decode
// as redacted identities (no raw values).
func decodeCurrentMeta(data []byte) (runcache.RunEntry, error) {
	var doc metaDoc
	if err := decodeStrict(data, &doc); err != nil {
		return runcache.RunEntry{}, fmt.Errorf("decoding run: %w", err)
	}
	entry, err := entryFromDoc(doc)
	if err != nil {
		return runcache.RunEntry{}, err
	}
	reuse, err := decodeReuse(doc.Reuse)
	if err != nil {
		return runcache.RunEntry{}, err
	}
	outcome, err := runcache.ParseMutationOutcome(doc.Mutation)
	if err != nil {
		return runcache.RunEntry{}, err
	}
	mutation, err := runcache.NewMutationStatus(outcome)
	if err != nil {
		return runcache.RunEntry{}, err
	}
	guardOutcome, err := runcache.ParseEffectOutcome(doc.EffectGuard)
	if err != nil {
		return runcache.RunEntry{}, err
	}
	guard, err := runcache.NewEffectGuardStatus(guardOutcome)
	if err != nil {
		return runcache.RunEntry{}, err
	}
	before, err := decodeObservation(doc.Observations.Before)
	if err != nil {
		return runcache.RunEntry{}, fmt.Errorf("before observation: %w", err)
	}
	after, err := decodeObservation(doc.Observations.After)
	if err != nil {
		return runcache.RunEntry{}, fmt.Errorf("after observation: %w", err)
	}
	entry.Reuse = reuse
	entry.Mutation = mutation
	entry.EffectGuard = guard
	entry.Before = before
	entry.After = after
	// Every stored record is a real execution, so it always observed pre-run state and
	// — unless that post-run scan failed — post-run state too. RunEntry.Validate owns
	// the same invariant for the domain; this is the hostile-boundary restatement, which
	// names the offending run id and rejects a hand-edited document before the shared
	// value-object guard ever sees it.
	if entry.Before == nil {
		return runcache.RunEntry{}, fmt.Errorf("run %s has no before-observation", doc.ID)
	}
	if wantAfter := !entry.Mutation.ScanFailed(); wantAfter != (entry.After != nil) {
		if wantAfter {
			return runcache.RunEntry{}, fmt.Errorf("run %s has no after-observation but its post-run scan did not fail", doc.ID)
		}
		return runcache.RunEntry{}, fmt.Errorf("run %s carries an after-observation but its post-run scan failed", doc.ID)
	}
	if err := entry.Validate(); err != nil {
		return runcache.RunEntry{}, err
	}
	return entry, nil
}

// entryFromDoc rebuilds a run entry's identity, timing, exit, decision, key input,
// and captured output, leaving the reuse/mutation/observation fields for the caller
// to fill once they have been decoded and cross-checked.
func entryFromDoc(c metaDoc) (runcache.RunEntry, error) {
	id, err := runcache.ParseRunID(c.ID)
	if err != nil {
		return runcache.RunEntry{}, err
	}
	key, err := runcache.ParseRunKey(c.Key)
	if err != nil {
		return runcache.RunEntry{}, err
	}
	startedAt, err := time.Parse(time.RFC3339Nano, c.StartedAt)
	if err != nil {
		return runcache.RunEntry{}, fmt.Errorf("run %s: invalid started_at: %w", c.ID, err)
	}
	finishedAt, err := time.Parse(time.RFC3339Nano, c.FinishedAt)
	if err != nil {
		return runcache.RunEntry{}, fmt.Errorf("run %s: invalid finished_at: %w", c.ID, err)
	}
	// duration_ms is a derived field; it must agree with the started/finished span it
	// is computed from. A persisted value that disagrees is a hand-edited or corrupt
	// record, not something to silently recompute over.
	if want := finishedAt.Sub(startedAt).Milliseconds(); c.DurationMs != want {
		return runcache.RunEntry{}, fmt.Errorf("run %s: duration_ms %d does not match the started/finished span %d", c.ID, c.DurationMs, want)
	}
	exitKind, err := runcache.ParseExitKind(c.Exit.Kind)
	if err != nil {
		return runcache.RunEntry{}, err
	}
	keyInput, err := decodeKeyInput(c.KeyInput)
	if err != nil {
		return runcache.RunEntry{}, err
	}
	stdout, err := decodeOutput(c.Stdout)
	if err != nil {
		return runcache.RunEntry{}, err
	}
	stderr, err := decodeOutput(c.Stderr)
	if err != nil {
		return runcache.RunEntry{}, err
	}
	return runcache.RunEntry{
		ID:         id,
		Key:        key,
		KeyInput:   keyInput,
		StartedAt:  startedAt,
		FinishedAt: finishedAt,
		Exit:       runcache.ExitStatus{Kind: exitKind, Code: c.Exit.Code, Signal: c.Exit.Signal},
		Stdout:     stdout,
		Stderr:     stderr,
		Decision:   runcache.CacheDecision{Cacheable: c.Decision.Cacheable, Reason: c.Decision.Reason},
		Skipped:    decodeSkipped(c.Skipped),
	}, nil
}

// decodeReuse rebuilds the typed reuse state from its persisted kind/reason,
// rejecting a contradictory pair through the domain constructor.
func decodeReuse(d reuseDoc) (runcache.ReuseState, error) {
	kind, err := runcache.ParseReuseKind(d.Kind)
	if err != nil {
		return runcache.ReuseState{}, err
	}
	var reason runcache.MismatchReason
	if d.Reason != "" {
		reason, err = runcache.ParseMismatchReason(d.Reason)
		if err != nil {
			return runcache.ReuseState{}, err
		}
	}
	return runcache.NewReuseState(kind, reason)
}

// decodeObservation rebuilds an observation from its persisted manifest ref and
// scan policy identity, or nil when the observation is absent. The scan_config_hash
// is hostile input like every other persisted field: obs.Validate rejects a zero
// policy hash, so a forged or truncated observation cannot decode into a weakened
// partial identity.
func decodeObservation(d *observationDoc) (*runcache.Observation, error) {
	if d == nil {
		return nil, nil
	}
	treeHash, err := hashing.ParseTreeHash(d.TreeHash)
	if err != nil {
		return nil, err
	}
	scanConfig, err := hashing.ParseConfigHash(d.ScanConfigHash)
	if err != nil {
		return nil, fmt.Errorf("scan config hash: %w", err)
	}
	obs := &runcache.Observation{
		Manifest: runcache.ManifestRef{
			File:        d.File,
			TreeHash:    treeHash,
			RecordCount: d.RecordCount,
		},
		ScanConfigHash: scanConfig,
	}
	if err := obs.Validate(); err != nil {
		return nil, err
	}
	return obs, nil
}

func decodeSkipped(d skippedDoc) runcache.SkippedSummary {
	s := runcache.SkippedSummary{Count: d.Count, Allowed: d.Allowed}
	for _, sm := range d.Samples {
		s.Samples = append(s.Samples, runcache.SkippedSample{Path: sm.Path, Reason: sm.Reason})
	}
	return s
}

func decodeKeyInput(d keyInputDoc) (runcache.KeyInput, error) {
	cwd, err := runcache.NewExecutionCWD(d.CWD)
	if err != nil {
		return runcache.KeyInput{}, err
	}
	treeHash, err := hashing.ParseTreeHash(d.InputTreeHash)
	if err != nil {
		return runcache.KeyInput{}, err
	}
	trust, err := config.ParseTrustMode(d.TrustMode)
	if err != nil {
		return runcache.KeyInput{}, err
	}
	cfgHash, err := hashing.ParseConfigHash(d.RunConfigHash)
	if err != nil {
		return runcache.KeyInput{}, err
	}
	stdin, err := runcache.ParseStdinMode(d.StdinMode)
	if err != nil {
		return runcache.KeyInput{}, err
	}
	effect, err := decodeEffect(d.Effect)
	if err != nil {
		return runcache.KeyInput{}, err
	}
	vars := make([]runcache.EnvVar, 0, len(d.Env))
	for _, v := range d.Env {
		presence, err := runcache.ParseEnvPresence(v.Presence)
		if err != nil {
			return runcache.KeyInput{}, err
		}
		var identity runcache.EnvValueIdentity
		if v.Identity != "" {
			identity, err = runcache.ParseEnvValueIdentity(v.Identity)
			if err != nil {
				return runcache.KeyInput{}, err
			}
		}
		envVar, err := runcache.NewEnvVar(v.Name, presence, identity)
		if err != nil {
			return runcache.KeyInput{}, err
		}
		vars = append(vars, envVar)
	}
	return runcache.KeyInput{
		CacheSchemaVersion: d.CacheSchemaVersion,
		AwaVersion:         d.AwaVersion,
		InvocationMode:     d.InvocationMode,
		Command: runcache.Command{
			Argv:          append([]string(nil), d.Command.Argv...),
			RawExecutable: d.Command.RawExecutable,
			ResolvedPath:  d.Command.ResolvedPath,
			ResolvedStat:  d.Command.ResolvedStat,
		},
		CWD:                cwd,
		InputTreeHash:      treeHash,
		Effect:             effect,
		IncludeScope:       append([]string(nil), d.IncludeScope...),
		ExcludeScope:       append([]string(nil), d.ExcludeScope...),
		UseGitignore:       d.UseGitignore,
		UseAwaignore:       d.UseAwaignore,
		TrustMode:          trust,
		RunConfigHash:      cfgHash,
		Env:                runcache.NewEnvironment(vars),
		Platform:           runcache.Platform{GOOS: d.Platform.GOOS, GOARCH: d.Platform.GOARCH},
		StdinMode:          stdin,
		TTYAllowed:         d.TTYAllowed,
		AllowSkippedInputs: d.AllowSkippedInputs,
	}, nil
}

// decodeEffect rebuilds the effect observation from its persisted fields through
// the domain constructor, so a hand-edited status/signature pairing is rejected. A
// current record always carries an effect object (present since v3); a missing or
// empty status fails ParseEffectStatus, so a doc without it cannot decode.
func decodeEffect(d effectDoc) (runcache.EffectObservation, error) {
	status, err := runcache.ParseEffectStatus(d.Status)
	if err != nil {
		return runcache.EffectObservation{}, err
	}
	var sig runcache.EffectHash
	if d.Signature != "" {
		sig, err = runcache.ParseEffectHash(d.Signature)
		if err != nil {
			return runcache.EffectObservation{}, err
		}
	}
	return runcache.NewEffectObservation(status, sig, d.RootCount)
}

func decodeOutput(d outputDoc) (runcache.OutputCapture, error) {
	policy, err := runcache.ParseTruncationPolicy(d.TruncationPolicy)
	if err != nil {
		return runcache.OutputCapture{}, err
	}
	hash, err := hashing.ParseContentHash(d.Hash)
	if err != nil {
		return runcache.OutputCapture{}, err
	}
	return runcache.OutputCapture{
		OriginalBytes:    d.OriginalBytes,
		StoredBytes:      d.StoredBytes,
		Truncated:        d.Truncated,
		OmittedBytes:     d.OmittedBytes,
		TruncationPolicy: policy,
		Hash:             hash,
		File:             d.File,
	}, nil
}

// encodeKeyPointer renders a key pointer file.
func encodeKeyPointer(id runcache.RunID) ([]byte, error) {
	out, err := json.MarshalIndent(keyPointerDoc{SchemaVersion: pointerSchemaVersion, RunID: id.String()}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// decodeKeyPointer parses a key pointer file into the run id it references.
func decodeKeyPointer(data []byte) (runcache.RunID, error) {
	var doc keyPointerDoc
	if err := decodeStrict(data, &doc); err != nil {
		return runcache.RunID{}, fmt.Errorf("decoding key pointer: %w", err)
	}
	if doc.SchemaVersion != pointerSchemaVersion {
		return runcache.RunID{}, fmt.Errorf("key pointer schema version %d is not supported (want %d)", doc.SchemaVersion, pointerSchemaVersion)
	}
	return runcache.ParseRunID(doc.RunID)
}
