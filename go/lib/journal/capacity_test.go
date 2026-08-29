package journal

import (
	"encoding/hex"
	"errors"
	"math"
	"testing"
	"time"
)

func validSizingConfig() SizingConfig {
	return SizingConfig{
		BatchMaxOccupiedBytes: 4096,
		BatchMaxAge:           2 * time.Second,
		ZstdBlockBytes:        2048,
		Bounds: Bounds{
			PeakOccupiedBytesPerSecond:      1000,
			PeakArchiveBytesPerSecond:       500,
			MinArchiveBytesPerSecond:        2000,
			MinObserverReplayBytesPerSecond: 3000,
			BurstOccupiedBytes:              512,
			MaxArchiveStall:                 time.Second,
			MaxSyncLatency:                  500 * time.Millisecond,
			MaxRecoveryScanBytes:            25856,
			MaxCatchUp:                      3 * time.Second,
			MaxObserverBacklogBytes:         5000,
		},
	}
}

func TestRequiredCapacityGoldenTable(t *testing.T) {
	tests := []struct {
		name      string
		maxRecord uint64
		mutate    func(*SizingConfig)
		want      uint64
	}{
		{name: "base", maxRecord: 1000, want: 32768},
		{
			name:      "power of two boundary",
			maxRecord: 64,
			mutate: func(cfg *SizingConfig) {
				cfg.BatchMaxOccupiedBytes = 2048
				cfg.ZstdBlockBytes = 128
				cfg.Bounds.PeakOccupiedBytesPerSecond = 1
				cfg.Bounds.PeakArchiveBytesPerSecond = 1
				cfg.Bounds.MinArchiveBytesPerSecond = 1 << 20
				cfg.Bounds.MinObserverReplayBytesPerSecond = 2
				cfg.Bounds.BurstOccupiedBytes = 64
				cfg.Bounds.MaxArchiveStall = time.Second
				cfg.Bounds.MaxSyncLatency = time.Second
				cfg.Bounds.MaxRecoveryScanBytes = 15104
				cfg.Bounds.MaxCatchUp = 0
				cfg.Bounds.MaxObserverBacklogBytes = 0
			},
			want: 4096,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validSizingConfig()
			if test.mutate != nil {
				test.mutate(&cfg)
			}
			got, err := RequiredCapacity(test.maxRecord, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("RequiredCapacity() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestRequiredCapacityRejectsNonConvergence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*SizingConfig)
		reason ConvergenceReason
	}{
		{
			name: "missing batch maximum age",
			mutate: func(cfg *SizingConfig) {
				cfg.BatchMaxAge = 0
			},
			reason: ConvergenceInvalidBound,
		},
		{
			name: "negative batch maximum age",
			mutate: func(cfg *SizingConfig) {
				cfg.BatchMaxAge = -time.Nanosecond
			},
			reason: ConvergenceInvalidBound,
		},
		{
			name: "archive equality",
			mutate: func(cfg *SizingConfig) {
				cfg.Bounds.MinArchiveBytesPerSecond = cfg.Bounds.PeakArchiveBytesPerSecond
			},
			reason: ConvergenceArchiveRate,
		},
		{
			name: "observer equality",
			mutate: func(cfg *SizingConfig) {
				cfg.Bounds.MinObserverReplayBytesPerSecond = cfg.Bounds.PeakArchiveBytesPerSecond
			},
			reason: ConvergenceObserverRate,
		},
		{
			name: "record exceeds batch",
			mutate: func(cfg *SizingConfig) {
				cfg.BatchMaxOccupiedBytes = 512
			},
			reason: ConvergenceRecordFit,
		},
		{
			name: "record exceeds block",
			mutate: func(cfg *SizingConfig) {
				cfg.ZstdBlockBytes = 1000
			},
			reason: ConvergenceRecordFit,
		},
		{
			name: "missing backlog",
			mutate: func(cfg *SizingConfig) {
				cfg.Bounds.MaxObserverBacklogBytes = 0
			},
			reason: ConvergenceInvalidBound,
		},
		{
			name: "deadline exceeded",
			mutate: func(cfg *SizingConfig) {
				cfg.Bounds.MaxCatchUp = time.Second
			},
			reason: ConvergenceObserverDeadline,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validSizingConfig()
			test.mutate(&cfg)
			_, err := RequiredCapacity(1000, cfg)
			if !errors.Is(err, ErrConvergence) {
				t.Fatalf("error = %v, want ErrConvergence", err)
			}
			var convergence *ConvergenceError
			if !errors.As(err, &convergence) {
				t.Fatalf("error type = %T, want *ConvergenceError", err)
			}
			if convergence.Reason != test.reason {
				t.Fatalf("reason = %v, want %v", convergence.Reason, test.reason)
			}
		})
	}
}

func TestRequiredCapacityAcceptsCatchUpEquality(t *testing.T) {
	cfg := validSizingConfig()
	cfg.Bounds.MaxCatchUp = 2 * time.Second
	if _, err := RequiredCapacity(1000, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestRequiredCapacityRejectsUnsupportedZstdBlockMemory(t *testing.T) {
	cfg := validSizingConfig()
	cfg.ZstdBlockBytes = uint64(math.MaxInt) + 1
	_, err := RequiredCapacity(1000, cfg)
	if !errors.Is(err, ErrConvergence) {
		t.Fatalf("error = %v, want ErrConvergence", err)
	}
	var convergence *ConvergenceError
	if !errors.As(err, &convergence) || convergence.Reason != ConvergenceInvalidBound {
		t.Fatalf("error = %#v, want invalid-bound ConvergenceError", err)
	}
}

func TestRequiredCapacityRejectsOverflow(t *testing.T) {
	cfg := validSizingConfig()
	cfg.BatchMaxOccupiedBytes = math.MaxUint64 - 63
	_, err := RequiredCapacity(1000, cfg)
	if !errors.Is(err, ErrConvergence) {
		t.Fatalf("error = %v, want ErrConvergence", err)
	}
	var convergence *ConvergenceError
	if !errors.As(err, &convergence) || convergence.Reason != ConvergenceOverflow {
		t.Fatalf("error = %#v, want overflow ConvergenceError", err)
	}
}

func TestMaximumArchiveBatchFileBytesGolden(t *testing.T) {
	got, err := MaximumArchiveBatchFileBytes(validSizingConfig())
	if err != nil {
		t.Fatal(err)
	}
	const want = 25856
	if got != want {
		t.Fatalf("MaximumArchiveBatchFileBytes() = %d, want %d", got, want)
	}
}

func TestRequiredCapacityRejectsShortRecoveryScan(t *testing.T) {
	cfg := validSizingConfig()
	cfg.Bounds.MaxRecoveryScanBytes--
	_, err := RequiredCapacity(1000, cfg)
	if !errors.Is(err, ErrConvergence) {
		t.Fatalf("error = %v, want ErrConvergence", err)
	}
	var convergence *ConvergenceError
	if !errors.As(err, &convergence) || convergence.Reason != ConvergenceInvalidBound {
		t.Fatalf("error = %#v, want invalid-bound ConvergenceError", err)
	}
	if convergence.MaximumArchiveBatchFileBytes != 25856 {
		t.Fatalf("maximum batch file = %d, want 25856", convergence.MaximumArchiveBatchFileBytes)
	}
}

func TestMaximumArchiveBatchFileBytesRejectsRecordFitAndOverflow(t *testing.T) {
	tests := []struct {
		name   string
		batch  uint64
		reason ConvergenceReason
	}{
		{name: "record fit", batch: uint64(JournalGrain) - 1, reason: ConvergenceRecordFit},
		{name: "overflow", batch: math.MaxUint64, reason: ConvergenceOverflow},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validSizingConfig()
			cfg.BatchMaxOccupiedBytes = test.batch
			_, err := MaximumArchiveBatchFileBytes(cfg)
			var convergence *ConvergenceError
			if !errors.As(err, &convergence) || convergence.Reason != test.reason {
				t.Fatalf("error = %#v, want reason %v", err, test.reason)
			}
		})
	}
}

func TestRequiredCapacityAcceptsMapperLimit(t *testing.T) {
	cfg := mapperLimitSizing(t, JournalMaxCapacity/2)
	got, err := RequiredCapacity(1000, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got != JournalMaxCapacity {
		t.Fatalf("RequiredCapacity() = %d, want %d", got, JournalMaxCapacity)
	}
	normalized, err := normalizeJournalCapacity(got)
	if err != nil {
		t.Fatalf("normalizeJournalCapacity(%d): %v", got, err)
	}
	if normalized != got {
		t.Fatalf("normalizeJournalCapacity(%d) = %d", got, normalized)
	}
	layout, err := journalLayoutForPage(got, 4096)
	if err != nil {
		t.Fatalf("journalLayoutForPage(%d): %v", got, err)
	}
	if layout.ControlBytes > uint64(math.MaxInt)-2*layout.ArenaBytes {
		t.Fatal("mapper span exceeds the signed-int limit")
	}
}

func TestRequiredCapacityRejectsAboveMapperLimit(t *testing.T) {
	cfg := mapperLimitSizing(t, JournalMaxCapacity)
	_, err := RequiredCapacity(1000, cfg)
	if !errors.Is(err, ErrConvergence) {
		t.Fatalf("error = %v, want ErrConvergence", err)
	}
	var convergence *ConvergenceError
	if !errors.As(err, &convergence) || convergence.Reason != ConvergenceOverflow {
		t.Fatalf("error = %#v, want overflow ConvergenceError", err)
	}
}

func mapperLimitSizing(t *testing.T, batchBytes uint64) SizingConfig {
	t.Helper()
	cfg := validSizingConfig()
	cfg.BatchMaxOccupiedBytes = batchBytes
	cfg.Bounds.PeakOccupiedBytesPerSecond = 1
	cfg.Bounds.PeakArchiveBytesPerSecond = 1
	cfg.Bounds.MinArchiveBytesPerSecond = math.MaxUint64
	cfg.Bounds.MinObserverReplayBytesPerSecond = 2
	cfg.Bounds.MaxCatchUp = 0
	cfg.Bounds.MaxObserverBacklogBytes = 0
	maximum, err := MaximumArchiveBatchFileBytes(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Bounds.MaxRecoveryScanBytes = maximum
	return cfg
}

func TestSizingConfigDigestGolden(t *testing.T) {
	sizing := validSizingConfig()
	digest := sizingConfigDigest(1000, 8192, sizing)
	const want = "5c916b9ff1b28486b4c77a5f71ec1807029e5d8c51d091cdd7ecb1d233cd44ce"
	if got := hex.EncodeToString(digest[:]); got != want {
		t.Fatalf("sizingConfigDigest() = %s, want %s", got, want)
	}
}

func TestSizingConfigDigestCoversEveryInput(t *testing.T) {
	base := validSizingConfig()
	want := sizingConfigDigest(1000, 8192, base)
	tests := []struct {
		name     string
		max      uint64
		required uint64
		mutate   func(*SizingConfig)
	}{
		{name: "maximum record", max: 1001, required: 8192},
		{name: "required capacity", max: 1000, required: 16384},
		{name: "batch maximum", max: 1000, required: 8192, mutate: func(s *SizingConfig) { s.BatchMaxOccupiedBytes++ }},
		{name: "batch maximum age", max: 1000, required: 8192, mutate: func(s *SizingConfig) { s.BatchMaxAge++ }},
		{name: "block maximum", max: 1000, required: 8192, mutate: func(s *SizingConfig) { s.ZstdBlockBytes++ }},
		{name: "occupied rate", max: 1000, required: 8192, mutate: func(s *SizingConfig) { s.Bounds.PeakOccupiedBytesPerSecond++ }},
		{name: "archive peak", max: 1000, required: 8192, mutate: func(s *SizingConfig) { s.Bounds.PeakArchiveBytesPerSecond++ }},
		{name: "archive minimum", max: 1000, required: 8192, mutate: func(s *SizingConfig) { s.Bounds.MinArchiveBytesPerSecond++ }},
		{name: "observer minimum", max: 1000, required: 8192, mutate: func(s *SizingConfig) { s.Bounds.MinObserverReplayBytesPerSecond++ }},
		{name: "burst", max: 1000, required: 8192, mutate: func(s *SizingConfig) { s.Bounds.BurstOccupiedBytes++ }},
		{name: "stall", max: 1000, required: 8192, mutate: func(s *SizingConfig) { s.Bounds.MaxArchiveStall++ }},
		{name: "sync", max: 1000, required: 8192, mutate: func(s *SizingConfig) { s.Bounds.MaxSyncLatency++ }},
		{name: "recovery", max: 1000, required: 8192, mutate: func(s *SizingConfig) { s.Bounds.MaxRecoveryScanBytes++ }},
		{name: "catch up", max: 1000, required: 8192, mutate: func(s *SizingConfig) { s.Bounds.MaxCatchUp++ }},
		{name: "backlog", max: 1000, required: 8192, mutate: func(s *SizingConfig) { s.Bounds.MaxObserverBacklogBytes++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sizing := base
			if test.mutate != nil {
				test.mutate(&sizing)
			}
			if got := sizingConfigDigest(test.max, test.required, sizing); got == want {
				t.Fatal("digest did not change")
			}
		})
	}
}

func TestOccupiedDuringStopRoundsOnce(t *testing.T) {
	got, ok := occupiedDuringStop(Bounds{
		PeakOccupiedBytesPerSecond: 3,
		MinArchiveBytesPerSecond:   2,
		MaxArchiveStall:            time.Nanosecond,
		MaxRecoveryScanBytes:       1,
	})
	if !ok {
		t.Fatal("occupiedDuringStop reported overflow")
	}
	if got != 2 {
		t.Fatalf("occupiedDuringStop = %d, want 2", got)
	}
}
