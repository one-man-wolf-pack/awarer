// Package worktreefs is the filesystem boundary for the worktree scanner.
//
// It walks a project root, applies the layered ignore rules (built-in excludes,
// per-directory .gitignore and .awaignore), classifies each node, and captures a
// stat signature, yielding worktree.Node values to the scanner. It owns all
// os/filepath details and the gitignore dependency, so the domain and the
// application scanner never touch the filesystem directly.
//
// By default symlinks are recorded as links and never traversed. When
// follow_symlinks is enabled, the walker resolves symlinks into their targets,
// rejecting escapes outside the project root, detecting cycles, and enforcing the
// configured maximum traversal depth.
package worktreefs

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"awarer/internal/domain/config"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/fsx"
)

// protectedNames is the set of base names that are a hard scan boundary: they are
// never scanned regardless of the configured ignore rules, so an ignore-file
// negation cannot re-include VCS or awa state. It mirrors config.ProtectedExcludes.
func protectedNames() map[string]bool {
	out := map[string]bool{}
	for _, p := range config.ProtectedExcludes() {
		out[p] = true
	}
	return out
}

// Walker walks a project worktree. It is stateless and safe to reuse across
// scans; per-scan state lives in the walk it creates.
type Walker struct{}

// New returns a Walker.
func New() *Walker { return &Walker{} }

// walk holds the state of a single Walk invocation.
type walk struct {
	ctx       context.Context
	root      string
	realRoot  string // root with symlinks resolved, for escape checks
	eng       *ignoreEngine
	inc       includeFilter
	protected map[string]bool // base names never scanned, whatever the ignore rules say
	scope     config.ScanScope
	visit     func(worktree.Node) error
}

// Walk implements worktree.Walker.
func (w *Walker) Walk(ctx context.Context, layout paths.Layout, scope config.ScanScope, visit func(worktree.Node) error) error {
	root := layout.Root()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root // best effort; escape checks fall back to the literal root
	}
	st := &walk{
		ctx:       ctx,
		root:      root,
		realRoot:  realRoot,
		eng:       newIgnoreEngine(scope.Exclude, scope.UseGitignore, scope.UseAwaignore),
		inc:       newIncludeFilter(scope.CanonicalInclude()),
		protected: protectedNames(),
		scope:     scope,
		visit:     visit,
	}
	return filepath.WalkDir(root, st.base)
}

// base is the WalkDir callback for the project's own tree. It prunes ignored and
// out-of-scope directories, records ignore files as it descends, classifies files
// and symlinks, and hands followed-symlink subtrees to follow.
func (w *walk) base(absPath string, d fs.DirEntry, err error) error {
	if err != nil {
		return fmt.Errorf("walking %s: %w", absPath, err)
	}
	if w.ctx.Err() != nil {
		return w.ctx.Err()
	}

	rel, relErr := filepath.Rel(w.root, absPath)
	if relErr != nil {
		return fmt.Errorf("relativizing %s: %w", absPath, relErr)
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return w.eng.loadDirAt(absPath, "")
	}
	if w.protected[path.Base(rel)] {
		// Protected paths (.git, .awa) are a hard boundary: they are skipped before
		// the ignore engine runs, so a .gitignore/.awaignore negation (e.g. "!.git")
		// can never re-include VCS or awa state into a scan. Returning fs.SkipDir is
		// only correct for a directory: from a non-directory entry it would skip the
		// rest of the parent, silently omitting sibling files, so a stray protected
		// file just skips itself.
		if d.IsDir() {
			return fs.SkipDir
		}
		return nil
	}

	isDir := d.IsDir()
	isSymlink := d.Type()&fs.ModeSymlink != 0
	// Under follow_symlinks a symlink may resolve to a directory, so for scope
	// pruning it is a potential ancestor of a deeper include (e.g. an include of
	// "dirlink/inner.txt"). Treating it as directory-like keeps the symlink from
	// being pruned before it is ever resolved, which would silently drop the scoped
	// include living beneath it.
	dirLike := isDir || (isSymlink && w.scope.FollowSymlinks)
	if !w.inc.allows(rel, dirLike) {
		if isDir {
			return fs.SkipDir
		}
		return nil
	}
	// An explicit, narrowed scope outranks the ignore engine: a path the user
	// scoped to is scanned even if a default/baseline/ignore-file rule would drop
	// it. Protected paths were already handled above and stay excluded regardless.
	if !w.inc.overridesExcludes() {
		if w.eng.ignores(rel, isDir) {
			if isDir {
				return fs.SkipDir
			}
			return nil
		}
	}

	if isSymlink {
		if w.scope.FollowSymlinks {
			// Depth 1 is the first symlink hop; an empty chain starts cycle
			// detection.
			// A top-level symlink is reached directly: no traversal source and no
			// ancestor symlink chain.
			return w.follow(absPath, rel, 1, map[string]bool{}, worktree.RelPath{}, nil)
		}
		node, err := classifySymlink(rel, absPath)
		if err != nil {
			return err
		}
		return w.visit(node)
	}

	if isDir {
		if err := w.eng.loadDirAt(absPath, rel); err != nil {
			return err
		}
		return w.emitDir(rel, worktree.TraversalInfo{})
	}

	node, err := w.classifyFile(rel, absPath, d)
	if err != nil {
		return err
	}
	return w.visit(node)
}

