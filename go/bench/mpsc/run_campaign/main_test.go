package main

import (
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/akalsi-org/polyglot-template/go/lib/hostcpu"
)

func TestParseOptionsAcceptsLegacyBenchmarkPosition(t *testing.T) {
	opts, err := parseOptions([]string{"./bench_queue", "--runs", "3", "--variants", "mpsc,spsc", "--writers", "1,4"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.benchmark != "./bench_queue" || opts.runs != 3 {
		t.Fatalf("options = %+v", opts)
	}
	if !reflect.DeepEqual(opts.variants, []string{"mpsc", "spsc"}) || !reflect.DeepEqual(opts.writers, []int{1, 4}) {
		t.Fatalf("options = %+v", opts)
	}
}

func TestParseOptionsConsumerModes(t *testing.T) {
	opts, err := parseOptions([]string{"./bench_queue", "--consumer-modes", "metadata-only,payload-touching"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"metadata-only", "payload-touching"}; !reflect.DeepEqual(opts.consumerModes, want) {
		t.Fatalf("consumer modes = %v, want %v", opts.consumerModes, want)
	}
	if _, err := parseOptions([]string{"./bench_queue", "--consumer-modes", "metadata-only,metadata-only"}); err == nil {
		t.Fatal("parseOptions accepted a repeated consumer mode")
	}
}

func TestBenchmarkCPUParserUsesHostCPUParity(t *testing.T) {
	set, err := hostcpu.ParseCPUSet("4,1-2,2")
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{1, 2, 4}; !reflect.DeepEqual(set.CPUs(), want) {
		t.Fatalf("CPUs = %v, want %v", set.CPUs(), want)
	}
	for _, invalid := range []string{"1,,2", "3-1", "-1", "1-2-3"} {
		if _, err := hostcpu.ParseCPUSet(invalid); err == nil {
			t.Errorf("ParseCPUSet accepted %q", invalid)
		}
	}
}

func TestParseOptionsRejectsRepeatedVariant(t *testing.T) {
	_, err := parseOptions([]string{"./bench_queue", "--variants", "mpsc,mpsc"})
	if err == nil {
		t.Fatal("parseOptions accepted a repeated variant")
	}
}

func TestCampaignConfigsRejectsEmptySPSCCrossProduct(t *testing.T) {
	_, err := campaignConfigs(options{
		variants: []string{"spsc"}, writers: []int{2}, consumerModes: []string{"metadata-only"},
	})
	if err == nil {
		t.Fatal("campaignConfigs accepted a campaign with no runnable configurations")
	}
}

func validResult(mode string) benchmarkResult {
	return benchmarkResult{
		SchemaVersion: schemaVersion, Mode: mode, Variant: "mpsc", ImplementationVariant: "go-mpsc-v4-compact",
		Writers: 1, Seconds: 1, Records: 10, MrecS: 1, PayloadBytes: 56, ExtentBytes: 64,
		PayloadGBS: 1, ReservedGBS: 1, RetryPolicy: "policy", AllocationsPerRecord: 0,
		TopologyVerified: true, ReaderCPU: 0, WriterCPUs: []int{1}, ConsumerMode: "metadata-only",
	}
}

func TestValidateBenchmarkResultRequiresTopologyProvenance(t *testing.T) {
	result := validResult("throughput")
	result.TopologyVerified = false
	if err := validateBenchmarkResult(result); err == nil {
		t.Fatal("validation accepted unverified topology without an override")
	}
	result.TopologyOverride = true
	if err := validateBenchmarkResult(result); err != nil {
		t.Fatal(err)
	}
	result.WriterCPUs = []int{result.ReaderCPU}
	if err := validateBenchmarkResult(result); err == nil {
		t.Fatal("validation accepted overlapping reader and writer CPUs")
	}
	result.WriterCPUs = nil
	if err := validateBenchmarkResult(result); err == nil {
		t.Fatal("validation accepted missing writer CPU provenance")
	}
}

func TestValidateBenchmarkResultRequiresAvailableCounters(t *testing.T) {
	result := validResult("counters")
	if err := validateBenchmarkResult(result); err == nil {
		t.Fatal("validation accepted unavailable counters")
	}
	result.CountersAvailable = true
	result.MissesPerRecord = 0.1
	result.CyclesPerRecord = 20
	result.InsnsPerRecord = 30
	result.IPC = 1.5
	if err := validateBenchmarkResult(result); err != nil {
		t.Fatal(err)
	}
}

func TestValidateBenchmarkResultAllowsNoAdmissionRetries(t *testing.T) {
	result := validResult("latency")
	result.LatencySamples = 1
	result.LatencyP50NS, result.LatencyP95NS, result.LatencyP99NS, result.LatencyP9999NS, result.LatencyMaxNS = 1, 1, 1, 1, 1
	if err := validateBenchmarkResult(result); err != nil {
		t.Fatal(err)
	}
}

func TestValidateBenchmarkResultRejectsNonfiniteEvidence(t *testing.T) {
	result := validResult("throughput")
	result.ReservedGBS = math.Inf(1)
	if err := validateBenchmarkResult(result); err == nil {
		t.Fatal("validation accepted infinite bandwidth")
	}
	result = validResult("latency")
	result.LatencySamples, result.AdmissionSamples = 1, 1
	result.LatencyP50NS, result.LatencyP95NS, result.LatencyP99NS, result.LatencyP9999NS, result.LatencyMaxNS = 1, 1, 1, 1, 1
	result.AdmissionP50NS, result.AdmissionP95NS, result.AdmissionP99NS, result.AdmissionP9999NS, result.AdmissionMaxNS = 1, 1, math.NaN(), 1, 1
	if err := validateBenchmarkResult(result); err == nil {
		t.Fatal("validation accepted nonfinite admission latency")
	}
}

func TestParseOptionsRequiresIsolatedTelemetryFlags(t *testing.T) {
	if _, err := parseOptions([]string{"./bench_queue", "--telemetry", "telemetry.jsonl"}); err == nil {
		t.Fatal("parseOptions accepted telemetry without a sampler CPU")
	}
	if _, err := parseOptions([]string{"./bench_queue", "--telemetry", "telemetry.jsonl", "--telemetry-cpu", "3"}); err == nil {
		t.Fatal("parseOptions accepted telemetry without benchmark CPUs")
	}
}

func TestCampaignTopologyHonorsUnverifiedOverride(t *testing.T) {
	original := readCampaignSnapshot
	defer func() { readCampaignSnapshot = original }()
	readCampaignSnapshot = func() (hostcpu.Snapshot, error) {
		return hostcpu.Snapshot{}, errors.New("topology unavailable")
	}
	if _, err := campaignTopology(hostcpu.CPUSet{}, false); err == nil {
		t.Fatal("campaignTopology accepted missing topology without the override")
	}
	topology, err := campaignTopology(hostcpu.CPUSet{}, true)
	if err != nil || topology != nil {
		t.Fatalf("override topology = %v, %v", topology, err)
	}
}
