package frontierjsonl_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/frontierjsonl"
)

var errCorrupt = errors.New("corrupt")

func rec(t *testing.T, p string, limit worktree.CoverageLimit) worktree.FrontierRecord {
	t.Helper()
	r, err := worktree.ParseRelPath(p)
	if err != nil {
		t.Fatal(err)
	}
	return worktree.FrontierRecord{Path: r, Limit: limit}
}

func drain(t *testing.T, s worktree.FrontierStream) ([]string, error) {
	t.Helper()
	cur, err := s.Open(context.Background())
	if err != nil {
		return nil, err
	}
	defer func() { _ = cur.Close() }()
	var out []string
	for cur.Next() {
		out = append(out, cur.Record().Path.String()+":"+cur.Record().Limit.String())
	}
	return out, cur.Err()
}

func TestDecodeLineIsStrict(t *testing.T) {
	for _, line := range []string{
		`{"path":"a","limit":"node","extra":1}`,
		`{"path":"a","limit":"node"} {}`,
		`{"path":"a/../b","limit":"node"}`,
		`{"path":"/abs","limit":"node"}`,
		`{"path":"a","limit":"everything"}`,
		`{"path":"","limit":"node"}`,
	} {
		if _, err := frontierjsonl.DecodeLine([]byte(line)); err == nil {
			t.Errorf("DecodeLine(%s) accepted a malformed record", line)
		}
	}
	got, err := frontierjsonl.DecodeLine([]byte(`{"path":"a b/c","limit":"subtree"}`))
	if err != nil || got.Path.String() != "a b/c" || got.Limit != worktree.LimitSubtree {
		t.Fatalf("DecodeLine = %+v, %v", got, err)
	}
}

func TestSorterSpillsSortsAndCleansUp(t *testing.T) {
	spill := t.TempDir()
	s := frontierjsonl.NewSorter(3, spill)
	names := []string{"z", "a/x", "a-b", "a", "m", "a.c", "b"}
	for _, n := range names {
		if err := s.Add(rec(t, n, worktree.LimitNode)); err != nil {
			t.Fatal(err)
		}
	}
	sorted, err := s.Finish(blake3hash.New())
	if err != nil {
		t.Fatal(err)
	}
	if !sorted.Spilled() {
		t.Fatal("seven records over a three-record buffer did not spill")
	}
	got, err := drain(t, sorted.Stream())
	if err != nil {
		t.Fatal(err)
	}
	want := "a:node a-b:node a.c:node a/x:node b:node m:node z:node"
	if strings.Join(got, " ") != want {
		t.Errorf("order = %v, want %s", got, want)
	}
	if sorted.Digest().Count != len(names) {
		t.Errorf("digest count = %d, want %d", sorted.Digest().Count, len(names))
	}

	mem := frontierjsonl.NewSorter(0, spill)
	for i := len(names) - 1; i >= 0; i-- {
		if err := mem.Add(rec(t, names[i], worktree.LimitNode)); err != nil {
			t.Fatal(err)
		}
	}
	inMemory, err := mem.Finish(blake3hash.New())
	if err != nil {
		t.Fatal(err)
	}
	if inMemory.Digest() != sorted.Digest() {
		t.Error("spilled and in-memory sorts of the same records disagree on the digest")
	}

	if err := sorted.Close(); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(spill); len(entries) != 0 {
		t.Errorf("Close left spill files: %v", entries)
	}
}

