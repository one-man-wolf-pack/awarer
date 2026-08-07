package provider_test

import (
	"testing"

	"awarer/internal/domain/evidence"
	"awarer/internal/domain/provider"
)

// Mutation proofs:
//   - drop a token from reasonSet -> TestReasonCatalogClosed goes red for that token.
//   - respell one of the evidence-derived Reasons as a bare literal that diverges from
//     the evidence token -> TestEvidenceDerivedReasonsMatchTokens goes red.

func TestReasonCatalogClosed(t *testing.T) {
	all := []provider.Reason{
		provider.ReasonNotInitialized,
		provider.ReasonNotFound,
		provider.ReasonAmbiguousReference,
		provider.ReasonMetadataIncompatible,
		provider.ReasonMetadataCorrupt,
		provider.ReasonManifestUnavailable,
		provider.ReasonPermissionDenied,
		provider.ReasonIOError,
		provider.ReasonObservationUnstable,
		provider.ReasonUnsupportedReference,
		provider.ReasonIncompatibleIdentityPolicy,
	}
	for _, r := range all {
		if !r.Valid() {
			t.Errorf("reason %q must be valid", r)
		}
	}
	if provider.Reason("").Valid() || provider.Reason("nope").Valid() {
		t.Error("unknown reasons must be invalid")
	}
	// Guard the exact wire spellings so a rename is a conscious contract change.
	wants := map[provider.Reason]string{
		provider.ReasonNotInitialized:             "not-initialized",
		provider.ReasonNotFound:                   "not-found",
		provider.ReasonAmbiguousReference:         "ambiguous-reference",
		provider.ReasonMetadataIncompatible:       "metadata-incompatible",
		provider.ReasonMetadataCorrupt:            "metadata-corrupt",
		provider.ReasonManifestUnavailable:        "manifest-unavailable",
		provider.ReasonPermissionDenied:           "permission-denied",
		provider.ReasonIOError:                    "io-error",
		provider.ReasonObservationUnstable:        "observation-unstable",
		provider.ReasonUnsupportedReference:       "unsupported-reference",
		provider.ReasonIncompatibleIdentityPolicy: "incompatible-identity-policy",
	}
	for r, want := range wants {
		if r.String() != want {
			t.Errorf("reason spelling drift: %q, want %q", r.String(), want)
		}
	}
}

// TestEvidenceDerivedReasonsMatchTokens proves the four overlapping reasons carry the
// exact evidence token spelling. The oracle is the evidence package's own token
// constants, independent of the provider reasonSet.
func TestEvidenceDerivedReasonsMatchTokens(t *testing.T) {
	pairs := []struct {
		reason provider.Reason
		token  evidence.DiagnosticToken
	}{
		{provider.ReasonMetadataIncompatible, evidence.TokenMetadataIncompatible},
		{provider.ReasonMetadataCorrupt, evidence.TokenMetadataCorrupt},
		{provider.ReasonPermissionDenied, evidence.TokenPermissionDenied},
		{provider.ReasonIOError, evidence.TokenIOError},
	}
	for _, p := range pairs {
		if p.reason.String() != p.token.String() {
			t.Errorf("reason %q must match evidence token %q", p.reason, p.token)
		}
	}
}

func TestContractToken(t *testing.T) {
	if provider.ContractStateV1.String() != "awa-state/v1" {
		t.Errorf("contract token = %q, want awa-state/v1", provider.ContractStateV1)
	}
	if !provider.ContractStateV1.Valid() {
		t.Error("awa-state/v1 must be a valid contract")
	}
	if provider.Contract("awa-state/v2").Valid() || provider.Contract("").Valid() {
		t.Error("unknown contracts must be invalid")
	}
}

func TestEvidenceBoundaryRejectsNegative(t *testing.T) {
	if _, err := provider.NewEvidenceBoundary(-1); err == nil {
		t.Error("expected error for negative skipped-input count")
	}
	b, err := provider.NewEvidenceBoundary(3)
	if err != nil {
		t.Fatalf("NewEvidenceBoundary: %v", err)
	}
	if b.SkippedInputs() != 3 || !b.IgnoredOutsideEvidence() {
		t.Errorf("boundary carried wrong facts: skipped=%d ignored=%v", b.SkippedInputs(), b.IgnoredOutsideEvidence())
	}
}
