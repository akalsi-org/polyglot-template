//go:build linux && (amd64 || arm64)

package mpsc

import "testing"

func BenchmarkMPSCEmptyPeek(b *testing.B) {
	q, err := CreateMPSC(Config{Capacity: 4096})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := q.Close(); err != nil {
			b.Error(err)
		}
	})
	r := mpscReader{q: q, hot: q.hot, rd: 0}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, ok, err := r.Peek()
		if err != nil || ok {
			b.Fatalf("empty peek: ok=%t err=%v", ok, err)
		}
	}
}
