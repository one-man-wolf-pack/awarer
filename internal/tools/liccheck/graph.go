package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// releaseTargets is the GOOS/GOARCH matrix the licence audit must cover, in a fixed
// canonical (sorted) order so output is deterministic.
//
// It is a short literal, deliberately not derived. `.goreleaser.yaml` owns what is
// actually built and published and the justfile's cross-build list is early compile
// feedback; this list owns licence reachability. Nothing synchronizes the three,
// because each fails at its own consequence boundary — a divergence shows up as
// missing legal evidence here and as a missing artifact there, and a parser tying
// them together would add a fourth authority rather than remove one.
var releaseTargets = []string{
	"darwin/amd64",
	"darwin/arm64",
	"freebsd/amd64",
	"linux/amd64",
	"linux/arm64",
	"windows/amd64",
}

// goEnv returns a child environment for a `go` invocation targeting goos/goarch, with
// the audit's hermetic policy applied over the current environment. The signature is
// deliberately narrow — the only inputs are the required target values — so a caller
// cannot pass GOWORK, GOPROXY, GOFLAGS, or any other setting that would weaken the
// compliance invariants. The policy pins, non-overridably:
//
//   - GOWORK=off             — ignore any workspace file that could change selection
//   - GOFLAGS=-mod=readonly  — never let a check mutate go.mod/go.sum
//   - GOENV=off              — ignore the user go env file
//   - GOTOOLCHAIN=local      — never auto-download a different toolchain
//   - GOPROXY=off            — offline: evidence is the pinned modules already present
//   - CGO_ENABLED=0          — match the pure-Go release build mode
//   - GOAMD64=v1             — baseline amd64 instruction set
//   - GOARM64=v8.0           — baseline arm64 instruction set
//   - GOEXPERIMENT=          — the toolchain's own defaults, no ambient experiment set
//   - GOFIPS140=off          — the standard crypto implementation
//
// A compliance gate must be reproducible and offline: an inherited GOWORK, GOFLAGS,
// GOENV, or proxy setting could otherwise change build tags, module selection, or
// replacements, or download missing data mid-gate.
//
// The last four pins earn their place for a different reason than the rest: they
// change the instructions emitted rather than which sources are selected, so a leaked
// GOAMD64=v3 would yield a release that simply does not run on the hardware the
// project claims to support, while the module graph, version stamp, and checksums all
// stay consistent with it. Nothing downstream inspects them, which is precisely why
// they are fixed here.
//
// GOEXPERIMENT is pinned empty rather than to "none": empty means the toolchain's
// defaults, whereas "none" would additionally switch off experiments that are on by
// default and is therefore its own deviation from the shipped toolchain.
//
// Module download is a prerequisite (like compilation) run before the gate, not
// during it, so GOPROXY=off makes a missing module fail closed instead of fetching.
func goEnv(goos, goarch string) ([]string, error) {
	if !validEnvValue(goos) || !validEnvValue(goarch) {
		return nil, fmt.Errorf("GOOS and GOARCH must be non-empty, space-free values (got %q/%q)", goos, goarch)
	}
	m := make(map[string]string)
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	// Fixed policy — applied last so nothing in the ambient environment can win.
	m["GOWORK"] = "off"
	m["GOFLAGS"] = "-mod=readonly"
	m["GOENV"] = "off"
	m["GOTOOLCHAIN"] = "local"
	m["GOPROXY"] = "off"
	m["CGO_ENABLED"] = "0"
	m["GOAMD64"] = "v1"
	m["GOARM64"] = "v8.0"
	m["GOEXPERIMENT"] = ""
	m["GOFIPS140"] = "off"
	m["GOOS"] = goos
	m["GOARCH"] = goarch
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out, nil
}

// validEnvValue reports whether a GOOS/GOARCH value is non-empty and free of spaces
// and separators that would corrupt the environment entry.
func validEnvValue(v string) bool {
	return v != "" && !strings.ContainsAny(v, " \t\n=")
}