// emitDir yields a directory node.
func (w *walk) emitDir(rel string, tr worktree.TraversalInfo) error {
	p, err := worktree.ParseRelPath(rel)
	if err != nil {
		return err
	}
	return w.visit(worktree.Node{Path: p, Kind: worktree.KindDir, Traversal: tr})
}

// symlinkHop records a symlink crossed while following a chain: the resolved path
// of the link node and the raw target read there during the validated traversal.
// Re-reading the same path at content-open time and comparing the target detects
// the link being repointed — or a hop inserted or removed at it — after follow()
// validated the chain, which a re-checkpoint taken at open time could miss if the
// change landed before the checkpoint.
type symlinkHop struct {
	linkPath  string
	rawTarget string
}

// follow resolves a symlink under follow_symlinks and either emits its target
// file or walks its target directory, enforcing depth, root-escape, and cycle
// rules. The symlink chain is resolved one hop at a time — not with a single
// EvalSymlinks, which would collapse the whole chain and so undercount the hops
// and turn a symlink-to-symlink loop (a -> b -> a) into an opaque resolve error.
// A link that cannot be resolved falls back to being recorded as a link. depth
// counts symlink hops; chain holds the resolved directories already entered on
// this traversal, for directory-cycle detection. source is the symlink through
// which this symlink itself was reached (zero for a top-level link), so a skipped
// link keeps its own provenance. priorChain holds the symlink hops already crossed
// to reach this link (the ancestor followed-directory symlinks), so a content
// opener can be bound to the full observed chain rather than a re-checkpointted one.
func (w *walk) follow(symlinkAbs, virtualRel string, depth int, chain map[string]bool, source worktree.RelPath, priorChain []symlinkHop) error {
	// The raw target of the link node identifies it in the project tree and must
	// travel with any skip, so re-pointing a skipped link changes the tree hash. The
	// link's own stat and provenance describe that node, regardless of how many hops
	// its resolution takes.
	rawTarget, err := os.Readlink(symlinkAbs)
	if err != nil {
		return fmt.Errorf("reading symlink %s: %w", symlinkAbs, err)
	}
	target, err := worktree.NewSymlinkTarget(rawTarget)
	if err != nil {
		return fmt.Errorf("symlink %s: %w", virtualRel, err)
	}
	linkInfo, err := os.Lstat(symlinkAbs)
	if err != nil {
		return fmt.Errorf("lstat %s: %w", symlinkAbs, err)
	}
	linkStat := statSignature(linkInfo)
	nodeTr := traversalOfReached(source, depth)

	// The include filter may have admitted this symlink only as a provisional
	// directory ancestor (dirLike) of a deeper include — valid only if it resolves
	// to a directory we can descend. Any leaf outcome (a file, a special, or a skip)
	// is in the result only if the link is in scope as a file in its own right;
	// otherwise a narrow scan would pull in a link it never included. The directory
	// descend below is exempt: that is the ancestor case the dirLike pass is for.
	leafInScope := w.inc.allows(virtualRel, false)
	skipLeaf := func(reason worktree.SkippedReason) error {
		if !leafInScope {
			return nil
		}
		return w.skipSymlink(virtualRel, target, linkStat, nodeTr, reason)
	}
	brokenLeaf := func() error {
		if !leafInScope {
			return nil
		}
		return w.recordBrokenLink(virtualRel, symlinkAbs, nodeTr)
	}

	// Walk the chain hop by hop: each hop is one symlink, so depth and cycle
	// detection see every link, not just the first. seen holds the link nodes
	// visited on this resolution, so a chain that loops back is a cycle rather than
	// an unbounded descent. cur is kept in the project's resolved path space (its
	// parent directories canonicalized), so the cycle and root-boundary checks are
	// exact even when the project root is itself reached through a symlink.
	cur := w.canonicalizeParent(symlinkAbs)
	// linkRealDir is the resolved directory the link lives in. A link that resolves
	// to this directory or an ancestor of it (e.g. "self -> ." or "loop -> ..")
	// would re-enter a directory it is already inside — a cycle caught from the link
	// alone, on the first hop, before it is emitted and its contents duplicated.
	linkRealDir := filepath.Dir(cur)
	hop := depth
	seen := map[string]bool{}
	// leafChain accumulates the symlink hops crossed on this resolution, captured as
	// they are validated, so a content opener checks against the chain follow() saw
	// rather than one re-read later.
	var leafChain []symlinkHop
	for {
		if hop > w.scope.SymlinkMaxDepth {
			return skipLeaf(worktree.ReasonSymlinkDepthExceeded)
		}
		if seen[cur] {
			return skipLeaf(worktree.ReasonSymlinkCycle)
		}
		seen[cur] = true

		hopTarget, err := os.Readlink(cur)
		if err != nil {
			return brokenLeaf()
		}
		leafChain = append(leafChain, symlinkHop{linkPath: cur, rawTarget: hopTarget})
		next := filepath.Clean(hopTarget)
		if !filepath.IsAbs(next) {
			next = filepath.Clean(filepath.Join(filepath.Dir(cur), hopTarget))
		}
		// Resolve the symlink components of next's own directory (e.g. a target of
		// "dirlink/file" where dirlink itself escapes the root) so the boundary check
		// sees where next truly lands, not where its in-root-looking string suggests.
		next = w.canonicalizeParent(next)

		// Decide the root boundary from the link target alone, before touching the
		// terminal node: if escapes are forbidden and the target leaves the root,
		// reject it here so the result never depends on whether the out-of-root path
		// exists or is readable. next's directory is fully resolved, so this is an
		// exact containment test, not a lexical guess, applied at every hop.
		if !w.scope.AllowSymlinkRootEscape && !w.withinRoot(next) {
			return skipLeaf(worktree.ReasonSymlinkRootEscape)
		}

		info, err := os.Lstat(next)
		if err != nil {
			// Broken or dangling target: record the link rather than guessing.
			return brokenLeaf()
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			// Another link: take the next hop.
			cur = next
			hop++
			continue
		}

		// next is a non-symlink whose parent is already resolved, so the hop-by-hop
		// walk is done — but its final element still carries the spelling the link's
		// target used to name it. The terminal exists and is not a symlink, so
		// EvalSymlinks can now canonicalize the path whole, settling the part no
		// hop resolution decides: the form the platform itself considers canonical,
		// such as a Windows 8.3 component or a case-insensitive name. followedOpener
		// re-resolves the virtual path with the same function, so the terminal
		// recorded here and the one checked at read time come from one resolver
		// rather than from two that agree only where the platform normalizes nothing.
		canonical, err := filepath.EvalSymlinks(next)
		if err != nil {
			// The terminal went away between the Lstat above and this resolve: record
			// the link, exactly as a target that was already missing is recorded,
			// rather than guessing at a path that no longer resolves.
			return brokenLeaf()
		}
		next = canonical

		realInfo, err := os.Stat(next)
		if err != nil {
			return fmt.Errorf("stat symlink target %s: %w", next, err)
		}

		// The node was reached through hop symlink hops — the real count after walking
		// the whole chain, not the base depth — so its persisted provenance reports
		// the true number of hops taken.
		tr := worktree.TraversalInfo{Followed: true, SourcePath: mustRel(virtualRel), Depth: hop}

		// The full observed chain to this terminal is the ancestor hops plus the hops
		// crossed here, ordered root-first. A content opener replays it to detect the
		// chain being restructured after this validated traversal.
		chainTo := concatHops(priorChain, leafChain)

		if realInfo.Mode().IsRegular() {
			if !leafInScope {
				return nil
			}
			return w.emitFollowedFile(virtualRel, next, chainTo, realInfo, tr)
		}
		if !realInfo.IsDir() {
			// A followed link to a device/socket/fifo: surfaced as special so the
			// scanner records the skip, consistent with special files in the base tree.
			if !leafInScope {
				return nil
			}
			p, perr := worktree.ParseRelPath(virtualRel)
			if perr != nil {
				return perr
			}
			return w.visit(worktree.Node{Path: p, Kind: worktree.KindSpecial, Stat: statSignature(realInfo), Traversal: tr})
		}
		// A link to a directory already on the path we are walking is a cycle:
		// descending it would re-enter a directory we are inside. That is either a
		// directory the follow-traversal already entered (chain — including ordinary
		// subdirectories, so a cross-symlink loop routed through them is caught) or
		// the link's own directory or an ancestor of it (a self/ancestor reference).
		if chain[next] || next == linkRealDir || strings.HasPrefix(linkRealDir, next+string(filepath.Separator)) {
			return skipLeaf(worktree.ReasonSymlinkCycle)
		}

		if err := w.emitDir(virtualRel, tr); err != nil {
			return err
		}
		if err := w.eng.loadDirAt(next, virtualRel); err != nil {
			return err
		}
		nextChain := cloneChain(chain)
		nextChain[next] = true
		// Descendants of this followed directory were reached through the same hop
		// count, so they carry hop as their base depth (a nested symlink adds one
		// more), this symlink as their traversal source, and the chain that reached
		// here as their ancestor chain.
		return w.walkRealChildren(next, virtualRel, hop, nextChain, tr.SourcePath, chainTo)
	}
}