func TestSorterRejectsDuplicatesAndAbandonsItsSpill(t *testing.T) {
	spill := t.TempDir()
	s := frontierjsonl.NewSorter(1, spill)
	for _, n := range []string{"a", "b", "a"} {
		if err := s.Add(rec(t, n, worktree.LimitNode)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Finish(blake3hash.New()); err == nil {
		t.Fatal("Finish accepted a duplicate name")
	}
	if entries, _ := os.ReadDir(spill); len(entries) != 0 {
		t.Errorf("a failed Finish left spill files: %v", entries)
	}

	abandoned := frontierjsonl.NewSorter(1, spill)
	for _, n := range []string{"a", "b"} {
		if err := abandoned.Add(rec(t, n, worktree.LimitNode)); err != nil {
			t.Fatal(err)
		}
	}
	if err := abandoned.Close(); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(spill); len(entries) != 0 {
		t.Errorf("an abandoned sorter left spill files: %v", entries)
	}
}

func persisted(t *testing.T, records []worktree.FrontierRecord, mutate func(string) string, declared *worktree.FrontierDigest) frontierjsonl.Stream {
	t.Helper()
	root := t.TempDir()
	var buf bytes.Buffer
	d, err := frontierjsonl.Tee(&buf, worktree.NewFrontierSliceCursor(records), blake3hash.New())
	if err != nil {
		t.Fatal(err)
	}
	if declared != nil {
		d = *declared
	}
	body := buf.String()
	if mutate != nil {
		body = mutate(body)
	}
	abs := filepath.Join(root, "frontier.jsonl")
	if err := os.WriteFile(abs, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return frontierjsonl.Stream{
		Root: root, Abs: abs, Expected: d, Hasher: blake3hash.New(), Label: "test",
		Corrupt: func(e error) error { return fmt.Errorf("%w: %v", errCorrupt, e) },
	}
}

func TestStreamVerifiesItsDeclaredIntegrity(t *testing.T) {
	records := []worktree.FrontierRecord{rec(t, "a", worktree.LimitSubtree), rec(t, "b", worktree.LimitNode), rec(t, "c", worktree.LimitNode)}
	if got, err := drain(t, persisted(t, records, nil, nil)); err != nil || len(got) != 3 {
		t.Fatalf("intact frontier = %v, %v", got, err)
	}
	cases := map[string]frontierjsonl.Stream{
		"truncated": persisted(t, records, func(s string) string { return s[:strings.LastIndex(s[:len(s)-1], "\n")+1] }, nil),
		"swapped limit": persisted(t, records, func(s string) string {
			return strings.Replace(s, `"limit":"subtree"`, `"limit":"node"`, 1)
		}, nil),
		"blank line": persisted(t, records, func(s string) string { return "\n" + s }, nil),
		"out of order": persisted(t, records, func(s string) string {
			lines := strings.SplitAfter(s, "\n")
			lines[0], lines[1] = lines[1], lines[0]
			return strings.Join(lines, "")
		}, nil),
	}
	for name, s := range cases {
		if _, err := drain(t, s); !errors.Is(err, errCorrupt) {
			t.Errorf("%s: err = %v, want corruption", name, err)
		}
	}
	missing := persisted(t, records, nil, nil)
	if err := os.Remove(missing.Abs); err != nil {
		t.Fatal(err)
	}
	if _, err := drain(t, missing); !errors.Is(err, errCorrupt) {
		t.Errorf("missing frontier err = %v, want corruption", err)
	}
}

func exactNames(t *testing.T) []worktree.FrontierRecord {
	t.Helper()
	var out []worktree.FrontierRecord
	for _, n := range []string{"bad\xff", "bad\xfe", "bad\uFFFD", "bad\xff/below", "plain"} {
		out = append(out, rec(t, n, worktree.LimitNode))
	}
	return out
}

func drainRecords(t *testing.T, s worktree.FrontierStream) []worktree.FrontierRecord {
	t.Helper()
	cur, err := s.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cur.Close() }()
	var out []worktree.FrontierRecord
	for cur.Next() {
		out = append(out, cur.Record())
	}
	if err := cur.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestNamesSurviveExactlyThroughMemorySpillAndFile(t *testing.T) {
	want := exactNames(t)
	sortRecords := func(r []worktree.FrontierRecord) []worktree.FrontierRecord {
		out := append([]worktree.FrontierRecord(nil), r...)
		for i := range out {
			for j := i + 1; j < len(out); j++ {
				if out[j].Path.Less(out[i].Path) {
					out[i], out[j] = out[j], out[i]
				}
			}
		}
		return out
	}
	canonical := sortRecords(want)
	oracle, err := worktree.ReduceFrontier(blake3hash.New(), worktree.NewFrontierSliceCursor(canonical))
	if err != nil {
		t.Fatal(err)
	}

	for name, buffer := range map[string]int{"memory": 0, "spill": 2} {
		s := frontierjsonl.NewSorter(buffer, t.TempDir())
		for i := len(want) - 1; i >= 0; i-- {
			if err := s.Add(want[i]); err != nil {
				t.Fatal(err)
			}
		}
		sorted, err := s.Finish(blake3hash.New())
		if err != nil {
			t.Fatal(err)
		}
		if sorted.Spilled() != (buffer > 0) {
			t.Fatalf("%s: spilled = %v", name, sorted.Spilled())
		}
		got := drainRecords(t, sorted.Stream())
		if len(got) != len(canonical) {
			t.Fatalf("%s: %d records, want %d", name, len(got), len(canonical))
		}
		for i := range got {
			if got[i] != canonical[i] {
				t.Errorf("%s: record %d = %q, want %q", name, i, got[i].Path, canonical[i].Path)
			}
		}
		if sorted.Digest() != oracle {
			t.Errorf("%s: digest %+v, want %+v", name, sorted.Digest(), oracle)
		}
		_ = sorted.Close()
	}

	file := persisted(t, canonical, nil, nil)
	got := drainRecords(t, file)
	for i := range got {
		if got[i] != canonical[i] {
			t.Errorf("file: record %d = %q, want %q", i, got[i].Path, canonical[i].Path)
		}
	}
	data, err := os.ReadFile(file.Abs)
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(data, []byte(`"path_base64"`)); n != 3 {
		t.Errorf("%d names travel as exact bytes, want the 3 invalid-UTF-8 names:\n%s", n, data)
	}
}

func TestDecodeLineKeepsOneEncodingPerName(t *testing.T) {
	for _, line := range []string{
		`{"path":"a","path_base64":"YQ==","limit":"node"}`,
		`{"path_base64":"YQ==","limit":"node"}`,
		`{"path_base64":"not base64!","limit":"node"}`,
		`{"limit":"node"}`,
	} {
		if _, err := frontierjsonl.DecodeLine([]byte(line)); err == nil {
			t.Errorf("DecodeLine(%s) accepted a non-canonical name", line)
		}
	}
	got, err := frontierjsonl.DecodeLine([]byte(`{"path_base64":"YmFk/w==","limit":"node"}`))
	if err != nil || got.Path.String() != "bad\xff" {
		t.Fatalf("DecodeLine = %q, %v; want the exact bytes bad\\xff", got.Path, err)
	}
}
