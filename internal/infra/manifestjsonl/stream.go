package manifestjsonl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"awarer/internal/domain/worktree"
	"awarer/internal/infra/fsx"
)

// Tee streams a manifest cursor into w as JSONL in canonical path order — one
// record per line — while folding each record into reducer, so the manifest is
// persisted and its derived fields (tree hash, stats, count) are computed in a
// single pass without materializing the manifest. The reducer validates each
// record and rejects out-of-order or duplicate paths, so a corrupt or hostile
// stream fails the write. json.Encoder writes each record followed by a newline,
// producing the same JSONL the reader parses line by line.
func Tee(w io.Writer, cur worktree.ManifestCursor, reducer *worktree.TreeReducer) error {
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	for cur.Next() {
		rec := cur.Record()
		if err := reducer.Add(rec); err != nil {
			return err
		}
		var line Line
		if e, ok := rec.Entry(); ok {
			ed := EncodeEntry(e)
			line.Entry = &ed
		} else if sk, ok := rec.Skipped(); ok {
			sd := EncodeSkipped(sk)
			line.Skipped = &sd
		}
		if err := enc.Encode(line); err != nil {
			return fmt.Errorf("encoding manifest record %q: %w", rec.Path(), err)
		}
	}
	if err := cur.Err(); err != nil {
		return err
	}
	return bw.Flush()
}

// DecodeLine parses one manifest.jsonl line into a manifest record. It is strict
// (unknown fields rejected, no trailing data, exactly one of entry/skipped) and
// rebuilds the record through the domain constructors.
func DecodeLine(data []byte) (worktree.ManifestRecord, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var line Line
	if err := dec.Decode(&line); err != nil {
		return worktree.ManifestRecord{}, err
	}
	// Require a clean end of input after the single object: a second decode must
	// report io.EOF. More() is not strict enough — trailing structural bytes like a
	// stray "]" would slip past it — so a manifest line that is anything other than
	// exactly one JSON object is rejected as corrupt structure.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return worktree.ManifestRecord{}, fmt.Errorf("manifest line has trailing data")
	}
	switch {
	case line.Entry != nil && line.Skipped != nil:
		return worktree.ManifestRecord{}, fmt.Errorf("manifest line carries both an entry and a skip")
	case line.Entry != nil:
		e, err := DecodeEntry(*line.Entry)
		if err != nil {
			return worktree.ManifestRecord{}, err
		}
		return worktree.EntryRecord(e), nil
	case line.Skipped != nil:
		sk, err := DecodeSkipped(*line.Skipped)
		if err != nil {
			return worktree.ManifestRecord{}, err
		}
		return worktree.SkippedRecord(sk), nil
	default:
		return worktree.ManifestRecord{}, fmt.Errorf("manifest line carries neither an entry nor a skip")
	}
}

// Stream opens canonical manifest cursors over a manifest.jsonl file. It holds
// the project root (the no-follow anchor), the absolute manifest path, the record
// count expected by the manifest's owning record (so a truncated manifest is
// caught at EOF), a Label naming the owner for error messages (e.g. "checkpoint
// abc123" or "run abc123"), and Corrupt — the store-specific wrapper that tags a
// structural error with that store's ErrCorruptStore sentinel. Corrupt is nil for
// callers that want the raw structural error.
type Stream struct {
	Root     string
	Abs      string
	Expected int
	Label    string
	Corrupt  func(error) error
}

// corrupt wraps a structural error with the store's sentinel, or returns it
// unchanged when no wrapper was provided.
func (s Stream) corrupt(err error) error {
	if s.Corrupt == nil {
		return err
	}
	return s.Corrupt(err)
}

// manifestMissing marks a "manifest payload is missing" error. It carries the store's
// corruption error as its message and unwrap target (so integrity callers still match
// ErrCorruptStore) while also matching worktree.ErrManifestMissing, so a payload-missing
// case is distinguishable from corrupt content through a single, clean error.
type manifestMissing struct{ err error }

func (m manifestMissing) Error() string        { return m.err.Error() }
func (m manifestMissing) Unwrap() error        { return m.err }
func (m manifestMissing) Is(target error) bool { return target == worktree.ErrManifestMissing }

