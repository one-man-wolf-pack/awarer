package state_test

import (
	"context"
	"errors"
	"testing"

	"awarer/internal/app/scanner"
	"awarer/internal/app/state"
	"awarer/internal/domain/config"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/worktreefs"
	"awarer/internal/scantest"
)

// TestNowContentRaceDegradesToContentChanged verifies that when a current-
// workspace file changes between the ephemeral scan and a diff content read, the
// read fails with ErrContentChanged (so diff degrades that one file to
// "unavailable") rather than a generic error that would abort the whole diff.
func TestNowContentRaceDegradesToContentChanged(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Write(t, root, "calc.go", "package calc\n")

	hasher := blake3hash.New()
	resolver := state.NewResolver(state.Deps{
		Scanner: scanner.New(worktreefs.New(), hasher, nil),
		Hasher:  hasher,
	})

	rs, err := resolver.Resolve(context.Background(), state.Ref{Kind: state.RefNow},
		state.NowContext{Project: proj, Config: config.Defaults(), NeedContent: true})
	if err != nil {
		t.Fatalf("Resolve now: %v", err)
	}

	p, err := worktree.ParseRelPath("calc.go")
	if err != nil {
		t.Fatalf("ParseRelPath: %v", err)
	}
	var scanned worktree.Entry
	cur, err := rs.Manifest(context.Background())
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	for cur.Next() {
		if e, ok := cur.Record().Entry(); ok && e.Path.Equal(p) {
			scanned = e
		}
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("manifest cursor: %v", err)
	}
	_ = cur.Close()
	if scanned.Content.IsZero() {
		t.Fatalf("calc.go not found in now scan")
	}

	// Mutate the file after the scan, then read its content for a diff: the
	// scanner's verified opener detects the change, and Content must surface it as
	// ErrContentChanged, not a raw error.
	scantest.Write(t, root, "calc.go", "package calc\n// changed after scan\n")

	if _, err := rs.Content(p, scanned.Content); !errors.Is(err, state.ErrContentChanged) {
		t.Fatalf("racing now read: got %v, want ErrContentChanged", err)
	}
}
