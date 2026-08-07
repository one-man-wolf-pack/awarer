package runcache_test

import "testing"

func BenchmarkKeyCompute(b *testing.B) {
	h := newHasher(b)
	in := baseline(b, h)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = in.Compute(h)
	}
}
