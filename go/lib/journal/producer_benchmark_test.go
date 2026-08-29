//go:build linux && (amd64 || arm64)

package journal

import "testing"

func BenchmarkProducerLargeReservation(b *testing.B) {
	const maximum = uint64(1 << 20)
	journal, err := Create(Config{
		Capacity:           1 << 22,
		MaxRecordBytes:     maximum,
		Sizing:             journalTestSizing(maximum),
		Backend:            BackendMemfd,
		DisablePreallocate: true,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer journal.Close()
	producer, err := journal.AttachProducer()
	if err != nil {
		b.Fatal(err)
	}
	defer producer.Close()

	b.ReportAllocs()
	b.SetBytes(int64(maximum))
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		span, reserveErr := producer.Reserve(maximum)
		if reserveErr != nil {
			b.Fatal(reserveErr)
		}
		if abortErr := producer.Abort(span); abortErr != nil {
			b.Fatal(abortErr)
		}
	}
}
