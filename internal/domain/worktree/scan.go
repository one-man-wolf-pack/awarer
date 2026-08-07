package worktree

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
)

// ErrObservationChanged reports that an observed node changed between the walk's stat
// and the read of its bytes, so the scan is not a consistent point-in-time observation
// of that node. Infra content openers wrap it (preserving the "changed during scan"
// phrasing) so the scan path can tell a moved input apart from an unreadable one: a
// tolerant scan skips it, while a strict "now" observation surfaces it instead.
var ErrObservationChanged = errors.New("changed during scan")

// ScanMetadata records the conditions under which a scan ran. It captures the
// trust mode actually used, which stat fields the platform could not supply, and
// whether fast mode's weaker signature was in effect — so a result can never be
// silently mistaken for stronger evidence than it is.
type ScanMetadata struct {
	ScanID                ScanID
	Root                  string
	ConfigHash            hashing.ConfigHash
	TrustMode             config.TrustMode
	StartedAt             time.Time
	CompletedAt           time.Time
	OmittedStatFields     FieldSet
	FastModeWeakSignature bool
}

// Validate checks that the metadata describing a scan is well-formed before it is
// persisted. It guards the persistence boundary against a zero-value metadata
// reaching the index. CompletedAt is intentionally not required: a scan is not
// yet complete when its metadata is first recorded.
func (m ScanMetadata) Validate() error {
	if m.ScanID.IsZero() {
		return fmt.Errorf("scan metadata has no scan id")
	}
	if !m.TrustMode.Valid() {
		return fmt.Errorf("scan metadata has invalid trust mode %v", m.TrustMode)
	}
	if m.StartedAt.IsZero() {
		return fmt.Errorf("scan metadata has no start time")
	}
	if m.Root == "" {
		return fmt.Errorf("scan metadata has no root")
	}
	// The root is the project's absolute, cleaned directory. Rejecting relative,
	// unclean, or whitespace-padded roots keeps a meaningless path (".", "../x",
	// " /x ") out of persisted metadata even if a caller bypassed the scanner.
	if !filepath.IsAbs(m.Root) || filepath.Clean(m.Root) != m.Root {
		return fmt.Errorf("scan metadata root %q must be an absolute, cleaned path", m.Root)
	}
	if m.ConfigHash.IsZero() {
		return fmt.Errorf("scan metadata has no config hash")
	}
	if !m.OmittedStatFields.Valid() {
		return fmt.Errorf("scan metadata has unknown omitted stat fields")
	}
	// The weak-signature flag is structurally tied to fast mode: fast mode always
	// produces the weaker stat signature, and no other mode ever does. Binding the
	// flag to the trust mode here keeps a result from claiming weak evidence it did
	// not produce, or from hiding the weakness it did — the flag cannot drift from
	// the mode it describes.
	if m.FastModeWeakSignature != (m.TrustMode == config.TrustFast) {
		return fmt.Errorf("scan metadata fast-mode weak signature %v contradicts trust mode %v", m.FastModeWeakSignature, m.TrustMode)
	}
	return nil
}

// ValidateCompleted checks metadata for a finished scan: everything Validate
// requires, plus a completion time that is set and not before the start.
func (m ScanMetadata) ValidateCompleted() error {
	if err := m.Validate(); err != nil {
		return err
	}
	if m.CompletedAt.IsZero() {
		return fmt.Errorf("completed scan metadata has no completion time")
	}
	if m.CompletedAt.Before(m.StartedAt) {
		return fmt.Errorf("completed scan metadata completes %s before it starts %s", m.CompletedAt, m.StartedAt)
	}
	return nil
}
