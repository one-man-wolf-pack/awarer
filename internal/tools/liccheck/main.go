// Command liccheck validates awa's third-party license manifest and generates the
// deterministic THIRD_PARTY_NOTICES artifact. It is a build/dev tool, not a shipped
// binary: it lives under internal/ so it is never part of the public `awa` surface,
// and it exists so the release gate can prove that what is linked into awa matches
// the reviewed license policy and that the committed notice is current.
//
// It is one flat package on purpose. The audit is a short procedure — inspect the
// production module graph with `go list`, read the reviewed manifest, verify the
// pinned source bytes, render the notice — and its whole consequence sits at the
// legal boundary, not in any internal layering. Everything the procedure needs is
// here; nothing outside it models licenses.
//
// Modes:
//
//	-mode check   (default) validate the manifest against the production graph and
//	              pinned source-text digests, and confirm the committed notice
//	              matches a freshly generated one. Mutates nothing; exits non-zero on
//	              any violation or drift.
//	-mode update  regenerate THIRD_PARTY_NOTICES from the reviewed manifest and
//	              atomically replace the committed file. Changes only the notice.
//
// Scope selects how much of the target matrix the check enumerates:
//
//	-scope fast   (default for `just check`) single host target: structural, policy,
//	              digest, and notice checks, deferring cross-target rules.
//	-scope full   (release gate) every supported release target, full union.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

