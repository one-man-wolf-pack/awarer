// Package doctor defines the domain model for the "awa doctor" diagnostic: the
// closed vocabulary of severities, health states, finding codes, subsystems, the
// Finding value that records one diagnosis, the typed RepairAction that records
// one safe mutation, and the DoctorResult that aggregates them.
//
// The package is pure: it performs no I/O and knows nothing about the filesystem,
// SQLite, or git. Its job is to make invalid diagnoses unrepresentable — a finding
// cannot carry an unknown code or an impossible repaired/non-repairable
// combination, a repair action cannot target a path outside .awa, and a result's
// summary and health are derived from its findings rather than set by hand. The
// infrastructure and application layers produce these values; the CLI renders them.
package doctor

import (
	"fmt"
	"strings"
)

// Severity ranks a finding from informational to fatal. The zero value is not a
// valid severity, so a finding must be built with an explicit one.
type Severity int

const (
	// SeverityInfo is a neutral observation that needs no action.
	SeverityInfo Severity = iota + 1
	// SeverityWarning is a problem that does not corrupt durable state but should
	// be surfaced (for example .awa tracked by git, or an unrecognized lock file).
	SeverityWarning
	// SeverityError is durable corruption or damage doctor will not silently
	// repair. A run that ends with any error finding is unhealthy.
	SeverityError
)

func (s Severity) String() string {
	switch s {
	case SeverityInfo:
		return "info"
	case SeverityWarning:
		return "warning"
	case SeverityError:
		return "error"
	default:
		return "unknown"
	}
}

// Valid reports whether s is a known severity. Go lets any int be cast to the
// type, so the domain checks validity rather than trusting construction.
func (s Severity) Valid() bool {
	return s == SeverityInfo || s == SeverityWarning || s == SeverityError
}

// Health is the overall verdict of a doctor run, derived from its findings so it
// always agrees with the exit code (see DoctorResult.Health). It is computed,
// never set directly.
type Health int

const (
	// HealthOK means nothing actionable remains: no findings, or only ones already
	// repaired.
	HealthOK Health = iota + 1
	// HealthWarning means only non-repairable warnings remain — informational, and
	// not enough to need attention.
	HealthWarning
	// HealthFailed means something actionable remains: an unrepaired error or an
	// unrepaired repairable finding.
	HealthFailed
)

