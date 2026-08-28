package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	benchschema "github.com/akalsi-org/polyglot-template/go/bench/mpsc/schema"
	"github.com/akalsi-org/polyglot-template/go/lib/hostcpu"
)

const schemaVersion = benchschema.Version

type options struct {
	benchmark       string
	seconds         float64
	runs            int
	payload         int
	seed            int64
	variants        []string
	writers         []int
	counters        bool
	cpus            string
	build           string
	compiler        string
	cxxflags        string
	allowUnverified bool
	consumerModes   []string
	telemetryPath   string
	telemetryCPU    int
	telemetryOutput io.Writer
}

type config struct {
	variant      string
	writers      int
	consumerMode string
}

func campaignConfigs(opts options) ([]config, error) {
	configs := make([]config, 0, len(opts.variants)*len(opts.writers)*len(opts.consumerModes))
	for _, variant := range opts.variants {
		for _, writers := range opts.writers {
			for _, consumerMode := range opts.consumerModes {
				if variant != "spsc" || writers == 1 {
					configs = append(configs, config{variant, writers, consumerMode})
				}
			}
		}
	}
	if len(configs) == 0 {
		return nil, errors.New("campaign has no runnable configurations")
	}
	return configs, nil
}

type benchmarkResult = benchschema.BenchmarkRow
type campaignRow = benchschema.CampaignRow
type metadataRow = benchschema.MetadataRow

