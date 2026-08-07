package run

import (
	"context"
	"errors"
	"fmt"

	"awarer/internal/app/scanner"
	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/perfdiag"
	"awarer/internal/domain/runcache"
	"awarer/internal/infra/projfs"
)

// lsScans memoizes one input scan per distinct recorded scan policy within a single Ls
// call, so N candidates sharing a scope cost one scan, not N — and the reusable and
// near-miss evaluations share it, so --near adds no scans. It keeps what each scan cost
// alongside it, because a listing pays the same content rehash a real run pays and owes
// the user the same explanation for it.
//
// The cached scans' manifests are consumed within the call (reusability checks and
// changed-path samples), so close releases any spill files behind them.
type lsScans struct {
	byPolicy map[string]scanner.Result
	reports  []inputReport
}

func newLsScans() *lsScans {
	return &lsScans{byPolicy: map[string]scanner.Result{}}
}

func (c *lsScans) get(sig string) (scanner.Result, bool) {
	res, ok := c.byPolicy[sig]
	return res, ok
}

func (c *lsScans) put(sig string, res scanner.Result, report inputReport) {
	c.byPolicy[sig] = res
	c.reports = append(c.reports, report)
}

// slowest returns the costliest single input observation of the call, or the zero
// report when nothing was scanned.
//
// The slowest one rather than a total: each report is one measured scan with its own
// exact file count, while a sum of overlapping scopes would be a file count no scan
// ever observed. perfdiag refuses fabricated numbers, and a listing that scans several
// scopes is better described by its worst one than by an invented aggregate.
func (c *lsScans) slowest() inputReport {
	var worst inputReport
	for _, r := range c.reports {
		if r.durationMs > worst.durationMs {
			worst = r
		}
	}
	return worst
}

func (c *lsScans) close() {
	for _, res := range c.byPolicy {
		_ = res.Close()
	}
}

// LsRequest selects which stored runs the current-state reuse view considers.
type LsRequest struct {
	Project projfs.Project
	Config  config.Config
	// Limit bounds the newest-first candidate window, mirroring run log's default. It
	// is a candidate window, not a reusable-row count: ls inspects the Limit newest
	// runs and reports which of them are reusable now. Command filters within that
	// window, so a command absent from the newest Limit runs yields no rows rather than
	// an unbounded search back through history. Zero means defaultLogLimit.
	Limit int
	// Command, when non-empty, keeps only runs whose first argv token matches it,
	// applied within the candidate window.
	Command string
	// All includes non-reusable rows in Reusable, each carrying an explicit reason. By
	// default Reusable holds only reusable entries.
	All bool
	// Near additionally collects the non-reusable runs in the window as ranked
	// near-miss candidates (LsResult.NearMisses). It shares the single scan pass with
	// the reusable evaluation, so enabling it never re-scans the worktree.
	Near bool
}

// LsEntry is one current-state reuse row. For a readable run, Entry holds its
// metadata and Reusability is the typed verdict (reusable now, or a reason why not).
// For a run whose metadata is corrupt, Corrupt is true, only ID is populated, and
// Reusability carries ReasonCorrupt — so the row still names why without trusting
// the bad document.
type LsEntry struct {
	ID          runcache.RunID
	Entry       runcache.RunEntry
	Reusability runcache.RunReusability
	Corrupt     bool
}

// Reusable reports whether this row is a valid cache-hit candidate now.
func (e LsEntry) Reusable() bool { return e.Reusability.IsReusable() }

// LsResult is the current-state reuse view produced by one scan pass. Reusable holds
// the reusable-now rows (plus, under --all, the non-reusable and corrupt ones);
// NearMisses holds the ranked near-miss candidates (only when Near was requested);
// Corrupt counts entries skipped because their metadata could not be decoded, so a
// near-miss view can report them as skipped rather than silently drop them.
type LsResult struct {
	Reusable   []LsEntry
	NearMisses []RunReuseCandidate
	Corrupt    int
	// Performance carries bounded latency diagnostics for this listing (currently a huge
	// watched effect root that dominated the single shared effect observation). It is
	// advisory and empty when no known cause crossed the interactive latency threshold.
	Performance []perfdiag.Diagnostic
}

