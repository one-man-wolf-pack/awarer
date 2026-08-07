package restore

import (
	"context"
	"errors"
	"fmt"
	"os"

	"awarer/internal/app/scanner"
	"awarer/internal/app/state"
	"awarer/internal/domain/restore"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/fsx"
)

// session holds one invocation's resolved evidence and owned scratch. Both preview
// and apply run through it, so the two cannot drift on how the source is resolved,
// how the current worktree is observed, or how the comparison is decided — a
// preview that predicted different work than the apply it advertises would be the
// most dangerous kind of wrong in this command.
type session struct {
	svc *Service
	req Request

	source  *state.ResolvedState
	current scanner.Result
	// currentOpen reports whether current holds an open scan whose temp spill must
	// be released.
	currentOpen bool

	id    restore.OperationID
	spool Spool

	sourceIdentity restore.Source
	compat         compatibility

	// scopeStream bounds a recovery-observation source to the exact path set it
	// proves. It is nil for a checkpoint or run source, whose evidence covers the
	// whole observed scope.
	scopeStream restore.CoveredScopeStream

	// counted facts accumulated while streaming.
	counts   restore.Counts
	boundary restore.Boundary
	samples  []restore.BlockedSample
	// mutating counts the ready operations that change the worktree; it is the
	// spool's population and the commit's denominator.
	mutating int
	// selectionSeen records which explicit path selections produced at least one
	// path inside the evidence, so an operand that named nothing awa can see is
	// explained rather than silently reported as "no changes".
	selectionSeen map[string]bool
	// recovered records that the pre-restore recovery observation was published, so
	// only an outcome that may have mutated the worktree reports one.
	recovered bool
}

// begin resolves the source once and observes the current worktree. mutating says
// whether this invocation may write, and it decides the one thing a preview must not
// have: an operation id with the spool that replays the plan.
//
// The observation itself is identical for both modes — a preview that observed under
// a different policy would not predict the apply it advertises. Apply's extra need,
// the current bytes it must preserve, is served on demand through the WorktreeContent
// port rather than by asking the scan to retain an opener per file.
//
// A preview gets no spool at all. It never replays the plan — it accumulates bounded
// counts and a bounded sample while streaming — so creating one would put a
// directory and a file under .awa/store/tmp for a command whose whole contract is
// that it changes nothing. That is not only a broken promise on paper: it made
// preview fail outright where awa's own temp area is read-only, and left scratch
// state behind if the process was killed mid-plan.
//
// The observation is a separate matter and not this function's to promise: a scan
// larger than the sorter's in-memory buffer spills through the same temp area, as it
// does for every read-only comparison. What is removed here is restore's own scratch.
func (s *Service) begin(ctx context.Context, req Request, mutating bool) (*session, error) {
	sess := &session{svc: s, req: req, selectionSeen: map[string]bool{}}
	for _, p := range req.Selection.Paths() {
		sess.selectionSeen[p.String()] = false
	}

	// One resolution, one identity: from here the invocation acts on a pinned
	// immutable state even if `latest` moves under it.
	src, err := s.deps.Resolver.Resolve(ctx, req.Ref, state.NowContext{Project: req.Project, Config: req.Config})
	if err != nil {
		return nil, err
	}
	sess.source = src
	identity, err := sourceOf(src)
	if err != nil {
		_ = src.Close()
		return nil, err
	}
	sess.sourceIdentity = identity
	if _, scope, ok := src.RecoveryObservation(); ok {
		sess.scopeStream = scope
	}

	cur, err := s.observeCurrent(ctx, req)
	if err != nil {
		_ = src.Close()
		return nil, err
	}
	sess.current, sess.currentOpen = cur, true
	// Compatibility is decided from the two identities themselves — never by
	// reconstructing old config from hashes, and never by assuming the current
	// policy also described the source.
	sess.compat = compatibilityOf(src, cur)

	if !mutating {
		return sess, nil
	}
	id, err := restore.NewOperationID(s.deps.Now().UnixNano(), s.deps.Rand)
	if err != nil {
		sess.close()
		return nil, err
	}
	sess.id = id
	spool, err := s.deps.Spools(id)
	if err != nil {
		sess.close()
		return nil, err
	}
	sess.spool = spool
	return sess, nil
}

// close releases everything the session owns: the resolved source, the current
// observation's temp spill, and the spool. It is idempotent.
func (sess *session) close() {
	if sess.source != nil {
		_ = sess.source.Close()
		sess.source = nil
	}
	if sess.currentOpen {
		_ = sess.current.Close()
		sess.currentOpen = false
	}
	if sess.spool != nil {
		_ = sess.spool.Discard()
		sess.spool = nil
	}
}

