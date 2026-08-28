package main

import (
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/akalsi-org/polyglot-template/go/lib/hostcpu"
	"github.com/akalsi-org/polyglot-template/go/lib/mpsc"
)

func TestPerfEventAttrMatchesLinuxABI(t *testing.T) {
	if got := unsafe.Sizeof(perfEventAttr{}); got != 128 {
		t.Fatalf("perf_event_attr size = %d, want 128", got)
	}
}

func TestVariantSpecsUseQueueExtentGeometry(t *testing.T) {
	for _, test := range []struct {
		variant string
		payload int
		want    int
	}{
		{"mpsc", 0, 64},
		{"mpsc", 65, 128},
		{"mpsc-padded-256", 56, 256},
		{"spsc", 56, 64},
	} {
		spec, ok := lookupVariant(test.variant)
		if !ok {
			t.Fatalf("lookupVariant rejected %q", test.variant)
		}
		got, err := spec.extent(test.payload)
		if err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Errorf("%s extent(%d) = %d, want %d", test.variant, test.payload, got, test.want)
		}
	}
}

func TestFairness(t *testing.T) {
	ratio, low, high := fairness([]uint64{10, 20, 15})
	if ratio != 0.5 || low != 10 || high != 20 {
		t.Fatalf("fairness = %v, %d, %d", ratio, low, high)
	}
}

func TestGapTableHasRequestedMean(t *testing.T) {
	gaps := makeGaps(2, 0.25)
	var total float64
	for _, gap := range gaps {
		total += float64(gap)
	}
	mean := total / float64(len(gaps))
	want := 4000.0
	if math.Abs(mean-want) > 1 {
		t.Fatalf("mean gap = %.3f ns, want %.3f ns", mean, want)
	}
}

