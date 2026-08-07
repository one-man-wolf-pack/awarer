package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"awarer/internal/infra/blake3hash"
)

// The live tests below run the real audit against this repository's own module graph,
// module cache, manifest, and committed notice. That is the point: the gate's whole
// consequence is whether it agrees with what Go actually links and what the module
// cache actually holds, and no fixture can stand in for either.

func testRoot(t *testing.T) string {
	t.Helper()
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// liveEvidence resolves the real module cache once per test.
func liveEvidence(t *testing.T) (string, *evidence) {
	t.Helper()
	root := testRoot(t)
	ev, err := newEvidence(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return root, ev
}

func committedManifestPath(root string) string {
	return filepath.Join(root, "third_party", "licenses.json")
}
func committedNoticePath(root string) string { return filepath.Join(root, "THIRD_PARTY_NOTICES") }

// editedManifest writes a copy of the committed manifest with one reviewed fact
// changed, and returns its path. The committed file is never touched.
func editedManifest(t *testing.T, root string, edit func(*rawManifest)) string {
	t.Helper()
	raw, err := loadManifest(committedManifestPath(root))
	if err != nil {
		t.Fatal(err)
	}
	edit(&raw)
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "licenses.json")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// entryIndex returns the position of a module's row, failing if it is absent so a
// renamed dependency turns into a clear test failure rather than a silent no-op edit.
func entryIndex(t *testing.T, raw rawManifest, modulePath string) int {
	t.Helper()
	for i, e := range raw.Entries {
		if e.ModulePath == modulePath {
			return i
		}
	}
	t.Fatalf("the committed manifest has no entry for %s", modulePath)
	return -1
}

// TestCommittedManifestPassesBothScopes is the baseline: the reviewed manifest, the
// pinned evidence, and the committed notice agree with the real graph in both scopes.
func TestCommittedManifestPassesBothScopes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live check under -short")
	}
	root, ev := liveEvidence(t)
	for _, scope := range []string{"fast", "full"} {
		t.Run(scope, func(t *testing.T) {
			if err := check(context.Background(), root, ev, scope, committedManifestPath(root), committedNoticePath(root)); err != nil {
				t.Fatalf("the committed manifest must pass the %s check: %v", scope, err)
			}
		})
	}
}

// TestReconcileScopeBoundary pins the honest boundary between the scopes at the
// function that owns it.
//
// A single host cannot observe the target union, so fast scope must stay silent about
// the two facts that need it — an entry's target set, and an entry no target selects —
// while both scopes must reject a module Go links that no reviewed row covers. The
// expected rules come from the accepted fast/full contract, not from the code: each
// case states a graph and a manifest and names what each scope may conclude.
func TestReconcileScopeBoundary(t *testing.T) {
	sixTargets := append([]string(nil), releaseTargets...)
	row := func(path, version string, targets ...string) entry {
		return entry{module: moduleID{path: path, version: version}, targets: targets}
	}
	saw := func(path, version string, targets ...string) map[string]*observed {
		return map[string]*observed{path: {module: moduleID{path: path, version: version}, targets: targets}}
	}

	cases := []struct {
		name     string
		entries  []entry
		union    map[string]*observed
		wantFast []string
		wantFull []string
	}{
		{
			name:    "agreement",
			entries: []entry{row("example.com/a", "v1.0.0", sixTargets...)},
			union:   saw("example.com/a", "v1.0.0", sixTargets...),
		},
		{
			name:    "module Go links has no reviewed row",
			entries: nil,
			union:   saw("example.com/a", "v1.0.0", sixTargets...),
			// One host is enough to prove it, so neither scope may stay silent.
			wantFast: []string{ruleUnmanifested},
			wantFull: []string{ruleUnmanifested},
		},
		{
			name:     "reviewed version is not the selected one",
			entries:  []entry{row("example.com/a", "v1.0.0", sixTargets...)},
			union:    saw("example.com/a", "v2.0.0", sixTargets...),
			wantFast: []string{ruleVersionDrift},
			wantFull: []string{ruleVersionDrift},
		},
		{
			name:    "reviewed target set is smaller than the union",
			entries: []entry{row("example.com/a", "v1.0.0", "linux/amd64")},
			union:   saw("example.com/a", "v1.0.0", sixTargets...),
			// Fast sees one target and cannot contradict a set claim.
			wantFull: []string{ruleTargetDrift},
		},
		{
			name:     "reviewed row that no target selects",
			entries:  []entry{row("example.com/gone", "v1.0.0", "linux/amd64")},
			union:    map[string]*observed{},
			wantFull: []string{ruleStaleEntry},
		},
		{
			name:    "single-target module is still an obligation",
			entries: []entry{row("example.com/a", "v1.0.0", "windows/amd64")},
			union:   saw("example.com/a", "v1.0.0", "windows/amd64"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := manifest{entries: tc.entries, declared: declaredPaths(tc.entries)}
			if got := rulesOf(reconcile(m, tc.union, false)); !slices.Equal(got, tc.wantFast) {
				t.Errorf("fast scope concluded %v, want %v", got, tc.wantFast)
			}
			if got := rulesOf(reconcile(m, tc.union, true)); !slices.Equal(got, tc.wantFull) {
				t.Errorf("full scope concluded %v, want %v", got, tc.wantFull)
			}
		})
	}
}

