package cli

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"awarer/internal/domain/paths"
)

// statusDegradationDoc mirrors the wire shape of one status degradation for tests.
type statusDegradationDoc struct {
	Component string `json:"component"`
	Token     string `json:"token"`
	Detail    string `json:"detail"`
}

// statusJSONDoc is the subset of status --json the honesty tests assert on.
type statusJSONDoc struct {
	Checkpoints struct {
		State     string `json:"state"`
		Recorded  int    `json:"recorded"`
		Populated bool   `json:"populated"`
	} `json:"checkpoints"`
	RunCache struct {
		State     string `json:"state"`
		Populated bool   `json:"populated"`
	} `json:"run_cache"`
	Store struct {
		Path      string `json:"path"`
		Footprint struct {
			Available bool   `json:"available"`
			Reason    string `json:"reason"`
		} `json:"footprint"`
	} `json:"store"`
	Degradations []statusDegradationDoc `json:"degradations"`
	Review       struct {
		Next []struct {
			Kind string   `json:"kind"`
			Argv []string `json:"argv"`
		} `json:"next"`
	} `json:"review"`
}

func statusJSON(t *testing.T, root string) statusJSONDoc {
	t.Helper()
	code, stdout, stderr := run("status", "--root", root, "--json")
	if code != int(ExitSuccess) {
		t.Fatalf("status --json exit = %d, stderr = %q", code, stderr)
	}
	var doc statusJSONDoc
	decodeEnvelope(t, stdout, &doc)
	// The state/degradation invariant is checked on every status JSON these tests read,
	// so a future change that lets a subsystem's state and its degradation facts drift
	// apart is caught across all scenarios, not just where it is asserted explicitly.
	assertStateDegradationInvariant(t, doc)
	return doc
}

// assertStateDegradationInvariant enforces the structural contract that a subsystem's
// authoritative state token and the degradations array never contradict each other: a
// degraded checkpoints/runs state must be backed by a degradation for that component,
// and a healthy/empty state must not carry one. Other components (names/blobs/index/…)
// have no subsystem state and are not constrained here.
func assertStateDegradationInvariant(t *testing.T, doc statusJSONDoc) {
	t.Helper()
	check := func(component, state string) {
		degraded := state != "healthy" && state != "store-empty"
		has := false
		for _, d := range doc.Degradations {
			if d.Component == component {
				has = true
			}
		}
		switch {
		case degraded && !has:
			t.Errorf("%s state %q is degraded but no %s degradation exists: %+v", component, state, component, doc.Degradations)
		case !degraded && has:
			t.Errorf("%s state %q is not degraded but a %s degradation exists: %+v", component, state, component, doc.Degradations)
		}
	}
	check("checkpoints", doc.Checkpoints.State)
	check("runs", doc.RunCache.State)
}

// A freshly initialized project with no checkpoints or runs reports both subsystems as
// the honest store-empty, with no degradations.
func TestHealthyEmptyIsStoreEmpty(t *testing.T) {
	root := initProject(t)

	doc := statusJSON(t, root)

	if doc.Checkpoints.State != "store-empty" {
		t.Errorf("checkpoints.state = %q, want store-empty", doc.Checkpoints.State)
	}
	if doc.RunCache.State != "store-empty" {
		t.Errorf("run_cache.state = %q, want store-empty", doc.RunCache.State)
	}
	if len(doc.Degradations) != 0 {
		t.Errorf("healthy empty project has degradations: %+v", doc.Degradations)
	}
}

// A store whose only checkpoint is incompatible must never read as healthy-empty via any
// single field. State is not store-empty, and a structured degradation carries the token.
func TestCorruptEmptyIsNotHealthyEmpty(t *testing.T) {
	root := checkpointProject(t)
	makeIncompatible(t, checkpointHeaderPaths(t, root)[0])

	doc := statusJSON(t, root)

	if doc.Checkpoints.State == "store-empty" || doc.Checkpoints.State == "healthy" {
		t.Errorf("incompatible-only store state = %q, want a degraded token", doc.Checkpoints.State)
	}
	// No single field may read as healthy-empty: state is degraded AND a degradation
	// exists for the checkpoints component.
	found := false
	for _, d := range doc.Degradations {
		if d.Component == "checkpoints" && d.Token == "metadata-incompatible" {
			found = true
		}
	}
	if !found {
		t.Errorf("degradations missing {checkpoints, metadata-incompatible}: %+v", doc.Degradations)
	}
}

// A mixed store's human warning and the JSON degradation come from the same fact — the
// human stderr detail is exactly the degradation's detail, and the JSON carries the
// component and stable token.
func TestHumanMirrorsJSONDegradation(t *testing.T) {
	root := checkpointProject(t)
	secondCheckpoint(t, root)
	makeIncompatible(t, checkpointHeaderPaths(t, root)[0]) // one healthy, one incompatible → partial

	doc := statusJSON(t, root)
	var deg *statusDegradationDoc
	for i := range doc.Degradations {
		if doc.Degradations[i].Component == "checkpoints" {
			deg = &doc.Degradations[i]
		}
	}
	if deg == nil {
		t.Fatalf("no checkpoints degradation in JSON: %+v", doc.Degradations)
	}
	if deg.Token != "read-partial" {
		t.Errorf("checkpoints degradation token = %q, want read-partial", deg.Token)
	}

	// The human command must print the same detail as a warning line.
	_, _, stderr := run("status", "--root", root)
	if !strings.Contains(stderr, deg.Detail) {
		t.Errorf("human stderr %q does not carry the JSON degradation detail %q", stderr, deg.Detail)
	}
}