func parseCSV(text string) []string {
	parts := strings.Split(text, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func parseWriters(text string) ([]int, error) {
	parts := parseCSV(text)
	out := make([]int, 0, len(parts))
	seen := make(map[int]struct{}, len(parts))
	for _, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value <= 0 {
			return nil, fmt.Errorf("invalid writer count %q", part)
		}
		if _, ok := seen[value]; ok {
			return nil, fmt.Errorf("writer count %d repeats", value)
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out, nil
}

func parseOptions(args []string) (options, error) {
	var opts options
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		opts.benchmark = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("run_campaign", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	seconds := fs.Float64("seconds", 2, "timed seconds per process")
	runs := fs.Int("runs", 11, "interleaved process runs per configuration")
	payload := fs.Int("payload", 56, "payload bytes per record")
	seed := fs.Int64("seed", 20260726, "shuffle seed")
	variants := fs.String("variants", "mpsc,mpsc-padded,mpsc-padded-256,spsc", "comma-separated variants")
	writers := fs.String("writers", "1,2,4,8", "comma-separated writer counts")
	counters := fs.Bool("counters", false, "run one counter process per configuration")
	cpus := fs.String("benchmark-cpus", "", "CPU list passed to taskset")
	build := fs.String("build-command", "", "benchmark build command metadata")
	compiler := fs.String("compiler", "", "compiler identity metadata")
	cxxflags := fs.String("cxxflags", "", "compiler flags metadata")
	allowUnverified := fs.Bool("allow-unverified-topology", false, "permit benchmark placement without proven physical-core separation")
	consumerModes := fs.String("consumer-modes", "metadata-only", "comma-separated consumer modes: metadata-only,payload-touching")
	telemetryPath := fs.String("telemetry", "", "write load and pressure telemetry as JSONL")
	telemetryCPU := fs.Int("telemetry-cpu", -1, "CPU reserved for the telemetry sampler")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if opts.benchmark == "" && fs.NArg() > 0 {
		opts.benchmark = fs.Arg(0)
	}
	if opts.benchmark == "" || fs.NArg() > 1 {
		return opts, errors.New("usage: run_campaign benchmark [flags]")
	}
	opts.seconds, opts.runs, opts.payload, opts.seed = *seconds, *runs, *payload, *seed
	opts.variants, opts.counters, opts.cpus = parseCSV(*variants), *counters, *cpus
	opts.build, opts.compiler, opts.cxxflags = *build, *compiler, *cxxflags
	opts.allowUnverified = *allowUnverified
	opts.consumerModes = parseCSV(*consumerModes)
	opts.telemetryPath, opts.telemetryCPU = *telemetryPath, *telemetryCPU
	seenConsumerModes := make(map[string]struct{}, len(opts.consumerModes))
	for _, mode := range opts.consumerModes {
		if mode != "metadata-only" && mode != "payload-touching" {
			return opts, fmt.Errorf("invalid consumer mode %q", mode)
		}
		if _, ok := seenConsumerModes[mode]; ok {
			return opts, fmt.Errorf("consumer mode %q repeats", mode)
		}
		seenConsumerModes[mode] = struct{}{}
	}
	var err error
	opts.writers, err = parseWriters(*writers)
	if err != nil {
		return opts, err
	}
	if opts.seconds <= 0 || math.IsNaN(opts.seconds) || math.IsInf(opts.seconds, 0) || opts.runs <= 0 || opts.payload < 0 || opts.payload > 65536 {
		return opts, errors.New("--seconds must be finite and positive; --runs must be positive; --payload must be in [0, 65536]")
	}
	if len(opts.variants) == 0 || len(opts.writers) == 0 || len(opts.consumerModes) == 0 {
		return opts, errors.New("--variants, --writers, and --consumer-modes must not be empty")
	}
	if (opts.telemetryPath == "") != (opts.telemetryCPU < 0) {
		return opts, errors.New("--telemetry and --telemetry-cpu must be specified together")
	}
	if opts.telemetryPath != "" && opts.cpus == "" {
		return opts, errors.New("--benchmark-cpus is required when telemetry is enabled")
	}
	valid := map[string]bool{"mpsc": true, "mpsc-padded": true, "mpsc-padded-256": true, "spsc": true}
	seen := make(map[string]struct{}, len(opts.variants))
	for _, variant := range opts.variants {
		if !valid[variant] {
			return opts, fmt.Errorf("invalid variant %q", variant)
		}
		if _, ok := seen[variant]; ok {
			return opts, fmt.Errorf("variant %q repeats", variant)
		}
		seen[variant] = struct{}{}
	}
	return opts, nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func validateBenchmarkResult(result benchmarkResult) error {
	if result.SchemaVersion != schemaVersion {
		return fmt.Errorf("unsupported benchmark schema version %d", result.SchemaVersion)
	}
	if result.ImplementationVariant == "" || result.RetryPolicy == "" {
		return errors.New("benchmark omitted implementation or retry policy provenance")
	}
	if !result.TopologyVerified && !result.TopologyOverride {
		return errors.New("benchmark omitted verified topology or an explicit override")
	}
	if result.ReaderCPU < 0 || len(result.WriterCPUs) != result.Writers {
		return errors.New("benchmark returned invalid reader or writer CPU provenance")
	}
	seenCPUs := map[int]struct{}{result.ReaderCPU: {}}
	for _, cpu := range result.WriterCPUs {
		if cpu < 0 {
			return errors.New("benchmark returned invalid reader or writer CPU provenance")
		}
		if _, exists := seenCPUs[cpu]; exists {
			return errors.New("benchmark returned overlapping reader or writer CPUs")
		}
		seenCPUs[cpu] = struct{}{}
	}
	if result.Seconds <= 0 || result.Records == 0 || result.ExtentBytes <= 0 || !finite(result.MrecS) || !finite(result.PayloadGBS) || !finite(result.ReservedGBS) || !finite(result.AllocationsPerRecord) {
		return errors.New("benchmark returned invalid records, extent, bandwidth, or allocation measurement")
	}
	if result.Mode == "paced" {
		if result.DelaySamples <= 0 || !finite(result.OfferedPerWriterMrecS) || !finite(result.OfferedMrecS) || !finite(result.AchievedRatio) || !finite(result.DelayP50NS) || !finite(result.DelayP99NS) || !finite(result.DelayMaxNS) || !finite(result.LateFraction) {
			return errors.New("paced benchmark returned invalid offered-load or delay measurement")
		}
	}
	if result.Mode == "latency" {
		if result.LatencySamples <= 0 || result.AdmissionSamples < 0 || !finite(result.LatencyP50NS) || !finite(result.LatencyP95NS) || !finite(result.LatencyP99NS) || !finite(result.LatencyP9999NS) || !finite(result.LatencyMaxNS) || !finite(result.AdmissionP50NS) || !finite(result.AdmissionP95NS) || !finite(result.AdmissionP99NS) || !finite(result.AdmissionP9999NS) || !finite(result.AdmissionMaxNS) {
			return errors.New("latency benchmark returned invalid write-cost or admission-delay measurement")
		}
	}
	if result.Mode == "counters" {
		if !result.CountersAvailable {
			return errors.New("counter benchmark reported counters unavailable")
		}
		if result.CyclesPerRecord <= 0 || result.InsnsPerRecord <= 0 || !finite(result.MissesPerRecord) || !finite(result.CyclesPerRecord) || !finite(result.InsnsPerRecord) || !finite(result.IPC) {
			return errors.New("counter benchmark returned invalid available counter fields")
		}
	}
	return nil
}

type telemetryRow struct {
	SchemaVersion  int     `json:"schema_version"`
	Mode           string  `json:"mode"`
	Phase          string  `json:"phase"`
	BenchmarkMode  string  `json:"benchmark_mode"`
	Variant        string  `json:"variant"`
	Writers        int     `json:"writers"`
	ConsumerMode   string  `json:"consumer_mode"`
	Repetition     int     `json:"repetition"`
	Ordinal        int     `json:"ordinal"`
	Timestamp      string  `json:"timestamp"`
	ElapsedSeconds float64 `json:"elapsed_seconds"`
	LoadAverage    string  `json:"load_average"`
	CPUStat        string  `json:"cpu_stat"`
	CPUPressure    string  `json:"cpu_pressure"`
	MemoryPressure string  `json:"memory_pressure"`
	IOPressure     string  `json:"io_pressure"`
}

func readTrimmed(path string) string {
	data, _ := os.ReadFile(path)
	return strings.TrimSpace(string(data))
}

func telemetrySample(start time.Time, phase, mode string, item config, repetition, ordinal int) telemetryRow {
	return telemetryRow{
		SchemaVersion: schemaVersion, Mode: "telemetry", Phase: phase, BenchmarkMode: mode,
		Variant: item.variant, Writers: item.writers, ConsumerMode: item.consumerMode,
		Repetition: repetition, Ordinal: ordinal, Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		ElapsedSeconds: time.Since(start).Seconds(), LoadAverage: readTrimmed("/proc/loadavg"),
		CPUStat: readTrimmed("/proc/stat"), CPUPressure: readTrimmed("/proc/pressure/cpu"),
		MemoryPressure: readTrimmed("/proc/pressure/memory"), IOPressure: readTrimmed("/proc/pressure/io"),
	}
}

func sampleWhileRunning(output io.Writer, cpu int, start time.Time, mode string, item config, repetition, ordinal int, stop <-chan struct{}) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	set, err := hostcpu.NewCPUSet(cpu)
	if err != nil {
		return err
	}
	if err := hostcpu.SetAffinity(0, set); err != nil {
		return fmt.Errorf("pin telemetry sampler: %w", err)
	}
	if err := emitJSON(output, telemetrySample(start, "start", mode, item, repetition, ordinal)); err != nil {
		return err
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := emitJSON(output, telemetrySample(start, "running", mode, item, repetition, ordinal)); err != nil {
				return err
			}
		case <-stop:
			return emitJSON(output, telemetrySample(start, "stop", mode, item, repetition, ordinal))
		}
	}
}

func invoke(opts options, mode string, item config, repetition, ordinal int) (benchmarkResult, error) {
	args := []string{mode, item.variant, strconv.Itoa(item.writers), strconv.FormatFloat(opts.seconds, 'g', -1, 64), strconv.Itoa(opts.payload), "--consumer-mode=" + item.consumerMode}
	if opts.allowUnverified {
		args = append(args, "--allow-unverified-topology")
	}
	command := exec.Command(opts.benchmark, args...)
	if opts.cpus != "" {
		command = exec.Command("taskset", append([]string{"-c", opts.cpus, opts.benchmark}, args...)...)
	}
	var output []byte
	var stderr string
	var err error
	if opts.telemetryOutput == nil {
		output, err = command.Output()
		if exit, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(exit.Stderr))
		}
	} else {
		var stdoutBuffer, stderrBuffer bytes.Buffer
		command.Stdout, command.Stderr = &stdoutBuffer, &stderrBuffer
		if err = command.Start(); err == nil {
			stop := make(chan struct{})
			samplerDone := make(chan error, 1)
			start := time.Now()
			go func() {
				samplerDone <- sampleWhileRunning(opts.telemetryOutput, opts.telemetryCPU, start, mode, item, repetition, ordinal, stop)
			}()
			err = command.Wait()
			close(stop)
			if samplerErr := <-samplerDone; samplerErr != nil && err == nil {
				err = samplerErr
			}
		}
		output, stderr = stdoutBuffer.Bytes(), strings.TrimSpace(stderrBuffer.String())
	}
	if err != nil {
		if stderr != "" {
			return benchmarkResult{}, fmt.Errorf("%s %s w=%d failed: %s", mode, item.variant, item.writers, stderr)
		}
		return benchmarkResult{}, err
	}
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	var lines [][]byte
	for scanner.Scan() {
		if len(scanner.Bytes()) > 0 {
			lines = append(lines, append([]byte(nil), scanner.Bytes()...))
		}
	}
	if len(lines) != 1 {
		return benchmarkResult{}, fmt.Errorf("expected one JSON line, got %d", len(lines))
	}
	var result benchmarkResult
	if err := json.Unmarshal(lines[0], &result); err != nil {
		return result, fmt.Errorf("decode benchmark JSON: %w", err)
	}
	if result.Mode != mode || result.Variant != item.variant || result.Writers != item.writers || result.PayloadBytes != opts.payload || result.ConsumerMode != item.consumerMode || result.TopologyOverride != opts.allowUnverified {
		return result, fmt.Errorf("benchmark returned wrong selector: %s", lines[0])
	}
	if err := validateBenchmarkResult(result); err != nil {
		return result, fmt.Errorf("benchmark returned invalid measurement: %w: %s", err, lines[0])
	}
	return result, nil
}

func emitJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

var readCampaignSnapshot = hostcpu.ReadSnapshot

func campaignTopology(currentEffective hostcpu.CPUSet, allowUnverified bool) ([]benchschema.CPUTopology, error) {
	snapshot, err := readCampaignSnapshot()
	if err != nil {
		if allowUnverified {
			return nil, nil
		}
		return nil, fmt.Errorf("read CPU topology: %w", err)
	}
	topology := make([]benchschema.CPUTopology, 0, len(snapshot.CPUs))
	for _, cpu := range snapshot.CPUs {
		if currentEffective.Contains(cpu.Id) {
			topology = append(topology, benchschema.CPUTopology{CPU: cpu.Id, PackageID: cpu.PackageId, CoreID: cpu.CoreId, NodeID: cpu.NodeId, ThreadSiblings: cpu.Siblings.CPUs()})
		}
	}
	return topology, nil
}

func run(opts options, output io.Writer) error {
	configs, err := campaignConfigs(opts)
	if err != nil {
		return err
	}
	binary, err := filepathAbs(opts.benchmark)
	if err != nil {
		return err
	}
	opts.benchmark = binary
	hash, err := fileSHA256(binary)
	if err != nil {
		return fmt.Errorf("hash benchmark: %w", err)
	}
	cpuSet, err := hostcpu.ParseCPUSet(opts.cpus)
	if err != nil {
		return err
	}
	cpus := cpuSet.CPUs()
	affinity, err := hostcpu.Affinity(0)
	if err != nil {
		return fmt.Errorf("read current affinity: %w", err)
	}
	effective, err := hostcpu.EffectiveCPUSet()
	if err != nil {
		return fmt.Errorf("read effective CPU set: %w", err)
	}
	currentEffective := affinity.Intersection(effective)
	if !cpuSet.IsSubsetOf(currentEffective) {
		return fmt.Errorf("benchmark CPUs %q are not a subset of current effective affinity %q", cpuSet.String(), currentEffective.String())
	}
	var telemetrySet hostcpu.CPUSet
	if opts.telemetryPath != "" {
		telemetrySet, err = hostcpu.NewCPUSet(opts.telemetryCPU)
		if err != nil {
			return err
		}
		if !telemetrySet.IsSubsetOf(currentEffective) {
			return fmt.Errorf("telemetry CPU %d is not in current effective affinity %q", opts.telemetryCPU, currentEffective.String())
		}
		if !telemetrySet.Intersection(cpuSet).Empty() {
			return fmt.Errorf("telemetry CPU %d overlaps benchmark CPUs %q", opts.telemetryCPU, cpuSet.String())
		}
	}
	host, _ := os.Hostname()
	kernelBytes, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	kernel := strings.TrimSpace(string(kernelBytes))
	topology, err := campaignTopology(currentEffective, opts.allowUnverified)
	if err != nil {
		return err
	}
	readProc := func(path string) string {
		data, _ := os.ReadFile(path)
		return strings.TrimSpace(string(data))
	}
	if opts.telemetryPath != "" {
		if err := os.MkdirAll(filepath.Dir(opts.telemetryPath), 0o755); err != nil {
			return fmt.Errorf("create telemetry output directory: %w", err)
		}
		telemetryFile, err := os.Create(opts.telemetryPath)
		if err != nil {
			return fmt.Errorf("create telemetry output: %w", err)
		}
		defer telemetryFile.Close()
		opts.telemetryOutput = telemetryFile
	}
	metadata := metadataRow{
		SchemaVersion: schemaVersion, Mode: "campaign_metadata", Binary: binary, BinarySHA256: hash,
		Go: runtime.Version(), Kernel: kernel, Host: host, BenchmarkCPUs: cpus,
		AffinityCPUs: affinity.CPUs(), EffectiveCPUs: currentEffective.CPUs(), TelemetryCPUs: telemetrySet.CPUs(),
		SamplerIsolated: opts.telemetryPath != "", Topology: topology, LoadAverage: readProc("/proc/loadavg"),
		CPUPressure: readProc("/proc/pressure/cpu"), MemoryPressure: readProc("/proc/pressure/memory"), IOPressure: readProc("/proc/pressure/io"),
		RequestedSecs: opts.seconds, PayloadBytes: opts.payload, Runs: opts.runs, Seed: opts.seed,
		Variants: opts.variants, WriterCounts: opts.writers, BuildCommand: opts.build, Compiler: opts.compiler,
		CXXFlags: opts.cxxflags, AllowUnverifiedTopology: opts.allowUnverified, ConsumerModes: opts.consumerModes,
		Counters: opts.counters, Telemetry: opts.telemetryPath != "",
	}
	if err := emitJSON(output, metadata); err != nil {
		return err
	}
	if opts.telemetryOutput != nil {
		if err := emitJSON(opts.telemetryOutput, metadata); err != nil {
			return fmt.Errorf("write telemetry metadata: %w", err)
		}
	}
	rng := rand.New(rand.NewSource(opts.seed))
	emitRun := func(mode string, item config, repetition, ordinal int) error {
		result, err := invoke(opts, mode, item, repetition, ordinal)
		if err != nil {
			return err
		}
		row := campaignRow{
			BenchmarkRow: result, Repetition: repetition, Ordinal: ordinal, Seed: opts.seed,
			BinarySHA256: hash, Binary: binary, BenchmarkCPUs: cpus, Kernel: kernel, Host: host,
			RequestedSecs: opts.seconds, BuildCommand: opts.build, Compiler: opts.compiler, CXXFlags: opts.cxxflags,
		}
		if row.SchemaVersion == 0 {
			row.SchemaVersion = schemaVersion
		}
		return emitJSON(output, row)
	}
	for repetition := 1; repetition <= opts.runs; repetition++ {
		order := append([]config(nil), configs...)
		rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		for ordinal, item := range order {
			if err := emitRun("throughput", item, repetition, ordinal+1); err != nil {
				return err
			}
		}
	}
	if opts.counters {
		order := append([]config(nil), configs...)
		rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		for ordinal, item := range order {
			if err := emitRun("counters", item, 1, ordinal+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func filepathAbs(path string) (string, error) {
	if !strings.ContainsRune(path, os.PathSeparator) {
		resolved, err := exec.LookPath(path)
		if err != nil {
			return "", err
		}
		path = resolved
	}
	return filepath.Abs(path)
}

func main() {
	opts, err := parseOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := run(opts, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "campaign failed:", err)
		os.Exit(2)
	}
}
