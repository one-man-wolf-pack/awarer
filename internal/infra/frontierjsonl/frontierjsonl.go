package frontierjsonl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"unicode/utf8"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/fsx"
)

// line carries a name as a JSON string only when it is valid UTF-8; encoding/json
// would silently replace invalid bytes with U+FFFD and merge distinct names, so
// any other name travels as its exact bytes in path_base64. Exactly one is set.
type line struct {
	Path       string `json:"path,omitempty"`
	PathBase64 string `json:"path_base64,omitempty"`
	Limit      string `json:"limit"`
}

func encode(r worktree.FrontierRecord) line {
	p := r.Path.String()
	if utf8.ValidString(p) {
		return line{Path: p, Limit: r.Limit.String()}
	}
	return line{PathBase64: base64.StdEncoding.EncodeToString([]byte(p)), Limit: r.Limit.String()}
}

func (l line) name() (string, error) {
	switch {
	case l.Path != "" && l.PathBase64 == "":
		if !utf8.ValidString(l.Path) {
			return "", fmt.Errorf("frontier path is not valid UTF-8")
		}
		return l.Path, nil
	case l.Path == "" && l.PathBase64 != "":
		raw, err := base64.StdEncoding.Strict().DecodeString(l.PathBase64)
		if err != nil {
			return "", fmt.Errorf("frontier path_base64: %w", err)
		}
		if utf8.Valid(raw) {
			return "", fmt.Errorf("frontier path_base64 carries a valid UTF-8 name")
		}
		return string(raw), nil
	default:
		return "", fmt.Errorf("frontier record must carry exactly one of path and path_base64")
	}
}

func DecodeLine(data []byte) (worktree.FrontierRecord, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var l line
	if err := dec.Decode(&l); err != nil {
		return worktree.FrontierRecord{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return worktree.FrontierRecord{}, fmt.Errorf("trailing data after frontier record")
	}
	name, err := l.name()
	if err != nil {
		return worktree.FrontierRecord{}, err
	}
	p, err := worktree.ParseRelPath(name)
	if err != nil {
		return worktree.FrontierRecord{}, err
	}
	if p.String() != name {
		return worktree.FrontierRecord{}, fmt.Errorf("frontier path %q is not canonical", name)
	}
	limit, err := worktree.ParseCoverageLimit(l.Limit)
	if err != nil {
		return worktree.FrontierRecord{}, err
	}
	rec := worktree.FrontierRecord{Path: p, Limit: limit}
	return rec, rec.Validate()
}

func Tee(w io.Writer, cur worktree.FrontierCursor, h hashing.Hasher) (worktree.FrontierDigest, error) {
	red, err := worktree.NewFrontierReducer(h)
	if err != nil {
		return worktree.FrontierDigest{}, err
	}
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	for cur.Next() {
		rec := cur.Record()
		if err := red.Add(rec); err != nil {
			return worktree.FrontierDigest{}, err
		}
		if err := enc.Encode(encode(rec)); err != nil {
			return worktree.FrontierDigest{}, fmt.Errorf("encoding frontier record %q: %w", rec.Path, err)
		}
	}
	if err := cur.Err(); err != nil {
		return worktree.FrontierDigest{}, err
	}
	if err := bw.Flush(); err != nil {
		return worktree.FrontierDigest{}, err
	}
	return red.Finish(), nil
}

type Stream struct {
	Root     string
	Abs      string
	Expected worktree.FrontierDigest
	Hasher   hashing.Hasher
	Label    string
	Corrupt  func(error) error
}

func (s Stream) corrupt(err error) error {
	if s.Corrupt == nil {
		return err
	}
	return s.Corrupt(err)
}

func (s Stream) Open(ctx context.Context) (worktree.FrontierCursor, error) {
	rel, err := fsx.RelUnder(s.Root, s.Abs)
	if err != nil {
		return nil, err
	}
	f, err := fsx.OpenNoFollowAt(s.Root, rel)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			return nil, s.corrupt(fmt.Errorf("%s exclusion frontier is missing", s.Label))
		case fsx.IsSymlinkOpenRejection(err):
			return nil, s.corrupt(fmt.Errorf("%s is reached through a symlink", s.Abs))
		case errors.Is(err, fsx.ErrNotDirectory):
			return nil, s.corrupt(fmt.Errorf("%s is not a directory", s.Abs))
		default:
			return nil, err
		}
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
	red, err := worktree.NewFrontierReducer(s.Hasher)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &cursor{
		f: f, br: bufio.NewReader(f), ctx: ctx, label: s.Label,
		corrupt: s.corrupt, expected: s.Expected, red: red, verify: true,
	}, nil
}

type cursor struct {
	f        *os.File
	br       *bufio.Reader
	ctx      context.Context
	label    string
	corrupt  func(error) error
	expected worktree.FrontierDigest
	red      *worktree.FrontierReducer
	verify   bool

	cur     worktree.FrontierRecord
	line    int
	err     error
	pending error
	done    bool
	closed  bool
}

func (c *cursor) Next() bool {
	if c.done {
		return false
	}
	if c.ctx != nil {
		if err := c.ctx.Err(); err != nil {
			c.fail(err)
			return false
		}
	}
	if c.pending != nil {
		c.fail(c.pending)
		return false
	}
	data, rerr := c.br.ReadBytes('\n')
	trimmed := bytes.TrimRight(data, "\n")
	if len(trimmed) == 0 {
		if rerr == io.EOF {
			c.finish()
			return false
		}
		if rerr != nil {
			c.fail(rerr)
			return false
		}
		c.line++
		c.fail(c.corrupt(fmt.Errorf("%s frontier line %d is blank", c.label, c.line)))
		return false
	}
	c.line++
	rec, err := DecodeLine(trimmed)
	if err != nil {
		c.fail(c.corrupt(fmt.Errorf("%s frontier line %d: %v", c.label, c.line, err)))
		return false
	}
	if c.red != nil {
		if err := c.red.Add(rec); err != nil {
			c.fail(c.corrupt(fmt.Errorf("%s frontier line %d: %v", c.label, c.line, err)))
			return false
		}
	}
	if rerr != nil && rerr != io.EOF {
		// The read failed after delivering this row; a later read may report a clean
		// EOF, so the failure is held here or the stream would verify as complete.
		c.pending = rerr
	}
	c.cur = rec
	return true
}

func (c *cursor) finish() {
	c.done = true
	if !c.verify {
		return
	}
	got := c.red.Finish()
	switch {
	case got.Count != c.expected.Count:
		c.err = c.corrupt(fmt.Errorf("%s frontier holds %d records, declared %d", c.label, got.Count, c.expected.Count))
	case got.Hash != c.expected.Hash:
		c.err = c.corrupt(fmt.Errorf("%s frontier digest %s does not match declared %s", c.label, got.Hash, c.expected.Hash))
	}
}

func (c *cursor) fail(err error) {
	c.err = err
	c.done = true
}

func (c *cursor) Record() worktree.FrontierRecord { return c.cur }
func (c *cursor) Err() error                      { return c.err }

func (c *cursor) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	return c.f.Close()
}