// result wraps a validated domain result with the presentation facts the CLI needs
// but the domain does not model.
func (sess *session) result(res restore.ApplyResult) Result {
	out := Result{Result: res}
	if h := sess.source.Header; h != nil {
		out.SourceMessage = h.Message.String()
	}
	if t, ok := sess.source.ObservedAt(); ok {
		out.SourceTime, out.HasSourceTime = t, true
	}
	if !res.Recovery().IsZero() {
		out.RecoveryRef = res.Recovery().BeforeRef()
	}
	return out
}

// plan streams the selected inverse comparison and returns the validated plan.
// Ready mutating operations go to the spool as they are decided; everything else
// contributes to bounded counts and a bounded blocked sample. The operation set is
// never held in memory.
func (sess *session) plan(ctx context.Context) (restore.Plan, error) {
	sess.boundary = restore.Boundary{
		SourceSkipped:    sess.source.Skipped(),
		CurrentSkipped:   sess.current.Stats().Skipped,
		PolicyCompatible: sess.compat.deletionsProven(),
		ScopeBounded:     sess.scopeStream != nil,
	}

	srcCur, err := sess.source.Manifest(ctx)
	if err != nil {
		return restore.Plan{}, err
	}
	defer func() { _ = srcCur.Close() }()

	curCur, err := sess.current.Manifest().Open(ctx)
	if err != nil {
		return restore.Plan{}, err
	}
	defer func() { _ = curCur.Close() }()

	var scopeCur restore.CoveredScopeCursor
	if sess.scopeStream != nil {
		scopeCur, err = sess.scopeStream.Open(ctx)
		if err != nil {
			return restore.Plan{}, err
		}
		defer func() { _ = scopeCur.Close() }()
	}

	m := newMerge(worktree.Ordered(srcCur), worktree.Ordered(curCur), scopeCur)
	for m.next() {
		if err := ctx.Err(); err != nil {
			return restore.Plan{}, err
		}
		if err := sess.decide(m.path, m.source, m.current, m.inScope); err != nil {
			return restore.Plan{}, err
		}
	}
	if err := m.err(); err != nil {
		return restore.Plan{}, err
	}
	if err := sess.explainEmptySelections(); err != nil {
		return restore.Plan{}, err
	}
	if sess.spool != nil {
		if err := sess.spool.Seal(); err != nil {
			return restore.Plan{}, err
		}
	}

	complete := sess.counts.Unavailable() == 0
	return restore.NewPlan(sess.sourceIdentity, sess.req.Selection, sess.counts, sess.boundary, sess.samples, complete)
}

