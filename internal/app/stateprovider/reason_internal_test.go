package stateprovider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"awarer/internal/app/state"
	"awarer/internal/domain/blob"
	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/provider"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/worktree"
)

// Mutation proofs:
//   - move the context.Canceled case below the default -> TestReasonForInterruptionIsFault
//     goes red (cancellation would map to io-error instead of a fault).
//   - drop the ErrLatestIncompatible case (so it falls through to the corrupt case, which
//     also matches because ErrLatestIncompatible wraps ErrLatestUnreadable) ->
//     the metadata-incompatible row goes red.
//   - change the default from ReasonIOError to returning ok=false ->
//     TestReasonForUnclassifiedFailsClosed goes red.

func TestReasonForTable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want provider.Reason
	}{
		{"no-checkpoints", state.ErrNoCheckpoints, provider.ReasonNotFound},
		{"out-of-range", state.ErrOutOfRange, provider.ReasonNotFound},
		{"checkpoint-not-found", checkpoint.ErrNotFound, provider.ReasonNotFound},
		{"run-not-found", runcache.ErrNotFound, provider.ReasonNotFound},
		{"latest-incompatible", state.ErrLatestIncompatible, provider.ReasonMetadataIncompatible},
		{"checkpoint-incompatible", checkpoint.ErrIncompatibleFormat, provider.ReasonMetadataIncompatible},
		{"latest-corrupt", state.ErrLatestCorrupt, provider.ReasonMetadataCorrupt},
		{"checkpoint-corrupt", checkpoint.ErrCorruptStore, provider.ReasonMetadataCorrupt},
		{"run-corrupt", runcache.ErrCorruptStore, provider.ReasonMetadataCorrupt},
		{"blob-corrupt", blob.ErrCorruptBlob, provider.ReasonMetadataCorrupt},
		{"latest-unreadable-bare", state.ErrLatestUnreadable, provider.ReasonMetadataCorrupt},
		{"checkpoint-ambiguous", checkpoint.ErrAmbiguousPrefix, provider.ReasonAmbiguousReference},
		{"run-ambiguous", runcache.ErrAmbiguousPrefix, provider.ReasonAmbiguousReference},
		{"run-invalid-prefix", runcache.ErrInvalidPrefix, provider.ReasonUnsupportedReference},
		{"observation-unavailable", runcache.ErrObservationUnavailable, provider.ReasonManifestUnavailable},
		{"manifest-missing", worktree.ErrManifestMissing, provider.ReasonManifestUnavailable},
		{"manifest-missing-wraps-corrupt", fmt.Errorf("read: %w", worktree.ErrManifestMissing), provider.ReasonManifestUnavailable},
		{"observation-unstable", state.ErrObservationUnstable, provider.ReasonObservationUnstable},
		{"permission", os.ErrPermission, provider.ReasonPermissionDenied},
		{"not-exist", os.ErrNotExist, provider.ReasonNotFound},
		{"wrapped-corrupt", fmt.Errorf("reading store: %w", checkpoint.ErrCorruptStore), provider.ReasonMetadataCorrupt},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, isAvailability := reasonFor(c.err)
			if !isAvailability {
				t.Fatalf("reasonFor(%v) reported a fault, want availability reason %q", c.err, c.want)
			}
			if got != c.want {
				t.Errorf("reasonFor(%v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}

func TestReasonForInterruptionIsFault(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, fmt.Errorf("scan: %w", context.Canceled)} {
		if _, isAvailability := reasonFor(err); isAvailability {
			t.Errorf("reasonFor(%v) must be a fault, not an availability reason", err)
		}
	}
}

func TestReasonForUnclassifiedFailsClosed(t *testing.T) {
	// An unclassified read-path error is still an availability assessment (io-error),
	// never a fault or an empty success.
	got, isAvailability := reasonFor(errors.New("some unexpected read failure"))
	if !isAvailability {
		t.Fatal("unclassified error must fail closed to an availability reason")
	}
	if got != provider.ReasonIOError {
		t.Errorf("unclassified error = %q, want io-error", got)
	}
}
