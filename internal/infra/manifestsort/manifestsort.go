// Package manifestsort externally sorts worktree manifest records into canonical
// RelPath order with bounded memory.
//
// The scanner's filesystem walker yields nodes in directory (depth-first) order,
// which is not the canonical RelPath order the tree hash, manifest storage, and
// comparison merge all require. Historically the scanner collected every record
// into a slice and sorted it in memory — fine for small repositories, but O(paths)
// memory for large ones, which was the last unbounded accumulation on the scan hot
// path.
//
// A Sorter buffers records and sorts them in memory while they fit under a
// configurable cap (the common case: no temp files, no I/O, behavior identical to
// the old in-memory sort). When a scan exceeds the cap it spills sorted runs to
// temporary files and k-way merges them on Finish, so peak memory stays bounded by
// the buffer plus one record per spilled run regardless of how many paths the
// worktree holds. Either way Finish folds the ordered records through a TreeReducer
// in a single pass, so the tree hash and stats are derived from the same sequence
// that backs the manifest.
package manifestsort

import (
	"bufio"
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/manifestjsonl"
)

// DefaultBufferRecords is the in-memory record cap before the sorter spills a sorted
// run to disk. Below it a scan sorts entirely in memory (no temp files, no I/O);
// above it memory stays bounded by the buffer plus the k-way merge front. Callers
// that want a different cap (tests and benchmarks exercising the spill path on small
// fixtures) pass it to New rather than mutating this default.
const DefaultBufferRecords = 200_000

// Sorter accumulates manifest records in arbitrary order and produces them in
// canonical RelPath order with bounded memory. Add records, then call Finish exactly
// once. A Sorter is not safe for concurrent use.
type Sorter struct {
	bufferMax int
	spillRoot string // parent dir for the spill temp dir; "" means the OS temp dir
	buf       []worktree.ManifestRecord
	runs      []string // absolute paths of spilled sorted run files
	dir       string   // temp dir owning the run/output files; "" until first spill
	seq       int
	err       error
}

// New returns a Sorter that spills once its in-memory buffer reaches bufferMax
// records. A non-positive bufferMax uses DefaultBufferRecords.
//
// spillRoot is the awa-owned directory the spill temp files live under (typically
// .awa/store/tmp), so scan spill stays inside the private state directory rather
// than leaking manifest paths into a world-readable system temp dir. An empty
// spillRoot falls back to the OS temp dir — used by tests and benchmarks that have
// no project layout; production callers always pass an awa-owned root.
func New(bufferMax int, spillRoot string) *Sorter {
	if bufferMax <= 0 {
		bufferMax = DefaultBufferRecords
	}
	return &Sorter{bufferMax: bufferMax, spillRoot: spillRoot}
}

// Add buffers one record, spilling a sorted run to disk when the buffer fills. A
// record with no path is a wiring error rejected loudly, since it could never sort
// or compare canonically.
func (s *Sorter) Add(rec worktree.ManifestRecord) error {
	if s.err != nil {
		return s.err
	}
	if rec.IsZero() {
		s.err = fmt.Errorf("manifestsort: record has no path")
		return s.err
	}
	s.buf = append(s.buf, rec)
	if len(s.buf) >= s.bufferMax {
		if err := s.spill(); err != nil {
			s.err = err
			return err
		}
	}
	return nil
}

// spill sorts the current buffer and writes it to a new run file, then resets the
// buffer. It is called when the buffer fills and once more for the tail in Finish.
func (s *Sorter) spill() error {
	if len(s.buf) == 0 {
		return nil
	}
	if s.dir == "" {
		// An awa-owned spillRoot must exist and be private before MkdirTemp writes into
		// it; MkdirTemp does not create parents. TmpDir is a required project dir (init
		// creates it), but a caller may have pointed us at a fresh root, so ensure it
		// here with the private mode rather than assume it.
		if s.spillRoot != "" {
			if err := os.MkdirAll(s.spillRoot, paths.DirPerm); err != nil {
				return fmt.Errorf("manifestsort: creating spill root: %w", err)
			}
		}
		dir, err := os.MkdirTemp(s.spillRoot, "awa-manifestsort-")
		if err != nil {
			return fmt.Errorf("manifestsort: creating temp dir: %w", err)
		}
		s.dir = dir
	}
	sortRecords(s.buf)
	runPath := filepath.Join(s.dir, fmt.Sprintf("run-%06d.jsonl", s.seq))
	s.seq++
	if err := writeRun(runPath, s.buf); err != nil {
		return err
	}
	s.runs = append(s.runs, runPath)
	// Release the buffer's backing array so the spilled records can be collected
	// rather than retained until the next growth.
	s.buf = nil
	return nil
}

// Close removes any temporary run files the sorter spilled. Call it to abandon a
// Sorter that will not be Finished (for example the walk feeding it failed). After a
// successful Finish the returned Sorted owns cleanup instead; calling both is safe
// because removing an already-removed temp dir is a no-op.
func (s *Sorter) Close() error {
	if s.dir == "" {
		return nil
	}
	return os.RemoveAll(s.dir)
}

