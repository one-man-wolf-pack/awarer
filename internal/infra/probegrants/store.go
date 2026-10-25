// Package probegrants holds operator-held identity-probe grants: per-user records
// outside every project, one per canonical project root, naming the probe contexts the
// operator approved there. Nothing a checkout supplies can write or stand in for one,
// and reading one never executes or resolves a probe.
package probegrants

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/runcache"
	"awarer/internal/infra/fsx"
)

const (
	grantsDir     = "awa/probe-grants"
	recordVersion = 1
	rootNameTag   = "probe-grant-root/v1\x00"
	// A policy has one context, so a record is bounded by the project's policy count;
	// these caps keep a damaged or hostile record from costing unbounded work.
	maxContexts    = 1024
	maxRecordBytes = 128 << 10
)

type Store struct {
	userDir func() (string, error)
	hasher  hashing.Hasher
}

func New(h hashing.Hasher) *Store { return &Store{userDir: os.UserConfigDir, hasher: h} }

func NewAt(dir string, h hashing.Hasher) *Store {
	return &Store{userDir: func() (string, error) { return dir, nil }, hasher: h}
}

type State string

const (
	StateAbsent      State = "absent"
	StateRecorded    State = "recorded"
	StateUnavailable State = "unavailable"
)

// Grant is one project's grant state; Root is empty when the project root could not
// be resolved.
type Grant struct {
	Root     string
	State    State
	Cause    runcache.ProbeAuthorizationCause
	Detail   string
	contexts []hashing.TreeHash
}

func (g Grant) Check(ctx hashing.TreeHash) runcache.ProbeAuthorization {
	switch g.State {
	case StateRecorded:
		if _, ok := slices.BinarySearchFunc(g.contexts, ctx, compareHash); ok {
			return runcache.ProbeAuthorization{State: runcache.ProbeGranted}
		}
		return runcache.ProbeAuthorization{State: runcache.ProbeDenied, Cause: runcache.ProbeCauseContextChanged}
	case StateAbsent:
		return runcache.ProbeAuthorization{State: runcache.ProbeDenied, Cause: runcache.ProbeCauseNoGrant}
	default:
		return runcache.ProbeAuthorization{State: runcache.ProbeUnavailable, Cause: g.Cause}
	}
}

type record struct {
	Version  int      `json:"version"`
	Root     string   `json:"root"`
	Contexts []string `json:"contexts"`
}

func (s *Store) Check(projectRoot string, ctx hashing.TreeHash) runcache.ProbeAuthorization {
	return s.Load(projectRoot).Check(ctx)
}

// Load reads the grant for projectRoot without failing: an unusable store is a state
// to report, never a reason to fail the inspection or run that asked.
func (s *Store) Load(projectRoot string) Grant {
	root, err := CanonicalRoot(projectRoot)
	if err != nil {
		return unavailable("", runcache.ProbeCauseUnresolvedRoot, err)
	}
	base, err := s.userDir()
	if err != nil {
		return unavailable(root, runcache.ProbeCauseStoreUnreadable, err)
	}
	canonBase, err := filepath.EvalSymlinks(base)
	if errors.Is(err, fs.ErrNotExist) {
		return Grant{Root: root, State: StateAbsent}
	}
	if err != nil {
		return unavailable(root, runcache.ProbeCauseStoreUnreadable, err)
	}
	if rec := s.recordPath(canonBase, root); within(rec, root) {
		return unavailable(root, runcache.ProbeCauseStoreInsideProject, fmt.Errorf("%s lies inside the project", rec))
	}
	// A FIFO or device anywhere on the way to the record is rejected from its kind,
	// never opened as a stream, so inspection cannot wait on a peer that never comes.
	f, err := fsx.OpenRegularNoFollowAt(canonBase, grantsDir+"/"+s.recordName(root))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Grant{Root: root, State: StateAbsent}
	case errors.Is(err, fsx.ErrNotRegular), errors.Is(err, fsx.ErrNotDirectory):
		return unavailable(root, runcache.ProbeCauseStoreInvalid, err)
	case err != nil:
		return unavailable(root, runcache.ProbeCauseStoreUnreadable, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxRecordBytes+1))
	if err != nil {
		return unavailable(root, runcache.ProbeCauseStoreUnreadable, err)
	}
	contexts, err := decode(data, root)
	if err != nil {
		return unavailable(root, runcache.ProbeCauseStoreInvalid, err)
	}
	return Grant{Root: root, State: StateRecorded, contexts: contexts}
}

