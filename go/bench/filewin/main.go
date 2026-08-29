package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/akalsi-org/polyglot-template/go/lib/filewin"
	"github.com/akalsi-org/polyglot-template/go/lib/hostcpu"
	"github.com/akalsi-org/polyglot-template/go/lib/tsc"
)

const (
	minPayload   = 8
	maxSamples   = 1_000_000
	stampBytes   = 8
	tmpfsMagic   = 0x01021994
	ramfsMagic   = 0x858458f6
	ext4Magic    = 0xEF53
	xfsMagic     = 0x58465342
	btrfsMagic   = 0x9123683E
	phaseWait    = 0
	phaseWarmup  = 1
	phaseMeasure = 2
	phaseStop    = 3
)

type result struct {
	PayloadBytes     int     `json:"payload_bytes"`
	Seconds          float64 `json:"seconds"`
	FSType           string  `json:"fs_type"`
	Dir              string  `json:"dir"`
	WriterCPU        int     `json:"writer_cpu"`
	ReaderCPU        int     `json:"reader_cpu"`
	TopologyVerified bool    `json:"topology_verified"`
	WriterRecords    uint64  `json:"writer_records"`
	ReaderRecords    uint64  `json:"reader_records"`
	WriterMrecS      float64 `json:"writer_mrec_s"`
	ReaderMrecS      float64 `json:"reader_mrec_s"`
	WriterGBS        float64 `json:"writer_gb_s"`
	ReaderGBS        float64 `json:"reader_gb_s"`
	FileBytes        uint64  `json:"file_bytes"`
	WriterSamples    int     `json:"writer_samples"`
	WriterP50NS      float64 `json:"writer_p50_ns"`
	WriterP90NS      float64 `json:"writer_p90_ns"`
	WriterP95NS      float64 `json:"writer_p95_ns"`
	WriterP99NS      float64 `json:"writer_p99_ns"`
	WriterP9999NS    float64 `json:"writer_p99_99_ns"`
	WriterMaxNS      float64 `json:"writer_max_ns"`
	E2ESamples       int     `json:"e2e_samples"`
	E2EP50NS         float64 `json:"e2e_p50_ns"`
	E2EP90NS         float64 `json:"e2e_p90_ns"`
	E2EP95NS         float64 `json:"e2e_p95_ns"`
	E2EP99NS         float64 `json:"e2e_p99_ns"`
	E2EP9999NS       float64 `json:"e2e_p99_99_ns"`
	E2EMaxNS         float64 `json:"e2e_max_ns"`
	Grows            uint64  `json:"grows"`
	GrowNs           uint64  `json:"grow_ns"`
	Waits            uint64  `json:"waits"`
	WaitNs           uint64  `json:"wait_ns"`
	Populates        uint64  `json:"populates"`
	Dontneed         uint64  `json:"dontneed"`
	Syncs            uint64  `json:"syncs"`
	Evicts           uint64  `json:"evicts"`
	Reaped           uint64  `json:"reaped"`
	Dropped          uint64  `json:"dropped"`
	SyncErrors       uint64  `json:"sync_errors"`
	GrowErrors       uint64  `json:"grow_errors"`
	ReaderEmpty      uint64  `json:"reader_empty"`
	ReaderDontneed   uint64  `json:"reader_dontneed"`
}

type reservoir struct {
	values []float64
	n      uint64
	rng    uint64
}

func newReservoir(seed uint64) *reservoir {
	if seed == 0 {
		seed = 0x9e3779b97f4a7c15
	}
	return &reservoir{
		values: make([]float64, 0, maxSamples),
		rng:    seed,
	}
}

func (r *reservoir) add(v float64) {
	r.n++
	if len(r.values) < cap(r.values) {
		r.values = append(r.values, v)
		return
	}
	x := r.rng
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	r.rng = x
	if j := x % r.n; j < uint64(cap(r.values)) {
		r.values[j] = v
	}
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	return values[int(p*float64(len(values)-1))]
}

func setPercentiles(samples []float64) (p50, p90, p95, p99, p9999, max float64) {
	if len(samples) == 0 {
		return 0, 0, 0, 0, 0, 0
	}
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	return percentile(sorted, 0.50),
		percentile(sorted, 0.90),
		percentile(sorted, 0.95),
		percentile(sorted, 0.99),
		percentile(sorted, 0.9999),
		percentile(sorted, 1)
}

func isRamFS(magic int64) bool {
	switch uint64(magic) {
	case tmpfsMagic, ramfsMagic:
		return true
	default:
		return false
	}
}

func fsName(magic int64) string {
	switch uint64(magic) {
	case tmpfsMagic:
		return "tmpfs"
	case ramfsMagic:
		return "ramfs"
	case ext4Magic:
		return "ext4"
	case xfsMagic:
		return "xfs"
	case btrfsMagic:
		return "btrfs"
	default:
		return fmt.Sprintf("magic=0x%x", uint64(magic))
	}
}

