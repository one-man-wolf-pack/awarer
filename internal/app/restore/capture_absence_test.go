package restore

import (
	"context"
	"io"
	"testing"

	"awarer/internal/domain/hashing"
	domrestore "awarer/internal/domain/restore"
)

// appearingApplier makes a destination the plan proved absent exist once staging
// begins, after planning and before recovery capture. The current policy hides it,
// so only a physical check at capture can see it.
type appearingApplier struct {
	Applier
	h     *harness
	path  string
	fired bool
}

func (a *appearingApplier) Stage(ctx context.Context, content hashing.ContentHash, open func() (io.ReadCloser, error)) (domrestore.StagedPayload, error) {
	if !a.fired {
		a.fired = true
		a.h.write(a.path, "appeared after planning")
	}
	return a.Applier.Stage(ctx, content, open)
}

func TestRecoveryCaptureRefusesADestinationThatAppearedAfterPlanning(t *testing.T) {
	h := setup(t)
	h.write("a.txt", "one")
	h.write("new.txt", "reviewed")
	id := h.checkpointNow(h.cfg)
	h.write("a.txt", "one changed")
	h.remove("new.txt")
	h.write(".awaignore", "new.txt\n")

	real := h.applierFactory()
	h.svc.deps.Appliers = func(oid domrestore.OperationID) (Applier, error) {
		a, err := real(oid)
		if err != nil {
			return nil, err
		}
		return &appearingApplier{Applier: a, h: h, path: "new.txt"}, nil
	}

	res := h.run(t, id.String(), h.selection("a.txt", "new.txt"), true)
	if got := res.Result.Outcome(); got != domrestore.OutcomeRefused {
		t.Fatalf("outcome = %s (%v; failures %+v), want a refusal before any write", got, res.Result.Reasons(), res.Result.Failures())
	}
	requireFailure(t, res.Result, "new.txt", domrestore.ReasonRecoveryIncomplete)
	if n := h.recoveryRecords(t); n != 0 {
		t.Errorf("published %d recovery observation(s) claiming an absence that no longer holds", n)
	}
	if got := h.read("new.txt"); got != "appeared after planning" {
		t.Errorf("new.txt = %q, want the appeared file untouched", got)
	}
	if got := h.read("a.txt"); got != "one changed" {
		t.Errorf("a.txt = %q, want no write before the refusal", got)
	}
}