// Ls is the current-state reuse view: it reports which recent stored runs are
// reusable right now (would replay as a cache hit) under their recorded run policy,
// so an agent can avoid rerunning checks already executed against the same inputs.
// Unlike Log (chronological history), Ls re-observes current state: it rebuilds each
// candidate's current key under its recorded scope and compares it to the stored
// key, then checks TTL and payload health.
//
// It bounds the expensive work to the newest Limit candidates: ListRefsNewest hands
// back at most Limit run-id names (no metadata decode, and the store keeps only the
// newest Limit rather than materializing every ref), and the per-entry costs —
// metadata decode, payload verification, and the worktree scan — run only for those
// candidates, with scans further grouped by recorded scan policy so a shared scope is
// scanned once rather than once per entry. The candidate window is the hard bound:
// Command filters within it, so no rare command triggers a full-history decode, and
// no entry outside the newest Limit is ever decoded or scanned. A mutating or
// record-only execution may now exist in run history (the store records all real
// executions), but reusability short-circuits any entry whose recorded reuse state
// is not reusable to its stored reason, so such a run never appears here as a
// reusable row — only under --all, with its reason.
//
// Reusable and near-miss results come from one pass over one scanCache: with Near
// set, the same scan that decides reusability also feeds the near-miss ranking and
// its changed-path samples, so "what can I replay" and "why can't I replay the rest"
// never cost two worktree scans.
//
// cwd-mismatch and command-changed reasons cannot arise here: ls reproduces each
// candidate's recorded command and cwd, so only inputs, environment, config, and
// platform can differ from the stored key.
func (s *Service) Ls(ctx context.Context, req LsRequest) (LsResult, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = defaultLogLimit
	}
	refs, err := s.deps.Store.ListRefsNewest(ctx, limit)
	if err != nil {
		return LsResult{}, err
	}

	base := Request{Project: req.Project, Config: req.Config}
	// The effect observation depends only on the project and the watched-roots policy,
	// both constant across candidates, so observe the watched generated-output roots
	// once here rather than re-walking them per candidate.
	effect, effectReport, err := s.observeEffect(base)
	if err != nil {
		return LsResult{}, err
	}
	scanCache := newLsScans()
	defer scanCache.close()

	res := LsResult{Reusable: make([]LsEntry, 0, len(refs))}
	for _, id := range refs {
		entry, err := s.deps.Store.Get(id)
		switch {
		case err == nil:
			// readable: evaluated below
		case errors.Is(err, runcache.ErrCorruptStore):
			// A command filter cannot match a run whose command is unreadable, so skip a
			// corrupt entry when filtering — mirroring run log — rather than pretend the
			// filter was honored. Without a filter, count it (so --near can report it
			// skipped) and, under --all, surface it as a row so it can be cleaned up.
			if req.Command == "" {
				res.Corrupt++
				if req.All {
					nr, _ := runcache.NotReusable(runcache.ReasonCorrupt)
					res.Reusable = append(res.Reusable, LsEntry{ID: id, Corrupt: true, Reusability: nr})
				}
			}
			continue
		case errors.Is(err, runcache.ErrNotFound):
			// Raced away (e.g. concurrent rm) between listing and reading; skip it.
			continue
		case errors.Is(err, runcache.ErrIncompatibleEntry):
			// An incompatible entry can never be reusable and cannot decode into the
			// current model; skip it silently (doctor surfaces it for cleanup) rather than
			// aborting the listing over benign upgrade leftovers.
			continue
		default:
			return LsResult{}, err
		}

		if req.Command != "" && !commandMatches(entry, req.Command) {
			continue
		}

		verdict, cmp, err := s.reusability(ctx, base, entry, effect, scanCache)
		if err != nil {
			return LsResult{}, err
		}
		if verdict.IsReusable() {
			res.Reusable = append(res.Reusable, LsEntry{ID: id, Entry: entry, Reusability: verdict})
			continue
		}
		if req.All {
			res.Reusable = append(res.Reusable, LsEntry{ID: id, Entry: entry, Reusability: verdict})
		}
		if req.Near {
			res.NearMisses = append(res.NearMisses, s.nearCandidate(entry, cmp, verdict.Reason(), effectReport))
		}
	}

	if req.Near {
		if err := s.rankNearMisses(ctx, base, res.NearMisses, scanCache); err != nil {
			return LsResult{}, err
		}
		if len(res.NearMisses) > maxCandidates {
			res.NearMisses = res.NearMisses[:maxCandidates]
		}
	}
	res.Performance = runPerformance([]inputReport{scanCache.slowest()}, effectReport)
	return res, nil
}

