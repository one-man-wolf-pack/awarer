package gitmeta

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

// fakeExit is an *exec.ExitError-like error is hard to fabricate, so tests that
// need a non-repo classification go through a runner that returns an ExitError via
// a real failing command. For the cases here a sentinel error plus stderr suffices
// because classifyProbe also handles non-exit errors as genuine; the not-a-repo
// path is exercised with an exec.ExitError obtained from a real git-less command.

func TestAwaTrackingNonRepo(t *testing.T) {
	// rev-parse fails with a real ExitError whose stderr says "not a git
	// repository" → absent, InRepo false, no error.
	runner := func(ctx context.Context, args ...string) ([]byte, string, error) {
		return nil, "fatal: not a git repository (or any of the parent directories): .git", exitErr(t)
	}
	p := newWithRunner("/proj", runner)
	tr, err := p.AwaTracking(context.Background())
	if err != nil {
		t.Fatalf("AwaTracking: %v", err)
	}
	if tr.InRepo || tr.AwaTracked {
		t.Fatalf("expected not-in-repo, got %+v", tr)
	}
}

func TestAwaTrackingGitMissing(t *testing.T) {
	runner := func(ctx context.Context, args ...string) ([]byte, string, error) {
		return nil, "", exec.ErrNotFound
	}
	p := newWithRunner("/proj", runner)
	tr, err := p.AwaTracking(context.Background())
	if err != nil {
		t.Fatalf("AwaTracking: %v", err)
	}
	if tr.InRepo {
		t.Fatalf("expected not-in-repo when git missing, got %+v", tr)
	}
}

func TestAwaTrackingTracked(t *testing.T) {
	runner := func(ctx context.Context, args ...string) ([]byte, string, error) {
		switch args[0] {
		case "rev-parse":
			return []byte("true\n"), "", nil
		case "ls-files":
			return []byte(".awa/config.toml\x00.awa/store/blobs/aa\x00"), "", nil
		default:
			t.Fatalf("unexpected git args %v", args)
			return nil, "", nil
		}
	}
	p := newWithRunner("/proj", runner)
	tr, err := p.AwaTracking(context.Background())
	if err != nil {
		t.Fatalf("AwaTracking: %v", err)
	}
	if !tr.InRepo || !tr.AwaTracked {
		t.Fatalf("expected tracked, got %+v", tr)
	}
}

func TestAwaTrackingUntracked(t *testing.T) {
	runner := func(ctx context.Context, args ...string) ([]byte, string, error) {
		switch args[0] {
		case "rev-parse":
			return []byte("true\n"), "", nil
		case "ls-files":
			return []byte(""), "", nil
		default:
			return nil, "", nil
		}
	}
	p := newWithRunner("/proj", runner)
	tr, err := p.AwaTracking(context.Background())
	if err != nil {
		t.Fatalf("AwaTracking: %v", err)
	}
	if !tr.InRepo || tr.AwaTracked {
		t.Fatalf("expected in-repo untracked, got %+v", tr)
	}
}

func TestAwaTrackingGenuineFailure(t *testing.T) {
	// rev-parse succeeds (in repo), but ls-files fails for a genuine reason →
	// surfaced as an error the caller turns into a warning.
	runner := func(ctx context.Context, args ...string) ([]byte, string, error) {
		switch args[0] {
		case "rev-parse":
			return []byte("true\n"), "", nil
		case "ls-files":
			return nil, "fatal: something broke", errors.New("exit status 128")
		default:
			return nil, "", nil
		}
	}
	p := newWithRunner("/proj", runner)
	if _, err := p.AwaTracking(context.Background()); err == nil {
		t.Fatalf("expected genuine ls-files failure to surface as error")
	}
}

// exitErr returns a real *exec.ExitError by running a command that exits non-zero,
// so classifyProbe's errors.As(*exec.ExitError) branch is exercised.
func exitErr(t *testing.T) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit 1").Run()
	if err == nil {
		t.Fatalf("expected a failing command")
	}
	return err
}
