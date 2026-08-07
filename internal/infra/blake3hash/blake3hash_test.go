package blake3hash

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// Known-answer vectors from the BLAKE3 reference test suite. They are the
// independent oracle for the one local primitive: a hasher that silently changed
// what it computes would still be self-consistent, but it could not reproduce
// these.
func TestHashReaderKnownAnswers(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"", "blake3:af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9adc112b7cc9a93cae41f3262"},
		{"abc", "blake3:6437b3ac38465133ffb63b75273a8db548c558465d79db03fd359c6cd5bd9d85"},
	}
	for _, c := range cases {
		h := New()
		got, err := h.HashReader(strings.NewReader(c.input))
		if err != nil {
			t.Fatalf("HashReader error: %v", err)
		}
		if got.String() != c.want {
			t.Errorf("hash of %q = %q, want %q", c.input, got.String(), c.want)
		}
	}
}

// TestStreamingEqualsOneShot proves the streaming io.Copy path produces the same
// digest as hashing the whole buffer at once, for a payload large enough to span
// many internal blocks.
func TestStreamingEqualsOneShot(t *testing.T) {
	payload := bytes.Repeat([]byte("the quick brown fox\n"), 100_000) // ~2 MiB

	h := New()
	oneShot, err := h.HashReader(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("HashReader(one-shot) error: %v", err)
	}
	// A reader that yields a few bytes at a time forces many Copy iterations.
	chunked, err := h.HashReader(&chunkReader{data: payload, chunk: 7})
	if err != nil {
		t.Fatalf("HashReader(chunked) error: %v", err)
	}
	if oneShot.String() != chunked.String() {
		t.Errorf("chunked %q != one-shot %q", chunked.String(), oneShot.String())
	}
}

func TestHashBytesMatchesHashReader(t *testing.T) {
	payload := []byte("tree canonical bytes")
	h := New()
	tree := h.HashBytes(payload)
	content, err := h.HashReader(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("HashReader error: %v", err)
	}
	// TreeHash and ContentHash are distinct types, but over the same bytes their
	// rendered "blake3:hex" forms must match.
	if tree.String() != content.String() {
		t.Errorf("HashBytes %q != HashReader %q", tree.String(), content.String())
	}
}

func TestHashReaderPropagatesReadError(t *testing.T) {
	h := New()
	if _, err := h.HashReader(errReader{}); err == nil {
		t.Fatalf("HashReader(errReader) = nil error, want propagation")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// chunkReader yields at most chunk bytes per Read, forcing io.Copy through many
// iterations so the streaming path is genuinely exercised.
type chunkReader struct {
	data  []byte
	chunk int
	pos   int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	n := c.chunk
	if n > len(p) {
		n = len(p)
	}
	if c.pos+n > len(c.data) {
		n = len(c.data) - c.pos
	}
	copy(p, c.data[c.pos:c.pos+n])
	c.pos += n
	return n, nil
}