// An unreadable checkpoint store surfaces as permission-denied, never as an empty store.
// It is skipped where permissions cannot be simulated (running as root bypasses the mode).
func TestPermissionDeniedIsNotNoneYet(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("cannot simulate permission denial as root")
	}
	root := checkpointProject(t)
	dir := paths.New(root).CheckpointsDir()
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	// Confirm the platform actually denies the read (some CI mounts ignore mode bits).
	if _, err := os.ReadDir(dir); err == nil {
		t.Skip("platform does not enforce directory permissions")
	}

	doc := statusJSON(t, root)
	if doc.Checkpoints.State != "permission-denied" {
		t.Errorf("checkpoints.state = %q, want permission-denied", doc.Checkpoints.State)
	}
	if doc.Checkpoints.State == "store-empty" {
		t.Error("unreadable store reported as empty")
	}
	found := false
	for _, d := range doc.Degradations {
		if d.Component == "checkpoints" && d.Token == "permission-denied" {
			found = true
		}
	}
	if !found {
		t.Errorf("degradations missing {checkpoints, permission-denied}: %+v", doc.Degradations)
	}
}

// Ordinary status omits the exact blob footprint (a whole-store walk) and points at
// the explicit diagnostic instead. Even with blobs on disk, no numeric blob total
// appears; the footprint is unavailable with a bounded reason, and a typed next
// names gc --dry-run --json.
func TestStatusDoesNotWalkBlobStore(t *testing.T) {
	root := checkpointProject(t) // creates at least one blob

	doc := statusJSON(t, root)
	if doc.Store.Footprint.Available {
		t.Error("status published an available blob footprint, want it bounded out")
	}
	if doc.Store.Footprint.Reason != "bounded" {
		t.Errorf("footprint reason = %q, want bounded", doc.Store.Footprint.Reason)
	}
	hasGC := false
	for _, n := range doc.Review.Next {
		if n.Kind == "gc-dry-run" {
			hasGC = true
		}
	}
	if !hasGC {
		t.Errorf("status next missing a gc-dry-run diagnostic: %+v", doc.Review.Next)
	}

	// The human dashboard must not print a blob count/size either.
	_, stdout, _ := run("status", "--root", root)
	if strings.Contains(stdout, "blobs:") {
		t.Errorf("human status still prints a blob total: %q", stdout)
	}
}

// A missing or unreadable run store must never be presented as an honestly-empty cache
// ("none yet"). Both the human dashboard and JSON must report the degraded state,
// matching the checkpoint behavior.
func TestRunStoreMissingIsNotNoneYet(t *testing.T) {
	root := checkpointProject(t)
	// Remove the run store directory so it is absent, not present-but-empty.
	if err := os.RemoveAll(paths.New(root).RunsDir()); err != nil {
		t.Fatalf("remove run store: %v", err)
	}

	doc := statusJSON(t, root)
	if doc.RunCache.State != "store-missing" {
		t.Errorf("run_cache.state = %q, want store-missing", doc.RunCache.State)
	}
	if doc.RunCache.State == "store-empty" {
		t.Error("missing run store reported as empty")
	}
	found := false
	for _, d := range doc.Degradations {
		if d.Component == "runs" && d.Token == "store-missing" {
			found = true
		}
	}
	if !found {
		t.Errorf("degradations missing {runs, store-missing}: %+v", doc.Degradations)
	}

	// The human dashboard must not say "none yet" for a degraded run store.
	_, stdout, _ := run("status", "--root", root)
	if strings.Contains(stdout, "run cache:   none yet") {
		t.Errorf("degraded run store printed 'none yet': %q", stdout)
	}
	if !strings.Contains(stdout, "run cache:   0 readable") {
		t.Errorf("degraded run store missing '0 readable' line: %q", stdout)
	}
}

// status --json is one document with structured, well-formed facts — degradations is
// always an array, and every next entry is a typed {kind, argv} rather than a bare
// string.
func TestStatusJSONCleanliness(t *testing.T) {
	root := checkpointProject(t)

	code, stdout, _ := run("status", "--root", root, "--json")
	if code != int(ExitSuccess) {
		t.Fatalf("status --json exit = %d", code)
	}
	// Exactly one JSON document on stdout: the first decode succeeds and a second decode
	// must hit EOF. Decoder.More only reports element availability inside an array/object,
	// so it would miss a stray second top-level document; a second Decode catches it.
	dec := json.NewDecoder(strings.NewReader(stdout))
	var first json.RawMessage
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("stdout is not valid JSON: %v", err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		t.Errorf("stdout carries more than one JSON document (second decode err = %v, want EOF)", err)
	}

	doc := statusJSON(t, root)
	if doc.Degradations == nil {
		t.Error("degradations is null, want an array")
	}
	for _, n := range doc.Review.Next {
		if n.Kind == "" || len(n.Argv) == 0 {
			t.Errorf("next entry is not a typed {kind, argv}: %+v", n)
		}
	}
}

// A store with one valid and one incompatible checkpoint still surfaces the readable
// evidence (recorded >= 1) and is marked partial with a structured degradation.
func TestMixedStoreYieldsPartial(t *testing.T) {
	root := checkpointProject(t)
	secondCheckpoint(t, root)
	makeIncompatible(t, checkpointHeaderPaths(t, root)[0])

	doc := statusJSON(t, root)
	if doc.Checkpoints.State != "read-partial" {
		t.Errorf("checkpoints.state = %q, want read-partial", doc.Checkpoints.State)
	}
	if doc.Checkpoints.Recorded < 1 {
		t.Errorf("recorded = %d, want the healthy checkpoint still counted", doc.Checkpoints.Recorded)
	}
	if !doc.Checkpoints.Populated {
		t.Error("populated = false, want true — a readable checkpoint remains")
	}
}
