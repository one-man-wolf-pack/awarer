package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	domconfig "awarer/internal/domain/config"
	"awarer/internal/domain/doctor"
	"awarer/internal/domain/paths"
)

// checkConfig raises advisory findings about the run/checkpoint config's
// local-evidence posture. Both are warnings, never blockers: the real privacy
// protection is that awa does not persist raw env values and keeps .awa private by
// default; these only flag configurations that make the durable evidence riskier.
func (s *Service) checkConfig(layout paths.Layout, resolved domconfig.ResolvedConfig, rep *report) {
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

	// content-storage-enabled: storing file contents is the point of checkpoints, but in
	// a project that looks like it holds secrets it means a plaintext secret file can
	// become a blob in .awa/store/blobs. Flag only when both are true, so an ordinary
	// project is never nagged.
	if cfg.Checkpoint.StoreFileContents {
		if secret := secretLookingFile(layout.Root()); secret != "" {
			source := resolved.SourcePathOf("checkpoint.store_file_contents")
			rep.add(finding(doctor.CodeContentStorageEnabled, doctor.SeverityWarning, doctor.SubsystemConfig,
				source, "checkpoint.store_file_contents",
				"checkpoint.store_file_contents is on and the project contains a secret-looking file ("+secret+"); its contents may be stored in .awa/store/blobs"+overrideHint(source), false))
		} else {
			rep.pass()
		}
	} else {
		rep.pass()
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

// secretLookingFile returns the name of the first top-level file in root that looks
// like it holds credentials, or "" if none. It reads only the root directory (one
// listing), never descends, so it stays cheap.
func secretLookingFile(root string) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		lower := strings.ToLower(name)
		switch {
		case lower == ".env" || strings.HasPrefix(lower, ".env."):
			return name
		case lower == ".netrc" || lower == ".pgpass" || lower == ".npmrc":
			return name
		case strings.HasPrefix(lower, "id_rsa") || strings.HasPrefix(lower, "id_dsa") ||
			strings.HasPrefix(lower, "id_ecdsa") || strings.HasPrefix(lower, "id_ed25519"):
			return name
		case lower == "credentials" || lower == "credentials.json" || lower == "secrets.json":
			return name
		}
	}
	return ""
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

// findNestedMarker searches strictly below root for another .awa marker, bounded by
// maxDirs directories. It scans the whole tree — including dependency directories
// like node_modules — because a nested .awa there still changes root discovery for
// commands run in that subtree, so treating it as a clean absence would mislead. The
// only directories it does not descend into are the protected ones the marker rule
// itself excludes: a found .awa (returned, never entered) and .git. A found marker
// returns immediately; if the cap is reached or a subtree cannot be read before a
// marker is found, the result is marked incomplete so the caller reports that
// honestly rather than as a clean absence.
func findNestedMarker(root string, maxDirs int) nestedScan {
	rootMarker := filepath.Join(root, paths.Dir)
	visited := 0
	res := nestedScan{}
	stack := []string{root}
	for len(stack) > 0 {
		dir := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		visited++
		if visited > maxDirs {
			res.incomplete = true
			res.detail = fmt.Sprintf("scan cap of %d directories reached", maxDirs)
			return res
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			// An unreadable subtree (e.g. permission denied) does not abort the scan — a
			// marker may still exist elsewhere — but it does mean absence is unproven.
			if !res.incomplete {
				res.incomplete = true
				res.detail = "an unreadable subtree (" + err.Error() + ")"
			}
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || e.Type()&os.ModeSymlink != 0 {
				continue
			}
			if e.Name() == paths.Dir {
				if child := filepath.Join(dir, e.Name()); child != rootMarker {
					return nestedScan{marker: child}
				}
				continue // the root's own marker: do not descend into it
			}
			if e.Name() == ".git" {
				continue // protected: .git is never a nested project location
			}
			stack = append(stack, filepath.Join(dir, e.Name()))
		}
	}
	return res
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
