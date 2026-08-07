package run

import (
	"testing"

	"awarer/internal/domain/runcache"
)

// BenchmarkEffectDiagnosticsOverBudget measures the huge-effect-root classifier on the
// fail-closed over-budget report — the hot path a run/ls/status invocation runs once to
// decide whether to raise a latency note. It must stay negligible against the seconds of
// I/O it explains.
func BenchmarkEffectDiagnosticsOverBudget(b *testing.B) {
	report := runcache.OverBudgetReport(runcache.OverBudgetFact{Path: "target", Limit: 100_000}).WithDuration(6800)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = effectDiagnostics(report)
	}
}