// runGo executes `go args...` in dir under the hermetic audit environment, returning
// stdout. A failure with GOPROXY=off usually means a module is not downloaded, so the
// go stderr is surfaced along with the fix.
func runGo(ctx context.Context, dir, goos, goarch string, args ...string) ([]byte, error) {
	env, err := goEnv(goos, goarch)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go %s: %w: %s (if a module is missing, run `go mod download` first)",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// listedPackage is the subset of `go list -json` package output the audit needs.
type listedPackage struct {
	Standard bool
	Module   *listedModule
}

// listedModule is the subset of a `go list` module record the audit needs. The same
// shape serves the package stream and `go list -m -json all`.
type listedModule struct {
	Path    string
	Version string
	Main    bool
	Dir     string
	Replace *listedModule
}

// observed is a production module the audit saw in the graph, with the release
// targets whose compiled package closure selected it.
type observed struct {
	module  moduleID
	targets []string
}

// productionModules returns the non-standard, non-main modules linked into
// ./cmd/awa for one target. This is the whole graph model: `go list -deps -json`
// already reports the compiled closure with each package's module, version, and
// replacement, so there is nothing left to derive.
func productionModules(ctx context.Context, root, target string) ([]moduleID, error) {
	goos, goarch, _ := strings.Cut(target, "/")
	out, err := runGo(ctx, root, goos, goarch, "list", "-deps", "-json", "./cmd/awa")
	if err != nil {
		return nil, fmt.Errorf("production graph for %s: %w", target, err)
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	seen := make(map[string]moduleID)
	for {
		var p listedPackage
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decoding go list output for %s: %w", target, err)
		}
		if p.Standard || p.Module == nil || p.Module.Main {
			continue
		}
		id, err := identityOf(p.Module)
		if err != nil {
			return nil, err
		}
		seen[id.path] = id
	}
	mods := make([]moduleID, 0, len(seen))
	for _, id := range seen {
		mods = append(mods, id)
	}
	sort.Slice(mods, func(i, j int) bool { return mods[i].path < mods[j].path })
	return mods, nil
}

// collectProduction folds the per-target production graphs into one union keyed by
// module path. A module selected on only one target is preserved with just that
// target: the union never drops a single-target production obligation. A module that
// resolves to different identities on different targets fails closed rather than
// letting either identity stand for both.
func collectProduction(ctx context.Context, root string, targets []string) (map[string]*observed, error) {
	if len(targets) == 0 {
		return nil, fmt.Errorf("no release targets to collect")
	}
	union := make(map[string]*observed)
	for _, target := range targets {
		mods, err := productionModules(ctx, root, target)
		if err != nil {
			return nil, err
		}
		for _, mod := range mods {
			cur, ok := union[mod.path]
			if !ok {
				union[mod.path] = &observed{module: mod, targets: []string{target}}
				continue
			}
			if cur.module.String() != mod.String() {
				return nil, fmt.Errorf("module %q resolves to conflicting identities across targets: %s vs %s",
					mod.path, cur.module, mod)
			}
			cur.targets = append(cur.targets, target)
		}
	}
	for _, om := range union {
		sort.Strings(om.targets)
	}
	return union, nil
}

// identityOf builds a module identity from a go list module record, applying any
// replacement Go reported. A module@version replacement is reproducible and
// supported; a local filesystem replacement (no version) is host-dependent and not
// reproducible, so the audit fails closed rather than recording an unreproducible
// identity.
func identityOf(m *listedModule) (moduleID, error) {
	base, err := newModuleID(m.Path, m.Version)
	if err != nil {
		return moduleID{}, err
	}
	if m.Replace == nil {
		return base, nil
	}
	if m.Replace.Version == "" {
		return moduleID{}, fmt.Errorf("module %s is replaced by a local path %q; local replacements are not supported in the release audit", m.Path, m.Replace.Path)
	}
	return base.withReplacement(m.Replace.Path, m.Replace.Version)
}
