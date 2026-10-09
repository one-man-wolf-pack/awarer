package frontierjsonl

import (
	"bufio"
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/worktree"
)

const DefaultBufferRecords = 200_000

type Sorter struct {
	bufferMax int
	spillRoot string
	buf       []worktree.FrontierRecord
	runs      []string
	dir       string
	seq       int
	err       error
}

func NewSorter(bufferMax int, spillRoot string) *Sorter {
	if bufferMax <= 0 {
		bufferMax = DefaultBufferRecords
	}
	return &Sorter{bufferMax: bufferMax, spillRoot: spillRoot}
}

func (s *Sorter) Add(rec worktree.FrontierRecord) error {
	if s.err != nil {
		return s.err
	}
	if err := rec.Validate(); err != nil {
		s.err = err
		return err
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

func (s *Sorter) spill() error {
	if len(s.buf) == 0 {
		return nil
	}
	if s.dir == "" {
		if s.spillRoot != "" {
			if err := os.MkdirAll(s.spillRoot, paths.DirPerm); err != nil {
				return fmt.Errorf("frontier sort: creating spill root: %w", err)
			}
		}
		dir, err := os.MkdirTemp(s.spillRoot, "awa-frontiersort-")
		if err != nil {
			return fmt.Errorf("frontier sort: creating temp dir: %w", err)
		}
		s.dir = dir
	}
	sortRecords(s.buf)
	run := filepath.Join(s.dir, fmt.Sprintf("run-%06d.jsonl", s.seq))
	s.seq++
	if err := writeRun(run, s.buf); err != nil {
		return err
	}
	s.runs = append(s.runs, run)
	s.buf = nil
	return nil
}

func (s *Sorter) Close() error {
	if s.dir == "" {
		return nil
	}
	return os.RemoveAll(s.dir)
}

type Sorted struct {
	stream worktree.FrontierStream
	digest worktree.FrontierDigest
	dir    string
}

func (s Sorted) Stream() worktree.FrontierStream { return s.stream }

func (s Sorted) Digest() worktree.FrontierDigest { return s.digest }

func (s Sorted) Spilled() bool { return s.dir != "" }

func (s Sorted) Close() error {
	if s.dir == "" {
		return nil
	}
	return os.RemoveAll(s.dir)
}

func (s *Sorter) Finish(h hashing.Hasher) (Sorted, error) {
	if s.err != nil {
		_ = s.Close()
		return Sorted{}, s.err
	}
	if len(s.runs) == 0 {
		sortRecords(s.buf)
		records := s.buf
		s.buf = nil
		d, err := worktree.ReduceFrontier(h, worktree.NewFrontierSliceCursor(records))
		if err != nil {
			return Sorted{}, err
		}
		return Sorted{stream: sliceStream{records: records}, digest: d}, nil
	}
	if err := s.spill(); err != nil {
		_ = s.Close()
		return Sorted{}, err
	}
	merged, err := newMergeCursor(s.runs)
	if err != nil {
		_ = s.Close()
		return Sorted{}, err
	}
	out := filepath.Join(s.dir, "frontier.jsonl")
	f, err := os.OpenFile(out, os.O_RDWR|os.O_CREATE|os.O_TRUNC, paths.FilePerm)
	if err != nil {
		_ = merged.Close()
		_ = s.Close()
		return Sorted{}, err
	}
	d, err := Tee(f, merged, h)
	cerr := merged.Close()
	ferr := f.Close()
	for _, err2 := range []error{cerr, ferr} {
		if err == nil {
			err = err2
		}
	}
	if err != nil {
		_ = s.Close()
		return Sorted{}, err
	}
	for _, r := range s.runs {
		_ = os.Remove(r)
	}
	return Sorted{stream: tempStream{abs: out, digest: d}, digest: d, dir: s.dir}, nil
}

func sortRecords(records []worktree.FrontierRecord) {
	sort.Slice(records, func(i, j int) bool { return records[i].Path.Less(records[j].Path) })
}

func writeRun(path string, records []worktree.FrontierRecord) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, paths.FilePerm)
	if err != nil {
		return fmt.Errorf("frontier sort: creating run file: %w", err)
	}
	bw := bufio.NewWriter(f)
	enc := json.NewEncoder(bw)
	for _, rec := range records {
		if err := enc.Encode(encode(rec)); err != nil {
			_ = f.Close()
			return fmt.Errorf("frontier sort: writing run record %q: %w", rec.Path, err)
		}
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

type sliceStream struct{ records []worktree.FrontierRecord }

func (s sliceStream) Open(context.Context) (worktree.FrontierCursor, error) {
	return worktree.OrderedFrontier(worktree.NewFrontierSliceCursor(s.records)), nil
}

type tempStream struct {
	abs    string
	digest worktree.FrontierDigest
}

func (s tempStream) Open(ctx context.Context) (worktree.FrontierCursor, error) {
	f, err := os.Open(s.abs)
	if err != nil {
		return nil, fmt.Errorf("frontier sort: opening sorted frontier: %w", err)
	}
	return worktree.OrderedFrontier(&cursor{
		f: f, br: bufio.NewReader(f), ctx: ctx, label: "scan",
		corrupt: func(err error) error { return err },
	}), nil
}

type runReader struct {
	c *cursor
}

type mergeCursor struct {
	h    *runHeap
	all  []*runReader
	cur  worktree.FrontierRecord
	err  error
	done bool
}

func newMergeCursor(runs []string) (*mergeCursor, error) {
	m := &mergeCursor{h: &runHeap{}}
	for _, p := range runs {
		f, err := os.Open(p)
		if err != nil {
			_ = m.Close()
			return nil, fmt.Errorf("frontier sort: opening run file: %w", err)
		}
		rr := &runReader{c: &cursor{f: f, br: bufio.NewReader(f), label: "frontier sort run", corrupt: func(err error) error { return err }}}
		m.all = append(m.all, rr)
		if rr.c.Next() {
			heap.Push(m.h, rr)
		} else if err := rr.c.Err(); err != nil {
			_ = m.Close()
			return nil, err
		}
	}
	return m, nil
}

func (m *mergeCursor) Next() bool {
	if m.done || m.h.Len() == 0 {
		m.done = true
		return false
	}
	rr := heap.Pop(m.h).(*runReader)
	m.cur = rr.c.Record()
	if rr.c.Next() {
		heap.Push(m.h, rr)
	} else if err := rr.c.Err(); err != nil {
		m.err, m.done = err, true
		return false
	}
	return true
}

func (m *mergeCursor) Record() worktree.FrontierRecord { return m.cur }
func (m *mergeCursor) Err() error                      { return m.err }

func (m *mergeCursor) Close() error {
	var first error
	for _, rr := range m.all {
		if err := rr.c.Close(); err != nil && first == nil {
			first = err
		}
	}
	m.all = nil
	return first
}

type runHeap []*runReader

func (h runHeap) Len() int           { return len(h) }
func (h runHeap) Less(i, j int) bool { return h[i].c.Record().Path.Less(h[j].c.Record().Path) }
func (h runHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *runHeap) Push(x any)        { *h = append(*h, x.(*runReader)) }
func (h *runHeap) Pop() any {
	old := *h
	n := len(old)
	last := old[n-1]
	*h = old[:n-1]
	return last
}