func decode(data []byte, root string) ([]hashing.TreeHash, error) {
	if len(data) > maxRecordBytes {
		return nil, fmt.Errorf("the grant record exceeds %d bytes", maxRecordBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r record
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("decoding the grant record: %w", err)
	}
	if err := dec.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("the grant record has trailing data")
	}
	switch {
	case r.Version != recordVersion:
		return nil, fmt.Errorf("unsupported grant record version %d", r.Version)
	case r.Root != root:
		return nil, errors.New("the grant record names a different project root")
	case len(r.Contexts) == 0 || len(r.Contexts) > maxContexts:
		return nil, fmt.Errorf("the grant record has %d contexts", len(r.Contexts))
	}
	out := make([]hashing.TreeHash, len(r.Contexts))
	for i, c := range r.Contexts {
		h, err := hashing.ParseTreeHash(c)
		if err != nil {
			return nil, err
		}
		if i > 0 && compareHash(out[i-1], h) >= 0 {
			return nil, errors.New("the grant record contexts are not sorted and unique")
		}
		out[i] = h
	}
	return out, nil
}

// Allow replaces projectRoot's record with exactly contexts and returns the canonical
// root. A failure before the rename leaves the previous record; a directory-sync
// failure after it leaves the new record installed but not confirmed durable.
func (s *Store) Allow(projectRoot string, contexts []hashing.TreeHash) (string, error) {
	if len(contexts) == 0 || len(contexts) > maxContexts {
		return "", fmt.Errorf("a grant approves 1 to %d probe contexts, not %d", maxContexts, len(contexts))
	}
	root, canonBase, err := s.writable(projectRoot, true)
	if err != nil {
		return "", err
	}
	sorted := slices.Clone(contexts)
	slices.SortFunc(sorted, compareHash)
	sorted = slices.CompactFunc(sorted, func(a, b hashing.TreeHash) bool { return a == b })
	r := record{Version: recordVersion, Root: root, Contexts: make([]string, len(sorted))}
	for i, c := range sorted {
		r.Contexts[i] = c.String()
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	// Directory-only traversal keeps a FIFO on the path from stalling allow.
	dir, err := fsx.MkdirAllDirOnlyNoFollow(canonBase, grantsDir, paths.DirPerm)
	if err != nil {
		return "", fmt.Errorf("writing the probe grant: %w", err)
	}
	defer func() { _ = dir.Close() }()
	if err := fsx.ReplaceBytesAt(dir, s.recordName(root), data, paths.FilePerm); err != nil {
		return "", fmt.Errorf("writing the probe grant: %w", err)
	}
	return root, nil
}

func (s *Store) Revoke(projectRoot string) (root string, removed bool, err error) {
	root, canonBase, err := s.writable(projectRoot, false)
	if err != nil || canonBase == "" {
		return root, false, err
	}
	dir, err := fsx.OpenDirOnlyNoFollow(canonBase, grantsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return root, false, nil
	}
	if err != nil {
		return root, false, fmt.Errorf("opening the probe grant store: %w", err)
	}
	defer func() { _ = dir.Close() }()
	if err := fsx.RemoveAt(dir, s.recordName(root)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return root, false, nil
		}
		return root, false, fmt.Errorf("removing the probe grant: %w", err)
	}
	if err := fsx.SyncDir(dir); err != nil {
		return root, true, fmt.Errorf("removing the probe grant: %w", err)
	}
	return root, true, nil
}

// writable yields an empty canonBase when the user directory is absent and create is unset.
func (s *Store) writable(projectRoot string, create bool) (root, canonBase string, err error) {
	root, err = CanonicalRoot(projectRoot)
	if err != nil {
		return "", "", fmt.Errorf("resolving the project root: %w", err)
	}
	base, err := s.userDir()
	if err != nil {
		return root, "", fmt.Errorf("locating the user configuration directory: %w", err)
	}
	if create {
		if err := os.MkdirAll(base, paths.DirPerm); err != nil {
			return root, "", fmt.Errorf("creating the user configuration directory: %w", err)
		}
	}
	canonBase, err = filepath.EvalSymlinks(base)
	if !create && errors.Is(err, fs.ErrNotExist) {
		return root, "", nil
	}
	if err != nil {
		return root, "", fmt.Errorf("resolving the user configuration directory: %w", err)
	}
	if rec := s.recordPath(canonBase, root); within(rec, root) {
		return root, "", fmt.Errorf("the grant record %s lies inside the project %s; a grant there could be supplied by the checkout", rec, root)
	}
	return root, canonBase, nil
}

// CanonicalRoot resolves every symlink in the project root, so aliases of one
// directory share a grant and a moved or copied project does not inherit it.
func CanonicalRoot(projectRoot string) (string, error) {
	abs, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// recordPath is where the record actually lives: the store's components are traversed
// without following symlinks, so this lexical path below the canonical base is its
// location, and a project containing any part of it could supply the record itself.
func (s *Store) recordPath(canonBase, root string) string {
	return filepath.Join(canonBase, filepath.FromSlash(grantsDir), s.recordName(root))
}

func (s *Store) recordName(root string) string {
	return s.hasher.HashBytes([]byte(rootNameTag+root)).Hex() + ".json"
}

func unavailable(root string, cause runcache.ProbeAuthorizationCause, err error) Grant {
	return Grant{Root: root, State: StateUnavailable, Cause: cause, Detail: err.Error()}
}

func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func compareHash(a, b hashing.TreeHash) int { return strings.Compare(a.Hex(), b.Hex()) }