// requireDisk rejects ram filesystems. The bench must pay real device costs.
func requireDisk(path string) (string, error) {
	probe := path
	for {
		var st syscall.Statfs_t
		err := syscall.Statfs(probe, &st)
		if err == nil {
			if isRamFS(st.Type) {
				return "", fmt.Errorf("%s is %s; filewin bench must be disk-backed", probe, fsName(st.Type))
			}
			return fsName(st.Type), nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("statfs %s: %w", probe, err)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", fmt.Errorf("statfs %s: %w", path, err)
		}
		probe = parent
	}
}

func parsePayloads(text string) ([]int, error) {
	fields := strings.Split(text, ",")
	out := make([]int, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		n, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("payload %q: %w", field, err)
		}
		if n < minPayload {
			return nil, fmt.Errorf("payload %d is below the %d-byte timestamp", n, minPayload)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no payloads")
	}
	return out, nil
}

func orderedCPUs(required int) ([]int, bool, error) {
	affinity, err := hostcpu.Affinity(0)
	if err != nil {
		return nil, false, err
	}
	allowed := affinity
	verified := false
	if effective, err := hostcpu.EffectiveCPUSet(); err == nil {
		allowed = affinity.Intersection(effective)
		verified = true
	}
	snapshot, snapErr := hostcpu.ReadSnapshot()
	primary := []int{}
	if snapErr == nil {
		type core struct{ pkg, id int }
		seen := map[core]bool{}
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
	} else {
		verified = false
		primary = append(primary, allowed.CPUs()...)
	}
	if len(primary) > required && primary[0] == 0 {
		primary = primary[1:]
	}
	if len(primary) < required {
		return nil, false, fmt.Errorf("need %d distinct CPUs to pin, found %d", required, len(primary))
	}
	return primary[:required], verified, nil
}

func pin(cpu int) error {
	runtime.LockOSThread()
	set, err := hostcpu.NewCPUSet(cpu)
	if err != nil {
		return err
	}
	if err := hostcpu.SetAffinity(0, set); err != nil {
		return err
	}
	got, err := hostcpu.Affinity(0)
	if err != nil {
		return err
	}
	if !got.Contains(cpu) || got.Count() != 1 {
		return fmt.Errorf("affinity is %v, want only CPU %d", got.CPUs(), cpu)
	}
	return nil
}

var epoch = time.Now()

// useTSC records whether the processor timestamp counter is a usable clock.
// It is read once so the hot loop never repeats the check.
var useTSC = tsc.Available()

// now returns a reading in whatever unit the clock counts.
//
// It returns counter ticks when the timestamp counter is usable, and
// nanoseconds otherwise. Deltas stay in that unit all the way to the report,
// where toNanos converts them once. Converting per sample would put a
// floating point divide inside the measured region.
//
// This matters more than it looks. time.Now costs about forty nanoseconds
// here and the counter costs about eight, and this benchmark reads the clock
// twice per record. With time.Now the instrument was the largest term in the
// throughput figure.
func now() int64 {
	if useTSC {
		return int64(tsc.Read())
	}
	return time.Since(epoch).Nanoseconds()
}

// toNanos converts a delta from now into nanoseconds.
func toNanos(delta float64) float64 {
	if !useTSC || delta < 0 {
		return delta
	}
	return delta / float64(tsc.TicksPerSecond()) * 1e9
}

// clockName reports which clock produced the numbers.
func clockName() string {
	if useTSC {
		return "tsc"
	}
	return "monotonic"
}

func removeLog(path string) {
	link := path + ".head"
	if target, err := os.Readlink(link); err == nil {
		_ = os.Remove(target)
	}
	_ = os.Remove(link)
	_ = os.Remove(path)
}

