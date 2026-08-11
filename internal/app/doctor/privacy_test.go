package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"awarer/internal/domain/config"
	dom "awarer/internal/domain/doctor"
	"awarer/internal/domain/paths"
)

// TestConfigFindingNamesSourceLayer proves a config privacy finding points at the
// exact layer that set the value — a shared awa.toml, not the (possibly absent)
// local .awa/config.toml — so a user fixes the right file (OC-9).
func TestConfigFindingNamesSourceLayer(t *testing.T) {
	e := newEnv(t)
	cfg := e.cfg
	cfg.Run.EnvAllowlist = append(cfg.Run.EnvAllowlist, "AWS_TOKEN")
	sharedPath := filepath.Join(e.root, "awa.toml")
	origins := config.DefaultOrigins()
	origins["run.env_allowlist"] = config.LayerShared
	sharedFact, err := config.NewLayerFact(config.LayerShared, sharedPath, true)
	if err != nil {
		t.Fatalf("layer fact: %v", err)
	}
	resolved, err := config.NewResolvedConfig(cfg, origins, []config.LayerFact{sharedFact})
	if err != nil {
		t.Fatalf("resolved config: %v", err)
	}

	res := e.run(t, noGit(), Request{Resolved: resolved})
	fs := findingsByCode(res, dom.CodeEnvAllowlistSuspicious)
	if len(fs) != 1 {
		t.Fatalf("suspicious allowlist findings = %d, want 1", len(fs))
	}
	if fs[0].Path() != sharedPath {
		t.Errorf("finding path = %q, want the shared awa.toml %q that set the value", fs[0].Path(), sharedPath)
	}
}

