package restore

import (
	"context"
	"strings"

	"awarer/internal/domain/restore"
	"awarer/internal/domain/worktree"
)

type pendingRemoval struct {
	op       restore.PlannedOperation
	conflict bool
	skipped  bool
	unproved bool
}

func (p pendingRemoval) reasons() []restore.Reason {
	var out []restore.Reason
	if p.conflict {
		out = append(out, restore.ReasonPathConflict)
	}
	if p.skipped {
		out = append(out, restore.ReasonSkippedBoundary)
	}
	if p.unproved {
		out = append(out, restore.ReasonAbsenceUnproved)
	}
	return out
}

type removalPreflight struct {
	open     []pendingRemoval
	recorded bool
	front    worktree.FrontierCursor
	pending  worktree.RelPath
	havePend bool
	drained  bool
	settle   func(op restore.PlannedOperation, reasons []restore.Reason) error
}

func newRemovalPreflight(ctx context.Context, current worktree.Coverage, settle func(restore.PlannedOperation, []restore.Reason) error) (*removalPreflight, error) {
	p := &removalPreflight{settle: settle}
	stream, _, ok := current.Frontier()
	if !ok {
		p.drained = true
		return p, nil
	}
	cur, err := stream.Open(ctx)
	if err != nil {
		return nil, err
	}
	p.recorded, p.front = true, worktree.OrderedFrontier(cur)
	return p, nil
}

func removesDirectory(op restore.PlannedOperation) bool {
	return op.Kind() == restore.OpDeleteEmptyDirectory || op.Rank() == restore.RankReplaceDirectory
}

func (p *removalPreflight) reach(ctx context.Context, path worktree.RelPath) error {
	if err := p.feed(ctx, &path); err != nil {
		return err
	}
	return p.pass(path.String())
}

func (p *removalPreflight) candidate(op restore.PlannedOperation) {
	p.open = append(p.open, pendingRemoval{op: op, unproved: !p.recorded})
}

func (p *removalPreflight) hold(path worktree.RelPath, skipped bool) {
	s := path.String()
	for i := range p.open {
		dir := p.open[i].op.Path().String()
		if len(s) <= len(dir) || s[len(dir)] != '/' || !strings.HasPrefix(s, dir) {
			continue
		}
		if skipped {
			p.open[i].skipped = true
		} else {
			p.open[i].conflict = true
		}
	}
}

func (p *removalPreflight) finish(ctx context.Context) error {
	if err := p.feed(ctx, nil); err != nil {
		return err
	}
	for len(p.open) > 0 {
		if err := p.settleTop(); err != nil {
			return err
		}
	}
	return nil
}

func (p *removalPreflight) close() error {
	if p.front == nil {
		return nil
	}
	return p.front.Close()
}

// feed consumes the current frontier through limit, or to its end for a nil limit.
// The end is always reached before a plan is returned: the stream verifies itself
// only there, so stopping at the first conflict would accept a tampered remainder.
func (p *removalPreflight) feed(ctx context.Context, limit *worktree.RelPath) error {
	for !p.drained {
		if !p.havePend {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !p.front.Next() {
				p.drained = true
				return p.front.Err()
			}
			p.pending, p.havePend = p.front.Record().Path, true
		}
		if limit != nil && limit.Less(p.pending) {
			return nil
		}
		p.havePend = false
		if err := p.pass(p.pending.String()); err != nil {
			return err
		}
		p.hold(p.pending, false)
	}
	return nil
}

// pass settles every directory whose descendants can no longer arrive. Canonical
// byte order is not a preorder: `d-` and `d.file` sort between `d` and `d/a`, so a
// directory stays open across every later path that extends its spelling with a
// byte up to '/'. Settling at the first non-descendant would call `d` empty before
// its children were read.
func (p *removalPreflight) pass(path string) error {
	for len(p.open) > 0 {
		dir := p.open[len(p.open)-1].op.Path().String()
		if strings.HasPrefix(path, dir) && (len(path) == len(dir) || path[len(dir)] <= '/') {
			return nil
		}
		if err := p.settleTop(); err != nil {
			return err
		}
	}
	return nil
}

func (p *removalPreflight) settleTop() error {
	top := p.open[len(p.open)-1]
	p.open = p.open[:len(p.open)-1]
	reasons := top.reasons()
	if len(reasons) > 0 {
		p.hold(top.op.Path(), false)
	}
	return p.settle(top.op, reasons)
}