func run(dir, fstype string, payload int, warmup, seconds float64, readerCPU, writerCPU int) (result, error) {
	path := filepath.Join(dir, fmt.Sprintf("log-%d", payload))
	removeLog(path)
	file, err := filewin.Create(filewin.Config{
		Path:               path,
		Reserve:            32 << 30,
		Extent:             64 << 20,
		Ahead:              4 << 20,
		MaxReserve:         1 << 20,
		HeadInSharedMemory: true,
	})
	if err != nil {
		return result{}, err
	}
	defer func() {
		_ = file.Close()
		removeLog(path)
	}()
	writer, err := file.AttachWriter()
	if err != nil {
		return result{}, err
	}
	defer writer.Close()
	reader, err := file.AttachReader()
	if err != nil {
		return result{}, err
	}
	defer reader.Close()

	var phase atomic.Uint32
	var ready atomic.Int32
	var writerDone atomic.Bool
	var runErr atomic.Value
	writerLat := newReservoir(0x9e3779b97f4a7c15)
	e2eLat := newReservoir(0x6a09e667f3bcc909)
	var writerRecs atomic.Uint64
	var readerRecs atomic.Uint64
	report := func(err error) {
		if err != nil {
			runErr.CompareAndSwap(nil, err)
			phase.Store(phaseStop)
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer writerDone.Store(true)
		if err := pin(writerCPU); err != nil {
			report(err)
			ready.Add(1)
			return
		}
		identity := hostcpu.CurrentThreadIdentity()
		template := make([]byte, payload)
		for i := range template {
			template[i] = 0xA5
		}
		ready.Add(1)
		for phase.Load() == phaseWait {
			runtime.Gosched()
		}
		n := uint64(payload)
		for {
			p := phase.Load()
			if p == phaseStop {
				break
			}
			t0 := now()
			span, err := writer.Reserve(n)
			if err != nil {
				report(err)
				break
			}
			copy(span, template)
			binary.LittleEndian.PutUint64(span, uint64(t0))
			if err := writer.Commit(n); err != nil {
				report(err)
				break
			}
			if p == phaseMeasure {
				writerLat.add(float64(now() - t0))
				writerRecs.Add(1)
			}
		}
		if hostcpu.CurrentThreadIdentity() != identity {
			report(fmt.Errorf("writer moved off pinned OS thread"))
		}
	}()
	go func() {
		defer wg.Done()
		if err := pin(readerCPU); err != nil {
			report(err)
			ready.Add(1)
			return
		}
		identity := hostcpu.CurrentThreadIdentity()
		ready.Add(1)
		for phase.Load() == phaseWait {
			runtime.Gosched()
		}
		for {
			p := phase.Load()
			span, err := reader.Peek(0)
			if err != nil {
				report(err)
				break
			}
			if len(span) < payload {
				if writerDone.Load() && reader.Pos() == file.WritePos() {
					break
				}
				continue
			}
			if len(span)%payload != 0 {
				usable := (len(span) / payload) * payload
				span = span[:usable]
			}
			sample := now()
			count := len(span) / payload
			if p == phaseMeasure {
				for i := 0; i < count; i++ {
					ts := int64(binary.LittleEndian.Uint64(span[i*payload:]))
					e2eLat.add(float64(sample - ts))
				}
				readerRecs.Add(uint64(count))
			}
			if err := reader.Advance(uint64(count * payload)); err != nil {
				report(err)
				break
			}
			if p == phaseStop && writerDone.Load() && reader.Pos() == file.WritePos() {
				break
			}
		}
		if hostcpu.CurrentThreadIdentity() != identity {
			report(fmt.Errorf("reader moved off pinned OS thread"))
		}
	}()

	for ready.Load() < 2 {
		if runErr.Load() != nil {
			phase.Store(phaseStop)
			wg.Wait()
			if v := runErr.Load(); v != nil {
				return result{}, v.(error)
			}
		}
		runtime.Gosched()
	}
	phase.Store(phaseWarmup)
	time.Sleep(time.Duration(warmup * float64(time.Second)))
	if runErr.Load() != nil {
		phase.Store(phaseStop)
		wg.Wait()
		return result{}, runErr.Load().(error)
	}
	phase.Store(phaseMeasure)
	start := time.Now()
	time.Sleep(time.Duration(seconds * float64(time.Second)))
	phase.Store(phaseStop)
	elapsed := time.Since(start).Seconds()
	wg.Wait()
	if v := runErr.Load(); v != nil {
		return result{}, v.(error)
	}

	wRec := writerRecs.Load()
	rRec := readerRecs.Load()
	for i, v := range writerLat.values {
		writerLat.values[i] = toNanos(v)
	}
	for i, v := range e2eLat.values {
		e2eLat.values[i] = toNanos(v)
	}
	wp50, wp90, wp95, wp99, wp9999, wmax := setPercentiles(writerLat.values)
	ep50, ep90, ep95, ep99, ep9999, emax := setPercentiles(e2eLat.values)
	return result{
		PayloadBytes:     payload,
		Seconds:          elapsed,
		FSType:           fstype,
		Dir:              dir,
		WriterCPU:        writerCPU,
		ReaderCPU:        readerCPU,
		TopologyVerified: false,
		WriterRecords:    wRec,
		ReaderRecords:    rRec,
		WriterMrecS:      float64(wRec) / elapsed / 1e6,
		ReaderMrecS:      float64(rRec) / elapsed / 1e6,
		WriterGBS:        float64(wRec) / elapsed * float64(payload) / 1e9,
		ReaderGBS:        float64(rRec) / elapsed * float64(payload) / 1e9,
		FileBytes:        file.WritePos(),
		WriterSamples:    len(writerLat.values),
		WriterP50NS:      wp50,
		WriterP90NS:      wp90,
		WriterP95NS:      wp95,
		WriterP99NS:      wp99,
		WriterP9999NS:    wp9999,
		WriterMaxNS:      wmax,
		E2ESamples:       len(e2eLat.values),
		E2EP50NS:         ep50,
		E2EP90NS:         ep90,
		E2EP95NS:         ep95,
		E2EP99NS:         ep99,
		E2EP9999NS:       ep9999,
		E2EMaxNS:         emax,
		Grows:            writer.Grows.Load(),
		GrowNs:           writer.GrowNs.Load(),
		Waits:            writer.Waits.Load(),
		WaitNs:           writer.WaitNs.Load(),
		Populates:        writer.Populates.Load(),
		Dontneed:         writer.Dontneed.Load(),
		Syncs:            writer.Syncs.Load(),
		Evicts:           writer.Evicts.Load(),
		Reaped:           writer.Reaped.Load(),
		Dropped:          writer.Dropped.Load(),
		SyncErrors:       writer.SyncErrors.Load(),
		GrowErrors:       writer.GrowErrors.Load(),
		ReaderEmpty:      reader.Empty.Load(),
		ReaderDontneed:   reader.Dontneed.Load(),
	}, nil
}

func printResult(row result) {
	fmt.Printf("filewin disk-backed fstype=%s dir=%s payload=%d writer_cpu=%d reader_cpu=%d topology_verified=%t\n",
		row.FSType, row.Dir, row.PayloadBytes, row.WriterCPU, row.ReaderCPU, row.TopologyVerified)
	fmt.Printf("  seconds=%.3f writer=%.3f Mrec/s %.3f GB/s reader=%.3f Mrec/s %.3f GB/s file=%d B\n",
		row.Seconds, row.WriterMrecS, row.WriterGBS, row.ReaderMrecS, row.ReaderGBS, row.FileBytes)
	fmt.Printf("  clock=%s ticks_per_second=%d\n", clockName(), tsc.TicksPerSecond())
	fmt.Printf("  writer_ns  p50=%.0f p90=%.0f p95=%.0f p99=%.0f p99.99=%.0f max=%.0f samples=%d\n",
		row.WriterP50NS, row.WriterP90NS, row.WriterP95NS, row.WriterP99NS, row.WriterP9999NS, row.WriterMaxNS, row.WriterSamples)
	fmt.Printf("  e2e_ns     p50=%.0f p90=%.0f p95=%.0f p99=%.0f p99.99=%.0f max=%.0f samples=%d\n",
		row.E2EP50NS, row.E2EP90NS, row.E2EP95NS, row.E2EP99NS, row.E2EP9999NS, row.E2EMaxNS, row.E2ESamples)
	fmt.Printf("  grows=%d grow_ns=%d waits=%d wait_ns=%d populates=%d dontneed=%d syncs=%d evicts=%d\n",
		row.Grows, row.GrowNs, row.Waits, row.WaitNs, row.Populates, row.Dontneed, row.Syncs, row.Evicts)
	fmt.Printf("  reaped=%d dropped=%d sync_errors=%d grow_errors=%d reader_empty=%d reader_dontneed=%d\n",
		row.Reaped, row.Dropped, row.SyncErrors, row.GrowErrors, row.ReaderEmpty, row.ReaderDontneed)
}

func main() {
	seconds := flag.Float64("seconds", 5, "measurement seconds")
	warmup := flag.Float64("warmup", 1, "warmup seconds")
	payloadsFlag := flag.String("payload", "64,256,4096", "comma-separated record sizes in bytes")
	dir := flag.String("dir", "build/filewin-bench", "directory for the disk-backed log")
	jsonOut := flag.Bool("json", false, "write one JSON object per payload to stdout")
	flag.Parse()
	if *seconds <= 0 || *warmup < 0 {
		fmt.Fprintln(os.Stderr, "seconds must be positive and warmup must be nonnegative")
		os.Exit(2)
	}
	payloads, err := parsePayloads(*payloadsFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fstype, err := requireDisk(abs)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	cpus, verified, err := orderedCPUs(2)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pin failed:", err)
		os.Exit(2)
	}
	if !verified {
		fmt.Fprintln(os.Stderr, "pin warning: cgroup effective cpuset was unavailable; pinning from affinity and topology")
	}
	readerCPU, writerCPU := cpus[0], cpus[1]
	for _, payload := range payloads {
		row, err := run(abs, fstype, payload, *warmup, *seconds, readerCPU, writerCPU)
		if err != nil {
			fmt.Fprintln(os.Stderr, "benchmark failed:", err)
			os.Exit(2)
		}
		row.TopologyVerified = verified
		if *jsonOut {
			if err := json.NewEncoder(os.Stdout).Encode(row); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			continue
		}
		printResult(row)
	}
}