// Sorted is the ordered result of a Sorter: a re-openable canonical manifest stream
// and the tree reduction folded from the same ordered sequence. When the scan spilled
// to disk, Close removes the temporary files; for an in-memory result Close is a
// no-op. Callers must Close it once the stream is no longer needed.
type Sorted struct {
	stream    worktree.ManifestStream
	reduction worktree.TreeReduction
	dir       string // temp dir to remove on Close; "" for an in-memory result
}

// Stream returns the re-openable canonical manifest stream.
func (s Sorted) Stream() worktree.ManifestStream { return s.stream }

// Reduction returns the tree hash, stats, taint, and record count folded from the
// ordered records.
func (s Sorted) Reduction() worktree.TreeReduction { return s.reduction }

// Spilled reports whether the sort spilled to disk (temp files back the stream).
func (s Sorted) Spilled() bool { return s.dir != "" }

// Close removes any temporary files backing the stream. It is safe to call on an
// in-memory result (a no-op) and safe to call more than once.
func (s Sorted) Close() error {
	if s.dir == "" {
		return nil
	}
	return os.RemoveAll(s.dir)
}

// Finish sorts and reduces the accumulated records with hasher and returns the
// ordered result. When nothing spilled it sorts in memory and returns an in-memory
// stream (no temp files); otherwise it flushes the tail and k-way merges the runs
// into a single sorted manifest file, folding the tree reduction during the merge.
// The Sorter must not be used after Finish. Finish is self-contained: it either
// returns a Sorted that owns cleanup of the temp files, or it removes them itself and
// returns an error — so a failed Finish never leaks the spill directory.
func (s *Sorter) Finish(hasher hashing.Hasher) (Sorted, error) {
	if s.err != nil {
		// A prior Add may have spilled runs before failing; clean them up.
		_ = os.RemoveAll(s.dir)
		return Sorted{}, s.err
	}
	if len(s.runs) == 0 {
		// Fast path: everything fit in memory. Sort in place and reduce over the sorted
		// slice — identical to the pre-spill behavior, with no temp files.
		sortRecords(s.buf)
		records := s.buf
		red, err := worktree.ReduceCursor(hasher, worktree.Ordered(worktree.NewSliceCursor(records)))
		if err != nil {
			return Sorted{}, err
		}
		return Sorted{stream: sliceStream{records: records}, reduction: red}, nil
	}

	// Spill path: flush the tail, then merge all runs into one sorted output file while
	// folding the reduction in the same pass.
	if err := s.spill(); err != nil {
		_ = os.RemoveAll(s.dir)
		return Sorted{}, err
	}
	merged, err := newMergeCursor(s.runs)
	if err != nil {
		_ = os.RemoveAll(s.dir)
		return Sorted{}, err
	}
	reducer, err := worktree.NewTreeReducer(hasher)
	if err != nil {
		_ = merged.Close()
		_ = os.RemoveAll(s.dir)
		return Sorted{}, err
	}
	outPath := filepath.Join(s.dir, "manifest.jsonl")
	out, err := createPrivate(outPath)
	if err != nil {
		_ = merged.Close()
		_ = os.RemoveAll(s.dir)
		return Sorted{}, fmt.Errorf("manifestsort: creating merged output: %w", err)
	}
	teeErr := manifestjsonl.Tee(out, worktree.Ordered(merged), reducer)
	closeErr := out.Close()
	_ = merged.Close()
	if teeErr != nil {
		_ = os.RemoveAll(s.dir)
		return Sorted{}, teeErr
	}
	if closeErr != nil {
		_ = os.RemoveAll(s.dir)
		return Sorted{}, closeErr
	}
	red := reducer.Finish()
	// Transfer temp-dir ownership to the returned Sorted: clearing s.dir makes a later
	// Sorter.Close() a no-op, so the two cannot both remove the dir. The stream captures
	// the paths as string copies, so clearing s.dir does not affect it.
	dir := s.dir
	s.dir = ""
	stream := manifestjsonl.Stream{
		Root:     dir,
		Abs:      outPath,
		Expected: red.Count,
		Label:    "scan manifest",
	}
	return Sorted{stream: stream, reduction: red, dir: dir}, nil
}

// sortRecords sorts records by canonical RelPath order in place.
func sortRecords(records []worktree.ManifestRecord) {
	sort.Slice(records, func(i, j int) bool {
		return records[i].Path().Less(records[j].Path())
	})
}

// lineOf renders a record as its persisted JSONL line form.
func lineOf(rec worktree.ManifestRecord) manifestjsonl.Line {
	if e, ok := rec.Entry(); ok {
		d := manifestjsonl.EncodeEntry(e)
		return manifestjsonl.Line{Entry: &d}
	}
	sk, _ := rec.Skipped()
	d := manifestjsonl.EncodeSkipped(sk)
	return manifestjsonl.Line{Skipped: &d}
}