// concatHops returns the ancestor chain followed by the hops crossed below it as a
// fresh slice, so threading a chain to a child never aliases a parent's backing
// array.
func concatHops(prior, leaf []symlinkHop) []symlinkHop {
	out := make([]symlinkHop, 0, len(prior)+len(leaf))
	out = append(out, prior...)
	out = append(out, leaf...)
	return out
}

// canonicalizeParent returns p with its parent directory's symlinks resolved,
// leaving the final element untouched so a symlink p is not itself followed (the
// loop follows it one hop at a time). Resolving the parent puts p in the project's
// resolved path space and, crucially, follows any symlinked directory inside the
// path — so a target like <root>/dirlink/file, where dirlink escapes the root, is
// seen at its true out-of-root location rather than by its in-root-looking string.
// This makes the containment and cycle checks exact.
//
// The final element keeps the spelling the caller (a link's raw target) gave it, so
// this is the hop walk's path space, not the platform's canonical one. Once the
// terminal is known to exist and not to be a symlink, follow() canonicalizes it
// whole with EvalSymlinks, which is the form the content opener re-derives.
//
// EvalSymlinks is the fast path. When it fails — because a parent component is
// missing, or a symlinked parent points at a location that does not exist — the
// tolerant resolver takes over rather than falling back to the lexical path: a
// symlinked directory escaping the root must be seen at its true out-of-root
// location even when that location is missing, so the escape is classified as one
// instead of depending on whether the external path exists.
func (w *walk) canonicalizeParent(p string) string {
	parent := filepath.Dir(p)
	if real, err := filepath.EvalSymlinks(parent); err == nil {
		return filepath.Join(real, filepath.Base(p))
	}
	return filepath.Join(resolveTolerant(parent), filepath.Base(p))
}

