package checkpointjson

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/worktree"
	"awarer/internal/scantest"
)

func rewriteHeader(t *testing.T, headerPath string, edit func(string) string) {
	t.Helper()
	data, err := os.ReadFile(headerPath) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(headerPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(headerPath, []byte(edit(string(data))), 0o600); err != nil {
		t.Fatal(err)
	}
}

var frontierField = regexp.MustCompile(`,\s*"frontier": \{[^}]*\}`)

func TestPublishedCheckpointCarriesItsFrontier(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	s := buildCheckpoint(t, idFrom(t, 0x31), time.Unix(1, 0).UTC(), false)
	excluded := worktree.FrontierRecord{Path: rel(t, "node_modules"), Limit: worktree.LimitSubtree}
	h, err := repo.PutManifest(context.Background(), s.build, s.stream(), scantest.FrontierStream(excluded))
	if err != nil {
		t.Fatal(err)
	}
	if h.Frontier == nil || h.Frontier.Count != 1 {
		t.Fatalf("header frontier = %+v, want one record", h.Frontier)
	}
	coverage, err := repo.Coverage(s.id())
	if err != nil {
		t.Fatal(err)
	}
	stream, _, ok := coverage.Frontier()
	if !ok {
		t.Fatalf("a published checkpoint has coverage %v", coverage.Kind())
	}
	cur, err := stream.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cur.Close() }()
	if !cur.Next() || cur.Record() != excluded || cur.Next() || cur.Err() != nil {
		t.Fatalf("frontier did not round-trip: %+v %v", cur.Record(), cur.Err())
	}
}

func TestPredecessorHeaderReadsWithUnknownCoverage(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	s := buildCheckpoint(t, idFrom(t, 0x32), time.Unix(1, 0).UTC(), false)
	s.put(t, repo)
	dir := filepath.Join(layout.CheckpointsDir(), s.id().String())
	rewriteHeader(t, filepath.Join(dir, headerName), func(d string) string {
		return frontierField.ReplaceAllString(regexp.MustCompile(`"schema_version": 2`).ReplaceAllString(d, `"schema_version": 1`), "")
	})
	if err := os.Remove(filepath.Join(dir, frontierName)); err != nil {
		t.Fatal(err)
	}

	h, err := repo.Header(s.id())
	if err != nil {
		t.Fatalf("a predecessor header is unreadable: %v", err)
	}
	if h.Frontier != nil {
		t.Fatalf("a predecessor header claims a frontier: %+v", h.Frontier)
	}
	coverage, err := repo.Coverage(s.id())
	if err != nil || coverage.Kind().Known() {
		t.Fatalf("predecessor coverage = %v, %v; want unknown", coverage.Kind(), err)
	}
	if err := readCheckpoint(repo, s.id()); err != nil {
		t.Fatalf("a predecessor manifest is unreadable: %v", err)
	}
}

func TestProofBearingHeaderRequiresItsFrontier(t *testing.T) {
	cases := map[string]func(string) string{
		"v2 without frontier": func(d string) string { return frontierField.ReplaceAllString(d, "") },
		"v1 with frontier": func(d string) string {
			return regexp.MustCompile(`"schema_version": 2`).ReplaceAllString(d, `"schema_version": 1`)
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			layout := paths.New(t.TempDir())
			repo := NewRepo(layout)
			s := buildCheckpoint(t, idFrom(t, 0x33), time.Unix(1, 0).UTC(), false)
			s.put(t, repo)
			rewriteHeader(t, filepath.Join(layout.CheckpointsDir(), s.id().String(), headerName), edit)
			_, err := repo.Header(s.id())
			if !errors.Is(err, checkpoint.ErrCorruptStore) || errors.Is(err, checkpoint.ErrIncompatibleFormat) {
				t.Fatalf("Header = %v, want corruption", err)
			}
		})
	}
}

func TestMissingFrontierPayloadIsCorrupt(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	s := buildCheckpoint(t, idFrom(t, 0x34), time.Unix(1, 0).UTC(), false)
	s.put(t, repo)
	if err := os.Remove(filepath.Join(layout.CheckpointsDir(), s.id().String(), frontierName)); err != nil {
		t.Fatal(err)
	}
	coverage, err := repo.Coverage(s.id())
	if err != nil {
		t.Fatal(err)
	}
	stream, _, _ := coverage.Frontier()
	if _, err := stream.Open(context.Background()); !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("opening a missing frontier = %v, want corruption", err)
	}
}

func TestPutManifestRequiresAFrontier(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	s := buildCheckpoint(t, idFrom(t, 0x35), time.Unix(1, 0).UTC(), false)
	if _, err := repo.PutManifest(context.Background(), s.build, s.stream(), nil); err == nil {
		t.Fatal("published a checkpoint without coverage evidence")
	}
	if _, err := repo.Header(s.id()); !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("Header after a refused publish = %v, want not found", err)
	}
}

func TestPublishedFrontierKeepsExactNameBytes(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	s := buildCheckpoint(t, idFrom(t, 0x36), time.Unix(1, 0).UTC(), false)
	var want []worktree.FrontierRecord
	for _, n := range []string{"bad\uFFFD", "bad\xfe", "bad\xff", "bad\xff/below"} {
		want = append(want, worktree.FrontierRecord{Path: rel(t, n), Limit: worktree.LimitSubtree})
	}
	h, err := repo.PutManifest(context.Background(), s.build, s.stream(), scantest.FrontierStream(want...))
	if err != nil {
		t.Fatal(err)
	}
	coverage, err := NewRepo(layout).Coverage(s.id())
	if err != nil {
		t.Fatal(err)
	}
	stream, digest, _ := coverage.Frontier()
	if digest != *h.Frontier {
		t.Fatalf("read digest %+v, published %+v", digest, *h.Frontier)
	}
	cur, err := stream.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cur.Close() }()
	var got []string
	for cur.Next() {
		got = append(got, cur.Record().Path.String())
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("reading the published frontier: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("read %q, want %d names", got, len(want))
	}
	for i := range want {
		if got[i] != want[i].Path.String() {
			t.Errorf("name %d = %q, want %q", i, got[i], want[i].Path)
		}
	}
}