func TestParseArgsAcceptsSPSC(t *testing.T) {
	_, _, _, _, _, _, _, _, err := parseArgs([]string{"throughput", "spsc", "1", "0.1", "56"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestParseArgsAcceptsPaddedVariants(t *testing.T) {
	for _, variant := range []string{"mpsc-padded", "mpsc-padded-256"} {
		_, _, _, _, _, _, _, _, err := parseArgs([]string{"throughput", variant, "1", "0.1", "56"})
		if err != nil {
			t.Errorf("parseArgs rejected variant %q: %v", variant, err)
		}
	}
}

func TestParseArgsRejectsNonfiniteTiming(t *testing.T) {
	for _, args := range [][]string{
		{"throughput", "mpsc", "1", "NaN", "56"},
		{"throughput", "mpsc", "1", "+Inf", "56"},
		{"paced", "mpsc", "1", "0.1", "56", "NaN"},
		{"paced", "mpsc", "1", "0.1", "56", "+Inf"},
	} {
		if _, _, _, _, _, _, _, _, err := parseArgs(args); err == nil {
			t.Errorf("parseArgs accepted nonfinite timing in %v", args)
		}
	}
}

func TestParseArgsConsumerModes(t *testing.T) {
	_, _, mode, _, _, _, _, _, err := parseArgs([]string{"throughput", "mpsc", "1", "0.1", "56"})
	if err != nil || mode != "metadata-only" {
		t.Fatalf("default consumer mode = %q, %v", mode, err)
	}
	_, _, mode, _, _, _, _, _, err = parseArgs([]string{"throughput", "mpsc", "1", "0.1", "56", "--consumer-mode=payload-touching"})
	if err != nil || mode != "payload-touching" {
		t.Fatalf("explicit consumer mode = %q, %v", mode, err)
	}
}

func TestOrderedCPUsFailsClosedWithoutVerifiedTopology(t *testing.T) {
	originalAffinity, originalEffective, originalSnapshot := readAffinity, readEffective, readCPUSnapshot
	defer func() {
		readAffinity, readEffective, readCPUSnapshot = originalAffinity, originalEffective, originalSnapshot
	}()
	set, err := hostcpu.NewCPUSet(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	readAffinity = func() (hostcpu.CPUSet, error) { return set, nil }
	readEffective = func() (hostcpu.CPUSet, error) { return set, nil }
	readCPUSnapshot = func() (hostcpu.Snapshot, error) { return hostcpu.Snapshot{}, errors.New("no topology") }
	if _, _, err := orderedCPUs(2, false); err == nil {
		t.Fatal("orderedCPUs accepted unverified topology")
	}
	cpus, verified, err := orderedCPUs(2, true)
	if err != nil || verified || len(cpus) != 2 {
		t.Fatalf("override result = %v, %v, %v", cpus, verified, err)
	}
}

func TestValidateSchedulerCapacityRequiresOnePPerPinnedThread(t *testing.T) {
	if err := validateSchedulerCapacity(3, 3); err == nil {
		t.Fatal("validateSchedulerCapacity accepted three Ps for four benchmark threads")
	}
	if err := validateSchedulerCapacity(3, 4); err != nil {
		t.Fatal(err)
	}
}

func TestMeasurementBarrierArmsBeforeRelease(t *testing.T) {
	var phase atomic.Uint32
	var armed atomic.Int32
	phase.Store(phaseArmed)
	state := producerState{phase: &phase, armed: &armed}
	done := make(chan bool, 1)
	go func() {
		started := false
		done <- state.awaitMeasurement(nil, &started)
	}()
	deadline := time.Now().Add(time.Second)
	for armed.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if armed.Load() != 1 {
		t.Fatal("producer did not arm")
	}
	select {
	case <-done:
		t.Fatal("producer crossed the barrier before measurement started")
	default:
	}
	phase.Store(phaseMeasure)
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("producer stopped at the measurement barrier")
		}
	case <-time.After(time.Second):
		t.Fatal("producer did not cross the measurement barrier")
	}
}

func TestWaitForOfferReportsPreWaitLateness(t *testing.T) {
	var phase atomic.Uint32
	phase.Store(phaseWarmup)
	state := producerState{rate: 1, phase: &phase}
	next := time.Now()
	gapIndex := 0
	_, wasLate := state.waitForOffer([]time.Duration{2 * time.Millisecond}, &next, &gapIndex)
	if wasLate {
		t.Fatal("waitForOffer marked an on-time arrival as already late")
	}
	next = time.Now().Add(-time.Millisecond)
	gapIndex = 0
	_, wasLate = state.waitForOffer([]time.Duration{time.Nanosecond}, &next, &gapIndex)
	if !wasLate {
		t.Fatal("waitForOffer did not mark an overdue arrival as late")
	}
}

func TestContentionRetryYieldsAfterBoundedStreak(t *testing.T) {
	var phase atomic.Uint32
	phase.Store(phaseMeasure)
	state := producerState{
		mode:        "paced",
		phase:       &phase,
		reportError: func(err error) { t.Fatalf("unexpected error: %v", err) },
	}
	var metrics producerMetrics
	streak := 0
	for range contentionYieldThreshold - 1 {
		if !state.recordRetry(mpsc.ErrContended, &streak, &metrics) {
			t.Fatal("contention retry stopped")
		}
	}
	if metrics.contentionYields != 0 {
		t.Fatalf("early contention yields = %d", metrics.contentionYields)
	}
	if !state.recordRetry(mpsc.ErrContended, &streak, &metrics) {
		t.Fatal("threshold contention retry stopped")
	}
	if metrics.contentionRetries != contentionYieldThreshold || metrics.contentionYields != 1 || streak != 0 {
		t.Fatalf("retry state = retries %d, yields %d, streak %d", metrics.contentionRetries, metrics.contentionYields, streak)
	}
}

func TestSaturatedRetryUsesRareSafetyYield(t *testing.T) {
	var phase atomic.Uint32
	phase.Store(phaseMeasure)
	state := producerState{
		mode:        "throughput",
		phase:       &phase,
		reportError: func(err error) { t.Fatalf("unexpected error: %v", err) },
	}
	var metrics producerMetrics
	streak := 0
	for range saturatedYieldThreshold - 1 {
		if !state.recordRetry(mpsc.ErrFull, &streak, &metrics) {
			t.Fatal("full retry stopped")
		}
	}
	if metrics.retryYields != 0 {
		t.Fatalf("early retry yields = %d", metrics.retryYields)
	}
	if !state.recordRetry(mpsc.ErrFull, &streak, &metrics) {
		t.Fatal("threshold full retry stopped")
	}
	if metrics.fullRetries != saturatedYieldThreshold || metrics.retryYields != 1 || streak != 0 {
		t.Fatalf("retry state = retries %d, yields %d, streak %d", metrics.fullRetries, metrics.retryYields, streak)
	}
}

func TestRetryPolicyNamesSchedulingBehavior(t *testing.T) {
	if got := retryPolicy("throughput"); got != "spin-wait-safety-yield-4096" {
		t.Fatalf("throughput retry policy = %q", got)
	}
	if got := retryPolicy("paced"); got != "paced-full-gosched-contention-yield-64" {
		t.Fatalf("paced retry policy = %q", got)
	}
}

func TestLatencyFieldsIncludeSamplesAndPercentiles(t *testing.T) {
	var row outputRow
	setLatencyFields(&row, []float64{10, 20, 30, 40, 50}, 5)
	if row.LatencySamples != 5 || row.LatencyP50NS != 30 || row.LatencyP95NS != 40 || row.LatencyP99NS != 40 || row.LatencyMaxNS != 50 {
		t.Fatalf("latency row = %+v", row)
	}
}

func TestLatencyCountsPreserveTotalsAfterReservoirsFill(t *testing.T) {
	var result runResult
	for writer := range 2 {
		metrics := producerMetrics{
			sampleState: uint64(writer + 1),
			latencies:   make([]float64, 0, 8),
			admissions:  make([]float64, 0, 8),
		}
		for sample := range 200 {
			recordBoundedSample(&metrics.latencies, &metrics.latencySampled, &metrics.sampleState, float64(sample))
			if sample%4 != 0 {
				recordBoundedSample(&metrics.admissions, &metrics.admissionSampled, &metrics.sampleState, float64(sample))
			}
		}
		result.aggregateProducerSamples(&metrics)
	}
	if len(result.latencies) != 16 || len(result.admissions) != 16 {
		t.Fatalf("stored samples = %d latency, %d admission; want 16 each", len(result.latencies), len(result.admissions))
	}
	var row outputRow
	setLatencyFields(&row, result.latencies, result.latencySampled)
	setAdmissionFields(&row, result.admissions, result.admissionSampled)
	if row.LatencySamples != 400 || row.AdmissionSamples != 300 {
		t.Fatalf("total samples = %d latency, %d admission; want 400 and 300", row.LatencySamples, row.AdmissionSamples)
	}
	if got := float64(row.AdmissionSamples) / float64(row.LatencySamples); got != 0.75 {
		t.Fatalf("admission ratio = %v, want 0.75", got)
	}
}

func TestSampleCapacitiesBoundTotalStorage(t *testing.T) {
	delayCapacity, latencyCapacity := sampleCapacities("paced", 3, 1e9, 1e9)
	if delayCapacity*3 > maxSamples || latencyCapacity != 0 {
		t.Fatalf("paced capacities = %d, %d", delayCapacity, latencyCapacity)
	}
	delayCapacity, latencyCapacity = sampleCapacities("latency", 3, 1, 0)
	if latencyCapacity*3 > maxSamples || delayCapacity != 0 {
		t.Fatalf("latency capacities = %d, %d", delayCapacity, latencyCapacity)
	}
}

func TestPacedSamplesKeepEvidenceAfterStorageFills(t *testing.T) {
	var phase atomic.Uint32
	phase.Store(phaseMeasure)
	state := producerState{rate: 1, phase: &phase}
	metrics := producerMetrics{delays: make([]float64, 0, 2)}
	deadline := time.Now()
	for range 129 {
		state.recordSuccess(&metrics, deadline, true)
	}
	if metrics.count != 129 || metrics.sampled != 3 || metrics.late != 3 || len(metrics.delays) != 2 {
		t.Fatalf("paced metrics = %+v", metrics)
	}
}

func BenchmarkRetryCounterInstrumentation(b *testing.B) {
	b.Run("local", func(b *testing.B) {
		var count uint64
		for range b.N {
			count++
		}
		payloadSink.Store(count)
	})
	b.Run("atomic", func(b *testing.B) {
		var count atomic.Uint64
		for range b.N {
			count.Add(1)
		}
		payloadSink.Store(count.Load())
	})
}

func BenchmarkPayloadTouchAggregation(b *testing.B) {
	payloadChecksum := uint64(56)
	b.Run("local", func(b *testing.B) {
		var total uint64
		for range b.N {
			total += payloadChecksum
		}
		payloadSink.Store(total)
	})
	b.Run("atomic", func(b *testing.B) {
		for range b.N {
			payloadSink.Add(payloadChecksum)
		}
	})
}

func BenchmarkMPSCConcreteWriteDrain(b *testing.B) {
	queue, err := mpsc.CreateMPSC(mpsc.Config{Capacity: 1 << 20})
	if err != nil {
		b.Fatal(err)
	}
	defer queue.Close()
	producer, err := queue.NewProducer()
	if err != nil {
		b.Fatal(err)
	}
	defer producer.Close()
	consumer, err := queue.AttachConsumer()
	if err != nil {
		b.Fatal(err)
	}
	defer consumer.Close()
	payload := make([]byte, 56)
	var payloadChecksum uint64
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := producer.Write(payload); err != nil {
			b.Fatal(err)
		}
		if _, err := drainMPSC(consumer, false, &payloadChecksum); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSPSCConcreteWriteDrain(b *testing.B) {
	queue, err := mpsc.CreateSPSC(mpsc.Config{Capacity: 1 << 20})
	if err != nil {
		b.Fatal(err)
	}
	defer queue.Close()
	producer, err := queue.AttachProducer()
	if err != nil {
		b.Fatal(err)
	}
	defer producer.Close()
	consumer, err := queue.AttachConsumer()
	if err != nil {
		b.Fatal(err)
	}
	defer consumer.Close()
	payload := make([]byte, 56)
	var payloadChecksum uint64
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := producer.Write(payload); err != nil {
			b.Fatal(err)
		}
		if _, err := drainSPSC(consumer, false, &payloadChecksum); err != nil {
			b.Fatal(err)
		}
	}
}