// TestConfigDefaultFindingHasNoSourcePath proves a finding about a product-default
// value (store_file_contents defaults to on) leaves the path empty — it does not
// name an absent .awa/config.toml — and moves the override guidance into the message
// (OC-13).
func TestConfigDefaultFindingHasNoSourcePath(t *testing.T) {
	e := newEnv(t)
	if err := os.WriteFile(filepath.Join(e.root, ".env"), []byte("TOKEN=abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// No config layers: store_file_contents is the product default.
	res := e.run(t, noGit(), Request{})
	fs := findingsByCode(res, dom.CodeContentStorageEnabled)
	if len(fs) != 1 {
		t.Fatalf("content-storage findings = %d, want 1", len(fs))
	}
	if fs[0].Path() != "" {
		t.Errorf("default-value finding path = %q, want empty (no file holds the setting)", fs[0].Path())
	}
	if !strings.Contains(fs[0].Message(), "product default") {
		t.Errorf("default-value finding should explain the override in its message, got %q", fs[0].Message())
	}
}

func TestEnvAllowlistSuspiciousFinding(t *testing.T) {
	e := newEnv(t)
	// The shipped default (CI/NODE_ENV) is not suspicious.
	if res := e.run(t, noGit(), Request{}); len(findingsByCode(res, dom.CodeEnvAllowlistSuspicious)) != 0 {
		t.Fatalf("default allowlist flagged as suspicious: %+v", res.Findings())
	}
	// A secret-looking name is a warning, but never blocks (health is not failed).
	e.cfg.Run.EnvAllowlist = append(e.cfg.Run.EnvAllowlist, "AWS_ACCESS_KEY_ID", "GITHUB_TOKEN")
	res := e.run(t, noGit(), Request{})
	fs := findingsByCode(res, dom.CodeEnvAllowlistSuspicious)
	if len(fs) != 1 {
		t.Fatalf("suspicious allowlist findings = %d, want 1", len(fs))
	}
	if fs[0].Severity() != dom.SeverityWarning {
		t.Errorf("severity = %v, want warning", fs[0].Severity())
	}
	if res.Health() == dom.HealthFailed {
		t.Error("a suspicious allowlist name must not fail the project")
	}
}

func TestContentStorageEnabledFinding(t *testing.T) {
	e := newEnv(t)
	// store_file_contents is on by default, but with no secret-looking file present the
	// posture is not flagged.
	if res := e.run(t, noGit(), Request{}); len(findingsByCode(res, dom.CodeContentStorageEnabled)) != 0 {
		t.Fatalf("content storage flagged without a secret file: %+v", res.Findings())
	}
	// A .env in the project root makes content storage a real leak risk.
	if err := os.WriteFile(filepath.Join(e.root, ".env"), []byte("TOKEN=abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := e.run(t, noGit(), Request{})
	if len(findingsByCode(res, dom.CodeContentStorageEnabled)) != 1 {
		t.Fatalf("content-storage-enabled findings = %d, want 1", len(findingsByCode(res, dom.CodeContentStorageEnabled)))
	}
}

func TestPermissionsTooBroadFinding(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission semantics")
	}
	e := newEnv(t)
	layout, _ := e.project.Paths()
	// A fresh init is owner-private, so nothing is flagged.
	if res := e.run(t, noGit(), Request{}); len(findingsByCode(res, dom.CodeStatePermissionsTooBroad)) != 0 {
		t.Fatalf("fresh project flagged for broad permissions: %+v", res.Findings())
	}
	// Widen a required dir the way a group-readable store would have been created.
	if err := os.Chmod(layout.TmpDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	res := e.run(t, noGit(), Request{})
	fs := findingsByCode(res, dom.CodeStatePermissionsTooBroad)
	if len(fs) != 1 {
		t.Fatalf("broad-permission findings = %d, want 1", len(fs))
	}
}

func TestNestedAndAncestorMarkerFindings(t *testing.T) {
	e := newEnv(t)
	// A marker nested below the root and one above it are both ambiguous-root hazards.
	nested := filepath.Join(e.root, "sub", paths.Dir)
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	ancestor := filepath.Join(filepath.Dir(e.root), paths.Dir)
	if err := os.MkdirAll(ancestor, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(ancestor) })

	res := e.run(t, noGit(), Request{})
	if len(findingsByCode(res, dom.CodeNestedProjectMarker)) != 1 {
		t.Errorf("nested-project-marker findings = %d, want 1", len(findingsByCode(res, dom.CodeNestedProjectMarker)))
	}
	if len(findingsByCode(res, dom.CodeAncestorProjectMarker)) != 1 {
		t.Errorf("ancestor-project-marker findings = %d, want 1", len(findingsByCode(res, dom.CodeAncestorProjectMarker)))
	}
}

// TestSymlinkedAncestorMarkerIgnored locks the rule that marker detection agrees
// with root discovery: a symlink named .awa above the root is not a real marker (awa
// never treats it as a root), so it must not be flagged as an ancestor project.
func TestSymlinkedAncestorMarkerIgnored(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	e := newEnv(t)
	target := t.TempDir()
	link := filepath.Join(filepath.Dir(e.root), paths.Dir)
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(link) })

	res := e.run(t, noGit(), Request{})
	if n := len(findingsByCode(res, dom.CodeAncestorProjectMarker)); n != 0 {
		t.Errorf("ancestor-project-marker findings = %d, want 0 (a symlinked .awa is not a real root)", n)
	}
}

// TestFindNestedMarkerHonesty proves the bounded nested-marker scan distinguishes a
// clean absence from an incomplete scan (cap reached or an unreadable subtree), so
// doctor never reports evidence it could not fully see as healthy.
func TestFindNestedMarkerHonesty(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b", "c"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Clean: a fully scanned tree with no nested marker is neither found nor incomplete.
	if scan := findNestedMarker(root, nestedScanCap); scan.found() || scan.incomplete {
		t.Errorf("clean tree = %+v, want not found and complete", scan)
	}

	// Found: a nested marker is returned and wins over completeness.
	nested := filepath.Join(root, "a", paths.Dir)
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if scan := findNestedMarker(root, nestedScanCap); scan.marker != nested {
		t.Errorf("marker = %q, want %q", scan.marker, nested)
	}
	_ = os.RemoveAll(nested)

	// Cap exhaustion: a scan that gives up at the cap is incomplete, not clean.
	if scan := findNestedMarker(root, 1); !scan.incomplete || scan.found() {
		t.Errorf("cap-exhausted scan = %+v, want incomplete", scan)
	}

	// A marker inside a dependency directory is still found: a .awa under node_modules
	// changes root discovery for commands run there, so it must not read as a clean
	// absence just because the tree is a dependency directory.
	nestedInDep := filepath.Join(root, "node_modules", "pkg", paths.Dir)
	if err := os.MkdirAll(nestedInDep, 0o700); err != nil {
		t.Fatal(err)
	}
	if scan := findNestedMarker(root, nestedScanCap); scan.marker != nestedInDep {
		t.Errorf("marker under node_modules = %+v, want it found at %s", scan, nestedInDep)
	}
	_ = os.RemoveAll(filepath.Join(root, "node_modules"))

	// Unreadable subtree: a directory that cannot be read makes the scan incomplete.
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		blocked := filepath.Join(root, "a", "b")
		if err := os.Chmod(blocked, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
		scan := findNestedMarker(root, nestedScanCap)
		if !scan.incomplete {
			t.Errorf("scan over an unreadable subtree = %+v, want incomplete", scan)
		}
		// The detail must locate the subtree, not just report a refusal: a bare component
		// name repeats all over a real worktree, so the user could not act on it.
		if !strings.Contains(scan.detail, "a/b") {
			t.Errorf("incomplete detail = %q, want it to name the unreadable subtree a/b", scan.detail)
		}
	}
}

// TestFindNestedMarkerAcrossEntryBatches proves the scan keeps reading a directory past
// the first streamed entry batch: a sibling set far wider than one batch is still
// traversed to the nested marker below it, so a wide directory cannot silently truncate
// the search into a false clean absence.
func TestFindNestedMarkerAcrossEntryBatches(t *testing.T) {
	root := t.TempDir()
	wide := filepath.Join(root, "wide")
	deep := filepath.Join(wide, "deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	// Comfortably more than one entry batch of plain files around the one subdirectory
	// that leads to the marker.
	for i := 0; i < 2100; i++ {
		if err := os.WriteFile(filepath.Join(wide, fmt.Sprintf("f%05d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(deep, paths.Dir)
	if err := os.MkdirAll(marker, 0o700); err != nil {
		t.Fatal(err)
	}

	if scan := findNestedMarker(root, nestedScanCap); scan.marker != marker {
		t.Errorf("scan over a file-heavy directory = %+v, want the marker at %s", scan, marker)
	}
}

// TestFindNestedMarkerBoundsDiscoveredDirectories proves the directory cap bounds the
// work the scan has discovered — visited plus queued — rather than only the directories
// already listed, so a directory-heavy sibling set cannot be queued in full before the
// cap can act.
func TestFindNestedMarkerBoundsDiscoveredDirectories(t *testing.T) {
	// Directory-heavy: 1,500 sibling directories under a cap of 1,200. The cap is chosen
	// above one entry batch on purpose — queueing has to continue past the batch boundary
	// before the budget is spent — so the streamed listing and the frontier bound are both
	// exercised, and the outcome is incomplete rather than a clean absence.
	root := t.TempDir()
	wide := filepath.Join(root, "wide")
	for i := 0; i < 1500; i++ {
		if err := os.MkdirAll(filepath.Join(wide, fmt.Sprintf("d%05d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if scan := findNestedMarker(root, 1200); !scan.incomplete || scan.found() {
		t.Errorf("scan over a directory-heavy tree = %+v, want incomplete", scan)
	}

	// Order-independent frontier witness: every candidate child holds its own marker, and
	// the budget is spent before any of them can be visited. Counting only the directories
	// already listed would queue all three, visit one, and report that marker as found.
	small := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		if err := os.MkdirAll(filepath.Join(small, name, paths.Dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if scan := findNestedMarker(small, 2); !scan.incomplete || scan.found() {
		t.Errorf("scan whose budget is spent before any child is visited = %+v, want incomplete", scan)
	}
}

// TestFindNestedMarkerScansRootWithUnreadableParent proves the scan reads the project
// root as the trusted directory it was given: a root a user can read inside a directory
// they may only traverse stays fully scannable, so listing never depends on permission
// the project itself does not own.
func TestFindNestedMarkerScansRootWithUnreadableParent(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs Unix directory-permission semantics as a non-root user")
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	marker := filepath.Join(root, "nested", paths.Dir)
	if err := os.MkdirAll(marker, 0o700); err != nil {
		t.Fatal(err)
	}
	// Traversable but not readable: the root can still be reached and read, while
	// anything that tried to list it as an entry of this directory could not.
	if err := os.Chmod(parent, 0o111); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chmod(parent, 0o755); err != nil {
			t.Fatalf("restore parent permissions: %v", err)
		}
	}()

	if scan := findNestedMarker(root, nestedScanCap); scan.marker != marker {
		t.Errorf("scan under a search-only parent = %+v, want the marker at %s", scan, marker)
	}
}

// TestFindNestedMarkerScansSymlinkReachedRoot proves a project root the user reached
// through a symlink is scanned like any other: the same clean verdict as the direct
// path, a nested marker still discovered, and the marker reported under the root path
// the caller chose rather than a resolved physical one.
func TestFindNestedMarkerScansSymlinkReachedRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	realRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(realRoot, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "project")
	if err := os.Symlink(realRoot, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if direct, viaLink := findNestedMarker(realRoot, nestedScanCap), findNestedMarker(link, nestedScanCap); direct != viaLink {
		t.Errorf("scan through the symlink = %+v, want the direct scan's %+v", viaLink, direct)
	}

	if err := os.MkdirAll(filepath.Join(realRoot, "nested", paths.Dir), 0o700); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(link, "nested", paths.Dir)
	if scan := findNestedMarker(link, nestedScanCap); scan.marker != want {
		t.Errorf("marker through the symlink = %+v, want %s under the caller's own root path", scan, want)
	}
}

// TestFindNestedMarkerScansNativeWorktreeNames proves the scan covers directory names
// the platform allows even when a store-style relative path could not express them: on
// Unix a backslash is an ordinary character in a name, and a marker below such a
// directory must be found rather than turned into an unscannable subtree.
func TestFindNestedMarkerScansNativeWorktreeNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip(`Windows file names cannot contain a backslash`)
	}
	root := t.TempDir()
	marker := filepath.Join(root, `we\ird`, "inner", paths.Dir)
	if err := os.MkdirAll(marker, 0o700); err != nil {
		t.Fatal(err)
	}

	if scan := findNestedMarker(root, nestedScanCap); scan.marker != marker {
		t.Errorf("scan over a backslash-bearing directory name = %+v, want the marker at %s", scan, marker)
	}
}

// TestNestedMarkerScanIncompleteFinding proves checkMarkers reports an incomplete
// nested scan as a stable warning finding rather than a clean pass.
func TestNestedMarkerScanIncompleteFinding(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs Unix directory-permission semantics as a non-root user")
	}
	e := newEnv(t)
	blocked := filepath.Join(e.root, "sub")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })

	res := e.run(t, noGit(), Request{})
	if len(findingsByCode(res, dom.CodeNestedMarkerScanIncomplete)) != 1 {
		t.Errorf("nested-marker-scan-incomplete findings = %d, want 1", len(findingsByCode(res, dom.CodeNestedMarkerScanIncomplete)))
	}
}

// TestEnvAllowlistInjectsCodeFinding covers the audit's never-default dispositions where
// a user can still opt in: a loader or startup variable in run.env_allowlist means the
// supervised command's behavior is decided partly outside the command. It is advisory —
// awa does not refuse the configuration — and it is deliberately a separate code from
// env-allowlist-suspicious, whose message makes a claim about secrets that would be wrong
// here.
func TestEnvAllowlistInjectsCodeFinding(t *testing.T) {
	e := newEnv(t)
	// The shipped default carries none of them.
	if res := e.run(t, noGit(), Request{}); len(findingsByCode(res, dom.CodeEnvAllowlistInjectsCode)) != 0 {
		t.Fatalf("default allowlist flagged as code-injecting: %+v", res.Findings())
	}

	e.cfg.Run.EnvAllowlist = append(e.cfg.Run.EnvAllowlist, "LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "NODE_OPTIONS")
	res := e.run(t, noGit(), Request{})
	fs := findingsByCode(res, dom.CodeEnvAllowlistInjectsCode)
	if len(fs) != 1 {
		t.Fatalf("code-injection findings = %d, want 1 (bounded, one per config)", len(fs))
	}
	if fs[0].Severity() != dom.SeverityWarning {
		t.Errorf("severity = %v, want warning", fs[0].Severity())
	}
	if res.Health() == dom.HealthFailed {
		t.Error("an advisory allowlist finding must not fail the project")
	}
	msg := fs[0].Message()
	// Deterministic, sorted, and naming every offender: an advisory a user cannot act on
	// is noise.
	if !strings.Contains(msg, "DYLD_INSERT_LIBRARIES, LD_PRELOAD, NODE_OPTIONS") {
		t.Errorf("message = %q, want the offending names in sorted order", msg)
	}
	if !strings.Contains(msg, "load or execute code") {
		t.Errorf("message = %q, want it to say what the risk is", msg)
	}
	// The two privacy advisories must stay distinct: these names are not secret-looking.
	if got := findingsByCode(res, dom.CodeEnvAllowlistSuspicious); len(got) != 0 {
		t.Errorf("code-injecting names also raised the secret-looking finding: %+v", got)
	}
}

// TestEnvAllowlistInjectsCodeIsBounded proves the check does not guess. An advisory that
// fires on ordinary configuration teaches users to ignore it, so only the audited set
// counts — a name that merely looks related does not.
func TestEnvAllowlistInjectsCodeIsBounded(t *testing.T) {
	e := newEnv(t)
	// ENV is in the last group deliberately: POSIX sh reads a startup file from it, but
	// it is far more often an ordinary application setting (ENV=production), so flagging
	// it would make the advisory fire on innocent configuration.
	e.cfg.Run.EnvAllowlist = append(e.cfg.Run.EnvAllowlist, "MY_PRELOAD_MODE", "NODE_ENV_OPTIONS", "PATH_EXTRA", "ENV")
	res := e.run(t, noGit(), Request{})
	if fs := findingsByCode(res, dom.CodeEnvAllowlistInjectsCode); len(fs) != 0 {
		t.Errorf("unrelated or ambiguous names raised the code-injection finding: %q", fs[0].Message())
	}
}