// symlinkResolveBudget bounds symlink hops during tolerant path resolution so a
// loop of symlinked directories cannot spin forever.
const symlinkResolveBudget = 255

// resolveTolerant resolves path component by component, following each symlink via
// readlink, and returns the resolved path. Unlike filepath.EvalSymlinks it tolerates
// components that do not exist (taken literally) and symlinks whose target does not
// exist (still followed), so a path reached through a symlink pointing at a missing
// location is resolved to that true location instead of aborting. That lets an
// out-of-root target be classified as a root escape even when it does not exist. A
// hop budget bounds symlink chains. path is expected to be absolute.
//
// It resolves symlinks and nothing else: a component is carried over as the caller
// spelled it, so on a platform that keeps a second spelling of the same name — a
// Windows 8.3 component, a case-insensitive filesystem — this does not agree with
// filepath.EvalSymlinks. That is why it stays inside the walk, where it classifies a
// target the caller's own path space can still describe, and why the content opener
// re-resolves with EvalSymlinks instead: comparing a recorded path against a
// re-resolved one requires both to come from the same resolver.
func resolveTolerant(path string) string {
	path = filepath.Clean(path)
	vol := filepath.VolumeName(path)
	resolved := vol + string(filepath.Separator)
	queue := strings.Split(filepath.ToSlash(strings.TrimPrefix(path, vol)), "/")
	hops := 0
	for len(queue) > 0 {
		comp := queue[0]
		queue = queue[1:]
		switch comp {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		candidate := filepath.Join(resolved, comp)
		fi, err := os.Lstat(candidate)
		if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			// Missing or a plain component: take it literally and continue.
			resolved = candidate
			continue
		}
		if hops >= symlinkResolveBudget {
			// Bounded: stop following links and keep the rest literally.
			return filepath.Join(append([]string{candidate}, queue...)...)
		}
		hops++
		target, rlErr := os.Readlink(candidate)
		if rlErr != nil {
			resolved = candidate
			continue
		}
		// Re-process the symlink target's components ahead of the remaining queue. A
		// relative target resolves against the symlink's directory (the current
		// resolved path); an absolute one restarts from its volume root.
		targetVol := filepath.VolumeName(target)
		if filepath.IsAbs(target) {
			resolved = targetVol + string(filepath.Separator)
		}
		queue = append(strings.Split(filepath.ToSlash(strings.TrimPrefix(target, targetVol)), "/"), queue...)
	}
	return resolved
}