// TestReconcileDoesNotCallARejectedRowMissing guards the diagnostic a reviewer acts
// on: a row that is present but was rejected must not also be reported as absent,
// which would send them to add an entry the file already has.
func TestReconcileDoesNotCallARejectedRowMissing(t *testing.T) {
	// entries is empty because the row failed to parse; declared still records it.
	m := manifest{declared: map[string]struct{}{"example.com/a": {}}}
	union := map[string]*observed{"example.com/a": {module: moduleID{path: "example.com/a", version: "v1.0.0"}, targets: []string{"linux/amd64"}}}
	for _, full := range []bool{false, true} {
		if got := rulesOf(reconcile(m, union, full)); len(got) != 0 {
			t.Errorf("full=%v: a declared-but-rejected row was also reported as %v", full, got)
		}
	}
}

// TestUnresolvedModuleIsReportedOncePerModule guards the diagnostic for the gate's
// most common operational failure — a cache that was never hydrated, or a version the
// manifest names and Go did not select. Whether the module resolves is one fact, so a
// module carrying five reviewed texts must produce one line naming it, not five
// identical lines that push the actionable one off the top of the output.
func TestUnresolvedModuleIsReportedOncePerModule(t *testing.T) {
	mod := moduleID{path: "example.com/a", version: "v1.0.0"}
	texts := []text{
		{relPath: "LICENSE", role: "license", mustShip: true},
		{relPath: "LICENSE-GO", role: "license", mustShip: true},
		{relPath: "AUTHORS", role: "authors"},
		{relPath: "NOTICE", role: "notice"},
		{relPath: "PATENTS", role: "patents", mustShip: true},
	}
	m := manifest{entries: []entry{{module: mod, texts: texts}}}
	// An empty index is the real shape of a module that was never downloaded.
	ev := &evidence{root: t.TempDir(), hasher: blake3hash.New(), dirs: map[string]string{}}

	vs := verifyEvidence(m, ev)
	if got := rulesOf(vs); !slices.Equal(got, []string{ruleUnresolvedModule}) {
		t.Fatalf("reported %v, want exactly one %s for %d reviewed texts", got, ruleUnresolvedModule, len(texts))
	}
	if !strings.Contains(vs[0].detail, "go mod download") {
		t.Errorf("the diagnostic does not tell the reader how to fix it: %v", vs[0])
	}
}

func declaredPaths(entries []entry) map[string]struct{} {
	out := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		out[e.module.path] = struct{}{}
	}
	return out
}

func rulesOf(vs []violation) []string {
	var out []string
	for _, v := range vs {
		out = append(out, v.rule)
	}
	sort.Strings(out)
	return out
}