// decide settles one path and folds it into the plan. It is the single place the
// inverse comparison's rules live, so preview counts, spooled work, and the reasons
// output prints all come from one decision.
func (sess *session) decide(path worktree.RelPath, src, cur worktree.ManifestRecord, inScope bool) error {
	if !sess.req.Selection.Covers(path) {
		return nil
	}

	// A recovery-observation source proves something about exactly the paths it
	// recorded. A path outside that set is not evidence at all, so it contributes no
	// operation — and, deliberately, it is not marked as something an operand
	// matched: an operand whose every path fell outside the proven scope produced no
	// evidence, and saying so is explainEmptySelections' job. Marking it here would
	// make "this record proves nothing about your path" indistinguishable from "your
	// path already matches".
	if sess.scopeStream != nil && !inScope {
		return nil
	}
	sess.markSelectionSeen(path)

	desiredNode, reason, ok := entryNode(src)
	if !ok {
		return sess.blocked(path, restore.AbsentNode(), restore.AbsentNode(), reason)
	}
	currentNode, reason, ok := entryNode(cur)
	if !ok {
		return sess.blocked(path, restore.AbsentNode(), desiredNode.state, reason)
	}
	desired, current := desiredNode.state, currentNode.state

	if !desired.Present() && !current.Present() {
		// Neither side has anything here. That happens only for a recovery source's
		// covered path whose entry was absent at capture time and is absent now: the
		// desired end state already holds.
		sess.counts.Equal++
		return nil
	}

	// A deletion needs absence to be *proven*, which a differing scan policy cannot
	// do: the path may simply have been outside the source observation's boundary.
	if !desired.Present() && !sess.compat.deletionsProven() {
		return sess.blocked(path, current, desired, restore.ReasonPolicyIncompatible)
	}

	// Provenance is part of the comparison, not a note beside it. A node state answers
	// "what is here" — kind, content identity, permission bits, link target — and two
	// sides can agree on all of it while describing different worktree shapes: a real
	// file at this path, versus the same bytes reached because a component above it
	// became a symlink, versus the same bytes now two hops away instead of one. The
	// project's own canonical identity separates those (the tree hash folds provenance),
	// so a comparison that collapsed them would report a restore that "already matches" a
	// tree whose form the source never proved, having changed nothing.
	//
	// Whole provenance, not just the followed flag: the link a node was reached through
	// and the number of hops taken are as much a part of the shape as the fact that some
	// link was involved. Any disagreement means at least one side is only reachable
	// through a link, which makes this path a boundary rather than work.
	if desiredNode.traversal != currentNode.traversal {
		return sess.blocked(path, current, desired, restore.ReasonSymlinkAncestor)
	}

	op, err := restore.NewPlannedOperation(path, current, desired)
	if err != nil {
		return err
	}
	if op.Kind() == restore.OpEqual {
		// Settled before any boundary question is asked, and deliberately so. A path whose
		// current state already equals the source needs no mutation, so no boundary can
		// refuse it — including the symlink boundary below. Blocking it would turn every
		// unchanged path reached through a symlink into an evidence gap, which under --all
		// refuses the whole restore over paths nobody asked to change.
		sess.counts.Add(op)
		return nil
	}

	// The states differ, so this path would have to be written or deleted. A record
	// reached by following a symlink describes a node that lives outside the path it is
	// filed under: every component below the link belongs to the link's target. Restore
	// descends no-follow, so it can neither read that node's current bytes nor install
	// anything there — the mutation boundary refuses at the link.
	//
	// Naming it here, from the record itself, is what keeps preview and apply agreeing.
	// Left ready it would preview as ordinary work and then fail at the descriptor with a
	// reason that reads like something changed underfoot, when nothing did.
	if desiredNode.followed() || currentNode.followed() {
		return sess.blocked(path, current, desired, restore.ReasonSymlinkAncestor)
	}

	// A regular-file write needs bytes, and an entry hash proves identity, not
	// recoverability. hash-only evidence and a blob the store no longer holds are
	// both evidence gaps found without reading a single byte, so preview reports
	// them exactly as apply would refuse on them.
	if op.RequiresContent() {
		if r, blockedNow, err := sess.contentReason(src); err != nil {
			return err
		} else if blockedNow {
			return sess.blocked(path, current, desired, r)
		}
	}

	sess.counts.Add(op)
	sess.mutating++
	if sess.spool == nil {
		// A preview counts the work and describes it; only an apply has to be able to
		// replay it.
		return nil
	}
	return sess.spool.Add(op)
}

// contentReason reports whether the source can actually produce this entry's
// bytes. It reads no content: hash-only is a manifest fact, and blob presence is a
// stat. Corruption is only detectable by reading, so it stays an apply-time
// refusal — which is why preview can be honest about what it did and did not check.
func (sess *session) contentReason(src worktree.ManifestRecord) (restore.Reason, bool, error) {
	e, ok := src.Entry()
	if !ok {
		return "", false, fmt.Errorf("restore: a content-bearing operation without a source entry")
	}
	if e.Storage != worktree.StorageBlob {
		return restore.ReasonHashOnlyContent, true, nil
	}
	has, err := sess.svc.deps.Blobs.Has(e.Content)
	if err != nil {
		return "", false, err
	}
	if !has {
		return restore.ReasonBlobMissing, true, nil
	}
	return "", false, nil
}

// blocked records one unavailable operation: it counts, and it contributes to the
// bounded sample until that is full.
func (sess *session) blocked(path worktree.RelPath, current, desired restore.NodeState, reasons ...restore.Reason) error {
	op, err := restore.NewBlockedOperation(path, current, desired, reasons...)
	if err != nil {
		return err
	}
	sess.counts.Add(op)
	if len(sess.samples) < restore.MaxBlockedSamples {
		sample, err := restore.NewBlockedSample(op)
		if err != nil {
			return err
		}
		sess.samples = append(sess.samples, sample)
	}
	return nil
}

// markSelectionSeen records that an explicit path operand matched something inside
// awa's evidence.
func (sess *session) markSelectionSeen(path worktree.RelPath) {
	if len(sess.selectionSeen) == 0 {
		return
	}
	for _, sel := range sess.req.Selection.Paths() {
		if restore.PathWithin(path, sel) {
			sess.selectionSeen[sel.String()] = true
		}
	}
}