// recordBrokenLink records a symlink that could not be resolved (a broken or
// dangling target) as a plain link node, preserving how it was reached rather
// than guessing at a target that does not exist.
func (w *walk) recordBrokenLink(virtualRel, symlinkAbs string, tr worktree.TraversalInfo) error {
	node, err := classifySymlink(virtualRel, symlinkAbs)
	if err != nil {
		return err
	}
	node.Traversal = tr
	return w.visit(node)
}

// walkRealChildren walks the real directory realDir whose contents are mapped to
// virtualDir under the project root. It is used for followed-symlink subtrees:
// nested real directories keep the same depth, while nested symlinks add a hop.
// source is the symlink path through which this subtree was reached, recorded as
// every node's traversal provenance; a nested symlink replaces it with its own.
// priorChain is the symlink chain that reached this directory, threaded to children
// so a content opener replays the full observed chain.
func (w *walk) walkRealChildren(realDir, virtualDir string, depth int, chain map[string]bool, source worktree.RelPath, priorChain []symlinkHop) error {
	entries, err := os.ReadDir(realDir)
	if err != nil {
		return fmt.Errorf("reading %s: %w", realDir, err)
	}
	tr := worktree.TraversalInfo{Followed: true, SourcePath: source, Depth: depth}
	for _, e := range entries {
		if w.ctx.Err() != nil {
			return w.ctx.Err()
		}
		childVirtual := virtualDir + "/" + e.Name()
		childAbs := filepath.Join(realDir, e.Name())
		isDir := e.IsDir()
		isSymlink := e.Type()&fs.ModeSymlink != 0
		// As in base(), a nested symlink under follow_symlinks may resolve to a
		// directory, so for scope pruning it is a potential ancestor of a deeper
		// include (e.g. "outer/nestedlink/file.txt"). Treat it as directory-like, or
		// the nested symlink would be pruned before it is resolved and the scoped
		// include beneath it would be silently dropped.
		dirLike := isDir || (isSymlink && w.scope.FollowSymlinks)

		// Protected names are a hard boundary inside followed subtrees too, so an
		// ignore-file negation cannot re-include VCS or awa state reached via a symlink.
		if w.protected[e.Name()] {
			continue
		}
		if !w.inc.allows(childVirtual, dirLike) {
			continue
		}
		// As in base(), an explicit narrowed scope outranks the ignore engine.
		if !w.inc.overridesExcludes() && w.eng.ignores(childVirtual, isDir) {
			continue
		}

		switch {
		case isSymlink:
			// The nested symlink was reached through this subtree's source symlink and
			// extends the chain that reached this directory.
			if err := w.follow(childAbs, childVirtual, depth+1, chain, source, priorChain); err != nil {
				return err
			}
		case isDir:
			if err := w.emitDir(childVirtual, tr); err != nil {
				return err
			}
			if err := w.eng.loadDirAt(childAbs, childVirtual); err != nil {
				return err
			}
			// Record this real subdirectory in the chain so a nested symlink that
			// loops back to it (or routes a cross-symlink cycle through it) is detected
			// rather than scanned again. childAbs is canonical — a real directory
			// inside an already-resolved directory — matching the keys follow compares.
			childChain := cloneChain(chain)
			childChain[childAbs] = true
			// Entering a real subdirectory crosses no symlink, so the ancestor chain
			// passes through unchanged.
			if err := w.walkRealChildren(childAbs, childVirtual, depth, childChain, source, priorChain); err != nil {
				return err
			}
		default:
			node, err := w.classifyFile(childVirtual, childAbs, e)
			if err != nil {
				return err
			}
			node.Traversal = tr
			// A regular child reached under a followed directory symlink must be read
			// through its virtual path, so a repointed ancestor symlink is caught at
			// read time rather than serving the previously-resolved file. childAbs is
			// the canonical real path follow()'s traversal validated for this child, and
			// priorChain is the ancestor symlink chain it crossed to get here, so the
			// opener is bound to those facts rather than to a state re-checkpointted later.
			if node.Kind == worktree.KindRegular {
				virtualAbs := filepath.Join(w.root, filepath.FromSlash(childVirtual))
				node.Open = w.followedOpener(virtualAbs, childAbs, priorChain, node.Stat)
			}
			if err := w.visit(node); err != nil {
				return err
			}
		}
	}
	return nil
}