// nearCandidate builds a near-miss candidate from a non-reusable entry, its verdict
// reason, and the key comparison behind that verdict. The score is filled only when
// the entry was actually re-keyed against current state (a reusable-kind entry whose
// inputs/env/config drifted): a short-circuited record-only or mutating run was never
// compared, so claiming its categories "matched" would be dishonest — its reason
// alone explains it.
//
// effectReport is the diagnostic side channel of the one effect observation Ls already
// performed — the same observation folded into every candidate's current key input — so
// an effect-state miss carries the root that observation named without a second walk.
// The same re-keyed/short-circuited split that decides the score decides the root: a
// short-circuited candidate's reason is its stored run's own verdict, which today's
// observation says nothing about, so it keeps the diagnosis without the root.
func (s *Service) nearCandidate(entry runcache.RunEntry, cmp runcache.KeyComparison, reason runcache.MismatchReason, effectReport runcache.EffectReport) RunReuseCandidate {
	cand := RunReuseCandidate{Entry: entry, Comparison: cmp, Reason: reason}
	origin := reasonFromStoredVerdict
	if entry.Reuse.Kind() == runcache.ReuseReusable {
		cand.Score = scoreFrom(cmp)
		origin = reasonFromComparison
	}
	cand.attachEffect(effectReport, origin)
	return cand
}

// rankNearMisses sorts the near-miss candidates deterministically, truncates to the
// bounded set, and attaches a changed-path sample and advisory hints to the surviving
// input-tree-differs candidates. The samples reuse the scan already cached for each
// candidate's scope, so the bounded diff work runs at most maxCandidates times and
// never re-scans.
func (s *Service) rankNearMisses(ctx context.Context, base Request, cands []RunReuseCandidate, scanCache *lsScans) error {
	sortCandidates(cands)
	limit := len(cands)
	if limit > maxCandidates {
		limit = maxCandidates
	}
	for i := 0; i < limit; i++ {
		if !runcache.ReasonCanSampleChangedPaths(cands[i].Reason) {
			continue
		}
		scanRes, ok := s.cachedScanFor(base, cands[i].Entry, scanCache)
		if !ok {
			continue
		}
		sample, hints, err := s.changedPathSample(ctx, cands[i].Entry.ID, scanRes.Manifest())
		if err != nil {
			return err
		}
		cands[i].Changed = &sample
		cands[i].Hints = hints
	}
	return nil
}

// cachedScanFor returns the read-only scan already computed for a candidate's
// recorded scope during this call, so a changed-path sample reuses it rather than
// re-scanning. It never scans on a miss (that would be an unbounded surprise); a
// miss simply means no sample is attached.
func (s *Service) cachedScanFor(base Request, entry runcache.RunEntry, scanCache *lsScans) (scanner.Result, bool) {
	req, err := s.requestFromEntry(base, entry)
	if err != nil {
		return scanner.Result{}, false
	}
	return scanCache.get(scopeSignature(*req.EffectiveScope))
}

