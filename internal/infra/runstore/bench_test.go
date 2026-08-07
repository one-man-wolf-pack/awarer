package runstore_test

import (
	"fmt"
	"testing"
)

func BenchmarkLookup(b *testing.B) {
	r, _, h := newStore(b)
	key := keyFor(h, "bench")
	storeRun(b, r, h, "bench", "some output", "", 1<<20)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok, err := r.Lookup(key); err != nil || !ok {
			b.Fatalf("lookup failed: %v ok=%v", err, ok)
		}
	}
}

// BenchmarkListMany measures enumerating a non-trivial run history (the path behind
// `awa run log`). Each run is discriminated so the store holds distinct entries, and
// the enumeration streams ids through ListRefs and decodes each entry with Get — the
// per-entry work that scales with history size.
func BenchmarkListMany(b *testing.B) {
	r, _, h := newStore(b)
	const runs = 500
	for i := 0; i < runs; i++ {
		storeRun(b, r, h, fmt.Sprintf("run-%d", i), "some output", "some stderr", 1<<20)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := listEntries(r)
		if err != nil {
			b.Fatalf("list failed: %v", err)
		}
		if len(got) == 0 {
			b.Fatal("expected entries")
		}
	}
}