// emitFollowedFile yields a regular-file node whose content lives at the resolved
// real path. real is the canonical terminal follow() validated for this node and
// chain is the symlink chain it crossed to reach it; the opener is bound to both so
// a read is checked against the facts that passed policy, not a state re-checkpointted
// later.
func (w *walk) emitFollowedFile(virtualRel, real string, chain []symlinkHop, info fs.FileInfo, tr worktree.TraversalInfo) error {
	p, err := worktree.ParseRelPath(virtualRel)
	if err != nil {
		return err
	}
	sig := statSignature(info)
	// Open through the virtual symlink path, not the resolved real path: if the
	// symlink (or an ancestor directory symlink) is repointed between walk and
	// read, replaying the observed chain and re-resolving the path rejects the stale
	// (or now-escaping) target instead of silently reading the old one.
	virtualAbs := filepath.Join(w.root, filepath.FromSlash(virtualRel))
	return w.visit(worktree.Node{
		Path:      p,
		Kind:      worktree.KindRegular,
		Stat:      sig,
		Traversal: tr,
		Open:      w.followedOpener(virtualAbs, real, chain, sig),
	})
}

// skipSymlink yields a skipped symlink node. It is used only for symlink skip
// reasons (cycle, depth, root escape), so it carries the full node model — the
// link target (part of the symlink's identity), the link's own stat (which holds
// its size), and how the link was reached — rather than leaving them zero.
func (w *walk) skipSymlink(virtualRel string, target worktree.SymlinkTarget, stat worktree.StatSignature, tr worktree.TraversalInfo, reason worktree.SkippedReason) error {
	p, err := worktree.ParseRelPath(virtualRel)
	if err != nil {
		return err
	}
	return w.visit(worktree.Node{
		Path:       p,
		Kind:       worktree.KindSymlink,
		Stat:       stat,
		Symlink:    target,
		Traversal:  tr,
		Skipped:    true,
		SkipReason: reason,
	})
}

// traversalOfReached returns the provenance of a node reached through the symlink
// at source. A zero source means the node was reached directly. The node's own
// reached-depth is one less than the hop count used to follow it.
func traversalOfReached(source worktree.RelPath, depth int) worktree.TraversalInfo {
	if source.IsZero() {
		return worktree.TraversalInfo{}
	}
	return worktree.TraversalInfo{Followed: true, SourcePath: source, Depth: depth - 1}
}

// withinRoot reports whether real lies within the resolved project root.
func (w *walk) withinRoot(real string) bool {
	if real == w.realRoot {
		return true
	}
	return strings.HasPrefix(real, w.realRoot+string(filepath.Separator))
}

func cloneChain(c map[string]bool) map[string]bool {
	out := make(map[string]bool, len(c)+1)
	for k, v := range c {
		out[k] = v
	}
	return out
}

