package schema

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestBenchmarkRowsRoundTripAllModes(t *testing.T) {
	base := BenchmarkRow{
		SchemaVersion: Version, Mode: "throughput", Variant: "mpsc", ImplementationVariant: "go-mpsc-v4-compact",
		Writers: 2, Seconds: 1.25, Records: 100, MrecS: 8, PayloadBytes: 56, ExtentBytes: 64,
		PayloadGBS: 0.448, ReservedGBS: 0.512, CountersAvailable: false, FullRetries: 3,
		ContentionRetries: 4, ContentionYields: 1, RetryYields: 2, RetryPolicy: "spin-wait-safety-yield-4096",
		Allocations: 1, AllocationsPerRecord: 0.01, TopologyVerified: true, ReaderCPU: 1,
		WriterCPUs: []int{2, 3}, ConsumerMode: "metadata-only",
	}
	rows := []BenchmarkRow{
		base,
		func() BenchmarkRow {
			row := base
			row.Mode = "paced"
			row.OfferedPerWriterMrecS = 2
			row.OfferedMrecS = 4
			row.AchievedRatio = 0.98
			row.DelayP50NS = 20
			row.DelayP99NS = 80
			row.DelayMaxNS = 100
			row.DelaySamples = 50
			row.LateFraction = 0.1
			return row
		}(),
		func() BenchmarkRow {
			row := base
			row.Mode = "latency"
			row.LatencySamples = 10
			row.LatencyP50NS = 5
			row.LatencyP95NS = 8
			row.LatencyP99NS = 9
			row.LatencyP9999NS = 10
			row.LatencyMaxNS = 11
			row.AdmissionSamples = 10
			row.AdmissionP50NS = 6
			row.AdmissionP95NS = 10
			row.AdmissionP99NS = 12
			row.AdmissionP9999NS = 13
			row.AdmissionMaxNS = 14
			return row
		}(),
		func() BenchmarkRow {
			row := base
			row.Mode = "counters"
			row.CountersAvailable = true
			row.Misses = 7
			row.MissesPerRecord = 0.07
			row.CyclesPerRecord = 20
			row.InsnsPerRecord = 30
			row.IPC = 1.5
			return row
		}(),
	}
	for _, want := range rows {
		t.Run(want.Mode, func(t *testing.T) {
			encoded, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			var got BenchmarkRow
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("round trip changed row:\ngot  %+v\nwant %+v", got, want)
			}
		})
	}
}

func TestBenchmarkRowKeepsRequiredSchemaFields(t *testing.T) {
	encoded, err := json.Marshal(BenchmarkRow{SchemaVersion: Version, ImplementationVariant: "go-mpsc-v4-compact"})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"schema_version", "implementation_variant", "counters_available"} {
		if _, ok := fields[field]; !ok {
			t.Errorf("encoded row omitted %q: %s", field, encoded)
		}
	}
}

func TestCampaignRowPreservesBenchmarkFields(t *testing.T) {
	want := CampaignRow{
		BenchmarkRow: BenchmarkRow{SchemaVersion: Version, Mode: "paced", Variant: "mpsc-padded", ImplementationVariant: "go-mpsc-v4-padded-64", CountersAvailable: false, OfferedMrecS: 2, DelayP99NS: 50, ConsumerMode: "payload-touching", WriterCPUs: []int{2, 4}},
		Repetition:   2, Ordinal: 3, Seed: 4, BinarySHA256: "abc", Binary: "/bench", BenchmarkCPUs: []int{1, 2, 4}, Kernel: "linux", Host: "host", RequestedSecs: 2,
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got CampaignRow
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("campaign round trip changed row:\ngot  %+v\nwant %+v", got, want)
	}
}