// reusability computes whether a stored entry is a valid cache-hit candidate right
// now, reusing the same key derivation a real run would: it rebuilds the current key
// for the entry's recorded command/cwd/scope under the current config and compares
// it to the stored key, then applies the same TTL and payload-health gates as the
// hit path. The worktree scan it needs is taken from scanCache, keyed by the
// recorded scan policy, so repeated scopes are scanned once.
//
// It also returns the typed key comparison behind the verdict — the drift the
// near-miss view ranks and explains — so a stale run is explained without re-deriving
// its key. The comparison is empty for a clean key match and for a short-circuited
// non-reusable run (which is never re-keyed).
func (s *Service) reusability(ctx context.Context, base Request, entry runcache.RunEntry, effect runcache.EffectObservation, scanCache *lsScans) (runcache.RunReusability, runcache.KeyComparison, error) {
	// A run the store recorded as non-reusable history — record-only, mutating, or
	// post-scan-failed — was never a cache candidate, so it can never become reusable
	// now: short-circuit to its stored reason rather than re-deriving a key for it.
	// This is what keeps run ls from ever labeling a mutating or record-only run
	// reusable, and keeps the idempotent-formatter case correct (the first mutating
	// run stays non-reusable history; only a later no-op run can be reusable).
	if entry.Reuse.Kind() != runcache.ReuseReusable {
		v, err := runcache.NotReusable(entry.Reuse.Reason())
		return v, runcache.KeyComparison{}, err
	}
	req, err := s.requestFromEntry(base, entry)
	if err != nil {
		return runcache.RunReusability{}, runcache.KeyComparison{}, err
	}
	scope := *req.EffectiveScope

	sig := scopeSignature(scope)
	scanRes, ok := scanCache.get(sig)
	if !ok {
		scanStart := s.deps.Clock.Now()
		// Read-only (ls holds no presence lock) but forcing a rehash, exactly like the
		// scan a real run would perform. This projection tells the user which stored runs
		// would replay right now, so it has to answer the question the run path answers,
		// with the same evidence: an answer computed from an indexed hash could call an
		// entry reusable while the bytes behind it have changed under a stat signature
		// that did not move. A projection that promises a hit the cache would not serve —
		// or hides a stale one — is worse than no projection.
		scanRes, err = s.deps.Scanner.Scan(ctx, req.Project, req.Config, scope, scanner.Options{
			AllowSkippedInputs: true,
			ReadOnly:           true,
			ForceRehash:        true,
			Now:                s.deps.Clock.Now(),
		})
		if err != nil {
			return runcache.RunReusability{}, runcache.KeyComparison{}, err
		}
		scanCache.put(sig, scanRes, newInputReport(s.deps.Clock.Now().Sub(scanStart).Milliseconds(), scanRes.Stats().Files))
	}

	current := s.currentKeyInputFor(req, scope, scanRes.TreeHash(), effect)
	cmp := runcache.CompareKeyInputs(entry.KeyInput, current)

	// A keyed difference means a real run would not hit this entry: name the most
	// explanatory difference as the reason.
	if !cmp.Equal() {
		v, err := runcache.NotReusable(runcache.ReasonFromDiffCode(cmp.PrimaryReason()))
		return v, cmp, err
	}
	// Key matches; apply the same freshness and payload gates the hit path enforces.
	if s.expired(entry, req.Config.Run.TTL) {
		v, err := runcache.NotReusable(runcache.ReasonExpired)
		return v, cmp, err
	}
	healthy, err := s.payloadHealth(entry)
	if err != nil {
		return runcache.RunReusability{}, runcache.KeyComparison{}, err
	}
	if !healthy {
		v, err := runcache.NotReusable(runcache.ReasonPayloadMissing)
		return v, cmp, err
	}
	return runcache.Reusable(), cmp, nil
}

// currentKeyInputFor builds the key input a real run of the entry's command would
// compute now: the current allowlisted environment checkpoint, the executable
// resolved under that environment, the freshly scanned input tree hash, and the
// effect observation the caller observed once for this Ls, folded through the same
// buildKeyInput the run path uses — so a deleted or changed generated-output root
// surfaces as effect-state-differs in the current-state reuse view.
func (s *Service) currentKeyInputFor(req Request, scope config.ScanScope, treeHash hashing.TreeHash, effect runcache.EffectObservation) runcache.KeyInput {
	effEnv := s.effectiveEnv(req.Config.Run.EnvAllowlist)
	resolvedPath, resolvedStat, _ := s.deps.Resolver.Resolve(req.Argv[0], req.AbsDir, effEnv.ChildEnv())
	return s.buildKeyInput(req, req.Config, scope, treeHash, effect, resolvedPath, resolvedStat, effEnv.Keyed())
}

// scopeSignature is a stable key grouping candidates whose worktree scan is
// identical: the same boundary (include/exclude/ignore flags). Trust mode is omitted
// because it changes only whether the index is reused, not the resulting hash.
func scopeSignature(scope config.ScanScope) string {
	return fmt.Sprintf("%q|%q|g=%t|a=%t",
		scope.Include, scope.Exclude, scope.UseGitignore, scope.UseAwaignore)
}