// explainEmptySelections turns the most confusing possible preview — an explicit
// path that produced nothing at all — into an honest statement. A selected path
// that matched no record on either side but does exist on disk is outside awa's
// evidence boundary: ignored, or excluded by scope. Reporting that is the whole
// point of the honesty rule, because the alternative reads exactly like "this
// path is already correct".
//
// The check costs one metadata-only stat per explicit operand, so it is bounded by
// what the user typed, never by the worktree.
func (sess *session) explainEmptySelections() error {
	if sess.req.Selection.All() {
		return nil
	}
	layout, err := sess.req.Project.Paths()
	if err != nil {
		return err
	}
	for _, sel := range sess.req.Selection.Paths() {
		if sess.selectionSeen[sel.String()] {
			continue
		}
		// A scope-bounded source answers first, and without consulting the filesystem.
		// A recovery observation proves exactly the paths one restore was going to
		// change; for anything else it holds no evidence at all, whether or not that
		// path exists, is ignored, or is perfectly ordinary. "This record says nothing
		// about that path" is the accurate answer, and it is a different answer from
		// "that path is outside awa's evidence".
		if sess.scopeStream != nil {
			if berr := sess.blocked(sel, restore.AbsentNode(), restore.AbsentNode(), restore.ReasonOutOfProvenScope); berr != nil {
				return berr
			}
			continue
		}
		err := fsx.StatNoFollowAt(layout.Root(), sel.String())
		switch {
		case err == nil:
			// It is there, and neither observation recorded it: the only way that
			// happens is that the path is outside the evidence boundary.
			if berr := sess.blocked(sel, restore.AbsentNode(), restore.AbsentNode(), restore.ReasonIgnoredBoundary); berr != nil {
				return berr
			}
		case errors.Is(err, os.ErrNotExist):
			// Absent on both sides and absent on disk: nothing to say.
		case fsx.IsSymlinkOpenRejection(err):
			if berr := sess.blocked(sel, restore.AbsentNode(), restore.AbsentNode(), restore.ReasonSymlinkAncestor); berr != nil {
				return berr
			}
		default:
			return err
		}
	}
	return nil
}

// merge walks the source manifest, the current manifest, and (for a
// recovery-observation source) the covered-path set in one canonical pass, holding
// at most one record per side. It is the reason a restore over a million paths
// costs constant memory: nothing is indexed, nothing is buffered, the three sorted
// streams are simply advanced together.
type merge struct {
	src   worktree.ManifestCursor
	cur   worktree.ManifestCursor
	scope restore.CoveredScopeCursor

	srcOK, curOK, scopeOK bool
	srcRec, curRec        worktree.ManifestRecord
	scopePath             worktree.RelPath

	path    worktree.RelPath
	source  worktree.ManifestRecord
	current worktree.ManifestRecord
	inScope bool

	started bool
	failure error
}

func newMerge(src, cur worktree.ManifestCursor, scope restore.CoveredScopeCursor) *merge {
	return &merge{src: src, cur: cur, scope: scope}
}

func (m *merge) next() bool {
	if m.failure != nil {
		return false
	}
	if !m.started {
		m.started = true
		m.srcOK = m.advanceManifest(m.src, &m.srcRec)
		m.curOK = m.advanceManifest(m.cur, &m.curRec)
		m.scopeOK = m.advanceScope()
		if m.failure != nil {
			return false
		}
	}
	if !m.srcOK && !m.curOK && !m.scopeOK {
		return false
	}

	// The next path is the smallest across every active stream.
	var least worktree.RelPath
	first := true
	consider := func(p worktree.RelPath) {
		if first || p.Less(least) {
			least, first = p, false
		}
	}
	if m.srcOK {
		consider(m.srcRec.Path())
	}
	if m.curOK {
		consider(m.curRec.Path())
	}
	if m.scopeOK {
		consider(m.scopePath)
	}

	m.path = least
	m.source = worktree.ManifestRecord{}
	m.current = worktree.ManifestRecord{}
	m.inScope = false

	if m.srcOK && m.srcRec.Path().Equal(least) {
		m.source = m.srcRec
		m.srcOK = m.advanceManifest(m.src, &m.srcRec)
	}
	if m.curOK && m.curRec.Path().Equal(least) {
		m.current = m.curRec
		m.curOK = m.advanceManifest(m.cur, &m.curRec)
	}
	if m.scopeOK && m.scopePath.Equal(least) {
		m.inScope = true
		m.scopeOK = m.advanceScope()
	}
	return m.failure == nil
}

func (m *merge) advanceManifest(c worktree.ManifestCursor, dst *worktree.ManifestRecord) bool {
	if c.Next() {
		*dst = c.Record()
		return true
	}
	if err := c.Err(); err != nil && m.failure == nil {
		m.failure = err
	}
	return false
}

func (m *merge) advanceScope() bool {
	if m.scope == nil {
		return false
	}
	if m.scope.Next() {
		m.scopePath = m.scope.Path()
		return true
	}
	if err := m.scope.Err(); err != nil && m.failure == nil {
		m.failure = err
	}
	return false
}

func (m *merge) err() error { return m.failure }
