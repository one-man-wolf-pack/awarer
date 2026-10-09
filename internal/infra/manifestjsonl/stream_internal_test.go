package manifestjsonl

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
)

var errInjectedRead = errors.New("injected read failure")

type dataThenErr struct {
	data []byte
	err  error
	sent bool
}

func (r *dataThenErr) Read(p []byte) (int, error) {
	if r.sent {
		return 0, io.EOF
	}
	r.sent = true
	return copy(p, r.data), r.err
}

func manifestBytes(t *testing.T, paths ...string) ([]byte, worktree.TreeReduction) {
	t.Helper()
	records := make([]worktree.ManifestRecord, 0, len(paths))
	for _, p := range paths {
		rp, err := worktree.ParseRelPath(p)
		if err != nil {
			t.Fatal(err)
		}
		e, err := worktree.NewDirEntry(rp, worktree.TraversalInfo{})
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, worktree.EntryRecord(e))
	}
	reducer, err := worktree.NewTreeReducer(blake3hash.New())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Tee(&buf, worktree.Ordered(worktree.NewSliceCursor(records)), reducer); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), reducer.Finish()
}

func readerCursor(r io.Reader, expected int) worktree.ManifestCursor {
	return worktree.Ordered(&cursor{
		br:       bufio.NewReader(r),
		label:    "test",
		expected: expected,
		ctx:      context.Background(),
	})
}

func TestCursorTerminalReadError(t *testing.T) {
	full, want := manifestBytes(t, "a", "b")
	unterminated := bytes.TrimSuffix(full, []byte("\n"))
	cases := []struct {
		name    string
		data    []byte
		readErr error
		wantErr error
	}{
		{name: "error with final row", data: unterminated, readErr: errInjectedRead, wantErr: errInjectedRead},
		{name: "error before any row", readErr: errInjectedRead, wantErr: errInjectedRead},
		{name: "clean eof with trailing newline", data: full, readErr: io.EOF},
		{name: "clean eof without trailing newline", data: unterminated, readErr: io.EOF},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			red, err := worktree.ReduceCursor(blake3hash.New(),
				readerCursor(&dataThenErr{data: tc.data, err: tc.readErr}, want.Count))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ReduceCursor err = %v, want %v", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("ReduceCursor: %v", err)
			} else if red != want {
				t.Fatalf("reduction = %+v, want %+v", red, want)
			}

			reducer, err := worktree.NewTreeReducer(blake3hash.New())
			if err != nil {
				t.Fatal(err)
			}
			cur := readerCursor(&dataThenErr{data: tc.data, err: tc.readErr}, want.Count)
			defer func() { _ = cur.Close() }()
			err = Tee(io.Discard, cur, reducer)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Tee err = %v, want %v", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("Tee: %v", err)
			}
		})
	}
}