// Open returns a fresh ordered cursor over the manifest. The file descriptor is
// owned by the cursor and released on Close.
func (s Stream) Open(ctx context.Context) (worktree.ManifestCursor, error) {
	rel, err := fsx.RelUnder(s.Root, s.Abs)
	if err != nil {
		return nil, err
	}
	f, err := fsx.OpenNoFollowAt(s.Root, rel)
	if err != nil {
		// A Stream is only built for a committed record whose owner exists, so a
		// missing manifest is corrupt durable state — the owner promised records that
		// are gone — not an ordinary "not found".
		if errors.Is(err, os.ErrNotExist) {
			// Tag a gone manifest both as this store's corruption (integrity callers treat
			// a partial store as damage) and as a payload-missing fact, so a consumer can
			// tell an absent payload from corrupt content without a second, noisier line.
			return nil, manifestMissing{err: s.corrupt(fmt.Errorf("%s manifest is missing", s.Label))}
		}
		return nil, s.openErr(err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, s.corrupt(fmt.Errorf("%s is not a regular file (%s)", s.Abs, info.Mode().Type()))
	}
	return worktree.Ordered(&cursor{
		f:        f,
		br:       bufio.NewReader(f),
		label:    s.Label,
		corrupt:  s.corrupt,
		expected: s.Expected,
		ctx:      ctx,
	}), nil
}

// openErr maps a no-follow rejection on the manifest path to a corrupt-store
// error, passing any other error (a genuine I/O failure) through unchanged.
func (s Stream) openErr(err error) error {
	switch {
	case fsx.IsSymlinkOpenRejection(err):
		return s.corrupt(fmt.Errorf("%s is reached through a symlink", s.Abs))
	case errors.Is(err, fsx.ErrNotDirectory):
		return s.corrupt(fmt.Errorf("%s is not a directory", s.Abs))
	default:
		return err
	}
}

// cursor is a pull cursor over a manifest.jsonl file. It decodes one record per
// line, fails at the first malformed line with its line number, and verifies the
// record count against the owner's declared count at clean EOF so a truncated
// manifest is not silently shortened.
type cursor struct {
	f        *os.File
	br       *bufio.Reader
	label    string
	corrupt  func(error) error
	expected int
	ctx      context.Context

	cur    worktree.ManifestRecord
	line   int
	count  int
	err    error
	done   bool
	closed bool
}

func (c *cursor) Next() bool {
	if c.done {
		return false
	}
	if err := c.ctx.Err(); err != nil {
		c.fail(err)
		return false
	}
	data, rerr := c.br.ReadBytes('\n')
	trimmed := bytes.TrimRight(data, "\n")
	if len(trimmed) == 0 {
		// No more content. A clean EOF here means the previous line ended the file;
		// verify the count. Any non-EOF read error is surfaced.
		if rerr == io.EOF {
			if c.count != c.expected {
				c.fail(c.corrupt(fmt.Errorf("%s manifest holds %d records, declared %d", c.label, c.count, c.expected)))
				return false
			}
			c.done = true
			return false
		}
		if rerr != nil {
			c.fail(rerr)
			return false
		}
		// A blank line in the middle of the manifest is corrupt structure.
		c.line++
		c.fail(c.corrupt(fmt.Errorf("%s manifest line %d is blank", c.label, c.line)))
		return false
	}
	c.line++
	rec, derr := DecodeLine(trimmed)
	if derr != nil {
		c.fail(c.corrupt(fmt.Errorf("%s manifest line %d: %v", c.label, c.line, derr)))
		return false
	}
	c.cur = rec
	c.count++
	// A read error that arrived with this final line (data but no trailing newline,
	// rerr != io.EOF) is left for the next Next: the buffered reader re-returns it,
	// so this record is yielded cleanly and the error surfaces only once Next is
	// false — keeping Err nil while Next is true.
	return true
}

func (c *cursor) fail(err error) {
	c.err = err
	c.done = true
}

func (c *cursor) Record() worktree.ManifestRecord { return c.cur }

func (c *cursor) Err() error { return c.err }

func (c *cursor) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	return c.f.Close()
}
