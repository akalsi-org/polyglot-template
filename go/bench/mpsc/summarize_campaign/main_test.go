package main

import (
	"strings"
	"testing"
)

func TestQuartilesMatchCampaignConvention(t *testing.T) {
	got := quartiles([]float64{4, 1, 3, 2})
	want := quartileSet{low: 1.5, median: 2.5, high: 3.5}
	if got != want {
		t.Fatalf("quartiles = %+v, want %+v", got, want)
	}
}

func TestSummarizeReportsSeparatedIQRs(t *testing.T) {
	input := strings.NewReader("" +
		`{"mode":"campaign_metadata"}` + "\n" +
		`{"mode":"throughput","variant":"mpsc","writers":1,"mrec_s":10}` + "\n" +
		`{"mode":"throughput","variant":"mpsc","writers":1,"mrec_s":12}` + "\n" +
		`{"mode":"throughput","variant":"mpsc-padded","writers":1,"mrec_s":20}` + "\n" +
		`{"mode":"throughput","variant":"mpsc-padded","writers":1,"mrec_s":22}` + "\n")
	var output strings.Builder
	if err := summarize(input, &output, "mpsc"); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"mpsc w=1: 2", "mpsc-padded w=1: 2", "+90.91%", "IQRs separated"} {
		if !strings.Contains(output.String(), text) {
			t.Errorf("output does not contain %q:\n%s", text, output.String())
		}
	}
}

func TestSummarizeAcceptsLargeMetadataRows(t *testing.T) {
	topology := strings.Repeat(`{"cpu":1,"package_id":0,"core_id":0,"node_id":0,"thread_siblings":[1]},`, 1000) + `{}`
	input := strings.NewReader(`{"mode":"campaign_metadata","cpu_topology":[` + topology + `]}` + "\n" +
		`{"mode":"throughput","variant":"mpsc","writers":1,"mrec_s":10}` + "\n")
	var output strings.Builder
	if err := summarize(input, &output, "mpsc"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "mpsc w=1: 1") {
		t.Fatalf("unexpected summary:\n%s", output.String())
	}
}

func TestSummarizeRejectsIncompleteThroughputRows(t *testing.T) {
	for _, row := range []string{
		`{"mode":"throughput","writers":1,"mrec_s":10}`,
		`{"mode":"throughput","variant":"mpsc","mrec_s":10}`,
		`{"mode":"throughput","variant":"mpsc","writers":1,"mrec_s":0}`,
	} {
		var output strings.Builder
		if err := summarize(strings.NewReader(row+"\n"), &output, "mpsc"); err == nil {
			t.Errorf("summarize accepted %s", row)
		}
	}
}

func TestSummarizeReportsExplicitMissingCells(t *testing.T) {
	input := strings.NewReader("" +
		`{"schema_version":2,"mode":"campaign_metadata","variants":["mpsc","mpsc-padded"],"writer_counts":[1,2],"consumer_modes":["metadata-only"]}` + "\n" +
		`{"mode":"throughput","variant":"mpsc","consumer_mode":"metadata-only","writers":1,"mrec_s":10}` + "\n")
	var output strings.Builder
	if err := summarize(input, &output, "mpsc"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"mpsc|metadata-only w=2: n/a",
		"mpsc-padded|metadata-only w=1: n/a",
		"n/a: candidate cell missing",
		"n/a: baseline cell missing",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("output does not contain %q:\n%s", want, output.String())
		}
	}
}

func TestSummarizeDoesNotReportUnsupportedSPSCCellsAsMissing(t *testing.T) {
	input := strings.NewReader("" +
		`{"schema_version":2,"mode":"campaign_metadata","variants":["mpsc","spsc"],"writer_counts":[1,2],"consumer_modes":["metadata-only"]}` + "\n" +
		`{"mode":"throughput","variant":"mpsc","consumer_mode":"metadata-only","writers":1,"mrec_s":10}` + "\n" +
		`{"mode":"throughput","variant":"mpsc","consumer_mode":"metadata-only","writers":2,"mrec_s":10}` + "\n" +
		`{"mode":"throughput","variant":"spsc","consumer_mode":"metadata-only","writers":1,"mrec_s":20}` + "\n")
	var output strings.Builder
	if err := summarize(input, &output, "mpsc"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "n/a: candidate cell missing") {
		t.Fatalf("summary reported an unsupported SPSC cell as missing:\n%s", output.String())
	}
}

func TestSummarizeKeepsConsumerModesSeparate(t *testing.T) {
	input := strings.NewReader("" +
		`{"mode":"throughput","variant":"mpsc","consumer_mode":"metadata-only","writers":1,"mrec_s":10}` + "\n" +
		`{"mode":"throughput","variant":"mpsc-padded","consumer_mode":"metadata-only","writers":1,"mrec_s":20}` + "\n" +
		`{"mode":"throughput","variant":"mpsc","consumer_mode":"payload-touching","writers":1,"mrec_s":100}` + "\n" +
		`{"mode":"throughput","variant":"mpsc-padded","consumer_mode":"payload-touching","writers":1,"mrec_s":110}` + "\n")
	var output strings.Builder
	if err := summarize(input, &output, "mpsc"); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, want := range []string{
		"mpsc|metadata-only w=1: 1",
		"mpsc|payload-touching w=1: 1",
		"mpsc-padded|metadata-only",
		"mpsc-padded|payload-touching",
		"+100.00%",
		"+10.00%",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output does not contain %q:\n%s", want, text)
		}
	}
}