func main() {
	mode := flag.String("mode", "check", "check | update")
	scope := flag.String("scope", "fast", "fast (single host target) | full (all release targets)")
	manifestPath := flag.String("manifest", "third_party/licenses.json", "path to the reviewed license manifest")
	noticesPath := flag.String("notices", "THIRD_PARTY_NOTICES", "path to the committed third-party notices")
	output := flag.String("output", "", "for -mode update: write the notice here (defaults to -notices)")
	flag.Parse()

	// Every value this tool takes arrives from a recipe as `-flag "$SOME_PATH"`, so an
	// unquoted expansion of a path holding a space would land as a flag value plus a
	// leftover word. Ignoring that word is how `liccheck check -scope full` would run
	// the default fast scope and still print OK — the same silent downgrade the scope
	// guard below refuses, arriving through the argument list instead. Refused here for
	// the same reason refgen refuses it.
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "liccheck: unexpected argument %q\n", flag.Arg(0))
		os.Exit(1)
	}

	// This tool is its own entrypoint, so minting the signal-aware root context here
	// is correct; Ctrl-C cancels the long-running graph collection and evidence
	// resolution rather than leaving a subprocess running.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt) //nolint:forbidigo
	defer stop()
	if err := run(ctx, *mode, *scope, *manifestPath, *noticesPath, *output); err != nil {
		fmt.Fprintf(os.Stderr, "liccheck: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, mode, scope, manifestPath, noticesPath, output string) error {
	// The scope is refused before anything else runs. It arrives from a Just recipe,
	// and a typo there must not turn the release gate's full cross-target enumeration
	// into the single-host one — a gate that covers less than it claims is worse than
	// one that fails. Refusing first also means the diagnostic names the mistake
	// rather than whatever the smaller scope happened to find.
	if mode == "check" && scope != "fast" && scope != "full" {
		return fmt.Errorf("unknown scope %q (want fast or full)", scope)
	}
	if mode != "check" && mode != "update" {
		return fmt.Errorf("unknown mode %q (want check or update)", mode)
	}

	root, err := moduleRoot()
	if err != nil {
		return err
	}
	// newEvidence resolves the module-cache index under the cancellable context, so
	// the whole audit can be interrupted mid-resolution and no unprepared store can be
	// used.
	ev, err := newEvidence(ctx, root)
	if err != nil {
		return err
	}

	if mode == "update" {
		if output == "" {
			output = noticesPath
		}
		return update(manifestPath, output, ev)
	}
	return check(ctx, root, ev, scope, manifestPath, noticesPath)
}

// check validates the manifest against the observed production graph and the pinned
// source bytes, then confirms the committed notice is current. It writes nothing.
func check(ctx context.Context, root string, ev *evidence, scope, manifestPath, noticesPath string) error {
	raw, err := loadManifest(manifestPath)
	if err != nil {
		return err
	}
	m, violations := parseManifest(raw)

	full := scope == "full"
	targets := []string{runtime.GOOS + "/" + runtime.GOARCH}
	if full {
		targets = releaseTargets
	}
	union, err := collectProduction(ctx, root, targets)
	if err != nil {
		return err
	}
	violations = append(violations, reconcile(m, union, full)...)
	violations = append(violations, verifyEvidence(m, ev)...)

	// The notice is regenerated in memory and compared to the committed file, but only
	// once the manifest is known good: a generator fed a rejected manifest would report
	// notice drift for what is really a policy failure.
	var noticeErr error
	if len(violations) == 0 {
		generated, gerr := renderNotice(m, ev)
		committed, rerr := os.ReadFile(noticesPath)
		switch {
		case gerr != nil:
			noticeErr = gerr
		case rerr != nil:
			noticeErr = fmt.Errorf("reading committed notices %s: %w", noticesPath, rerr)
		case !bytes.Equal(committed, generated):
			noticeErr = fmt.Errorf("%s is stale; regenerate with `just notices-update`", noticesPath)
		}
	}

	if len(violations) == 0 && noticeErr == nil {
		fmt.Printf("liccheck: OK (production=%d, scope=%s)\n", len(union), scope)
		return nil
	}
	sortViolations(violations)
	for _, v := range violations {
		fmt.Fprintln(os.Stderr, "  "+v.String())
	}
	failures := len(violations)
	if noticeErr != nil {
		fmt.Fprintln(os.Stderr, "  [notice] "+noticeErr.Error())
		failures++
	}
	return fmt.Errorf("license compliance check failed: %d issue(s)", failures)
}

// reconcile compares the observed production union with the reviewed manifest.
//
// Both scopes require every observed module to be manifested at the exact identity Go
// selected, because a module linked into a binary with no reviewed license is the
// failure this gate exists for and one host can prove it.
//
// The two cross-target rules — a per-module target set, and an entry no target
// selects — run only in full scope. A single host cannot observe the union, so
// claiming either from it would be an assertion the evidence does not support; fast
// scope therefore stays silent about them rather than guessing.
func reconcile(m manifest, union map[string]*observed, full bool) []violation {
	var vs []violation
	byPath := make(map[string]entry, len(m.entries))
	for _, e := range m.entries {
		byPath[e.module.path] = e
	}

	for path, om := range union {
		e, ok := byPath[path]
		if !ok {
			// A module whose row exists but was rejected is already reported by the
			// defect that rejected it; calling it unmanifested as well would point a
			// reviewer at a missing entry that is in fact sitting in the file.
			if _, declared := m.declared[path]; !declared {
				vs = append(vs, violation{rule: ruleUnmanifested, module: om.module.String(), detail: "production module is not in the reviewed manifest"})
			}
			continue
		}
		if e.module.String() != om.module.String() {
			vs = append(vs, violation{rule: ruleVersionDrift, module: path,
				detail: "manifest has " + e.module.String() + ", observed " + om.module.String()})
		}
		if full && !slices.Equal(e.targets, om.targets) {
			vs = append(vs, violation{rule: ruleTargetDrift, module: path,
				detail: "manifest targets " + joinTargets(e.targets) + ", observed " + joinTargets(om.targets)})
		}
	}

	if full {
		for path := range byPath {
			if _, ok := union[path]; !ok {
				vs = append(vs, violation{rule: ruleStaleEntry, module: path, detail: "production manifest entry is not selected by any release target"})
			}
		}
	}
	return vs
}

// verifyEvidence recomputes every reviewed text's digest from the pinned module and
// compares it to the manifest. It covers texts the notice never reproduces too, so a
// reviewed AUTHORS or informational NOTICE cannot drift unnoticed just because it
// carries no redistribution obligation.
func verifyEvidence(m manifest, ev *evidence) []violation {
	var vs []violation
	// A zero digest means the reviewed project-license record itself failed to parse,
	// which parseManifest has already reported. Comparing an unparsed record against
	// the file on disk would add a second violation describing the same defect, so the
	// disk check is what a parsed record earns — this guard is load-bearing, not a
	// defensive nil check.
	if !m.projectLicense.digest.IsZero() {
		got, err := ev.projectLicenseDigest(m.projectLicense.path)
		switch {
		case err != nil:
			vs = append(vs, violation{rule: ruleProjectLicense, file: m.projectLicense.path, detail: err.Error()})
		case got.String() != m.projectLicense.digest.String():
			vs = append(vs, violation{rule: ruleProjectLicense, file: m.projectLicense.path,
				detail: "project license changed since review (manifest " + m.projectLicense.digest.String() + ", on disk " + got.String() + ")"})
		}
	}
	for _, e := range m.entries {
		// Whether the module resolves at all is asked once. It is a fact about the
		// identity, not about any file inside it, and reporting it per reviewed text
		// would bury the one actionable line — an un-hydrated cache under a dozen
		// duplicates, a version bump under one per license file.
		if _, err := ev.dirOf(e.module); err != nil {
			vs = append(vs, violation{rule: ruleUnresolvedModule, module: e.module.path, detail: err.Error()})
			continue
		}
		for _, t := range e.texts {
			_, got, err := ev.read(e.module, t.relPath)
			if err != nil {
				vs = append(vs, violation{rule: ruleMissingText, module: e.module.path, file: t.relPath, detail: err.Error()})
				continue
			}
			if got.String() != t.digest.String() {
				vs = append(vs, violation{rule: ruleTextChanged, module: e.module.path, file: t.relPath,
					detail: "manifest digest " + t.digest.String() + ", pinned bytes hash to " + got.String()})
			}
		}
	}
	return vs
}

// update regenerates the committed notice atomically from the reviewed manifest.
// renderNotice verifies every reviewed digest against the pinned bytes as it
// materializes, so update fails closed on evidence drift instead of publishing
// unreviewed text.
func update(manifestPath, output string, ev *evidence) error {
	raw, err := loadManifest(manifestPath)
	if err != nil {
		return err
	}
	m, violations := parseManifest(raw)
	if len(violations) > 0 {
		for _, v := range violations {
			fmt.Fprintln(os.Stderr, "  "+v.String())
		}
		return fmt.Errorf("manifest is not valid; run `just check` first: %d issue(s)", len(violations))
	}
	// Build the whole document in memory first, so a failure leaves the committed
	// notice untouched.
	generated, err := renderNotice(m, ev)
	if err != nil {
		return err
	}
	if err := atomicWrite(output, generated); err != nil {
		return err
	}
	fmt.Printf("notices-update: wrote %s (%d bytes)\n", output, len(generated))
	return nil
}

// loadManifest decodes the manifest JSON strictly: unknown fields are refused at
// every level, and the file must be exactly one JSON document, so a second object or
// trailing garbage cannot ride along unnoticed behind a valid-looking manifest.
func loadManifest(path string) (rawManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return rawManifest{}, fmt.Errorf("reading manifest %s: %w", path, err)
	}
	var raw rawManifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return rawManifest{}, fmt.Errorf("decoding manifest %s: %w", path, err)
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return rawManifest{}, fmt.Errorf("decoding manifest %s: unexpected trailing data after the JSON document", path)
	}
	return raw, nil
}

// moduleRoot walks up from the working directory to the directory holding go.mod.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found from %s upward; liccheck must run inside the awa module", dir)
		}
		dir = parent
	}
}

// joinTargets renders a target set for a diagnostic, naming the empty set rather than
// rendering it as nothing.
func joinTargets(ts []string) string {
	if len(ts) == 0 {
		return "(none)"
	}
	return strings.Join(ts, ",")
}
