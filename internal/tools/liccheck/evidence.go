package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"awarer/internal/domain/hashing"
	"awarer/internal/infra/blake3hash"
)

// errTextMissing names the one read failure a reviewer has to act on differently: a
// reviewed license file that is gone from the pinned module, rather than a file that
// could not be read. It keeps that diagnosis out of a bare "no such file" message.
var errTextMissing = errors.New("license text missing from module")

// evidence reads license/attribution bytes from the read-only Go module cache. It
// never writes and never reaches the network: a module absent from the local cache is
// an error telling the caller to run `go mod download`, so what a release gate reads
// is exactly the pinned bytes Go already selected.
type evidence struct {
	root   string
	hasher *blake3hash.Hasher
	dirs   map[string]string
}

// newEvidence resolves the module-directory index under ctx and returns a ready
// store. The one subprocess it runs (`go list -m -json all`) happens here, so it is
// cancellable with the audit and a caller can never accidentally use an unresolved
// store: there is no separate prepare step to forget.
//
// The index is keyed by full module identity — path, version, and any module@version
// replacement — not by path alone. A manifest naming module@old-version therefore
// cannot silently read the directory of the currently selected module@new-version:
// the identity is simply not found, so version and replacement drift fail closed
// before any file is read.
func newEvidence(ctx context.Context, root string) (*evidence, error) {
	e := &evidence{root: root, hasher: blake3hash.New(), dirs: map[string]string{}}
	out, err := runGo(ctx, root, runtime.GOOS, runtime.GOARCH, "list", "-m", "-json", "all")
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var m listedModule
		if err := dec.Decode(&m); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decoding go list -m output: %w", err)
		}
		id, dir, ok := identityAndDir(m)
		if ok && dir != "" {
			e.dirs[id.String()] = dir
		}
	}
	return e, nil
}

// identityAndDir derives the full identity and effective cache directory for one
// `go list -m` module, returning ok=false for the main module or a local
// (versionless) replacement, neither of which has a reproducible identity.
func identityAndDir(m listedModule) (moduleID, string, bool) {
	base, err := newModuleID(m.Path, m.Version)
	if err != nil {
		return moduleID{}, "", false // main module or missing version
	}
	if m.Replace == nil {
		return base, m.Dir, true
	}
	if m.Replace.Version == "" {
		return moduleID{}, "", false // local replacement: not supported
	}
	id, err := base.withReplacement(m.Replace.Path, m.Replace.Version)
	if err != nil {
		return moduleID{}, "", false
	}
	return id, m.Replace.Dir, true
}

// read returns a module-relative file's verbatim bytes together with the digest of
// those exact bytes. Reading and hashing in one operation is the only shape this
// store offers, so the bytes a caller publishes are always the bytes it verified —
// there is no read-then-hash window for a mutable cache to slip through.
//
// It rejects any path that escapes the module directory even though the manifest
// already validates paths, and reports a missing file by wrapping errTextMissing.
func (e *evidence) read(mod moduleID, relPath string) ([]byte, hashing.TreeHash, error) {
	dir, err := e.dirOf(mod)
	if err != nil {
		return nil, hashing.TreeHash{}, err
	}
	data, err := readUnder(dir, relPath)
	if err != nil {
		return nil, hashing.TreeHash{}, fmt.Errorf("%s in %s: %w", relPath, mod, err)
	}
	return data, e.hasher.HashBytes(data), nil
}

// dirOf returns the extracted cache directory for an exact module identity. A miss
// means the identity Go selected is not the one being asked for, or the module was
// never downloaded — both are properties of the module, not of any file inside it, so
// a caller checking several texts asks once rather than once per text.
func (e *evidence) dirOf(mod moduleID) (string, error) {
	dir, ok := e.dirs[mod.String()]
	if !ok || dir == "" {
		return "", fmt.Errorf("module %s not found in the local cache (version/replacement drift, or not downloaded); run `go mod download` and re-run `just check`", mod)
	}
	return dir, nil
}

// projectLicenseDigest returns the digest of a repo-root-relative file (the project's
// own LICENSE). It reads from the module root — never the module cache — so the
// project license is verified the same way third-party evidence is.
func (e *evidence) projectLicenseDigest(relPath string) (hashing.TreeHash, error) {
	data, err := readUnder(e.root, relPath)
	if err != nil {
		return hashing.TreeHash{}, fmt.Errorf("project license %s: %w", relPath, err)
	}
	return e.hasher.HashBytes(data), nil
}

// readUnder reads base/relPath, refusing any path that leaves base and reporting a
// missing file as errTextMissing.
func readUnder(base, relPath string) ([]byte, error) {
	full := filepath.Join(base, filepath.FromSlash(relPath))
	rel, err := filepath.Rel(base, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("refusing unsafe path %q", relPath)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errTextMissing
		}
		return nil, err
	}
	return data, nil
}
