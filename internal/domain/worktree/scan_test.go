package worktree_test

import (
	"bytes"
	"testing"
	"time"

	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
)

func validMeta(t *testing.T) worktree.ScanMetadata {
	t.Helper()
	id, err := worktree.NewScanID(1000, bytes.NewReader(make([]byte, 64)))
	if err != nil {
		t.Fatal(err)
	}
	cfgHash, err := hashing.ParseConfigHash("blake3:" + hexA)
	if err != nil {
		t.Fatal(err)
	}
	return worktree.ScanMetadata{
		ScanID:      id,
		Root:        "/tmp/proj",
		ConfigHash:  cfgHash,
		TrustMode:   config.TrustNormal,
		StartedAt:   time.Unix(0, 1000),
		CompletedAt: time.Unix(1, 0),
	}
}

// TestScanMetadataValidateCompleted covers the completion rules the scanner applies
// before it hands back a finished scan: a complete-and-consistent block is accepted,
// and a missing, backwards, or zero-value completion is refused. Validate's own
// (pre-completion) rules are covered by the tests below.
func TestScanMetadataValidateCompleted(t *testing.T) {
	if err := validMeta(t).ValidateCompleted(); err != nil {
		t.Errorf("valid completed metadata rejected: %v", err)
	}

	noCompletion := validMeta(t)
	noCompletion.CompletedAt = time.Time{}
	if err := noCompletion.ValidateCompleted(); err == nil {
		t.Errorf("metadata with no completion time accepted")
	}

	backwards := validMeta(t)
	backwards.CompletedAt = backwards.StartedAt.Add(-time.Hour)
	if err := backwards.ValidateCompleted(); err == nil {
		t.Errorf("metadata completing before it starts accepted")
	}

	if err := (worktree.ScanMetadata{}).ValidateCompleted(); err == nil {
		t.Errorf("zero-value metadata accepted")
	}
}

// TestScanMetadataRejectsFastSignatureMismatch proves the weak-signature flag is
// bound to the trust mode: it must be set for fast mode and clear for every other.
func TestScanMetadataRejectsFastSignatureMismatch(t *testing.T) {
	// Non-fast mode claiming a weak signature is rejected.
	m := validMeta(t)
	m.TrustMode = config.TrustNormal
	m.FastModeWeakSignature = true
	if err := m.Validate(); err == nil {
		t.Errorf("Validate accepted weak signature outside fast mode")
	}

	// Fast mode without the weak signature flag is rejected.
	m = validMeta(t)
	m.TrustMode = config.TrustFast
	m.FastModeWeakSignature = false
	if err := m.Validate(); err == nil {
		t.Errorf("Validate accepted fast mode without weak signature")
	}

	// Fast mode with the flag set is accepted.
	m = validMeta(t)
	m.TrustMode = config.TrustFast
	m.FastModeWeakSignature = true
	if err := m.Validate(); err != nil {
		t.Errorf("Validate rejected fast mode with weak signature: %v", err)
	}
}

func TestScanMetadataRejectsBadRoot(t *testing.T) {
	for _, root := range []string{".", "../x", "relative/path", "/x/", " /x ", "/x/../y"} {
		m := validMeta(t)
		m.Root = root
		if err := m.Validate(); err == nil {
			t.Errorf("Validate accepted non-absolute/unclean root %q", root)
		}
	}
	m := validMeta(t)
	m.Root = "/clean/abs"
	if err := m.Validate(); err != nil {
		t.Errorf("Validate rejected a clean absolute root: %v", err)
	}
}

func TestScanMetadataRejectsUnknownOmittedFields(t *testing.T) {
	m := validMeta(t)
	m.OmittedStatFields = worktree.FieldSet(1 << 7) // bit outside known fields
	if err := m.Validate(); err == nil {
		t.Errorf("Validate accepted unknown omitted stat fields")
	}
}