// TestFastScopeMakesNoTargetSetClaim is the same boundary against the real graph, so
// the table above cannot drift from what `go list` actually reports. modernc.org/sqlite
// is selected on all six targets; dropping one from its reviewed row leaves the notice
// bytes untouched, so the full-scope failure can only be the target-set rule.
func TestFastScopeMakesNoTargetSetClaim(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live check under -short")
	}
	root, ev := liveEvidence(t)
	manifest := editedManifest(t, root, func(raw *rawManifest) {
		i := entryIndex(t, *raw, "modernc.org/sqlite")
		var kept []string
		for _, target := range raw.Entries[i].Targets {
			if target != "windows/amd64" {
				kept = append(kept, target)
			}
		}
		if len(kept) == len(raw.Entries[i].Targets) {
			t.Fatal("modernc.org/sqlite no longer claims windows/amd64; the edit proves nothing")
		}
		raw.Entries[i].Targets = kept
	})
	if err := check(context.Background(), root, ev, "fast", manifest, committedNoticePath(root)); err != nil {
		t.Errorf("fast scope must not claim target-set evidence it cannot observe: %v", err)
	}
	if err := check(context.Background(), root, ev, "full", manifest, committedNoticePath(root)); err == nil {
		t.Fatal("full scope must reject a target set that disagrees with the union")
	}
}

// TestObservedModuleMustBeManifested is the failure the gate exists for: something Go
// links into awa whose license nobody reviewed. One host target proves it, so the fast
// scope must reject it.
//
// The notice is regenerated from the edited manifest first, and that is what makes this
// a test of the graph rule. Checked against the committed notice instead, dropping an
// entry would also make that file stale — so the check would still fail with the graph
// rule deleted, and this test would pass while proving nothing.
func TestObservedModuleMustBeManifested(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live check under -short")
	}
	root, ev := liveEvidence(t)
	manifest := editedManifest(t, root, func(raw *rawManifest) {
		i := entryIndex(t, *raw, "modernc.org/sqlite")
		raw.Entries = append(raw.Entries[:i], raw.Entries[i+1:]...)
	})
	notice := filepath.Join(t.TempDir(), "NOTICES")
	if err := update(manifest, notice, ev); err != nil {
		t.Fatal(err)
	}
	if err := check(context.Background(), root, ev, "fast", manifest, notice); err == nil {
		t.Fatal("a production module with no reviewed entry must fail the fast check")
	}
}

// TestEvidenceIsKeyedByExactIdentity proves a manifest naming one version cannot read
// another version's directory. Version drift therefore fails closed at the evidence
// boundary — the digest cannot accidentally match because the wrong bytes were read.
func TestEvidenceIsKeyedByExactIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live check under -short")
	}
	root, ev := liveEvidence(t)

	// The reviewed digest still describes the real v1.55.0 bytes; only the version
	// moved. If the store fell back to the selected version's directory the digest
	// would match and the drift would pass.
	mod := moduleID{path: "modernc.org/sqlite", version: "v1.0.0"}
	if _, _, err := ev.read(mod, "LICENSE"); err == nil {
		t.Error("reading a non-selected version must fail closed, not fall back to the current version")
	} else if !strings.Contains(err.Error(), "not found in the local cache") {
		t.Errorf("the diagnostic does not name the identity miss: %v", err)
	}

	manifest := editedManifest(t, root, func(raw *rawManifest) {
		raw.Entries[entryIndex(t, *raw, "modernc.org/sqlite")].Version = "v1.0.0"
	})
	if err := check(context.Background(), root, ev, "fast", manifest, committedNoticePath(root)); err == nil {
		t.Fatal("a manifest naming a version Go did not select must fail the check")
	}
}

