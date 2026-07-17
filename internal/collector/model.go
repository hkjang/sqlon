// Package collector implements target-database workload and capacity
// collection. Providers use fixed engine-native system queries and normalize
// their results before persistence in the operational store.
package collector

import "time"

type Metric struct {
	Name       string  `json:"name"`
	Value      float64 `json:"value"`
	Unit       string  `json:"unit"`
	Cumulative bool    `json:"cumulative"`
}

type Wait struct {
	Class  string  `json:"class"`
	Event  string  `json:"event,omitempty"`
	Count  float64 `json:"count,omitempty"`
	TimeMS float64 `json:"time_ms,omitempty"`
}

type SQLStat struct {
	Fingerprint string  `json:"fingerprint"`
	PlanHash    string  `json:"plan_hash,omitempty"`
	Calls       float64 `json:"calls"`
	ElapsedMS   float64 `json:"elapsed_ms"`
	CPUMS       float64 `json:"cpu_ms,omitempty"`
	Reads       float64 `json:"reads,omitempty"`
	Rows        float64 `json:"rows,omitempty"`
}

type Capacity struct {
	Scope          string  `json:"scope"`
	Name           string  `json:"name"`
	UsedBytes      float64 `json:"used_bytes"`
	AllocatedBytes float64 `json:"allocated_bytes,omitempty"`
	MaxBytes       float64 `json:"max_bytes,omitempty"`
	UsagePercent   float64 `json:"usage_percent,omitempty"`
}

type Evidence struct {
	Code        string         `json:"code"`
	Severity    string         `json:"severity"`
	Summary     string         `json:"summary"`
	Attributes  map[string]any `json:"attributes,omitempty"`
	CollectedAt time.Time      `json:"collected_at"`
}

type Snapshot struct {
	ProfileID   string             `json:"profile_id"`
	Engine      string             `json:"engine"`
	Counters    []Metric           `json:"counters"`
	Rates       map[string]float64 `json:"rates"`
	Waits       []Wait             `json:"waits"`
	TopSQL      []SQLStat          `json:"top_sql"`
	Capacity    []Capacity         `json:"capacity"`
	Evidence    []Evidence         `json:"evidence"`
	Warnings    []string           `json:"warnings"`
	Limitations []string           `json:"limitations"`
	CollectedAt time.Time          `json:"collected_at"`
	TraceID     string             `json:"trace_id"`
}

type ProfileResult struct {
	Status      string    `json:"status"`
	Snapshot    Snapshot  `json:"snapshot"`
	Persisted   bool      `json:"persisted"`
	ErrorCode   string    `json:"error_code,omitempty"`
	Error       string    `json:"error,omitempty"`
	CollectedAt time.Time `json:"collected_at"`
}

type BatchResult struct {
	Status      string          `json:"status"`
	Results     []ProfileResult `json:"results"`
	Succeeded   int             `json:"succeeded"`
	Failed      int             `json:"failed"`
	CollectedAt time.Time       `json:"collected_at"`
	TraceID     string          `json:"trace_id"`
}