// mustRel parses a rel path that the walker has already validated, panicking on
// the impossible error so callers stay readable. The walker only ever passes
// normalized, in-root paths here.
func mustRel(rel string) worktree.RelPath {
	p, err := worktree.ParseRelPath(rel)
	if err != nil {
		panic(fmt.Sprintf("worktreefs: invalid internal rel path %q: %v", rel, err))
	}
	return p
}

// classifySymlink builds a symlink node recorded by its target.
func classifySymlink(rel, absPath string) (worktree.Node, error) {
	target, err := os.Readlink(absPath)
	if err != nil {
		return worktree.Node{}, fmt.Errorf("reading symlink %s: %w", absPath, err)
	}
	st, err := worktree.NewSymlinkTarget(target)
	if err != nil {
		return worktree.Node{}, fmt.Errorf("symlink %s: %w", rel, err)
	}
	info, err := os.Lstat(absPath)
	if err != nil {
		return worktree.Node{}, fmt.Errorf("lstat %s: %w", absPath, err)
	}
	p, err := worktree.ParseRelPath(rel)
	if err != nil {
		return worktree.Node{}, err
	}
	return worktree.Node{
		Path:    p,
		Kind:    worktree.KindSymlink,
		Stat:    statSignature(info),
		Symlink: st,
	}, nil
}

// classifyFile builds a node for a non-symlink directory entry: a regular file
// (with a lazy content opener) or a special file. The regular opener reads rel
// relative to the project root, refusing a symlink at every path component, so an
// ancestor directory swapped to a symlink after the walk cannot redirect the read.
func (w *walk) classifyFile(rel, absPath string, d fs.DirEntry) (worktree.Node, error) {
	p, err := worktree.ParseRelPath(rel)
	if err != nil {
		return worktree.Node{}, err
	}
	info, err := d.Info()
	if err != nil {
		return worktree.Node{}, fmt.Errorf("stat %s: %w", absPath, err)
	}
	if !info.Mode().IsRegular() {
		return worktree.Node{
			Path: p,
			Kind: worktree.KindSpecial,
			Stat: statSignature(info),
		}, nil
	}
	sig := statSignature(info)
	root := w.root
	return worktree.Node{
		Path: p,
		Kind: worktree.KindRegular,
		Stat: sig,
		Open: func() (io.ReadCloser, error) { return openVerifiedRegularAt(root, rel, sig) },
	}, nil
}

