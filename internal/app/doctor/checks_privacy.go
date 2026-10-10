package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"awarer/internal/app/checkpointcmd"
	"awarer/internal/app/scanner"
	domconfig "awarer/internal/domain/config"
	"awarer/internal/domain/doctor"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/pattern"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/fsx"
	"awarer/internal/infra/worktreefs"
)

// checkConfig raises advisory findings about the run/checkpoint config's
// local-evidence posture. Both are warnings, never blockers: the real privacy
// protection is that awa does not persist raw env values and keeps .awa private by
// default; these only flag configurations that make the durable evidence riskier.
func (s *Service) checkConfig(ctx context.Context, layout paths.Layout, resolved domconfig.ResolvedConfig, rep *report) {
	cfg := resolved.Config()

	// env-allowlist-suspicious: names that commonly carry secrets. Adding them does not
	// leak the value (it is persisted as a redacted identity), but it is worth flagging
	// because the child still receives the real value and a broad allowlist is a smell.
	if names := suspiciousEnvNames(cfg.Run.EnvAllowlist); len(names) > 0 {
		source := resolved.SourcePathOf("run.env_allowlist")
		rep.add(finding(doctor.CodeEnvAllowlistSuspicious, doctor.SeverityWarning, doctor.SubsystemConfig,
			source, "run.env_allowlist",
			"run.env_allowlist contains secret-looking names ("+strings.Join(names, ", ")+"); their values reach the child and are keyed (as redacted identities, not stored raw)"+overrideHint(source), false))
	} else {
		rep.pass()
	}

	// env-allowlist-injects-code: names whose documented purpose is to make the process
	// load or execute something it otherwise would not — a preloaded library, a startup
	// file, an interpreter option. Inheriting one is sometimes deliberate, so this is
	// advisory; but it means the supervised command's behavior is decided partly outside
	// the command, which is worth saying once.
	if names := codeInjectingEnvNames(cfg.Run.EnvAllowlist); len(names) > 0 {
		source := resolved.SourcePathOf("run.env_allowlist")
		rep.add(finding(doctor.CodeEnvAllowlistInjectsCode, doctor.SeverityWarning, doctor.SubsystemConfig,
			source, "run.env_allowlist",
			"run.env_allowlist contains names that can load or execute code in the child ("+strings.Join(names, ", ")+"); remove them unless the command genuinely needs to inherit them"+overrideHint(source), false))
	} else {
		rep.pass()
	}

	s.checkPatterns(layout, resolved, rep)

	// content-storage-enabled: storing file contents is the point of checkpoints, but in
	// a project that looks like it holds secrets it means a plaintext secret file can
	// become a blob in .awa/store/blobs. Flag only a recognized file a checkpoint would
	// actually own bytes for, so an ordinary project — or one whose secret-looking files
	// are excluded, hash-only, or skipped — is never nagged. A candidate the check could
	// not inspect is its own warning, never a pass.
	capture := secretCaptureCandidates(ctx, layout, cfg)
	source := resolved.SourcePathOf("checkpoint.store_file_contents")
	if capture.eligible != "" {
		rep.add(finding(doctor.CodeContentStorageEnabled, doctor.SeverityWarning, doctor.SubsystemConfig,
			source, "checkpoint.store_file_contents",
			"checkpoint.store_file_contents is on and a secret-looking file ("+capture.eligible+") is in checkpoint scope and not kept hash-only; its contents may be stored in .awa/store/blobs — exclude it, or add it to checkpoint.hash_only_patterns (setting that list replaces its env defaults, so restate them)"+overrideHint(source), false))
	}
	if capture.uncertain != "" {
		rep.add(finding(doctor.CodeContentCaptureCheckIncomplete, doctor.SeverityWarning, doctor.SubsystemConfig,
			"", "checkpoint.store_file_contents",
			"could not determine whether secret-looking files would have their contents stored by a checkpoint ("+capture.uncertain+"); treat them as possibly captured", false))
	}
	if capture.eligible == "" && capture.uncertain == "" {
		rep.pass()
	}
}