func (h Health) String() string {
	switch h {
	case HealthOK:
		return "ok"
	case HealthWarning:
		return "warning"
	case HealthFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// Valid reports whether h is a known health state.
func (h Health) Valid() bool {
	return h == HealthOK || h == HealthWarning || h == HealthFailed
}

// Subsystem names the part of the project a finding is about. It is a closed set
// so reports group findings consistently and machine consumers can filter on it.
type Subsystem string

const (
	SubsystemConfig      Subsystem = "config"
	SubsystemLayout      Subsystem = "layout"
	SubsystemCheckpoints Subsystem = "checkpoints"
	SubsystemRuns        Subsystem = "runs"
	SubsystemRestores    Subsystem = "restores"
	SubsystemIndex       Subsystem = "index"
	SubsystemTemp        Subsystem = "temp"
	SubsystemLocks       Subsystem = "locks"
	SubsystemGit         Subsystem = "git"
)

// Valid reports whether s is a known subsystem.
func (s Subsystem) Valid() bool {
	switch s {
	case SubsystemConfig, SubsystemLayout, SubsystemCheckpoints, SubsystemRuns,
		SubsystemRestores, SubsystemIndex, SubsystemTemp, SubsystemLocks, SubsystemGit:
		return true
	default:
		return false
	}
}

// FindingCode is a stable machine token for one kind of diagnosis. The set is
// closed: a finding cannot be built with a code outside it, so reports never carry
// free-form strings that an agent or script cannot rely on. Codes are stable once
// shipped.
type FindingCode string

const (
	CodeConfigInvalid                FindingCode = "config-invalid"
	CodeRequiredDirMissing           FindingCode = "required-dir-missing"
	CodeRequiredDirNotDir            FindingCode = "required-dir-not-directory"
	CodeCheckpointCorrupt            FindingCode = "checkpoint-corrupt"
	CodeCheckpointIncompatibleFormat FindingCode = "checkpoint-incompatible-format"
	CodeCheckpointManifestCorrupt    FindingCode = "checkpoint-manifest-corrupt"
	CodeCheckpointMissingBlob        FindingCode = "checkpoint-missing-blob"
	CodeCheckpointMissingManifest    FindingCode = "checkpoint-missing-manifest"
	CodeRunCorrupt                   FindingCode = "run-corrupt"
	CodeRunIncompatibleFormat        FindingCode = "run-incompatible-format"
	CodeRunPointerCorrupt            FindingCode = "run-pointer-corrupt"
	CodeRunPayloadCorrupt            FindingCode = "run-payload-corrupt"
	CodeRestoreRecoveryCorrupt       FindingCode = "restore-recovery-corrupt"
	CodeRestoreRecoveryMissingBlob   FindingCode = "restore-recovery-missing-blob"
	CodeIndexUnreadable              FindingCode = "index-unreadable"
	CodeIndexSchemaInvalid           FindingCode = "index-schema-invalid"
	CodeIndexStale                   FindingCode = "index-stale"
	CodeLockStale                    FindingCode = "lock-stale"
	CodeLockUnknown                  FindingCode = "lock-unknown"
	CodeOrphanTemp                   FindingCode = "orphan-temp"
	CodeTempUnreadable               FindingCode = "temp-unreadable"
	CodeAwaTrackedByGit              FindingCode = "awa-tracked-by-git"
	CodeGitCheckFailed               FindingCode = "git-check-failed"
	CodeStateGitignoreMissing        FindingCode = "state-gitignore-missing"
	CodeStateGitignoreIneffective    FindingCode = "state-gitignore-ineffective"
	CodeStateGitignoreUnreadable     FindingCode = "state-gitignore-unreadable"
	CodeRepairFailed                 FindingCode = "repair-failed"
	// Local-privacy findings. They diagnose the local evidence foot-guns
	// that make .awa/ risky to keep or share; each names a distinct kind of risk so an
	// agent can tell missing evidence from unsafe evidence from a healthy absence.
	CodeStatePermissionsTooBroad FindingCode = "state-permissions-too-broad"
	CodeEnvAllowlistSuspicious   FindingCode = "env-allowlist-suspicious"
	// CodeEnvAllowlistInjectsCode is separate from CodeEnvAllowlistSuspicious because
	// the two answer different questions. A suspicious name risks a secret *leaving*
	// through the allowlist; a code-injecting name lets arbitrary code *enter* the child
	// the run supervises, which is a capability concern rather than a confidentiality
	// one, and its remediation differs.
	CodeEnvAllowlistInjectsCode    FindingCode = "env-allowlist-injects-code"
	CodeContentStorageEnabled      FindingCode = "content-storage-enabled"
	CodeNestedProjectMarker        FindingCode = "nested-project-marker"
	CodeNestedMarkerScanIncomplete FindingCode = "nested-marker-scan-incomplete"
	CodeAncestorProjectMarker      FindingCode = "ancestor-project-marker"
)

// findingCodes is the canonical enumeration of the closed vocabulary. It is the one
// list that Valid, FindingCodes, and the documentation coverage guard all read, so
// membership, the published order, and what the documentation owes cannot disagree.
// Go cannot enumerate the constants above, so adding one here is still a manual step;
// the count assertion in the package tests is what makes it a deliberate one, and it
// names the consumer — the documentation coverage matrix — that a new code obliges.
//
// The order is semantic and deliberate, not alphabetical: configuration, then the
// store's own shape, then the records inside it, then the worktree index, then locks
// and temp debris, then what the evidence's exposure looks like from outside, and
// finally the meta-finding that a repair itself failed. Consumers may rely on it
// being stable.
var findingCodes = []FindingCode{
	// configuration
	CodeConfigInvalid,
	// the store's own shape
	CodeRequiredDirMissing,
	CodeRequiredDirNotDir,
	// checkpoint records
	CodeCheckpointCorrupt,
	CodeCheckpointIncompatibleFormat,
	CodeCheckpointManifestCorrupt,
	CodeCheckpointMissingBlob,
	CodeCheckpointMissingManifest,
	// run records
	CodeRunCorrupt,
	CodeRunIncompatibleFormat,
	CodeRunPointerCorrupt,
	CodeRunPayloadCorrupt,
	// restore recovery observations
	CodeRestoreRecoveryCorrupt,
	CodeRestoreRecoveryMissingBlob,
	// the worktree index
	CodeIndexUnreadable,
	CodeIndexSchemaInvalid,
	CodeIndexStale,
	// locks and temp debris
	CodeLockStale,
	CodeLockUnknown,
	CodeOrphanTemp,
	CodeTempUnreadable,
	// how exposed the local evidence is
	CodeAwaTrackedByGit,
	CodeGitCheckFailed,
	CodeStateGitignoreMissing,
	CodeStateGitignoreIneffective,
	CodeStateGitignoreUnreadable,
	CodeStatePermissionsTooBroad,
	CodeEnvAllowlistSuspicious,
	CodeEnvAllowlistInjectsCode,
	CodeContentStorageEnabled,
	CodeNestedProjectMarker,
	CodeNestedMarkerScanIncomplete,
	CodeAncestorProjectMarker,
	// the diagnosis of a failed repair
	CodeRepairFailed,
}

// FindingCodes returns the closed diagnosis vocabulary in canonical order. The result
// is a copy: the catalog is package state, and a consumer that walks it must not be
// able to reorder or truncate what every other consumer sees.
func FindingCodes() []FindingCode {
	return append([]FindingCode(nil), findingCodes...)
}

// knownCodes is the membership view of findingCodes, built once so Valid is a lookup
// rather than a second hand-maintained list that could drift from the enumeration.
var knownCodes = func() map[FindingCode]bool {
	set := make(map[FindingCode]bool, len(findingCodes))
	for _, c := range findingCodes {
		set[c] = true
	}
	return set
}()

// Valid reports whether c is a known finding code.
func (c FindingCode) Valid() bool    { return knownCodes[c] }
func (c FindingCode) String() string { return string(c) }

// Finding records one diagnosis. Its fields are unexported so a Finding can only
// be built through NewFinding (which enforces the invariants) and marked repaired
// through MarkRepaired (which enforces that only a repairable finding can be
// repaired). This keeps an impossible finding — an unknown code, an invalid
// severity, or repaired-but-not-repairable — out of every report.
type Finding struct {
	code       FindingCode
	severity   Severity
	subsystem  Subsystem
	path       string
	subject    string
	message    string
	repairable bool
	repaired   bool
}

// NewFinding builds a validated finding. code, severity, and subsystem must be
// known; subject and message must be non-empty so every report line is locatable
// and human-readable. path is optional (some findings, like a git-tracking
// warning, are about the project as a whole). A finding starts not-repaired; use
// MarkRepaired after a successful repair.
func NewFinding(code FindingCode, severity Severity, subsystem Subsystem, path, subject, message string, repairable bool) (Finding, error) {
	if !code.Valid() {
		return Finding{}, fmt.Errorf("invalid finding code %q", code)
	}
	if !severity.Valid() {
		return Finding{}, fmt.Errorf("invalid finding severity for %s", code)
	}
	if !subsystem.Valid() {
		return Finding{}, fmt.Errorf("invalid finding subsystem %q for %s", subsystem, code)
	}
	if strings.TrimSpace(subject) == "" {
		return Finding{}, fmt.Errorf("finding %s requires a subject", code)
	}
	if strings.TrimSpace(message) == "" {
		return Finding{}, fmt.Errorf("finding %s requires a message", code)
	}
	return Finding{
		code:       code,
		severity:   severity,
		subsystem:  subsystem,
		path:       path,
		subject:    subject,
		message:    message,
		repairable: repairable,
	}, nil
}

// MarkRepaired returns a copy of the finding flagged repaired. It fails if the
// finding is not repairable, so a report can never claim it fixed damage it had
// already declared unfixable.
func (f Finding) MarkRepaired() (Finding, error) {
	if !f.repairable {
		return Finding{}, fmt.Errorf("finding %s is not repairable", f.code)
	}
	f.repaired = true
	return f, nil
}

func (f Finding) Code() FindingCode    { return f.code }
func (f Finding) Severity() Severity   { return f.severity }
func (f Finding) Subsystem() Subsystem { return f.subsystem }
func (f Finding) Path() string         { return f.path }
func (f Finding) Subject() string      { return f.subject }
func (f Finding) Message() string      { return f.message }
func (f Finding) Repairable() bool     { return f.repairable }
func (f Finding) Repaired() bool       { return f.repaired }
