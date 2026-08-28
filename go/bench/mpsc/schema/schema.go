// Package schema defines the versioned MPSC benchmark JSONL format.
package schema

const Version = 2

type BenchmarkRow struct {
	SchemaVersion         int     `json:"schema_version"`
	Mode                  string  `json:"mode"`
	Variant               string  `json:"variant"`
	ImplementationVariant string  `json:"implementation_variant"`
	Writers               int     `json:"writers"`
	Seconds               float64 `json:"seconds"`
	Records               uint64  `json:"records"`
	MrecS                 float64 `json:"mrec_s"`
	PayloadBytes          int     `json:"payload_bytes"`
	ExtentBytes           int     `json:"extent_bytes"`
	PayloadGBS            float64 `json:"payload_gb_s"`
	ReservedGBS           float64 `json:"reserved_gb_s"`
	FairnessMinMax        float64 `json:"fairness_min_max,omitempty"`
	MinWriterRecords      uint64  `json:"min_writer_records,omitempty"`
	MaxWriterRecords      uint64  `json:"max_writer_records,omitempty"`
	OfferedPerWriterMrecS float64 `json:"offered_per_writer_mrec_s,omitempty"`
	OfferedMrecS          float64 `json:"offered_mrec_s,omitempty"`
	AchievedRatio         float64 `json:"achieved_ratio,omitempty"`
	DelayP50NS            float64 `json:"delay_p50_ns,omitempty"`
	DelayP99NS            float64 `json:"delay_p99_ns,omitempty"`
	DelayMaxNS            float64 `json:"delay_max_ns,omitempty"`
	DelaySamples          int     `json:"delay_samples,omitempty"`
	LateFraction          float64 `json:"late_fraction,omitempty"`
	Misses                uint64  `json:"misses,omitempty"`
	MissesPerRecord       float64 `json:"misses_per_record,omitempty"`
	CyclesPerRecord       float64 `json:"cycles_per_record,omitempty"`
	InsnsPerRecord        float64 `json:"insns_per_record,omitempty"`
	IPC                   float64 `json:"ipc,omitempty"`
	CountersAvailable     bool    `json:"counters_available"`
	FullRetries           uint64  `json:"full_retries"`
	ContentionRetries     uint64  `json:"contention_retries"`
	ContentionYields      uint64  `json:"contention_yields"`
	RetryYields           uint64  `json:"retry_yields"`
	RetryPolicy           string  `json:"retry_policy"`
	Allocations           uint64  `json:"allocations"`
	AllocationsPerRecord  float64 `json:"allocations_per_record"`
	TopologyVerified      bool    `json:"topology_verified"`
	TopologyOverride      bool    `json:"topology_override"`
	ReaderCPU             int     `json:"reader_cpu"`
	WriterCPUs            []int   `json:"writer_cpus"`
	LatencySamples        int     `json:"latency_samples,omitempty"`
	LatencyP50NS          float64 `json:"latency_p50_ns,omitempty"`
	LatencyP95NS          float64 `json:"latency_p95_ns,omitempty"`
	LatencyP99NS          float64 `json:"latency_p99_ns,omitempty"`
	LatencyP9999NS        float64 `json:"latency_p99_99_ns,omitempty"`
	LatencyMaxNS          float64 `json:"latency_max_ns,omitempty"`
	AdmissionSamples      int     `json:"admission_samples,omitempty"`
	AdmissionP50NS        float64 `json:"admission_p50_ns,omitempty"`
	AdmissionP95NS        float64 `json:"admission_p95_ns,omitempty"`
	AdmissionP99NS        float64 `json:"admission_p99_ns,omitempty"`
	AdmissionP9999NS      float64 `json:"admission_p99_99_ns,omitempty"`
	AdmissionMaxNS        float64 `json:"admission_max_ns,omitempty"`
	ConsumerMode          string  `json:"consumer_mode"`
}

type CampaignRow struct {
	BenchmarkRow
	Repetition    int     `json:"repetition"`
	Ordinal       int     `json:"ordinal"`
	Seed          int64   `json:"seed"`
	BinarySHA256  string  `json:"binary_sha256"`
	Binary        string  `json:"binary"`
	BenchmarkCPUs []int   `json:"benchmark_cpus"`
	Kernel        string  `json:"kernel"`
	Host          string  `json:"host"`
	RequestedSecs float64 `json:"requested_seconds"`
	BuildCommand  string  `json:"build_command,omitempty"`
	Compiler      string  `json:"compiler,omitempty"`
	CXXFlags      string  `json:"cxxflags,omitempty"`
}

type CPUTopology struct {
	CPU            int   `json:"cpu"`
	PackageID      int   `json:"package_id"`
	CoreID         int   `json:"core_id"`
	NodeID         int   `json:"node_id"`
	ThreadSiblings []int `json:"thread_siblings"`
}

type MetadataRow struct {
	SchemaVersion           int           `json:"schema_version"`
	Mode                    string        `json:"mode"`
	Binary                  string        `json:"binary"`
	BinarySHA256            string        `json:"binary_sha256"`
	Go                      string        `json:"go"`
	Kernel                  string        `json:"kernel"`
	Host                    string        `json:"host"`
	BenchmarkCPUs           []int         `json:"benchmark_cpus"`
	AffinityCPUs            []int         `json:"affinity_cpus"`
	EffectiveCPUs           []int         `json:"effective_cpus"`
	TelemetryCPUs           []int         `json:"telemetry_cpus,omitempty"`
	SamplerIsolated         bool          `json:"sampler_isolated"`
	Topology                []CPUTopology `json:"cpu_topology"`
	LoadAverage             string        `json:"load_average,omitempty"`
	CPUPressure             string        `json:"cpu_pressure,omitempty"`
	MemoryPressure          string        `json:"memory_pressure,omitempty"`
	IOPressure              string        `json:"io_pressure,omitempty"`
	RequestedSecs           float64       `json:"requested_seconds"`
	PayloadBytes            int           `json:"payload_bytes"`
	Runs                    int           `json:"runs"`
	Seed                    int64         `json:"seed"`
	Variants                []string      `json:"variants"`
	WriterCounts            []int         `json:"writer_counts"`
	BuildCommand            string        `json:"build_command,omitempty"`
	Compiler                string        `json:"compiler,omitempty"`
	CXXFlags                string        `json:"cxxflags,omitempty"`
	AllowUnverifiedTopology bool          `json:"allow_unverified_topology"`
	ConsumerModes           []string      `json:"consumer_modes"`
	Counters                bool          `json:"counters"`
	Telemetry               bool          `json:"telemetry"`
}
