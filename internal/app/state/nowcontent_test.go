package state_test

import (
	"context"
	"strings"
	"testing"

	"awarer/internal/app/scanner"
	"awarer/internal/app/state"
	"awarer/internal/domain/config"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/worktreefs"
	"awarer/internal/scantest"
)

// nowFixture builds a worktree holding one ordinary file and one file reached through a
// symlink, then resolves the current workspace under the given content need.
func nowFixture(t *testing.T, needContent bool) (*state.ResolvedState, map[string]worktree.Entry) {
	t.Helper()
	scantest.RequireSymlinks(t)
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Write(t, root, "plain.txt", "plain bytes\n")
	scantest.Write(t, root, "real.txt", "followed bytes\n")
	scantest.Symlink(t, root, "link.txt", "real.txt")

	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true

	hasher := blake3hash.New()
	resolver := state.NewResolver(state.Deps{
		Scanner: scanner.New(worktreefs.New(), hasher, nil),
		Hasher:  hasher,
	})
	rs, err := resolver.Resolve(context.Background(), state.Ref{Kind: state.RefNow},
		state.NowContext{Project: proj, Config: cfg, NeedContent: needContent})
	if err != nil {
		t.Fatalf("Resolve now: %v", err)
	}

	scanned := map[string]worktree.Entry{}
	cur, err := rs.Manifest(context.Background())
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	for cur.Next() {
		if e, ok := cur.Record().Entry(); ok {
			scanned[e.Path.String()] = e
		}
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("manifest cursor: %v", err)
	}
	_ = cur.Close()
	return rs, scanned
}

// TestNowContentServesOrdinaryAndFollowedFiles pins what a content-needing "now"
// resolution must be able to answer for. Unlike a checkpoint, this state has no
// publication boundary at which an ordinary file's read could be substituted for a
// reopen — Content is asked for an arbitrary current path — so its scan retains every
// blob-intent source, ordinary ones included.
//
// The ordinary half is the load-bearing assertion: narrowing this caller to the
// checkpoint's followed-only retention would leave plain.txt without a source and fail
// here, which is exactly the mistake that must not pass silently.
func TestNowContentServesOrdinaryAndFollowedFiles(t *testing.T) {
	rs, scanned := nowFixture(t, true)

	for _, tc := range []struct{ path, want string }{
		{"plain.txt", "plain bytes\n"},
		{"link.txt", "followed bytes\n"},
	} {
		e, ok := scanned[tc.path]
		if !ok {
			t.Fatalf("%s missing from the now scan", tc.path)
		}
		got, err := rs.Content(e.Path, e.Content)
		if err != nil {
			t.Fatalf("Content(%s): %v", tc.path, err)
		}
		if string(got) != tc.want {
			t.Errorf("%s content = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestNowWithoutContentRetainsNothing pins the other direction of the same mapping. A
// content-free comparison (changes) must retain no opener at all, and the only
// observable consequence of that is that this state cannot serve content — so asserting
// it here is what keeps a silent widening back to per-file retention from passing. The
// tree identity it does owe is unaffected.
func TestNowWithoutContentRetainsNothing(t *testing.T) {
	rs, scanned := nowFixture(t, false)

	for _, path := range []string{"plain.txt", "link.txt"} {
		e, ok := scanned[path]
		if !ok {
			t.Fatalf("%s missing from the now scan", path)
		}
		_, err := rs.Content(e.Path, e.Content)
		if err == nil {
			t.Errorf("a content-free now resolution served content for %s; its scan retained a source it must not have", path)
			continue
		}
		// Named, so an unrelated failure cannot stand in for the absence under test.
		if !strings.Contains(err.Error(), "no content source") {
			t.Errorf("Content(%s) err = %v, want it to report the absent content source", path, err)
		}
	}
	if rs.TreeHash.IsZero() {
		t.Error("a content-free now resolution must still carry a tree hash")
	}
}
