package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	benchschema "github.com/akalsi-org/polyglot-template/go/bench/mpsc/schema"
	"github.com/akalsi-org/polyglot-template/go/lib/hostcpu"
	"github.com/akalsi-org/polyglot-template/go/lib/mpsc"
)

const (
	schemaVersion             = benchschema.Version
	contentionYieldThreshold  = 64
	saturatedYieldThreshold   = 4096
	maxSamples                = 1 << 20
	pacedSleepThreshold       = 100 * time.Microsecond
	pacedMaxSleep             = time.Millisecond
	pacedSpinWindow           = 25 * time.Microsecond
	pacedFinishYieldThreshold = 64
)

const (
	phaseWait uint32 = iota
	phaseWarmup
	phaseArmed
	phaseMeasure
	phaseStop
)

type runResult struct {
	seconds           float64
	records           uint64
	perWriter         []uint64
	delays            []float64
	sampled           uint64
	late              uint64
	fullRetries       uint64
	contentionRetries uint64
	contentionYields  uint64
	retryYields       uint64
	allocations       uint64
	misses            uint64
	cycles            uint64
	insns             uint64
	latencySampled    uint64
	admissionSampled  uint64
	latencies         []float64
	admissions        []float64
}

type outputRow = benchschema.BenchmarkRow

const (
	perfTypeHardware          = 0
	perfCountHardwareCycles   = 0
	perfCountHardwareInsns    = 1
	perfCountHardwareMisses   = 3
	perfEventIOCEnable        = 0x2400
	perfEventIOCDisable       = 0x2401
	perfEventIOCReset         = 0x2403
	perfAttrDisabled          = 1 << 0
	perfAttrExcludeKernel     = 1 << 5
	perfAttrExcludeHypervisor = 1 << 6
)

type perfEventAttr struct {
	Type             uint32
	Size             uint32
	Config           uint64
	SamplePeriod     uint64
	SampleType       uint64
	ReadFormat       uint64
	Flags            uint64
	WakeupEvents     uint32
	BPType           uint32
	Config1          uint64
	Config2          uint64
	BranchSampleType uint64
	SampleRegsUser   uint64
	SampleStackUser  uint32
	ClockID          int32
	SampleRegsIntr   uint64
	AuxWatermark     uint32
	SampleMaxStack   uint16
	Reserved         uint16
	AuxSampleSize    uint32
	SigData          uint64
}

type perfEvent struct {
	name string
	fd   int
}

type threadCounters struct {
	misses perfEvent
	cycles perfEvent
	insns  perfEvent
}

func openPerfEvent(name string, config uint64) (perfEvent, error) {
	attr := perfEventAttr{
		Type:   perfTypeHardware,
		Size:   uint32(unsafe.Sizeof(perfEventAttr{})),
		Config: config,
		Flags:  perfAttrDisabled | perfAttrExcludeKernel | perfAttrExcludeHypervisor,
	}
	fd, _, errno := syscall.Syscall6(syscall.SYS_PERF_EVENT_OPEN, uintptr(unsafe.Pointer(&attr)), 0, ^uintptr(0), ^uintptr(0), 0, 0)
	if errno != 0 {
		return perfEvent{fd: -1}, fmt.Errorf("open %s perf event: %w", name, errno)
	}
	return perfEvent{name: name, fd: int(fd)}, nil
}

func openThreadCounters() (*threadCounters, error) {
	counters := threadCounters{
		misses: perfEvent{fd: -1},
		cycles: perfEvent{fd: -1},
		insns:  perfEvent{fd: -1},
	}
	var err error
	if counters.misses, err = openPerfEvent("cache-misses", perfCountHardwareMisses); err != nil {
		return nil, err
	}
	if counters.cycles, err = openPerfEvent("cpu-cycles", perfCountHardwareCycles); err != nil {
		counters.close()
		return nil, err
	}
	if counters.insns, err = openPerfEvent("instructions", perfCountHardwareInsns); err != nil {
		counters.close()
		return nil, err
	}
	return &counters, nil
}

func (c *threadCounters) events() []*perfEvent {
	return []*perfEvent{&c.misses, &c.cycles, &c.insns}
}

func (c *threadCounters) ioctl(request uintptr) error {
	for _, event := range c.events() {
		if event.fd < 0 {
			continue
		}
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(event.fd), request, 0)
		if errno != 0 {
			return fmt.Errorf("control %s perf event: %w", event.name, errno)
		}
	}
	return nil
}

func (c *threadCounters) start() error {
	if err := c.ioctl(perfEventIOCReset); err != nil {
		return err
	}
	return c.ioctl(perfEventIOCEnable)
}

