package restore

import (
	"testing"

	domrestore "awarer/internal/domain/restore"
)

// TestASelectedHashOnlySourceRefusesEvenWithItsBytesInTheStore: under the default
// selection a new checkpoint records .env hash-only while a.txt, holding identical
// bytes, owns that very blob. The shared blob grants .env no byte capability — its
// changed path blocks the restore rather than vanishing from it — while the ordinary
// byte-owning a.txt restores from the same checkpoint.
func TestASelectedHashOnlySourceRefusesEvenWithItsBytesInTheStore(t *testing.T) {
	h := setup(t)
	h.write(".env", "DUMMY_TOKEN=one\n")
	h.write("a.txt", "DUMMY_TOKEN=one\n")
	id := h.checkpointNow(h.cfg)
	h.write(".env", "DUMMY_TOKEN=two\n")
	h.write("a.txt", "rewritten")

	blocked := h.run(t, id.String(), h.selection(".env", "a.txt"), true)
	if got := blocked.Result.Outcome(); got != domrestore.OutcomeRefused {
		t.Fatalf("outcome = %s, want refused", got)
	}
	if !hasReason(blocked.Result, domrestore.ReasonHashOnlyContent) {
		t.Errorf("refusal reasons = %v, want hash-only-content", blocked.Result.Reasons())
	}
	if got := h.read(".env"); got != "DUMMY_TOKEN=two\n" {
		t.Errorf("a refused apply mutated .env: %q", got)
	}

	ordinary := h.run(t, id.String(), h.selection("a.txt"), true)
	if got := ordinary.Result.Outcome(); got != domrestore.OutcomeApplied {
		t.Fatalf("ordinary restore outcome = %s (%v)", got, ordinary.Result.Reasons())
	}
	if got := h.read("a.txt"); got != "DUMMY_TOKEN=one\n" {
		t.Errorf("a.txt = %q, want the checkpointed bytes", got)
	}
}

// TestARecoveryCaptureIgnoresTodaysSelectiveRetention: an older byte-owning source
// (taken under an explicit empty selection) restores .env while today's policy would
// keep .env hash-only. The recovery observation must still capture the current .env
// bytes, so the undo returns exactly them.
func TestARecoveryCaptureIgnoresTodaysSelectiveRetention(t *testing.T) {
	h := setup(t)
	keep := h.cfg
	keep.Checkpoint.HashOnlyPatterns = []string{}
	h.write(".env", "DUMMY_TOKEN=reviewed\n")
	id := h.checkpointNow(keep)
	h.write(".env", "DUMMY_TOKEN=current\n")

	res := h.run(t, id.String(), h.selection(".env"), true)
	if got := res.Result.Outcome(); got != domrestore.OutcomeApplied {
		t.Fatalf("outcome = %s (%v)", got, res.Result.Reasons())
	}
	if got := h.read(".env"); got != "DUMMY_TOKEN=reviewed\n" {
		t.Fatalf(".env = %q, want the byte-owning source's bytes", got)
	}

	undo := h.run(t, res.RecoveryRef, h.selection(".env"), true)
	if got := undo.Result.Outcome(); got != domrestore.OutcomeApplied {
		t.Fatalf("undo outcome = %s (%v), failures %+v", got, undo.Result.Reasons(), undo.Result.Failures())
	}
	if got := h.read(".env"); got != "DUMMY_TOKEN=current\n" {
		t.Errorf("undo restored %q, want the pre-restore bytes recovery captured", got)
	}
}