// openVerifiedRegularAt opens the regular file at the slash-separated rel under
// root through a component-wise, symlink-refusing traversal, then confirms the
// opened descriptor is still the regular node the walker stat'd. Unlike
// openVerifiedRegular it protects every path component, not just the final one, so
// an ancestor directory swapped to a symlink between the walk and the read — even
// one whose target holds a hard link of the same inode and content — is rejected
// rather than read through the altered path. It is used for direct (in-root,
// non-followed) regular entries, where rel is meaningful relative to the root;
// followed entries re-resolve their virtual path instead (see followedOpener).
func openVerifiedRegularAt(root, rel string, want worktree.StatSignature) (io.ReadCloser, error) {
	f, err := fsx.OpenNoFollowAt(root, rel)
	if err != nil {
		// A component became a symlink between the walk and the open: a shape change,
		// surfaced like the other "changed during scan" rejections.
		if fsx.IsSymlinkOpenRejection(err) {
			return nil, fmt.Errorf("%s: %w", filepath.Join(root, filepath.FromSlash(rel)), worktree.ErrObservationChanged)
		}
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() || !sameRegularNode(want, statSignature(fi)) {
		_ = f.Close()
		return nil, fmt.Errorf("%s: %w", filepath.Join(root, filepath.FromSlash(rel)), worktree.ErrObservationChanged)
	}
	return f, nil
}

// openVerifiedRegular opens absPath and confirms it is still the same regular
// node the walker stat'd. A path swapped between stat and open is rejected as
// "changed during scan" rather than hashed under the old entry's kind/mode/stat.
// Three checks combine: an Lstat that does not follow symlinks, so the path itself
// must still be a regular file (a path now pointing at a symlink — even to the same
// inode/content — must not be stored as a regular entry, which would be an illegal
// stale shape); a no-follow open, so a swap of the path to a symlink in the gap
// between that Lstat and the open is refused at the open itself rather than
// resolved through the new shape (which Lstat alone cannot make atomic); and a stat
// of the opened descriptor, so a swap to a different regular file is caught on the
// very bytes about to be hashed.
func openVerifiedRegular(absPath string, want worktree.StatSignature) (io.ReadCloser, error) {
	li, err := os.Lstat(absPath)
	if err != nil {
		return nil, err
	}
	if !li.Mode().IsRegular() || !sameRegularNode(want, statSignature(li)) {
		return nil, fmt.Errorf("%s: %w", absPath, worktree.ErrObservationChanged)
	}
	f, err := fsx.OpenNoFollow(absPath)
	if err != nil {
		// The path became a symlink between the Lstat and the open: that is a shape
		// change, surfaced like the Lstat rejection rather than as a raw open error.
		if fsx.IsSymlinkOpenRejection(err) {
			return nil, fmt.Errorf("%s: %w", absPath, worktree.ErrObservationChanged)
		}
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !sameRegularNode(want, statSignature(fi)) {
		_ = f.Close()
		return nil, fmt.Errorf("%s: %w", absPath, worktree.ErrObservationChanged)
	}
	return f, nil
}

// followedOpener returns a content opener for a node reached through one or more
// symlinks, bound to the facts follow() validated rather than to a state re-read at
// open time. validatedReal is the canonical terminal that passed follow()'s depth,
// root-escape, and cycle checks; chain is the symlink chain (resolved link path and
// raw target per hop) follow() crossed to reach it, captured during that validated
// traversal.
//
// At read time the opener (1) replays the chain — re-reading each recorded link and
// requiring its raw target unchanged — then (2) re-resolves the virtual path and
// requires it still lands on validatedReal, then (3) verifies the terminal node's
// bytes via openVerifiedRegular. Step 1 catches a recorded link being repointed, or
// a hop inserted or removed at it after the chain was validated (e.g. link->real
// restructured to link->hop->real, whose first link's target changes from "real" to
// "hop"); because the chain is the one follow() observed, this holds even if the
// change landed before any open-time checkpoint. Step 2 catches a change to a
// silently-canonicalized parent directory of a chain target (not itself a recorded
// hop) or a new symlink appearing at the terminal — including a repoint that now
// escapes the root, even to a hard link sharing the recorded file's inode.
//
// Step 2 resolves with filepath.EvalSymlinks, the same function follow() used to
// canonicalize the terminal it recorded, so the two strings are produced by one
// resolver and an unchanged path is never falsely rejected — including on a platform
// that normalizes a path's spelling, where a hop-by-hop resolution of the caller's
// own root would differ from the recorded terminal without anything having changed.
// A path that no longer resolves is a change like any other and is rejected as one:
// there is deliberately no tolerant fallback here, because tolerance exists to
// classify a target that is missing during the walk, and by read time a terminal
// that will not resolve is not the terminal this node was recorded for.
func (w *walk) followedOpener(virtualAbs, validatedReal string, chain []symlinkHop, want worktree.StatSignature) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) {
		for _, h := range chain {
			tgt, err := os.Readlink(h.linkPath)
			if err != nil || tgt != h.rawTarget {
				return nil, fmt.Errorf("%s: %w", virtualAbs, worktree.ErrObservationChanged)
			}
		}
		real, err := filepath.EvalSymlinks(virtualAbs)
		if err != nil || real != validatedReal {
			return nil, fmt.Errorf("%s: %w", virtualAbs, worktree.ErrObservationChanged)
		}
		return openVerifiedRegular(real, want)
	}
}

// sameRegularNode reports whether two stat signatures describe the same regular
// node about to be hashed: the same inode when the platform supplies one, and the
// same change-signaling fields (size, mtime, permission bits). nlink and ctime
// are excluded — a hardlink added elsewhere, or a metadata-only ctime bump, does
// not change the bytes or the identity we are reading.
func sameRegularNode(want, got worktree.StatSignature) bool {
	wi, gi := want.HardlinkIdentity(), got.HardlinkIdentity()
	if wi.Known && gi.Known && (wi.Dev != gi.Dev || wi.Ino != gi.Ino) {
		return false
	}
	return want.Size == got.Size &&
		want.MtimeNs == got.MtimeNs &&
		want.Mode&0o777 == got.Mode&0o777
}