func readPerfEvent(event *perfEvent) (uint64, error) {
	var encoded [8]byte
	n, err := syscall.Read(event.fd, encoded[:])
	if err != nil {
		return 0, fmt.Errorf("read %s perf event: %w", event.name, err)
	}
	if n != len(encoded) {
		return 0, fmt.Errorf("read %s perf event: got %d bytes, want %d", event.name, n, len(encoded))
	}
	return binary.LittleEndian.Uint64(encoded[:]), nil
}

func (c *threadCounters) stop() (uint64, uint64, uint64, error) {
	if err := c.ioctl(perfEventIOCDisable); err != nil {
		return 0, 0, 0, err
	}
	misses, err := readPerfEvent(&c.misses)
	if err != nil {
		return 0, 0, 0, err
	}
	cycles, err := readPerfEvent(&c.cycles)
	if err != nil {
		return 0, 0, 0, err
	}
	insns, err := readPerfEvent(&c.insns)
	if err != nil {
		return 0, 0, 0, err
	}
	return misses, cycles, insns, nil
}

func (c *threadCounters) close() {
	for _, event := range c.events() {
		if event.fd >= 0 {
			syscall.Close(event.fd)
			event.fd = -1
		}
	}
}

type variantSpec struct {
	layout         mpsc.MPSCLayout
	spsc           bool
	implementation string
}

func lookupVariant(variant string) (variantSpec, bool) {
	version := mpsc.FormatVersion
	switch variant {
	case "mpsc":
		return variantSpec{layout: mpsc.MPSCCompact, implementation: fmt.Sprintf("go-mpsc-v%d-compact", version)}, true
	case "mpsc-padded":
		return variantSpec{layout: mpsc.MPSCPadded64, implementation: fmt.Sprintf("go-mpsc-v%d-padded-64", version)}, true
	case "mpsc-padded-256":
		return variantSpec{layout: mpsc.MPSCPadded256, implementation: fmt.Sprintf("go-mpsc-v%d-padded-256", version)}, true
	case "spsc":
		return variantSpec{spsc: true, implementation: fmt.Sprintf("go-spsc-v%d", version)}, true
	default:
		return variantSpec{}, false
	}
}

func (s variantSpec) extent(payload int) (int, error) {
	if s.spsc {
		extent, err := mpsc.SPSCExtent(uint64(payload))
		return int(extent), err
	}
	_, extent, err := mpsc.MPSCLayoutExtent(s.layout, uint64(payload))
	return int(extent), err
}

var (
	readAffinity    = func() (hostcpu.CPUSet, error) { return hostcpu.Affinity(0) }
	readEffective   = hostcpu.EffectiveCPUSet
	readCPUSnapshot = hostcpu.ReadSnapshot
)

func orderedCPUs(required int, allowUnverified bool) ([]int, bool, error) {
	affinity, err := readAffinity()
	if err != nil {
		return nil, false, err
	}
	effective, err := readEffective()
	if err != nil {
		if allowUnverified && len(affinity.CPUs()) >= required {
			return affinity.CPUs(), false, nil
		}
		return nil, false, fmt.Errorf("cannot verify effective CPU set: %w", err)
	}
	allowed := affinity.Intersection(effective)
	snapshot, err := readCPUSnapshot()
	if err != nil {
		if allowUnverified && len(allowed.CPUs()) >= required {
			return allowed.CPUs(), false, nil
		}
		return nil, false, fmt.Errorf("cannot verify CPU topology: %w", err)
	}
	type core struct{ pkg, id int }
	seen := make(map[core]bool)
	primary := make([]int, 0, allowed.Count())
	for _, cpu := range snapshot.CPUs {
		if !allowed.Contains(cpu.Id) {
			continue
		}
		key := core{cpu.PackageId, cpu.CoreId}
		if !seen[key] {
			seen[key] = true
			primary = append(primary, cpu.Id)
		}
	}
	if len(primary) >= required {
		return primary, true, nil
	}
	if allowUnverified && len(allowed.CPUs()) >= required {
		return allowed.CPUs(), false, nil
	}
	return nil, false, fmt.Errorf("cannot place %d benchmark threads on distinct physical cores; found %d", required, len(primary))
}

func pin(cpu int) error {
	set, err := hostcpu.NewCPUSet(cpu)
	if err != nil {
		return err
	}
	return hostcpu.SetAffinity(0, set)
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	return values[int(p*float64(len(values)-1))]
}

func fairness(counts []uint64) (float64, uint64, uint64) {
	if len(counts) == 0 {
		return 0, 0, 0
	}
	low, high := counts[0], counts[0]
	for _, count := range counts[1:] {
		if count < low {
			low = count
		}
		if count > high {
			high = count
		}
	}
	if high == 0 {
		return 0, low, high
	}
	return float64(low) / float64(high), low, high
}

