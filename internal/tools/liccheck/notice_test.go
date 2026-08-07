package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"awarer/internal/domain/hashing"
	"awarer/internal/infra/blake3hash"
)

// The committed manifest's fourteen entries all take the same shape: unreplaced, no
// copyright line, every component shipping at least one text. Byte-identity against
// the committed THIRD_PARTY_NOTICES therefore proves nothing about the three notice
// behaviors no current entry reaches, even though each is a reviewed fact that would
// appear verbatim in every release archive the day a dependency first needs one.
//
// These tests supply that shape directly. The store is the real one pointed at a
// directory holding the reviewed bytes — the only property of a module cache the
// notice path uses — so nothing here re-implements reading, hashing, or rendering.

// stubbedModule writes files into a temp directory and returns a store that resolves
// mod to it, plus the digest of each file.
func stubbedModule(t *testing.T, mod moduleID, files map[string]string) (*evidence, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	hasher := blake3hash.New()
	digests := make(map[string]string, len(files))
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		digests[name] = hasher.HashBytes([]byte(body)).String()
	}
	// The project LICENSE is read from the store's root, so the same directory serves
	// as both module and repository root here.
	if err := os.WriteFile(filepath.Join(dir, "PROJECT-LICENSE"), []byte("Apache text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	digests["PROJECT-LICENSE"] = hasher.HashBytes([]byte("Apache text\n")).String()

	return &evidence{root: dir, hasher: hasher, dirs: map[string]string{mod.String(): dir}}, digests
}

// parseDigest turns a recorded digest string into the value a reviewed text carries.
func parseDigest(t *testing.T, s string) hashing.TreeHash {
	t.Helper()
	d, err := hashing.ParseTreeHash(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestNoticeRendersReplacementAndCopyright covers the two component-header lines the
// current graph never produces. Both are reviewed facts: a replacement changes which
// module's bytes the attribution belongs to, and a required copyright is an
// attribution obligation in its own right.
func TestNoticeRendersReplacementAndCopyright(t *testing.T) {
	mod, err := newModuleID("example.com/a", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	mod, err = mod.withReplacement("example.com/fork", "v2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	ev, digests := stubbedModule(t, mod, map[string]string{"LICENSE": "MIT text\n"})

	m := manifest{
		projectLicense: projectLicense{spdx: "Apache-2.0", path: "PROJECT-LICENSE", digest: parseDigest(t, digests["PROJECT-LICENSE"])},
		entries: []entry{{
			module:    mod,
			spdx:      "MIT",
			copyright: "Copyright (c) 2026 Someone",
			texts:     []text{{relPath: "LICENSE", role: "license", digest: parseDigest(t, digests["LICENSE"]), mustShip: true}},
		}},
	}

	got, err := renderNotice(m, ev)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Component: example.com/a\n",
		"Version:   v1.0.0\n",
		"Replaced:  example.com/fork@v2.3.4\n",
		"License:   MIT\n",
		"Copyright: Copyright (c) 2026 Someone\n",
		"LICENSE (LICENSE):\n\nMIT text\n",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("the notice does not carry %q:\n%s", want, got)
		}
	}
}

// TestNoticeRefusesToPublishAMissingText is the other half of the publish guard: a
// reviewed text can drift by changing or by disappearing, and both must stop the
// document before any of it is written. The module resolves here; only the file the
// review names is gone.
func TestNoticeRefusesToPublishAMissingText(t *testing.T) {
	mod, err := newModuleID("example.com/a", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	ev, digests := stubbedModule(t, mod, map[string]string{"LICENSE": "MIT text\n"})

	m := manifest{
		projectLicense: projectLicense{spdx: "Apache-2.0", path: "PROJECT-LICENSE", digest: parseDigest(t, digests["PROJECT-LICENSE"])},
		entries: []entry{{
			module: mod,
			spdx:   "MIT",
			texts: []text{{
				relPath: "LICENSE-THAT-IS-GONE", role: "license",
				digest: parseDigest(t, digests["LICENSE"]), mustShip: true,
			}},
		}},
	}

	got, err := renderNotice(m, ev)
	if err == nil {
		t.Fatal("a reviewed text that is no longer in the module must stop the notice")
	}
	if got != nil {
		t.Error("renderNotice returned partial output alongside its error")
	}
	// The sentinel, not the wording: a digest comparison against zero bytes would also
	// refuse, so matching on prose would let this pass with the read check deleted and
	// report a vanished file as a changed one.
	if !errors.Is(err, errTextMissing) {
		t.Errorf("a vanished text must be reported as missing, not as some other drift: %v", err)
	}
	if !strings.Contains(err.Error(), "LICENSE-THAT-IS-GONE") {
		t.Errorf("the diagnostic does not name the missing text: %v", err)
	}
}

// TestNoticeOmitsComponentsThatShipNothing covers the inclusion rule. An entry whose
// texts all carry must_ship=false imposes no redistribution obligation, so it must
// contribute no component block at all — not an empty one naming a component whose
// license the file then fails to reproduce.
func TestNoticeOmitsComponentsThatShipNothing(t *testing.T) {
	shipping, err := newModuleID("example.com/shipping", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	silent, err := newModuleID("example.com/silent", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	ev, digests := stubbedModule(t, shipping, map[string]string{"LICENSE": "MIT text\n"})

	m := manifest{
		projectLicense: projectLicense{spdx: "Apache-2.0", path: "PROJECT-LICENSE", digest: parseDigest(t, digests["PROJECT-LICENSE"])},
		entries: []entry{
			{
				module: shipping,
				spdx:   "MIT",
				texts:  []text{{relPath: "LICENSE", role: "license", digest: parseDigest(t, digests["LICENSE"]), mustShip: true}},
			},
			{
				// Never resolved by the store: proving it is skipped before any read is
				// exactly the point — a component that ships nothing is not consulted.
				module: silent,
				spdx:   "CC0-1.0",
				texts:  []text{{relPath: "AUTHORS", role: "authors", digest: parseDigest(t, digests["LICENSE"])}},
			},
		},
	}

	got, err := renderNotice(m, ev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "Component: example.com/shipping\n") {
		t.Errorf("the shipping component is missing:\n%s", got)
	}
	if strings.Contains(string(got), "example.com/silent") {
		t.Errorf("a component with no must-ship text reached the notice:\n%s", got)
	}
}
