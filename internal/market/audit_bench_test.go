package market

import "testing"

// AuditCritical runs on every backtest, so what it costs is part of its
// contract: a few linear passes over bars already in memory, and no
// allocation at all when nothing is wrong. The full battery allocates a
// calendar and is an order of magnitude dearer, which is why it stays behind
// `pyrite audit`.
func BenchmarkAuditCritical(b *testing.B) {
	s := NewSeries("X", cleanBars("2015-01-02", "2023-12-29", 100))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		AuditCritical(s)
	}
}

func BenchmarkAuditFull(b *testing.B) {
	s := NewSeries("X", cleanBars("2015-01-02", "2023-12-29", 100))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Audit(s)
	}
}
