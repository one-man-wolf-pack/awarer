// Package effectobserve computes a run's project-local effect-observation
// identity: a bounded metadata signature over the configured watched
// generated-output directories (build, dist, target, node_modules, ...) that are
// excluded from the run input scan. Deleting or changing one of those directories
// after a reusable run must miss rather than falsely hit, so this signature is
// folded into the run key alongside the input tree hash.
//
// The watched names are basenames matched wherever they appear in the tree — at the
// project root and nested (packages/api/node_modules) — mirroring how the input
// scan excludes them, so a monorepo's nested generated output is covered too. The
// observation is deliberately stat-only (existence + size/mtime/mode), never a
// content hash. The traversal is a filesystem port (implemented by
// internal/infra/effectfs) so this orchestration stays testable with a fake.
package effectobserve

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/runcache"
)

// OverBudgetError is returned by a Walker when a watched directory holds more entries
// than can be observed with fidelity. It carries the offending root's project-relative
// path and the entry budget it exceeded (the effective budget for that root, which may
// be the shared-observation remainder rather than the per-root cap), so a latency
// diagnostic can name the huge root even though the observation itself fails closed. The
// observation identity is unaffected — Observe still maps any Discover error to
// UnavailableEffect; this error only enriches the diagnostic side channel.
type OverBudgetError struct {
	Path  string
	Limit int
}

func (e *OverBudgetError) Error() string {
	return fmt.Sprintf("effectobserve: watched directory %q exceeds the %d-entry observation budget", e.Path, e.Limit)
}

// DiscoveredRoot is one fully-observed watched directory: its project-relative slash
// path, a sub-hash reducing its (bounded) stat content, and the number of entries
// folded. Discover only ever returns fully-observed directories — one too large to
// observe with fidelity fails the whole call closed — so there is no partial state to
// carry.
type DiscoveredRoot struct {
	Path    string
	SubHash hashing.TreeHash
	Entries int64
}

// Walker is the filesystem port the observer drives. Discover walks the project tree
// and returns every directory whose basename is one of watchNames — at any depth,
// pruning into matched directories, skipping protected state (.git/.awa) — each
// observed fully with a no-follow, bounded traversal. It returns a non-nil error when
// the tree cannot be observed safely (an unreadable directory, more matching
// directories than the bound, or a directory too large to observe with fidelity) —
// the single fail-closed signal that makes the whole observation unavailable.
type Walker interface {
	Discover(project string, watchNames []string) ([]DiscoveredRoot, error)
}

// Service computes an EffectObservation from the configured watch list.
type Service struct {
	walker Walker
	hasher hashing.Hasher
}

// New builds a Service over a filesystem walker and the project hasher.
func New(walker Walker, hasher hashing.Hasher) *Service {
	return &Service{walker: walker, hasher: hasher}
}

// Observe discovers the watched directories and reduces them to a bounded effect
// identity. If the tree cannot be observed safely — an unreadable directory or one
// too large to observe with fidelity — the whole observation is UnavailableEffect
// (fail closed: the run executes and records but is not reusable). Otherwise it yields
// ObservedEffect with the folded signature. Both the watch-name list and the
// discovered directories (sorted) are folded, so a config change and a
// created/deleted/changed watched directory each change the identity.
//
// The watch set is non-empty product policy (config.EffectiveEffectRoots is additive
// over a non-empty built-in set), so an empty one is a wiring or caller bug rather
// than a third observation state: it fails loudly here instead of manufacturing an
// identity no execution can produce.
func (s *Service) Observe(project string, roots []string) (runcache.EffectObservation, runcache.EffectReport, error) {
	canon := canonicalRoots(roots)
	if len(canon) == 0 {
		return runcache.EffectObservation{}, runcache.EffectReport{}, fmt.Errorf("effectobserve: no watched effect roots to observe")
	}

	discovered, err := s.walker.Discover(project, canon)
	if err != nil {
		// Fail closed: a tree that cannot be observed safely (or a watched directory too
		// large to observe fully) leaves the effect state unknown, so the run must not
		// publish a reusable hit. The observation identity mapping is unchanged; the
		// report is a pure side channel that names the huge root when the failure was a
		// budget overrun, so a latency diagnostic can explain the stall.
		report := runcache.EffectReport{}
		var over *OverBudgetError
		if errors.As(err, &over) {
			report = runcache.OverBudgetReport(runcache.OverBudgetFact{Path: over.Path, Limit: over.Limit})
		}
		obs, oerr := runcache.UnavailableEffect(len(canon))
		return obs, report, oerr
	}
	sort.Slice(discovered, func(i, j int) bool { return discovered[i].Path < discovered[j].Path })

	sig := runcache.EffectHashFromTree(s.hasher.HashBytes(fold(canon, discovered)))
	obs, oerr := runcache.ObservedEffect(sig, len(canon))
	return obs, largestRootReport(discovered), oerr
}

// largestRootReport builds the success-path diagnostic report: the fully-observed
// watched root with the most entries, so a latency diagnostic can name it with a true
// count. It returns an empty report when no root was discovered.
func largestRootReport(discovered []DiscoveredRoot) runcache.EffectReport {
	var largest *DiscoveredRoot
	for i := range discovered {
		if largest == nil || discovered[i].Entries > largest.Entries {
			largest = &discovered[i]
		}
	}
	if largest == nil {
		return runcache.EffectReport{}
	}
	return runcache.LargestRootReport(runcache.LargestRootFact{Path: largest.Path, Entries: largest.Entries})
}

// canonicalRoots returns the watch-name list in a deterministic, de-duplicated order
// so the folded signature does not depend on config order, and adding or removing a
// watched name always changes the signature.
func canonicalRoots(roots []string) []string {
	seen := make(map[string]bool, len(roots))
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// fold renders the watch names and the discovered directories into a deterministic,
// injective byte string: the sorted watch names (so a config change alone changes
// identity), then each discovered directory's path, sub-hash, and entry count, every
// field length-prefixed so no value can be confused for a field boundary. names and
// discovered arrive already sorted.
func fold(names []string, discovered []DiscoveredRoot) []byte {
	var b bytes.Buffer
	putUint(&b, uint64(len(names)))
	for _, n := range names {
		putString(&b, n)
	}
	putUint(&b, uint64(len(discovered)))
	for _, d := range discovered {
		putString(&b, d.Path)
		putString(&b, d.SubHash.String())
		putUint(&b, uint64(d.Entries))
	}
	return b.Bytes()
}

func putString(b *bytes.Buffer, s string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(s)))
	b.Write(n[:])
	b.WriteString(s)
}

func putUint(b *bytes.Buffer, v uint64) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], v)
	b.Write(n[:])
}