func sampleCapacities(mode string, writers int, seconds, rate float64) (int, int) {
	perWriterLimit := maxSamples / writers
	if mode == "latency" {
		return 0, perWriterLimit
	}
	if mode != "paced" {
		return 0, 0
	}
	expectedSamples := math.Ceil(seconds*rate*1e6/64) + 1
	if expectedSamples >= float64(perWriterLimit) {
		return perWriterLimit, 0
	}
	return int(expectedSamples), 0
}

func makeGaps(writer int, rate float64) []time.Duration {
	const count = 4096
	x := uint64(0x9e3779b97f4a7c15) * uint64(writer+1)
	draws := make([]float64, count)
	var sum float64
	for i := range draws {
		x += 0x9e3779b97f4a7c15
		z := x
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		z ^= z >> 31
		u := (float64(z>>11) + 1) / 9007199254740992
		draws[i] = -math.Log(u)
		sum += draws[i]
	}
	mean := float64(time.Second) / (rate * 1e6)
	scale := mean * count / sum
	gaps := make([]time.Duration, count)
	for i, draw := range draws {
		gaps[i] = time.Duration(draw * scale)
	}
	return gaps
}

func drainMPSC(consumer *mpsc.MPSCConsumer, touchPayload bool, payloadChecksum *uint64) (int, error) {
	count := 0
	for count < 256 {
		record, ok, err := consumer.Peek()
		if err != nil {
			return count, err
		}
		if !ok {
			break
		}
		if touchPayload {
			*payloadChecksum += checksum(record.UnsafeBytes())
		}
		if err := consumer.Pop(); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func drainSPSC(consumer *mpsc.SPSCConsumer, touchPayload bool, payloadChecksum *uint64) (int, error) {
	count := 0
	for count < 256 {
		record, ok, err := consumer.Peek()
		if err != nil {
			return count, err
		}
		if !ok {
			break
		}
		if touchPayload {
			*payloadChecksum += checksum(record.UnsafeBytes())
		}
		if err := consumer.Pop(); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

type producerMetrics struct {
	count             uint64
	fullRetries       uint64
	contentionRetries uint64
	contentionYields  uint64
	retryYields       uint64
	misses            uint64
	cycles            uint64
	insns             uint64
	late              uint64
	sampled           uint64
	latencySampled    uint64
	admissionSampled  uint64
	sampleState       uint64
	delays            []float64
	latencies         []float64
	admissions        []float64
}

func (result *runResult) aggregateProducerSamples(metrics *producerMetrics) {
	result.sampled += metrics.sampled
	result.late += metrics.late
	result.latencySampled += metrics.latencySampled
	result.admissionSampled += metrics.admissionSampled
	result.delays = append(result.delays, metrics.delays...)
	result.latencies = append(result.latencies, metrics.latencies...)
	result.admissions = append(result.admissions, metrics.admissions...)
}

func recordBoundedSample(samples *[]float64, sampled *uint64, sampleState *uint64, value float64) {
	*sampled++
	if len(*samples) < cap(*samples) {
		*samples = append(*samples, value)
		return
	}
	if cap(*samples) == 0 {
		return
	}
	x := *sampleState
	if x == 0 {
		x = 0x9e3779b97f4a7c15
	}
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	*sampleState = x
	if replacement := x % *sampled; replacement < uint64(cap(*samples)) {
		(*samples)[replacement] = value
	}
}

type producerState struct {
	mode            string
	rate            float64
	payload         []byte
	cpus            []int
	phase           *atomic.Uint32
	ready           *atomic.Int32
	armed           *atomic.Int32
	metrics         []producerMetrics
	delayCapacity   int
	latencyCapacity int
	reportError     func(error)
}

func (s *producerState) prepare(index int) ([]time.Duration, *threadCounters, bool) {
	if err := pin(s.cpus[index+1]); err != nil {
		s.reportError(err)
		s.ready.Add(1)
		return nil, nil, false
	}
	var counters *threadCounters
	if s.mode == "counters" {
		var err error
		counters, err = openThreadCounters()
		if err != nil {
			s.reportError(fmt.Errorf("perf counters unavailable on writer %d: %w", index, err))
			s.ready.Add(1)
			return nil, nil, false
		}
	}
	var gaps []time.Duration
	if s.rate > 0 {
		gaps = makeGaps(index, s.rate)
	}
	s.ready.Add(1)
	for s.phase.Load() == phaseWait {
		runtime.Gosched()
	}
	return gaps, counters, true
}

func (s *producerState) recordRetry(err error, failureStreak *int, metrics *producerMetrics) bool {
	if s.phase.Load() == phaseStop {
		return false
	}
	full := errors.Is(err, mpsc.ErrFull)
	contended := errors.Is(err, mpsc.ErrContended)
	if !full && !contended {
		s.reportError(err)
		s.phase.Store(phaseStop)
		return false
	}
	measuring := s.phase.Load() == phaseMeasure
	if measuring {
		if full {
			metrics.fullRetries++
		} else {
			metrics.contentionRetries++
		}
	}
	*failureStreak++
	if s.mode != "paced" {
		mpsc.SpinWait()
		if *failureStreak >= saturatedYieldThreshold {
			*failureStreak = 0
			if measuring {
				metrics.retryYields++
			}
			runtime.Gosched()
		}
		return true
	}
	if full {
		*failureStreak = 0
		runtime.Gosched()
		return true
	}
	if *failureStreak >= contentionYieldThreshold {
		*failureStreak = 0
		if measuring {
			metrics.contentionYields++
		}
		runtime.Gosched()
	}
	return true
}

func (s *producerState) startCounters(counters *threadCounters, started *bool) bool {
	if counters == nil || *started || s.phase.Load() != phaseMeasure {
		return true
	}
	if err := counters.start(); err != nil {
		s.reportError(err)
		s.phase.Store(phaseStop)
		return false
	}
	*started = true
	return true
}

func (s *producerState) awaitMeasurement(counters *threadCounters, counterStarted *bool) bool {
	if s.phase.Load() != phaseArmed {
		return s.phase.Load() != phaseStop
	}
	s.armed.Add(1)
	for s.phase.Load() == phaseArmed {
		runtime.Gosched()
	}
	return s.phase.Load() != phaseStop && s.startCounters(counters, counterStarted)
}

func (s *producerState) finishCounters(counters *threadCounters, started bool, metrics *producerMetrics) {
	if counters == nil {
		return
	}
	if !started {
		s.reportError(errors.New("perf counters did not enter the measured phase"))
		return
	}
	misses, cycles, insns, err := counters.stop()
	if err != nil {
		s.reportError(err)
		return
	}
	metrics.misses = misses
	metrics.cycles = cycles
	metrics.insns = insns
}

func (s *producerState) runMPSC(index int, writer *mpsc.MPSCProducer) {
	defer writer.Close()
	metrics := producerMetrics{
		sampleState: uint64(index+1) * 0x9e3779b97f4a7c15,
		delays:      make([]float64, 0, s.delayCapacity),
		latencies:   make([]float64, 0, s.latencyCapacity),
		admissions:  make([]float64, 0, s.latencyCapacity),
	}
	gaps, counters, ok := s.prepare(index)
	if !ok {
		return
	}
	if counters != nil {
		defer counters.close()
	}
	threadIdentity := hostcpu.CurrentThreadIdentity()
	s.runMPSCLoop(index, writer, gaps, counters, &metrics)
	if hostcpu.CurrentThreadIdentity() != threadIdentity {
		s.reportError(fmt.Errorf("writer %d moved to another OS thread during measurement", index))
	}
}

func (s *producerState) runMPSCLoop(index int, writer *mpsc.MPSCProducer, gaps []time.Duration, counters *threadCounters, metrics *producerMetrics) {
	var next time.Time
	gapIndex := 0
	failureStreak := 0
	counterStarted := false
	for s.phase.Load() != phaseStop {
		if !s.awaitMeasurement(counters, &counterStarted) {
			break
		}
		deadline, wasLate := s.waitForOffer(gaps, &next, &gapIndex)
		if s.phase.Load() == phaseArmed {
			continue
		}
		if s.phase.Load() == phaseStop || !s.startCounters(counters, &counterStarted) {
			break
		}
		sampleLatency := s.mode == "latency" && s.phase.Load() == phaseMeasure && metrics.count&63 == 0
		var admissionStart time.Time
		if sampleLatency {
			admissionStart = time.Now()
		}
		succeeded := false
		retried := false
		for {
			var attemptStart time.Time
			if sampleLatency {
				attemptStart = time.Now()
			}
			err := writer.Write(s.payload)
			if err == nil {
				failureStreak = 0
				succeeded = true
				if sampleLatency {
					recordBoundedSample(&metrics.latencies, &metrics.latencySampled, &metrics.sampleState, float64(time.Since(attemptStart).Nanoseconds()))
					if retried {
						recordBoundedSample(&metrics.admissions, &metrics.admissionSampled, &metrics.sampleState, float64(attemptStart.Sub(admissionStart).Nanoseconds()))
					}
				}
				break
			}
			if sampleLatency {
				retried = true
			}
			if !s.recordRetry(err, &failureStreak, metrics) {
				break
			}
		}
		if succeeded {
			s.recordSuccess(metrics, deadline, wasLate)
		}
	}
	s.finishCounters(counters, counterStarted, metrics)
	s.metrics[index] = *metrics
}

func (s *producerState) runSPSC(index int, writer *mpsc.SPSCProducer) {
	defer writer.Close()
	metrics := producerMetrics{
		sampleState: uint64(index+1) * 0x9e3779b97f4a7c15,
		delays:      make([]float64, 0, s.delayCapacity),
		latencies:   make([]float64, 0, s.latencyCapacity),
		admissions:  make([]float64, 0, s.latencyCapacity),
	}
	gaps, counters, ok := s.prepare(index)
	if !ok {
		return
	}
	if counters != nil {
		defer counters.close()
	}
	threadIdentity := hostcpu.CurrentThreadIdentity()
	s.runSPSCLoop(index, writer, gaps, counters, &metrics)
	if hostcpu.CurrentThreadIdentity() != threadIdentity {
		s.reportError(fmt.Errorf("writer %d moved to another OS thread during measurement", index))
	}
}

func (s *producerState) runSPSCLoop(index int, writer *mpsc.SPSCProducer, gaps []time.Duration, counters *threadCounters, metrics *producerMetrics) {
	var next time.Time
	gapIndex := 0
	failureStreak := 0
	counterStarted := false
	for s.phase.Load() != phaseStop {
		if !s.awaitMeasurement(counters, &counterStarted) {
			break
		}
		deadline, wasLate := s.waitForOffer(gaps, &next, &gapIndex)
		if s.phase.Load() == phaseArmed {
			continue
		}
		if s.phase.Load() == phaseStop || !s.startCounters(counters, &counterStarted) {
			break
		}
		sampleLatency := s.mode == "latency" && s.phase.Load() == phaseMeasure && metrics.count&63 == 0
		var admissionStart time.Time
		if sampleLatency {
			admissionStart = time.Now()
		}
		succeeded := false
		retried := false
		for {
			var attemptStart time.Time
			if sampleLatency {
				attemptStart = time.Now()
			}
			err := writer.Write(s.payload)
			if err == nil {
				failureStreak = 0
				succeeded = true
				if sampleLatency {
					recordBoundedSample(&metrics.latencies, &metrics.latencySampled, &metrics.sampleState, float64(time.Since(attemptStart).Nanoseconds()))
					if retried {
						recordBoundedSample(&metrics.admissions, &metrics.admissionSampled, &metrics.sampleState, float64(attemptStart.Sub(admissionStart).Nanoseconds()))
					}
				}
				break
			}
			if sampleLatency {
				retried = true
			}
			if !s.recordRetry(err, &failureStreak, metrics) {
				break
			}
		}
		if succeeded {
			s.recordSuccess(metrics, deadline, wasLate)
		}
	}
	s.finishCounters(counters, counterStarted, metrics)
	s.metrics[index] = *metrics
}

func (s *producerState) waitForOffer(gaps []time.Duration, next *time.Time, gapIndex *int) (time.Time, bool) {
	if s.rate <= 0 {
		return time.Time{}, false
	}
	if next.IsZero() {
		*next = time.Now()
	}
	*next = next.Add(gaps[*gapIndex&(len(gaps)-1)])
	*gapIndex++
	deadline := *next
	wasLate := time.Now().After(deadline)
	for s.phase.Load() != phaseStop && s.phase.Load() != phaseArmed {
		remaining := time.Until(deadline)
		if remaining <= pacedSleepThreshold {
			break
		}
		sleepFor := remaining - pacedSpinWindow
		if sleepFor > pacedMaxSleep {
			sleepFor = pacedMaxSleep
		}
		time.Sleep(sleepFor)
	}
	finishStreak := 0
	for time.Now().Before(deadline) && s.phase.Load() != phaseStop && s.phase.Load() != phaseArmed {
		mpsc.SpinWait()
		finishStreak++
		if finishStreak == pacedFinishYieldThreshold {
			finishStreak = 0
			runtime.Gosched()
		}
	}
	return deadline, wasLate
}

func (s *producerState) recordSuccess(metrics *producerMetrics, deadline time.Time, wasLate bool) {
	if s.phase.Load() != phaseMeasure {
		return
	}
	if metrics.count&63 == 0 && s.rate > 0 {
		recordBoundedSample(&metrics.delays, &metrics.sampled, &metrics.sampleState, float64(time.Since(deadline).Nanoseconds()))
		if wasLate {
			metrics.late++
		}
	}
	metrics.count++
}

func validateSchedulerCapacity(writers, gomaxprocs int) error {
	required := writers + 1
	if gomaxprocs < required {
		return fmt.Errorf("GOMAXPROCS=%d cannot schedule %d pinned benchmark threads", gomaxprocs, required)
	}
	return nil
}

func runQueue(mode, variant, consumerMode string, writers int, seconds float64, payload []byte, rate float64, cpus []int) (runResult, error) {
	if err := validateSchedulerCapacity(writers, runtime.GOMAXPROCS(0)); err != nil {
		return runResult{}, err
	}
	spec, ok := lookupVariant(variant)
	if !ok {
		return runResult{}, fmt.Errorf("unsupported variant %q", variant)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var payloadChecksum uint64
	var drainBatch func() (int, error)
	var startWriter func(int, *producerState)
	var closeConsumer func() error
	var closeQueue func() error
	if spec.spsc {
		queue, err := mpsc.CreateSPSC(mpsc.Config{Capacity: 1 << 20})
		if err != nil {
			return runResult{}, err
		}
		closeQueue = queue.Close
		consumer, err := queue.AttachConsumer()
		if err != nil {
			queue.Close()
			return runResult{}, err
		}
		touchPayload := consumerMode == "payload-touching"
		drainBatch = func() (int, error) { return drainSPSC(consumer, touchPayload, &payloadChecksum) }
		closeConsumer = consumer.Close
		startWriter = func(index int, state *producerState) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			producer, err := queue.AttachProducer()
			if err != nil {
				state.reportError(err)
				state.ready.Add(1)
				return
			}
			state.runSPSC(index, producer)
		}
	} else {
		queue, err := mpsc.CreateMPSC(mpsc.Config{Capacity: 1 << 20, MPSCLayout: spec.layout})
		if err != nil {
			return runResult{}, err
		}
		closeQueue = queue.Close
		consumer, err := queue.AttachConsumer()
		if err != nil {
			queue.Close()
			return runResult{}, err
		}
		touchPayload := consumerMode == "payload-touching"
		drainBatch = func() (int, error) { return drainMPSC(consumer, touchPayload, &payloadChecksum) }
		closeConsumer = consumer.Close
		startWriter = func(index int, state *producerState) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			producer, err := queue.NewProducer()
			if err != nil {
				state.reportError(err)
				state.ready.Add(1)
				return
			}
			state.runMPSC(index, producer)
		}
	}
	defer closeQueue()
	defer closeConsumer()
	if err := pin(cpus[0]); err != nil {
		return runResult{}, err
	}
	consumerThreadIdentity := hostcpu.CurrentThreadIdentity()

	var phase atomic.Uint32
	var ready atomic.Int32
	var armed atomic.Int32
	delayCapacity, latencyCapacity := sampleCapacities(mode, writers, seconds, rate)
	metrics := make([]producerMetrics, writers)
	workerErrors := make(chan error, writers)
	reportWorkerError := func(err error) {
		select {
		case workerErrors <- err:
		default:
		}
	}
	state := producerState{
		mode:            mode,
		rate:            rate,
		payload:         payload,
		cpus:            cpus,
		phase:           &phase,
		ready:           &ready,
		armed:           &armed,
		metrics:         metrics,
		delayCapacity:   delayCapacity,
		latencyCapacity: latencyCapacity,
		reportError:     reportWorkerError,
	}
	var wg sync.WaitGroup
	stopAndWait := func() {
		phase.Store(phaseStop)
		wg.Wait()
	}
	pollWorkerError := func() error {
		select {
		case err := <-workerErrors:
			return err
		default:
			return nil
		}
	}
	for writerIndex := 0; writerIndex < writers; writerIndex++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			startWriter(index, &state)
		}(writerIndex)
	}
	for ready.Load() != int32(writers) {
		if err := pollWorkerError(); err != nil {
			stopAndWait()
			return runResult{}, err
		}
		if _, err := drainBatch(); err != nil {
			stopAndWait()
			return runResult{}, fmt.Errorf("consumer drain: %w", err)
		}
	}
	if err := pollWorkerError(); err != nil {
		stopAndWait()
		return runResult{}, err
	}
	phase.Store(phaseWarmup)
	warmupEnd := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(warmupEnd) {
		if err := pollWorkerError(); err != nil {
			stopAndWait()
			return runResult{}, err
		}
		if _, err := drainBatch(); err != nil {
			stopAndWait()
			return runResult{}, fmt.Errorf("consumer drain: %w", err)
		}
	}
	if !phase.CompareAndSwap(phaseWarmup, phaseArmed) {
		stopAndWait()
		if err := pollWorkerError(); err != nil {
			return runResult{}, err
		}
		return runResult{}, errors.New("producer stopped during warmup")
	}
	for armed.Load() != int32(writers) {
		if err := pollWorkerError(); err != nil {
			stopAndWait()
			return runResult{}, err
		}
		if _, err := drainBatch(); err != nil {
			stopAndWait()
			return runResult{}, fmt.Errorf("consumer drain: %w", err)
		}
	}
	var memoryStart runtime.MemStats
	runtime.ReadMemStats(&memoryStart)
	start := time.Now()
	phase.Store(phaseMeasure)
	end := start.Add(time.Duration(seconds * float64(time.Second)))
	for time.Now().Before(end) {
		if err := pollWorkerError(); err != nil {
			stopAndWait()
			return runResult{}, err
		}
		if _, err := drainBatch(); err != nil {
			stopAndWait()
			return runResult{}, fmt.Errorf("consumer drain: %w", err)
		}
	}
	measuredEnd := time.Now()
	phase.Store(phaseStop)
	var memoryEnd runtime.MemStats
	runtime.ReadMemStats(&memoryEnd)
	wg.Wait()
	if err := pollWorkerError(); err != nil {
		return runResult{}, err
	}
	if hostcpu.CurrentThreadIdentity() != consumerThreadIdentity {
		return runResult{}, errors.New("consumer moved to another OS thread during measurement")
	}
	payloadSink.Add(payloadChecksum)
	var result runResult
	result.allocations = memoryEnd.Mallocs - memoryStart.Mallocs
	result.seconds = measuredEnd.Sub(start).Seconds()
	result.perWriter = make([]uint64, writers)
	result.delays = make([]float64, 0, delayCapacity*writers)
	result.latencies = make([]float64, 0, latencyCapacity*writers)
	result.admissions = make([]float64, 0, latencyCapacity*writers)
	for i := range metrics {
		writerMetrics := &metrics[i]
		result.perWriter[i] = writerMetrics.count
		result.records += writerMetrics.count
		result.fullRetries += writerMetrics.fullRetries
		result.contentionRetries += writerMetrics.contentionRetries
		result.contentionYields += writerMetrics.contentionYields
		result.retryYields += writerMetrics.retryYields
		result.misses += writerMetrics.misses
		result.cycles += writerMetrics.cycles
		result.insns += writerMetrics.insns
		result.aggregateProducerSamples(writerMetrics)
	}
	return result, nil
}

var payloadSink atomic.Uint64

func checksum(payload []byte) uint64 {
	var sum uint64
	for _, value := range payload {
		sum += uint64(value)
	}
	return sum
}

func retryPolicy(mode string) string {
	if mode == "paced" {
		return "paced-full-gosched-contention-yield-64"
	}
	return "spin-wait-safety-yield-4096"
}

func parseArgs(args []string) (mode, variant, consumerMode string, writers int, seconds float64, payload int, rate float64, allowUnverified bool, err error) {
	consumerMode = "metadata-only"
	filtered := args[:0]
	for _, arg := range args {
		if arg == "--allow-unverified-topology" {
			allowUnverified = true
			continue
		}
		if strings.HasPrefix(arg, "--consumer-mode=") {
			consumerMode = strings.TrimPrefix(arg, "--consumer-mode=")
			continue
		}
		filtered = append(filtered, arg)
	}
	args = filtered
	if len(args) < 4 {
		err = errors.New("usage: bench_queue throughput|counters|latency|paced variant writers seconds [payload] [per_writer_mrec_s] [--allow-unverified-topology]")
		return
	}
	mode, variant = args[0], args[1]
	writers, err = strconv.Atoi(args[2])
	if err != nil {
		return
	}
	seconds, err = strconv.ParseFloat(args[3], 64)
	if err != nil {
		return
	}
	payload = 56
	if len(args) > 4 {
		payload, err = strconv.Atoi(args[4])
		if err != nil {
			return
		}
	}
	if mode == "paced" {
		if len(args) < 6 {
			err = errors.New("paced needs a positive per-writer rate in Mrec/s")
			return
		}
		rate, err = strconv.ParseFloat(args[5], 64)
	}
	_, validVariant := lookupVariant(variant)
	if mode != "throughput" && mode != "counters" && mode != "latency" && mode != "paced" {
		err = fmt.Errorf("unsupported mode %q", mode)
	} else if !validVariant {
		err = fmt.Errorf("unsupported variant %q", variant)
	} else if consumerMode != "metadata-only" && consumerMode != "payload-touching" {
		err = fmt.Errorf("unsupported consumer mode %q", consumerMode)
	} else if variant == "spsc" && writers != 1 {
		err = errors.New("spsc takes exactly one writer")
	} else if writers <= 0 || seconds <= 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) || payload < 0 || payload > 65536 || mode == "paced" && (rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0)) {
		err = errors.New("writers, finite seconds, and finite paced rate must be positive; payload must be in [0, 65536]")
	}
	return
}