func (s *Service) checkPatterns(layout paths.Layout, resolved domconfig.ResolvedConfig, rep *report) {
	cfg := resolved.Config()
	rules := pattern.KeyRules("scope.extra_excludes", cfg.Scope.ExtraExcludes)
	rules = append(rules, pattern.KeyRules("history.extra_excludes", cfg.History.ExtraExcludes)...)
	rules = append(rules, pattern.KeyRules("run.extra_excludes", cfg.Run.ExtraExcludes)...)
	diags, err := worktreefs.RootDiagnostics(layout.Root(), rules,
		cfg.Scope.UseGitignore || cfg.Run.UseGitignore, cfg.Scope.UseAwaignore || cfg.Run.UseAwaignore)
	if err != nil {
		rep.add(finding(doctor.CodePatternCheckIncomplete, doctor.SeverityWarning, doctor.SubsystemConfig,
			"", "exclusion patterns", "could not check exclusion patterns: "+err.Error(), false))
		return
	}
	if len(diags) == 0 {
		rep.pass()
		return
	}
	for _, d := range diags {
		var path string
		if d.Origin.InFile() {
			path = filepath.Join(layout.Root(), filepath.FromSlash(d.Origin.File))
		} else {
			path = resolved.SourcePathOf(d.Origin.Key)
		}
		rep.add(finding(doctor.CodePatternNeverMatches, doctor.SeverityWarning, doctor.SubsystemConfig,
			path, d.Origin.String(),
			fmt.Sprintf("pattern %q never matches (%s); it excludes nothing — correct or remove it", d.Text, d.Reason), false))
	}
}

// overrideHint returns the sentence a config finding appends when the flagged value
// is a product default (no source file): the finding's path is left empty — it names
// where the setting IS, not where it could be — so the guidance to add an override
// lives in the message instead. A value that came from a file needs no hint; its path
// already points there.
func overrideHint(sourcePath string) string {
	if sourcePath != "" {
		return ""
	}
	return "; this is a product default — set it in a committed awa.toml or a local .awa/config.toml to change it"
}

