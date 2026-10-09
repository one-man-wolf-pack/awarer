package frontierjsonl

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"

	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
)

var errDisk = errors.New("disk read failed")

type failingTail struct {
	body string
	sent bool
}

func (f *failingTail) Read(p []byte) (int, error) {
	if f.sent {
		return 0, io.EOF
	}
	f.sent = true
	return copy(p, f.body), errDisk
}

func drainCursor(t *testing.T, body string, r io.Reader) error {
	t.Helper()
	rec, err := DecodeLine([]byte(strings.TrimSpace(body)))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := worktree.ReduceFrontier(blake3hash.New(), worktree.NewFrontierSliceCursor([]worktree.FrontierRecord{rec}))
	if err != nil {
		t.Fatal(err)
	}
	red, err := worktree.NewFrontierReducer(blake3hash.New())
	if err != nil {
		t.Fatal(err)
	}
	c := &cursor{br: bufio.NewReader(r), label: "test", corrupt: func(e error) error { return e }, expected: digest, red: red, verify: true}
	for c.Next() {
	}
	return c.Err()
}

func TestFinalRowWithAReadErrorDoesNotVerify(t *testing.T) {
	body := `{"path":"a","limit":"node"}`
	if err := drainCursor(t, body, &failingTail{body: body}); !errors.Is(err, errDisk) {
		t.Fatalf("drain = %v, want the read error that arrived with the final row", err)
	}
	if err := drainCursor(t, body, strings.NewReader(body)); err != nil {
		t.Fatalf("a final row at EOF without a newline = %v, want a verified stream", err)
	}
}
