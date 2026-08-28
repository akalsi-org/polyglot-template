package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"

	benchschema "github.com/akalsi-org/polyglot-template/go/bench/mpsc/schema"
)

type cellKey struct {
	variant string
	writers int
}

func variantLabel(variant, consumerMode string) string {
	if consumerMode == "" {
		return variant
	}
	return variant + "|" + consumerMode
}

func variantParts(label string) (string, string) {
	variant, consumerMode, _ := strings.Cut(label, "|")
	return variant, consumerMode
}

func runnableCell(variant string, writers int) bool {
	variantName, _ := variantParts(variant)
	return variantName != "spsc" || writers == 1
}

type quartileSet struct {
	low    float64
	median float64
	high   float64
}

func median(values []float64) float64 {
	n := len(values)
	if n%2 == 1 {
		return values[n/2]
	}
	return (values[n/2-1] + values[n/2]) / 2
}

func quartiles(values []float64) quartileSet {
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	if len(ordered) == 1 {
		return quartileSet{ordered[0], ordered[0], ordered[0]}
	}
	n := len(ordered)
	return quartileSet{
		low:    median(ordered[:n/2]),
		median: median(ordered),
		high:   median(ordered[(n+1)/2:]),
	}
}

func summarize(input io.Reader, output io.Writer, baseline string) error {
	groups := make(map[cellKey][]float64)
	var metadata *benchschema.MetadataRow
	decoder := json.NewDecoder(input)
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("decode JSONL row: %w", err)
		}
		var envelope struct {
			Mode string `json:"mode"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return fmt.Errorf("decode JSONL row: %w", err)
		}
		if envelope.Mode == "campaign_metadata" {
			var row benchschema.MetadataRow
			if err := json.Unmarshal(raw, &row); err != nil {
				return fmt.Errorf("decode campaign metadata: %w", err)
			}
			metadata = &row
			continue
		}
		if envelope.Mode == "throughput" {
			var row benchschema.CampaignRow
			if err := json.Unmarshal(raw, &row); err != nil {
				return fmt.Errorf("decode throughput row: %w", err)
			}
			if row.SchemaVersion != 0 && row.SchemaVersion != benchschema.Version {
				return fmt.Errorf("throughput row has unsupported schema version %d", row.SchemaVersion)
			}
			if row.Variant == "" || row.Writers <= 0 || row.MrecS <= 0 || math.IsNaN(row.MrecS) || math.IsInf(row.MrecS, 0) {
				return fmt.Errorf("throughput row has invalid variant, writers, or mrec_s")
			}
			key := cellKey{variantLabel(row.Variant, row.ConsumerMode), row.Writers}
			groups[key] = append(groups[key], row.MrecS)
		}
	}
	if len(groups) == 0 && metadata == nil {
		fmt.Fprintln(output, "no throughput rows")
		return nil
	}

	variantSet := make(map[string]struct{})
	writerSet := make(map[int]struct{})
	if metadata != nil {
		consumerModes := metadata.ConsumerModes
		if len(consumerModes) == 0 {
			consumerModes = []string{""}
		}
		for _, variant := range metadata.Variants {
			for _, consumerMode := range consumerModes {
				variantSet[variantLabel(variant, consumerMode)] = struct{}{}
			}
		}
		for _, writers := range metadata.WriterCounts {
			writerSet[writers] = struct{}{}
		}
	}
	stats := make(map[cellKey]quartileSet, len(groups))
	for key, values := range groups {
		variantSet[key.variant] = struct{}{}
		writerSet[key.writers] = struct{}{}
		stats[key] = quartiles(values)
	}
	variants := make([]string, 0, len(variantSet))
	for variant := range variantSet {
		variants = append(variants, variant)
	}
	sort.Strings(variants)
	writers := make([]int, 0, len(writerSet))
	for writerCount := range writerSet {
		writers = append(writers, writerCount)
	}
	sort.Ints(writers)

	fmt.Fprintln(output, "n per cell:")
	for _, variant := range variants {
		for _, writerCount := range writers {
			if !runnableCell(variant, writerCount) {
				continue
			}
			key := cellKey{variant, writerCount}
			if len(groups[key]) == 0 {
				fmt.Fprintf(output, "  %s w=%d: n/a\n", key.variant, key.writers)
			} else {
				fmt.Fprintf(output, "  %s w=%d: %d\n", key.variant, key.writers, len(groups[key]))
			}
		}
	}
	fmt.Fprintf(output, "%8s", "writers")
	for _, variant := range variants {
		fmt.Fprintf(output, "  %34s", variant)
	}
	fmt.Fprintln(output)
	for _, writerCount := range writers {
		fmt.Fprintf(output, "%8d", writerCount)
		for _, variant := range variants {
			stat, ok := stats[cellKey{variant, writerCount}]
			if !ok {
				fmt.Fprintf(output, "  %10s [%8s, %8s]", "n/a", "", "")
				continue
			}
			fmt.Fprintf(output, "  %10.4f [%8.4f, %8.4f]", stat.median, stat.low, stat.high)
		}
		fmt.Fprintln(output)
	}

	fmt.Fprintln(output)
	fmt.Fprintf(output, "%8s  %34s  %14s  verdict\n", "writers", "variant|consumer_mode", "median change")
	for _, writerCount := range writers {
		for _, variant := range variants {
			if !runnableCell(variant, writerCount) {
				continue
			}
			variantName, consumerMode := variantParts(variant)
			if variantName == baseline {
				continue
			}
			baseLabel := variantLabel(baseline, consumerMode)
			baseKey := cellKey{baseLabel, writerCount}
			base, ok := stats[baseKey]
			if !ok {
				fmt.Fprintf(output, "%8d  %34s  %14s  n/a: baseline cell missing\n", writerCount, variant, "n/a")
				continue
			}
			key := cellKey{variant, writerCount}
			stat, ok := stats[key]
			if !ok {
				fmt.Fprintf(output, "%8d  %34s  %14s  n/a: candidate cell missing\n", writerCount, variant, "n/a")
				continue
			}
			change := (stat.median/base.median - 1) * 100
			verdict := "IQRs OVERLAP: no measured difference"
			if min(len(groups[baseKey]), len(groups[key])) < 2 {
				verdict = "single sample: smoke only, no interval"
			} else if stat.low > base.high || stat.high < base.low {
				verdict = "IQRs separated"
			}
			fmt.Fprintf(output, "%8d  %34s  %+13.2f%%  %s\n", writerCount, variant, change, verdict)
		}
	}
	return nil
}

func main() {
	baseline := flag.String("baseline", "mpsc", "variant to compare against")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: summarize_campaign [--baseline variant] results.jsonl")
		os.Exit(1)
	}
	input, err := os.Open(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer input.Close()
	if err := summarize(input, os.Stdout, *baseline); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
