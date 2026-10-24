package runstore_test

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"awarer/internal/domain/runcache"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestValidatedRunPersistsItsPolicy(t *testing.T) {
	r, _, h := newStore(t)
	v, err := runcache.NewValidationScope("cargo-test", []string{"target", "build/gen"})
	if err != nil {
		t.Fatal(err)
	}
	ki := keyInputFor(h, "validated")
	ki.Validation = &v
	entry := storeRunKeyed(t, r, h, ki, "out", "", 1<<20, unchangedObs())

	got, err := r.Get(entry.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	gv := got.KeyInput.Validation
	if gv == nil || gv.Label != "cargo-test" || !slices.Equal(gv.UnobservedDirs, []string{"build/gen", "target"}) {
		t.Fatalf("recorded validation = %+v", gv)
	}
	ce, ok, err := r.Lookup(entry.Key)
	if err != nil || !ok || ce.Entry().ID != entry.ID {
		t.Fatalf("Lookup = (%v, %v, %v), want the validated entry", ce.Entry().ID, ok, err)
	}
}

func TestValidationPolicyRecordsRejectTampering(t *testing.T) {
	cases := []struct {
		name string
		edit func(string) string
	}{
		{"a predecessor generation cannot carry a policy", func(d string) string {
			return strings.Replace(d, `"schema_version": 3`, `"schema_version": 2`, 1)
		}},
		{"an unknown replay meaning", func(d string) string {
			return strings.Replace(d, `"replay": "result-only"`, `"replay": "artifacts"`, 1)
		}},
		{"an unsorted directory set", func(d string) string {
			return strings.Replace(d, `"build",`, `"zz",`, 1)
		}},
		{"a changed directory set no longer matches the key", func(d string) string {
			return strings.Replace(d, `"target"`, `"target2"`, 1)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, layout, h := newStore(t)
			v, err := runcache.NewValidationScope("p", []string{"build", "target"})
			if err != nil {
				t.Fatal(err)
			}
			ki := keyInputFor(h, "tamper")
			ki.Validation = &v
			entry := storeRunKeyed(t, r, h, ki, "out", "", 1<<20, unchangedObs())
			meta := metaPathOf(t, layout.RunsDir(), entry.ID)
			before := readFile(t, meta)
			editMeta(t, meta, tc.edit)
			if readFile(t, meta) == before {
				t.Fatal("the edit did not apply")
			}
			_, err = r.Get(entry.ID)
			if !errors.Is(err, runcache.ErrCorruptStore) {
				t.Fatalf("Get = %v, want corruption", err)
			}
		})
	}
}

func TestOrdinaryPredecessorGenerationStaysReadable(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "ordinary", "out", "", 1<<20)
	editMeta(t, metaPathOf(t, layout.RunsDir(), entry.ID), func(d string) string {
		return strings.Replace(d, `"schema_version": 3`, `"schema_version": 2`, 1)
	})
	got, err := r.Get(entry.ID)
	if err != nil {
		t.Fatalf("a schema-2 run is unreadable: %v", err)
	}
	if got.KeyInput.Validation != nil {
		t.Fatal("a predecessor run acquired a validation policy")
	}
	if _, ok, err := r.Lookup(entry.Key); err != nil || !ok {
		t.Fatalf("Lookup of a schema-2 reusable run = (%v, %v), want a hit", ok, err)
	}
}