// createPrivate creates (or truncates) a spill file owner-private, so the worktree
// manifest data it holds does not depend on the spill directory's mode alone —
// matching the owner-private policy every other awa-owned file follows.
func createPrivate(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, paths.FilePerm)
}

// writeRun writes an already-sorted slice of records to a run file as JSONL.
func writeRun(path string, records []worktree.ManifestRecord) error {
	f, err := createPrivate(path)
	if err != nil {
		return fmt.Errorf("manifestsort: creating run file: %w", err)
	}
	bw := bufio.NewWriter(f)
	enc := json.NewEncoder(bw)
	for _, rec := range records {
		if err := enc.Encode(lineOf(rec)); err != nil {
			_ = f.Close()
			return fmt.Errorf("manifestsort: writing run record %q: %w", rec.Path(), err)
		}
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// sliceStream is the in-memory re-openable stream over an already-sorted record
// slice. Each Open yields a fresh ordered cursor; Ordered re-checks the ordering and
// rejects a duplicate path, so a wiring bug fails loudly rather than corrupting a
// hash.
type sliceStream struct {
	records []worktree.ManifestRecord
}

func (s sliceStream) Open(context.Context) (worktree.ManifestCursor, error) {
	return worktree.Ordered(worktree.NewSliceCursor(s.records)), nil
}

// runReader reads one sorted run file, decoding a record at a time.
type runReader struct {
	f    *os.File
	br   *bufio.Reader
	cur  worktree.ManifestRecord
	path string
}

func openRun(path string) (*runReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("manifestsort: opening run file: %w", err)
	}
	return &runReader{f: f, br: bufio.NewReader(f), path: path}, nil
}

// advance decodes the next record into cur. It returns false at a clean end of
// file, and a non-nil error on a malformed line — these are our own temp files, so a
// decode failure is corruption of internal state, surfaced loudly rather than hidden.
func (r *runReader) advance() (bool, error) {
	line, err := r.br.ReadBytes('\n')
	trimmed := line
	if n := len(trimmed); n > 0 && trimmed[n-1] == '\n' {
		trimmed = trimmed[:n-1]
	}
	if len(trimmed) == 0 {
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return false, fmt.Errorf("manifestsort: blank line in run file %s", r.path)
	}
	rec, derr := manifestjsonl.DecodeLine(trimmed)
	if derr != nil {
		return false, fmt.Errorf("manifestsort: decoding run file %s: %w", r.path, derr)
	}
	r.cur = rec
	return true, nil
}

func (r *runReader) Close() error { return r.f.Close() }

// mergeCursor k-way merges sorted run files into one canonical record stream using a
// min-heap keyed by RelPath. It holds one record per run, so memory scales with the
// number of runs, not the number of records.
type mergeCursor struct {
	h      *runHeap
	cur    worktree.ManifestRecord
	err    error
	done   bool
	closed bool
	all    []*runReader
}

func newMergeCursor(runPaths []string) (*mergeCursor, error) {
	m := &mergeCursor{h: &runHeap{}}
	for _, p := range runPaths {
		rr, err := openRun(p)
		if err != nil {
			_ = m.closeAll()
			return nil, err
		}
		m.all = append(m.all, rr)
		ok, aerr := rr.advance()
		if aerr != nil {
			_ = m.closeAll()
			return nil, aerr
		}
		if ok {
			heap.Push(m.h, rr)
		}
	}
	return m, nil
}

func (m *mergeCursor) Next() bool {
	if m.done {
		return false
	}
	if m.h.Len() == 0 {
		m.done = true
		return false
	}
	rr := heap.Pop(m.h).(*runReader)
	m.cur = rr.cur
	ok, err := rr.advance()
	if err != nil {
		m.err = err
		m.done = true
		return false
	}
	if ok {
		heap.Push(m.h, rr)
	}
	return true
}

func (m *mergeCursor) Record() worktree.ManifestRecord { return m.cur }
func (m *mergeCursor) Err() error                      { return m.err }

func (m *mergeCursor) Close() error {
	if m.closed {
		return nil
	}
	m.closed = true
	return m.closeAll()
}

func (m *mergeCursor) closeAll() error {
	var first error
	for _, rr := range m.all {
		if err := rr.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// runHeap is a min-heap of run readers ordered by their current record's path, so the
// merge always advances the run holding the smallest remaining path.
type runHeap []*runReader

func (h runHeap) Len() int { return len(h) }
func (h runHeap) Less(i, j int) bool {
	return h[i].cur.Path().Less(h[j].cur.Path())
}
func (h runHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *runHeap) Push(x any)   { *h = append(*h, x.(*runReader)) }
func (h *runHeap) Pop() any {
	old := *h
	n := len(old)
	last := old[n-1]
	*h = old[:n-1]
	return last
}