func setLatencyFields(row *outputRow, samples []float64, sampled uint64) {
	sort.Float64s(samples)
	row.LatencySamples = int(sampled)
	row.LatencyP50NS = percentile(samples, 0.50)
	row.LatencyP95NS = percentile(samples, 0.95)
	row.LatencyP99NS = percentile(samples, 0.99)
	row.LatencyP9999NS = percentile(samples, 0.9999)
	row.LatencyMaxNS = percentile(samples, 1)
}

func setAdmissionFields(row *outputRow, samples []float64, sampled uint64) {
	sort.Float64s(samples)
	row.AdmissionSamples = int(sampled)
	row.AdmissionP50NS = percentile(samples, 0.50)
	row.AdmissionP95NS = percentile(samples, 0.95)
	row.AdmissionP99NS = percentile(samples, 0.99)
	row.AdmissionP9999NS = percentile(samples, 0.9999)
	row.AdmissionMaxNS = percentile(samples, 1)
}

func main() {
	mode, variant, consumerMode, writers, seconds, payloadSize, rate, allowUnverified, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	spec, _ := lookupVariant(variant)
	extent, err := spec.extent(payloadSize)
	if err != nil {
		fmt.Fprintln(os.Stderr, "benchmark setup failed:", err)
		os.Exit(2)
	}
	cpus, topologyVerified, err := orderedCPUs(writers+1, allowUnverified)
	if err != nil {
		fmt.Fprintln(os.Stderr, "benchmark setup failed:", err)
		os.Exit(2)
	}
	if !topologyVerified {
		fmt.Fprintln(os.Stderr, "benchmark warning: --allow-unverified-topology permits placement without proven physical-core separation")
	}
	result, err := runQueue(mode, variant, consumerMode, writers, seconds, make([]byte, payloadSize), rate, cpus)
	if err != nil {
		fmt.Fprintln(os.Stderr, "benchmark failed:", err)
		os.Exit(2)
	}
	mrecS := float64(result.records) / result.seconds / 1e6
	fair, low, high := fairness(result.perWriter)
	row := outputRow{
		SchemaVersion:         schemaVersion,
		Mode:                  mode,
		Variant:               variant,
		ImplementationVariant: spec.implementation,
		Writers:               writers,
		Seconds:               result.seconds,
		Records:               result.records,
		MrecS:                 mrecS,
		PayloadBytes:          payloadSize,
		ExtentBytes:           extent,
		PayloadGBS:            float64(result.records) / result.seconds * float64(payloadSize) / 1e9,
		ReservedGBS:           float64(result.records) / result.seconds * float64(extent) / 1e9,
		FullRetries:           result.fullRetries,
		ContentionRetries:     result.contentionRetries,
		ContentionYields:      result.contentionYields,
		RetryYields:           result.retryYields,
		RetryPolicy:           retryPolicy(mode),
		Allocations:           result.allocations,
		TopologyVerified:      topologyVerified,
		TopologyOverride:      allowUnverified,
		ReaderCPU:             cpus[0],
		WriterCPUs:            append([]int(nil), cpus[1:writers+1]...),
		ConsumerMode:          consumerMode,
	}
	if result.records > 0 {
		row.AllocationsPerRecord = float64(result.allocations) / float64(result.records)
	}
	if mode == "counters" || mode == "paced" {
		row.FairnessMinMax, row.MinWriterRecords, row.MaxWriterRecords = fair, low, high
	}
	if mode == "counters" {
		row.CountersAvailable = true
		row.Misses = result.misses
		if result.records > 0 {
			row.MissesPerRecord = float64(result.misses) / float64(result.records)
			row.CyclesPerRecord = float64(result.cycles) / float64(result.records)
			row.InsnsPerRecord = float64(result.insns) / float64(result.records)
		}
		if result.cycles > 0 {
			row.IPC = float64(result.insns) / float64(result.cycles)
		}
	}
	if mode == "latency" {
		setLatencyFields(&row, result.latencies, result.latencySampled)
		setAdmissionFields(&row, result.admissions, result.admissionSampled)
	}
	if mode == "paced" {
		sort.Float64s(result.delays)
		row.OfferedPerWriterMrecS = rate
		row.OfferedMrecS = rate * float64(writers)
		row.AchievedRatio = mrecS / row.OfferedMrecS
		row.DelayP50NS = percentile(result.delays, 0.50)
		row.DelayP99NS = percentile(result.delays, 0.99)
		row.DelayMaxNS = percentile(result.delays, 1)
		row.DelaySamples = len(result.delays)
		if result.sampled > 0 {
			row.LateFraction = float64(result.late) / float64(result.sampled)
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(row); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
