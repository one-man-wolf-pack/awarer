package state_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"awarer/internal/app/state"
	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/paths"
	"awarer/internal/infra/checkpointjson"
)

func TestParseRefAcceptsSingleReferences(t *testing.T) {
	cases := map[string]state.RefKind{
		"now":            state.RefNow,
		"latest":         state.RefLatest,
		"@-3":            state.RefAtN,
		"run:abc:before": state.RefRunObservation,
		"run:abc:after":  state.RefRunObservation,
		"7k3f9":          state.RefCheckpointPrefix,
	}
	for tok, want := range cases {
		t.Run(tok, func(t *testing.T) {
			ref, err := state.ParseRef(tok)
			if err != nil {
				t.Fatalf("ParseRef(%q): %v", tok, err)
			}
			if ref.Kind != want {
				t.Errorf("ParseRef(%q).Kind = %v, want %v", tok, ref.Kind, want)
			}
		})
	}
}

func TestParseRefRejectsNonSingleReferenceTokens(t *testing.T) {
	// A range, the bare "-N" range shortcut, and an empty token are all usage errors:
	// resolve names exactly one state. So is a token that cannot be a checkpoint id
	// prefix ("baseline" carries an "l", "UPPER" is out of the lowercase alphabet):
	// the parser rejects it before the store is opened.
	for _, tok := range []string{"", "latest..now", "a..b", "-2", "@bad", "@", "baseline", "UPPER"} {
		if _, err := state.ParseRef(tok); err == nil {
			t.Errorf("ParseRef(%q) = nil error, want usage error", tok)
		}
	}
}

// TestResolveLatestClassifiesCorrupt proves a corrupt checkpoint header makes latest
// resolution fail with ErrLatestCorrupt (which still wraps ErrLatestUnreadable, so the
// changes/diff exit mapping is unaffected). Oracle: the sentinel identity, independent
// of the health count the producer reads.
func TestResolveLatestClassifiesCorrupt(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := checkpointjson.NewRepo(layout)
	r := state.NewResolver(state.Deps{
		Checkpoints: repo,
	})
	id := putCheckpoint(t, repo, 0x22, time.Unix(1_700_000_000, 0).UTC(), fullHex)
	// Overwrite the header with garbage so the store reads it back as corrupt. The
	// store writes checkpoint files owner-read-only, so make the file writable first.
	header := filepath.Join(layout.CheckpointsDir(), id.String(), "header.json")
	if err := os.Chmod(header, 0o644); err != nil {
		t.Fatalf("chmod header: %v", err)
	}
	if err := os.WriteFile(header, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("corrupt header: %v", err)
	}
	_, err := r.Resolve(context.Background(), state.Ref{Kind: state.RefLatest}, state.NowContext{})
	if !errors.Is(err, state.ErrLatestCorrupt) {
		t.Fatalf("want ErrLatestCorrupt, got %v", err)
	}
	if !errors.Is(err, state.ErrLatestUnreadable) {
		t.Fatalf("ErrLatestCorrupt must still wrap ErrLatestUnreadable, got %v", err)
	}
}

// TestResolveLatestClassifiesIncompatible proves that when every unreadable checkpoint is a
// an incompatible schema, latest resolution fails with ErrLatestIncompatible, distinct
// from the corrupt case.
func TestResolveLatestClassifiesIncompatible(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := checkpointjson.NewRepo(layout)
	r := state.NewResolver(state.Deps{
		Checkpoints: repo,
	})
	// A header at the correct address declaring a schema this build has no reader for.
	id, err := checkpoint.NewCheckpointID(bytes.NewReader(bytes.Repeat([]byte{0x33}, 20)))
	if err != nil {
		t.Fatalf("NewCheckpointID: %v", err)
	}
	dir := filepath.Join(layout.CheckpointsDir(), id.String())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "header.json"), []byte(`{"schema_version":99}`), 0o644); err != nil {
		t.Fatalf("write incompatible header: %v", err)
	}
	_, resErr := r.Resolve(context.Background(), state.Ref{Kind: state.RefLatest}, state.NowContext{})
	if !errors.Is(resErr, state.ErrLatestIncompatible) {
		t.Fatalf("want ErrLatestIncompatible, got %v", resErr)
	}
	if errors.Is(resErr, state.ErrLatestCorrupt) {
		t.Fatalf("incompatible-only store must not classify as corrupt: %v", resErr)
	}
}
