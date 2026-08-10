package blake3hash

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"awarer/internal/domain/hashing"
)

// abcDigest is the BLAKE3 reference answer for "abc", reused wherever a test needs
// a digest it did not compute with the code under test.
const abcDigest = "blake3:6437b3ac38465133ffb63b75273a8db548c558465d79db03fd359c6cd5bd9d85"

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
		{"abc", abcDigest},
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

// TestHashReaderPropagatesReadError covers the reader that fails before yielding
// anything, the partition its mid-stream sibling below cannot reach.
func TestHashReaderPropagatesReadError(t *testing.T) {
	h := New()
	_, err := h.HashReader(errReader{})
	if err == nil {
		t.Fatalf("HashReader(errReader) = nil error, want propagation")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("error %v does not wrap the read cause", err)
	}
}

// TestHashReaderRecoversAfterFailedRead covers what a shared, reused hasher must
// still guarantee after a file goes wrong halfway through: the cause survives, the
// reader is the caller's to close, and the next call over the same hasher is not
// contaminated by whatever the failed one left in its scratch.
func TestHashReaderRecoversAfterFailedRead(t *testing.T) {
	h := New()

	failing := &failingReader{prefix: bytes.Repeat([]byte("z"), 4096), err: errBoom}
	_, err := h.HashReader(failing)
	if err == nil {
		t.Fatalf("HashReader(failingReader) = nil error, want propagation")
	}
	if !errors.Is(err, errBoom) {
		t.Errorf("error %v does not wrap the read cause", err)
	}
	// The wrapping text is the packet's named contract, not incidental phrasing.
	if want := "hashing content: "; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("error %q does not start with %q", err.Error(), want)
	}
	if failing.closed {
		t.Error("HashReader closed a reader it does not own")
	}

	got, err := h.HashReader(strings.NewReader("abc"))
	if err != nil {
		t.Fatalf("HashReader after failure: %v", err)
	}
	if got.String() != abcDigest {
		t.Errorf("hash after failed read = %q, want %q", got.String(), abcDigest)
	}
}

// TestHashReaderHashesActualFile is the file-shaped digest case. Every other reader
// here is in-memory, and an in-memory reader takes a different io.Copy branch, so
// without this the path the scanner actually runs would have no digest oracle.
func TestHashReaderHashesActualFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	got, err := New().HashReader(f)
	if err != nil {
		t.Fatalf("HashReader(file) error: %v", err)
	}
	if got.String() != abcDigest {
		t.Errorf("hash of file = %q, want %q", got.String(), abcDigest)
	}
}

// TestHashReaderPrefersSourceWriteTo owns the dispatch property: a source that can
// write itself must keep doing so. Giving the digest a ReadFrom would otherwise be
// free to intercept readers that were already handing over their whole buffer, and
// nothing about the resulting digest would reveal the regression.
func TestHashReaderPrefersSourceWriteTo(t *testing.T) {
	src := &writerToReader{data: []byte("abc")}
	got, err := New().HashReader(src)
	if err != nil {
		t.Fatalf("HashReader(writerToReader) error: %v", err)
	}
	if got.String() != abcDigest {
		t.Errorf("hash = %q, want %q", got.String(), abcDigest)
	}
	if src.readCalled {
		t.Error("Read was selected for a source that implements io.WriterTo")
	}
}

// TestReleaseCopyBufferClearsScratch observes the clearing directly, because no
// digest can: a buffer returned dirty computes exactly the same hashes. The buffer
// is this test's own, and the package runs nothing in parallel, so reading it back
// after release races with no pool user.
func TestReleaseCopyBufferClearsScratch(t *testing.T) {
	buf := make([]byte, copyBufferSize)
	for i := range buf {
		buf[i] = 0xAB
	}

	releaseCopyBuffer(&buf)

	for i, b := range buf {
		if b != 0 {
			t.Fatalf("byte %d = %#x after release, want the whole buffer cleared", i, b)
		}
	}
}

// TestHashReaderConcurrentCallsShareHasher proves one Hasher stays shareable: each
// call needs scratch of its own for its whole duration, and a hasher that leaked
// state or handed the same buffer to two callers would produce a wrong digest here
// rather than a detectable error. chunkReader, not bytes.Reader, so the calls
// genuinely contend for scratch instead of writing straight through.
func TestHashReaderConcurrentCallsShareHasher(t *testing.T) {
	h := New()
	payloads := [][]byte{
		bytes.Repeat([]byte("alpha\n"), 40_000),
		bytes.Repeat([]byte("beta\n"), 50_000),
		bytes.Repeat([]byte("gamma\n"), 30_000),
		[]byte("abc"),
		nil,
	}

	want := make([]string, len(payloads))
	for i, p := range payloads {
		got, err := h.HashReader(&chunkReader{data: p, chunk: 4093})
		if err != nil {
			t.Fatalf("sequential HashReader(%d): %v", i, err)
		}
		want[i] = got.String()
	}

	const rounds = 8
	got := make([]hashing.ContentHash, len(payloads)*rounds)
	errs := make([]error, len(payloads)*rounds)
	var wg sync.WaitGroup
	for r := range rounds {
		for i, p := range payloads {
			slot := r*len(payloads) + i
			wg.Go(func() {
				got[slot], errs[slot] = h.HashReader(&chunkReader{data: p, chunk: 4093})
			})
		}
	}
	wg.Wait()

	for slot := range got {
		i := slot % len(payloads)
		if errs[slot] != nil {
			t.Errorf("concurrent HashReader(payload %d): %v", i, errs[slot])
			continue
		}
		if got[slot].String() != want[i] {
			t.Errorf("concurrent hash of payload %d = %q, want %q", i, got[slot].String(), want[i])
		}
	}
}

var errBoom = errors.New("read failed mid-stream")

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// failingReader yields real bytes before failing, so the digest has already consumed
// content when the error arrives. It is also an io.Closer purely so the test can see
// whether HashReader closes what it was handed.
type failingReader struct {
	prefix []byte
	pos    int
	err    error
	closed bool
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.pos >= len(f.prefix) {
		return 0, f.err
	}
	n := copy(p, f.prefix[f.pos:])
	f.pos += n
	return n, nil
}

func (f *failingReader) Close() error {
	f.closed = true
	return nil
}

// writerToReader hands its whole payload over through WriteTo and records any use of
// Read, which io.Copy must not select while WriteTo exists.
type writerToReader struct {
	data       []byte
	readCalled bool
}

func (w *writerToReader) Read(p []byte) (int, error) {
	w.readCalled = true
	return 0, io.EOF
}

func (w *writerToReader) WriteTo(dst io.Writer) (int64, error) {
	n, err := dst.Write(w.data)
	return int64(n), err
}

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