// TestReviewedEvidenceDriftFails covers the other two ways pinned bytes stop matching
// the review: the file changed, or it is gone. Both must fail before any output.
func TestReviewedEvidenceDriftFails(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live check under -short")
	}
	root, ev := liveEvidence(t)

	t.Run("digest changed", func(t *testing.T) {
		manifest := editedManifest(t, root, func(raw *rawManifest) {
			i := entryIndex(t, *raw, "modernc.org/sqlite")
			raw.Entries[i].Texts[0].Digest = "blake3:" + strings.Repeat("a", 64)
		})
		if err := check(context.Background(), root, ev, "fast", manifest, committedNoticePath(root)); err == nil {
			t.Fatal("a reviewed digest that no longer describes the pinned bytes must fail")
		}
	})

	t.Run("text missing", func(t *testing.T) {
		manifest := editedManifest(t, root, func(raw *rawManifest) {
			i := entryIndex(t, *raw, "modernc.org/sqlite")
			raw.Entries[i].Texts[0].Path = "NO-SUCH-LICENSE-FILE"
		})
		if err := check(context.Background(), root, ev, "fast", manifest, committedNoticePath(root)); err == nil {
			t.Fatal("a reviewed license file missing from the pinned module must fail")
		}
	})

	t.Run("project license changed", func(t *testing.T) {
		manifest := editedManifest(t, root, func(raw *rawManifest) {
			raw.ProjectLicense.Digest = "blake3:" + strings.Repeat("b", 64)
		})
		if err := check(context.Background(), root, ev, "fast", manifest, committedNoticePath(root)); err == nil {
			t.Fatal("a project LICENSE that changed since review must fail")
		}
	})

	t.Run("traversal path refused", func(t *testing.T) {
		mod := moduleID{path: "modernc.org/sqlite", version: "v1.55.0"}
		if _, _, err := ev.read(mod, "../escape"); err == nil {
			t.Error("a path escaping the module directory must be refused")
		}
	})
}

// TestNoticeIsDeterministicAndMatchesTheCommittedFile is the artifact's own oracle:
// the notice a user receives in every release archive must be reproducible and must be
// the file that is committed. Two independent generations rule out ordering or map
// iteration leaking in.
func TestNoticeIsDeterministicAndMatchesTheCommittedFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live update under -short")
	}
	root, ev := liveEvidence(t)
	manifest := committedManifestPath(root)
	a := filepath.Join(t.TempDir(), "a")
	b := filepath.Join(t.TempDir(), "b")
	if err := update(manifest, a, ev); err != nil {
		t.Fatal(err)
	}
	if err := update(manifest, b, ev); err != nil {
		t.Fatal(err)
	}
	da, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	db, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(da, db) {
		t.Error("notice generation is not deterministic")
	}
	committed, err := os.ReadFile(committedNoticePath(root))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(da, committed) {
		t.Error("the generated notice does not match the committed THIRD_PARTY_NOTICES")
	}
}

// TestCheckDetectsNoticeTamperWithoutMutating proves the two halves of `check`'s
// contract at once: a notice that is not what the manifest generates is drift, and
// check never writes.
func TestCheckDetectsNoticeTamperWithoutMutating(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live check under -short")
	}
	root, ev := liveEvidence(t)
	committed, err := os.ReadFile(committedNoticePath(root))
	if err != nil {
		t.Fatal(err)
	}
	tampered := filepath.Join(t.TempDir(), "NOTICES")
	before := append(committed, []byte("\nTAMPERED\n")...)
	if err := os.WriteFile(tampered, before, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := check(context.Background(), root, ev, "fast", committedManifestPath(root), tampered); err == nil {
		t.Fatal("a tampered notice must fail the check")
	}
	after, err := os.ReadFile(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("check mutated the notice file; check must be non-mutating")
	}
}

// TestUpdateFailsClosedLeavesFileUnchanged proves the published artifact is replaced
// only after the whole document was built from verified bytes. The store here has an
// empty index — the real shape of a module that was never downloaded — so every
// evidence read fails, which is the same path a changed or missing digest takes.
func TestUpdateFailsClosedLeavesFileUnchanged(t *testing.T) {
	root := testRoot(t)
	out := filepath.Join(t.TempDir(), "NOTICES")
	original := []byte("ORIGINAL NOTICE\n")
	if err := os.WriteFile(out, original, 0o644); err != nil {
		t.Fatal(err)
	}
	unresolved := &evidence{root: filepath.Join(t.TempDir(), "no-such-root"), hasher: blake3hash.New(), dirs: map[string]string{}}
	if err := update(committedManifestPath(root), out, unresolved); err == nil {
		t.Fatal("update must fail when evidence cannot be materialized")
	}
	after, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Error("update overwrote the committed file despite failing")
	}
}