// suspiciousEnvNames returns the allowlisted names that look like they carry
// secrets, in stable order, so the finding is deterministic.
func suspiciousEnvNames(allowlist []string) []string {
	var out []string
	for _, name := range allowlist {
		if envNameLooksSensitive(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// envNameLooksSensitive reports whether an environment variable name commonly
// carries a secret value. It is a deliberately broad, advisory heuristic.
func envNameLooksSensitive(name string) bool {
	u := strings.ToUpper(name)
	if strings.HasPrefix(u, "AWS_") {
		return true
	}
	// PASS also covers PASSWORD/PASSWD/PASSPHRASE; KEY also covers APIKEY/API_KEY.
	for _, frag := range []string{"TOKEN", "SECRET", "PASS", "KEY", "CREDENTIAL"} {
		if strings.Contains(u, frag) {
			return true
		}
	}
	return false
}

// codeInjectingEnvNames returns the allowlisted names from the bounded loader/startup
// set, in stable order, so the finding is deterministic.
func codeInjectingEnvNames(allowlist []string) []string {
	var out []string
	for _, name := range allowlist {
		if envNameInjectsCode(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// codeInjectingEnvExact and codeInjectingEnvPrefixes are the closed, high-confidence set
// of names whose documented job is to make a process load or run extra code: dynamic
// loader preloads and search paths, shell startup files, and interpreter option
// variables that accept `-r`/`-e`-style arguments.
//
// It is deliberately bounded rather than heuristic. Guessing from a substring would
// sweep in ordinary settings that merely look related, and an advisory that fires on
// innocent configuration teaches users to ignore it. Every entry here can change what
// code runs, and every entry means only that.
//
// POSIX `sh` reads a startup file from ENV, which is why the environment audit lists it
// as a never-default name. It is still absent here: ENV is also one of the most common
// application-level settings there is (ENV=production), so flagging it would fire on
// ordinary configuration far more often than on the shell behavior it means to warn
// about — and an advisory a user learns to dismiss protects nobody.
var (
	codeInjectingEnvExact = []string{
		"LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT",
		"BASH_ENV", "ZDOTDIR",
		"NODE_OPTIONS", "PYTHONSTARTUP", "RUBYOPT", "PERL5OPT", "JAVA_TOOL_OPTIONS",
	}
	// DYLD_ is macOS's loader namespace (DYLD_INSERT_LIBRARIES, DYLD_LIBRARY_PATH,
	// DYLD_FRAMEWORK_PATH, ...); the whole family exists to redirect loading.
	codeInjectingEnvPrefixes = []string{"DYLD_"}
)

// envNameInjectsCode reports whether an environment variable name is in the bounded
// loader/startup set. Names are compared case-insensitively so the advisory behaves the
// same on a case-insensitive platform as on a case-sensitive one.
func envNameInjectsCode(name string) bool {
	u := strings.ToUpper(name)
	for _, exact := range codeInjectingEnvExact {
		if u == exact {
			return true
		}
	}
	for _, prefix := range codeInjectingEnvPrefixes {
		if strings.HasPrefix(u, prefix) {
			return true
		}
	}
	return false
}

// captureCheck is the outcome of the bounded capture-eligibility check: the first
// recognized root file a checkpoint would own bytes for, and the first inspection
// failure, if any. Both may be set; neither set is a confirmed clean answer for the
// candidates it covers and nothing more.
type captureCheck struct {
	eligible  string
	uncertain string
}

// secretCaptureCandidates applies the effective checkpoint policy to each
// secret-looking top-level file, in listing order. It reads only the root listing and
// candidate metadata — the scan's own scope classification (ClassifyRootFile), the
// scanner's large-file storage decision, and the checkpoint retention decision — and
// never opens, hashes, or indexes a candidate. A candidate that is excluded, not a
// regular file, skipped, or kept hash-only is passed over, so it cannot hide a later
// eligible one.
func secretCaptureCandidates(ctx context.Context, layout paths.Layout, cfg domconfig.Config) captureCheck {
	var out captureCheck
	if !cfg.Checkpoint.StoreFileContents {
		return out // no checkpoint owns any file's bytes
	}
	entries, err := os.ReadDir(layout.Root())
	if err != nil {
		out.uncertain = "listing the project root: " + err.Error()
		return out
	}
	walker := worktreefs.New()
	scope := cfg.HistoryScanScope()
	retention, err := checkpointcmd.NewRetention(cfg.Checkpoint, worktreefs.CompilePatterns)
	if err != nil {
		out.uncertain = "compiling checkpoint.hash_only_patterns: " + err.Error()
		return out
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return captureCheck{} // Run reports the interruption, not a partial answer
		}
		name := e.Name()
		if e.IsDir() || !secretLookingName(name) {
			continue
		}
		var captured bool
		err := walker.ClassifyRootFile(ctx, layout, scope, name, func(n worktree.Node) error {
			if n.Kind != worktree.KindRegular || n.Skipped {
				return nil
			}
			captured = retention.Storage(n.Path, scanner.RegularStorage(cfg.Hashing, n.Size())) == worktree.StorageBlob
			return nil
		})
		switch {
		case ctx.Err() != nil:
			return captureCheck{}
		case err != nil:
			if out.uncertain == "" {
				out.uncertain = name + ": " + err.Error()
			}
		case captured && out.eligible == "":
			out.eligible = name
		}
	}
	return out
}

// secretLookingName reports whether a top-level file name conventionally holds
// credentials. Matching is case-insensitive and by name only; it is a bounded
// candidate set, not secret detection.
func secretLookingName(name string) bool {
	lower := strings.ToLower(name)
	switch {
	case lower == ".env" || strings.HasPrefix(lower, ".env."):
		return true
	case lower == ".netrc" || lower == ".pgpass" || lower == ".npmrc":
		return true
	case strings.HasPrefix(lower, "id_rsa") || strings.HasPrefix(lower, "id_dsa") ||
		strings.HasPrefix(lower, "id_ecdsa") || strings.HasPrefix(lower, "id_ed25519"):
		return true
	case lower == "credentials" || lower == "credentials.json" || lower == "secrets.json":
		return true
	}
	return false
}

// checkPermissions verifies the awa-owned state directory and its top-level
// directories and files are owner-private. It checks the known layout paths (the
// .awa marker, the required subdirectories, the config file, and the guard) rather
// than walking every blob: a private (0700) directory already blocks traversal to
// the files inside it, and the risk is exactly these directories being
// created group/world-accessible. A broad mode is a single aggregated warning that
// names the paths and the one-line fix, never a per-file dump.
func (s *Service) checkPermissions(layout paths.Layout, rep *report) {
	type target struct {
		path string
		dir  bool
	}
	targets := []target{{layout.AwaDir(), true}}
	for _, d := range layout.RequiredDirs() {
		targets = append(targets, target{d, true})
	}
	targets = append(targets,
		target{layout.ConfigFile(), false},
		target{layout.StateGitignore(), false},
	)

	var broad []string
	for _, t := range targets {
		info, err := os.Lstat(t.path)
		if err != nil {
			continue // absence/other damage is reported by the layout and guard checks
		}
		if info.Mode()&os.ModeSymlink != 0 {
			continue // symlinked awa paths are the layout check's concern, not a mode check
		}
		// Group- or world-accessible in any bit is broader than owner-private.
		if info.Mode().Perm()&0o077 != 0 {
			broad = append(broad, relForDisplay(layout.Root(), t.path))
		}
	}
	if len(broad) == 0 {
		rep.pass()
		return
	}
	sort.Strings(broad)
	rep.add(finding(doctor.CodeStatePermissionsTooBroad, doctor.SeverityWarning, doctor.SubsystemLayout,
		layout.AwaDir(), ".awa",
		fmt.Sprintf("%d awa-owned path(s) are group/world-accessible (%s); run 'chmod -R go-rwx %s' to make local evidence private",
			len(broad), strings.Join(broad, ", "), paths.Dir), false))
}

// checkMarkers diagnoses ambiguous project roots: a .awa marker above the resolved
// root (an ancestor project whose scope silently contains this one) or below it (a
// nested project that changes scope for commands run in that subtree). Routine
// commands keep using the nearest valid root; doctor makes the hazard visible.
func (s *Service) checkMarkers(layout paths.Layout, rep *report) {
	root := layout.Root()
	found := false

	// Ancestor: walk up from the parent of root looking for another .awa directory.
	// This is bounded by path depth.
	for dir := filepath.Dir(root); ; {
		marker := filepath.Join(dir, paths.Dir)
		if isRealMarkerDir(marker) {
			rep.add(finding(doctor.CodeAncestorProjectMarker, doctor.SeverityWarning, doctor.SubsystemLayout,
				marker, paths.Dir,
				"an ancestor directory also contains a "+paths.Dir+" marker ("+marker+"); its scope overlaps this project", false))
			found = true
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break // reached the filesystem root
		}
		dir = parent
	}

	// Nested: bounded search below root for another .awa marker. A found marker is a
	// hazard; an incomplete scan (cap reached or an unreadable subtree) is reported as
	// its own warning rather than as a clean pass, so doctor never presents evidence it
	// could not fully see as healthy absence.
	switch scan := findNestedMarker(root, nestedScanCap); {
	case scan.found():
		rep.add(finding(doctor.CodeNestedProjectMarker, doctor.SeverityWarning, doctor.SubsystemLayout,
			scan.marker, paths.Dir,
			"a nested directory contains its own "+paths.Dir+" marker ("+scan.marker+"); commands run there use a different project root", false))
		found = true
	case scan.incomplete:
		rep.add(finding(doctor.CodeNestedMarkerScanIncomplete, doctor.SeverityWarning, doctor.SubsystemLayout,
			root, "nested-marker-scan",
			"could not fully scan for nested "+paths.Dir+" markers ("+scan.detail+"); an ambiguous nested project root may exist undetected", false))
		found = true
	}

	if !found {
		rep.pass()
	}
}

// nestedScanCap bounds how many directories checkMarkers descends into before
// giving up the nested-marker search, keeping doctor bounded on a huge worktree.
// It is generous so an ordinary repository — including its node_modules and other
// dependency trees, which are scanned because a marker there still changes root
// discovery — completes and reports a clean absence; only a genuinely enormous tree
// trips the cap and is reported honestly as an incomplete scan rather than clean.
const nestedScanCap = 50_000

// nestedScan is the typed outcome of the bounded search for a nested .awa marker:
// exactly one of found (marker set), incomplete (the scan could not cover the whole
// subtree), or clean (neither). incomplete must never be collapsed into clean — a
// scan that gave up at the cap or hit an unreadable subtree has not proven the
// absence of a nested marker.
type nestedScan struct {
	marker     string // a nested marker path, when one was found (found wins over incomplete)
	incomplete bool   // the scan gave up before covering the whole subtree
	detail     string // why it is incomplete: scan cap reached, or an unreadable subtree
}

func (n nestedScan) found() bool { return n.marker != "" }

// errStopNestedScan stops the fsx directory iteration once the scan already has its
// answer for that listing: a nested marker, or a child that would push discovered work
// past the cap. It is local control flow rather than a scan outcome — findNestedMarker
// decides which of the two happened from its own locals, so this sentinel is never
// mistaken for an unreadable subtree and never leaves the function.
var errStopNestedScan = errors.New("doctor: nested-marker scan stopped")

// findNestedMarker searches strictly below root for another .awa marker, bounded by
// maxDirs directories. It scans the whole tree — including dependency directories
// like node_modules — because a nested .awa there still changes root discovery for
// commands run in that subtree, so treating it as a clean absence would mislead. The
// only directories it does not descend into are the protected ones the marker rule
// itself excludes: a found .awa (returned, never entered) and .git. A found marker
// returns immediately; if the cap is reached or a subtree cannot be read before a
// marker is found, the result is marked incomplete so the caller reports that
// honestly rather than as a clean absence.
//
// Two separate bounds keep it from retaining a whole worktree: each directory is
// streamed one fsx batch at a time instead of being listed whole, and maxDirs bounds
// the directories the scan has committed to visiting — those already listed plus those
// still queued — so a sibling set of directories cannot be queued in full before the
// cap can act. Neither bound makes the scan O(batch) overall: the frontier is a second,
// separately bounded collection.
//
// The stack holds the worktree's own absolute paths. The root is opened as the trusted
// anchor the caller named — reachable through a symlink, like any project path a user
// works from — while every directory below it is opened no-follow, so a link swapped in
// during the scan cannot redirect it. Keeping native paths is also what lets the scan
// cover names a store-style relative path could not express, such as a Unix directory
// whose name contains a backslash.
func findNestedMarker(root string, maxDirs int) nestedScan {
	rootMarker := filepath.Join(root, paths.Dir)
	capped := nestedScan{incomplete: true, detail: fmt.Sprintf("scan cap of %d directories reached", maxDirs)}
	// The root is itself the first directory the scan commits to, so a cap below one
	// leaves even it unlisted. That is an incomplete scan, never a clean absence.
	if maxDirs < 1 {
		return capped
	}
	// discovered counts every directory committed to: incremented exactly where one is
	// queued, so it stays equal to the listed count plus len(stack) and bounds both.
	discovered := 1
	res := nestedScan{}
	stack := []string{root}
	for len(stack) > 0 {
		dir := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		var marker string
		atCap := false
		err := eachScanDirEntry(root, dir, func(e os.DirEntry) error {
			if !e.IsDir() || e.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if e.Name() == paths.Dir {
				if child := filepath.Join(dir, e.Name()); child != rootMarker {
					marker = child
					return errStopNestedScan
				}
				return nil // the root's own marker: do not descend into it
			}
			if e.Name() == ".git" {
				return nil // protected: .git is never a nested project location
			}
			if discovered+1 > maxDirs {
				// Stop here rather than queueing the rest of the siblings first: the scan
				// cannot promise to visit this child, which is exactly the cap being spent.
				atCap = true
				return errStopNestedScan
			}
			discovered++
			stack = append(stack, filepath.Join(dir, e.Name()))
			return nil
		})
		switch {
		case marker != "":
			return nestedScan{marker: marker}
		case atCap:
			return capped
		case err != nil:
			// An unreadable subtree (e.g. permission denied) does not abort the scan — a
			// marker may still exist elsewhere — but it does mean absence is unproven.
			if !res.incomplete {
				res.incomplete = true
				res.detail = "an unreadable subtree (" + err.Error() + ")"
			}
		}
	}
	return res
}

// eachScanDirEntry streams one scanned directory's entries to fn in bounded batches,
// holding a single descriptor for the duration of that listing and no longer. The scan's
// own root is opened plainly — it is the path the caller chose to work from, and
// reaching a project through a symlink is ordinary — while every directory below it is
// opened no-follow, so a link swapped in mid-scan cannot redirect the walk.
func eachScanDirEntry(root, dir string, fn func(os.DirEntry) error) error {
	open := fsx.OpenNoFollow
	if dir == root {
		open = os.Open
	}
	f, err := open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return fsx.EachDirEntryAt(f, fn)
}

// relForDisplay renders path relative to root for a compact message, falling back to
// the base name if it is not under root.
func relForDisplay(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return filepath.Base(path)
}

// isRealMarkerDir reports whether path is a real .awa marker directory — a real
// directory, not a symlink — matching how root discovery recognizes a project root
// (projfs rejects a symlinked .awa). Lstat does not follow the final component, so a
// symlink named .awa has IsDir()==false and is correctly not counted as a marker.
// This is the same rule the nested-marker scan applies, so ancestor and nested
// detection agree on what a marker is.
func isRealMarkerDir(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir()
}
