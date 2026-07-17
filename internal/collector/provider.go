package collector

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"sqlon/internal/dbconn"
)

type SystemQueryer interface {
	SystemQuery(context.Context, string, string, ...any) ([]map[string]any, error)
}

// Provider is the workload/capacity collection role. Engine implementations
// live in internal/engine/<engine> and are wired by internal/engine/adapters;
// this package owns the result model and the service logic only.
type Provider interface {
	Collect(context.Context, SystemQueryer, dbconn.Profile) (Snapshot, error)
}

type Registry struct{ providers map[string]Provider }

// NewRegistry builds a provider registry from an engine-name→implementation
// map (normally adapters.CollectorProviders()). A nil map yields an empty
// registry, which reports every engine as PROVIDER_UNAVAILABLE.
func NewRegistry(providers map[string]Provider) *Registry {
	normalized := make(map[string]Provider, len(providers))
	for name, provider := range providers {
		normalized[strings.ToLower(strings.TrimSpace(name))] = provider
	}
	return &Registry{providers: normalized}
}

func (r *Registry) Get(engine string) (Provider, bool) {
	p, ok := r.providers[strings.ToLower(strings.TrimSpace(engine))]
	return p, ok
}

func value(row map[string]any, name string) any {
	for key, v := range row {
		if strings.EqualFold(key, name) {
			return v
		}
	}
	return nil
}

// Text and Number read a column case-insensitively from a system-query row.
// They are exported for the engine adapter packages.
func Text(row map[string]any, name string) string {
	v := value(row, name)
	if v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func Number(row map[string]any, name string) float64 {
	v := value(row, name)
	switch n := v.(type) {
	case int:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	case float32:
		return float64(n)
	case float64:
		return n
	}
	n, _ := strconv.ParseFloat(strings.TrimSpace(fmt.Sprint(v)), 64)
	return n
}

// CumulativeMetric reads a cumulative counter column into a Metric.
func CumulativeMetric(row map[string]any, name, unit string) Metric {
	return Metric{Name: name, Value: Number(row, name), Unit: unit, Cumulative: true}
}

// CollectCapacity runs an engine's fixed capacity query and appends the rows
// to the snapshot. A failure is reported as a snapshot warning, not an error:
// capacity is best-effort on top of the mandatory workload counters.
func CollectCapacity(ctx context.Context, q SystemQueryer, profileID, query string, snapshot *Snapshot) {
	rows, err := q.SystemQuery(ctx, profileID, query)
	if err != nil {
		snapshot.Warnings = append(snapshot.Warnings, "용량 수집 실패: "+err.Error())
		return
	}
	for _, row := range rows {
		snapshot.Capacity = append(snapshot.Capacity, Capacity{Scope: Text(row, "scope"), Name: Text(row, "name"), UsedBytes: Number(row, "used_bytes"), AllocatedBytes: Number(row, "allocated_bytes"), MaxBytes: Number(row, "max_bytes"), UsagePercent: Number(row, "usage_percent")})
	}
}

// NewSnapshot returns an initialized, fully non-nil snapshot for an engine
// provider to fill.
func NewSnapshot(profileID, engine string) Snapshot {
	return Snapshot{ProfileID: profileID, Engine: engine, Counters: []Metric{}, Rates: map[string]float64{}, Waits: []Wait{}, TopSQL: []SQLStat{}, Capacity: []Capacity{}, Evidence: []Evidence{}, Warnings: []string{}, Limitations: []string{}, CollectedAt: time.Now().UTC()}
}
