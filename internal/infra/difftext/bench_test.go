package difftext_test

import (
	"fmt"
	"strings"
	"testing"

	"awarer/internal/infra/difftext"
)

func buildText(lines int, changeEvery int) (old, new []byte) {
	var a, b strings.Builder
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&a, "package line %d: the quick brown fox\n", i)
		if changeEvery > 0 && i%changeEvery == 0 {
			fmt.Fprintf(&b, "package line %d: the lazy dog jumps\n", i)
		} else {
			fmt.Fprintf(&b, "package line %d: the quick brown fox\n", i)
		}
	}
	return []byte(a.String()), []byte(b.String())
}

func BenchmarkUnifiedRepresentativeFile(b *testing.B) {
	old, new := buildText(2000, 20)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = difftext.Unified("old", "new", old, new, 3)
	}
}

// BenchmarkHistogramRepresentativeFile mirrors the Myers benchmark on the same
// anchor-rich input, so the two engines are comparable on the default path.
func BenchmarkHistogramRepresentativeFile(b *testing.B) {
	old, new := buildText(2000, 20)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = difftext.UnifiedHistogram("old", "new", old, new, 3)
	}
}

// buildRepeated builds two versions of a file made of one repeated line with a
// single edited line, the repeated-input pathology that forces Histogram to fall
// back to Myers locally.
func buildRepeated(lines int) (old, new []byte) {
	var a, b strings.Builder
	for i := 0; i < lines; i++ {
		a.WriteString("{\"v\":1},\n")
		if i == lines/2 {
			b.WriteString("{\"v\":2},\n")
		} else {
			b.WriteString("{\"v\":1},\n")
		}
	}
	return []byte(a.String()), []byte(b.String())
}

// BenchmarkHistogramRepeatedFallback measures the fallback path on repeated
// input, confirming it stays bounded rather than degrading.
func BenchmarkHistogramRepeatedFallback(b *testing.B) {
	old, new := buildRepeated(2000)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = difftext.UnifiedHistogram("old", "new", old, new, 3)
	}
}

func BenchmarkIsBinary(b *testing.B) {
	old, _ := buildText(2000, 0)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = difftext.IsBinary(old)
	}
}