// TestUpdateRefusesToPublishDriftedEvidence is the red sensitivity for the guard that
// stands between drifted evidence and the file users receive.
//
// `just notices-update` writes THIRD_PARTY_NOTICES without consulting the graph, so
// inside that path the digest comparison in renderNotice is the only thing stopping
// unreviewed license bytes from being published. `check` catching the same drift
// elsewhere is not evidence for this path.
func TestUpdateRefusesToPublishDriftedEvidence(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live update under -short")
	}
	root, ev := liveEvidence(t)

	cases := []struct {
		name string
		edit func(*rawManifest)
	}{
		{"reviewed text drifted", func(raw *rawManifest) {
			i := entryIndex(t, *raw, "modernc.org/sqlite")
			raw.Entries[i].Texts[0].Digest = "blake3:" + strings.Repeat("c", 64)
		}},
		{"project license drifted", func(raw *rawManifest) {
			raw.ProjectLicense.Digest = "blake3:" + strings.Repeat("d", 64)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := editedManifest(t, root, tc.edit)
			out := filepath.Join(t.TempDir(), "NOTICES")
			original := []byte("ORIGINAL NOTICE\n")
			if err := os.WriteFile(out, original, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := update(manifest, out, ev); err == nil {
				t.Fatal("update must refuse to publish bytes that no longer match the review")
			}
			after, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, original) {
				t.Error("update replaced the published notice despite failing")
			}
		})
	}
}

// TestUnknownScopeIsRefusedBeforeAnyWork pins the fail-closed reading of -scope.
//
// The scope decides how much of the target matrix the audit enumerates and it reaches
// this tool as a recipe parameter. Treating an unrecognised value as "fast" — which is
// what an unvalidated `scope == "full"` comparison does — would let a typo in the
// release gate silently downgrade the full cross-target enumeration to a single host
// target and still print OK. The refusal must be a loud usage error, not a default.
//
// The paths are absent on purpose: the refusal has to happen before anything is read
// or collected, so an error naming the missing manifest catches the guard moving late.
func TestUnknownScopeIsRefusedBeforeAnyWork(t *testing.T) {
	for _, scope := range []string{"", "Fast", "FULL", "fastest", "all", "host", "-full"} {
		t.Run("scope="+scope, func(t *testing.T) {
			absent := filepath.Join(t.TempDir(), "absent")
			err := run(context.Background(), "check", scope, absent, absent, "")
			if err == nil {
				t.Fatalf("scope %q must be refused, not treated as fast", scope)
			}
			if !strings.Contains(err.Error(), "unknown scope") || !strings.Contains(err.Error(), "want fast or full") {
				t.Errorf("the diagnostic does not name the mistake or the valid values: %v", err)
			}
		})
	}
}

// TestUnknownModeIsRefused keeps the command surface to the two modes that exist:
// anything else is a usage error, never a quiet fallback to check.
func TestUnknownModeIsRefused(t *testing.T) {
	for _, mode := range []string{"", "Check", "verify", "-update", "generate"} {
		t.Run("mode="+mode, func(t *testing.T) {
			absent := filepath.Join(t.TempDir(), "absent")
			err := run(context.Background(), mode, "fast", absent, absent, "")
			if err == nil {
				t.Fatalf("mode %q must be refused", mode)
			}
			if !strings.Contains(err.Error(), "unknown mode") {
				t.Errorf("the diagnostic does not name the mistake: %v", err)
			}
		})
	}
}

// TestAcceptedScopesPassTheGuard is the other half: the guard must not have become so
// strict that a real lane cannot run. Both accepted values get past it and fail later,
// on the absent manifest, rather than at the scope.
func TestAcceptedScopesPassTheGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live run under -short")
	}
	for _, scope := range []string{"fast", "full"} {
		t.Run("scope="+scope, func(t *testing.T) {
			absent := filepath.Join(t.TempDir(), "absent-manifest.json")
			err := run(context.Background(), "check", scope, absent, absent, "")
			if err == nil {
				t.Fatal("an absent manifest must still fail")
			}
			if strings.Contains(err.Error(), "unknown scope") {
				t.Errorf("%q is an accepted scope but was refused as unknown: %v", scope, err)
			}
		})
	}
}
