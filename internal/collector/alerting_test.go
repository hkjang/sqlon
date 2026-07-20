package collector

import (
	"context"
	"testing"
	"time"
)

func TestAlertingEngineQueryRegression(t *testing.T) {
	ClearAlerts()

	curr := &Snapshot{
		ProfileID: "test-profile",
		TopSQL: []SQLStat{
			{
				Fingerprint: "sql-1",
				Calls:       10,
				ElapsedMS:   2000, // 200ms mean
			},
		},
		CollectedAt: time.Now(),
	}

	prev := &Snapshot{
		ProfileID: "test-profile",
		TopSQL: []SQLStat{
			{
				Fingerprint: "sql-1",
				Calls:       10,
				ElapsedMS:   500, // 50ms mean
			},
		},
		CollectedAt: time.Now().Add(-time.Minute),
	}

	RunAlertingEngine(context.Background(), curr, prev, "")

	alerts := GetAlerts()
	if len(alerts) == 0 {
		t.Fatal("expected alert for query regression but got none")
	}

	if alerts[0].MetricName != "query_latency_regression" {
		t.Fatalf("expected query_latency_regression alert, got %s", alerts[0].MetricName)
	}
}

func TestAlertingEngineSchemaDrift(t *testing.T) {
	ClearAlerts()

	curr := &Snapshot{
		ProfileID: "test-profile",
		Warnings:  []string{"schema drift detected on table orders"},
		CollectedAt: time.Now(),
	}

	RunAlertingEngine(context.Background(), curr, nil, "")

	alerts := GetAlerts()
	if len(alerts) == 0 {
		t.Fatal("expected alert for schema drift but got none")
	}

	if alerts[0].MetricName != "schema_drift" {
		t.Fatalf("expected schema_drift alert, got %s", alerts[0].MetricName)
	}
}

func TestAlertingCapacitySaturationThresholds(t *testing.T) {
	ClearAlerts()
	ClearAlertDedup()
	now := time.Now()
	curr := &Snapshot{
		ProfileID: "cap-profile",
		Capacity: []Capacity{
			{Scope: "tablespace", Name: "critical_ts", UsagePercent: 95},
			{Scope: "tablespace", Name: "warn_ts", UsagePercent: 82},
			{Scope: "tablespace", Name: "ok_ts", UsagePercent: 40},
			{Scope: "disk", Name: "computed", UsedBytes: 950, MaxBytes: 1000}, // 95% via computation
		},
		CollectedAt: now,
	}
	RunAlertingEngine(context.Background(), curr, nil, "")
	var crit, warn, total int
	for _, a := range GetAlerts() {
		if a.MetricName != "capacity_saturation" {
			continue
		}
		total++
		switch a.Severity {
		case "critical":
			crit++
		case "warning":
			warn++
		}
	}
	if total != 3 {
		t.Fatalf("expected 3 capacity alerts (95, 82, computed-95), got %d", total)
	}
	if crit != 2 || warn != 1 {
		t.Fatalf("expected 2 critical + 1 warning, got crit=%d warn=%d", crit, warn)
	}
}

func TestAlertingDedupSuppressesRepeats(t *testing.T) {
	ClearAlerts()
	ClearAlertDedup()
	base := time.Now()
	snap := func(ts time.Time) *Snapshot {
		return &Snapshot{ProfileID: "dedup-profile",
			Capacity:    []Capacity{{Scope: "disk", Name: "d", UsagePercent: 95}},
			CollectedAt: ts}
	}
	RunAlertingEngine(context.Background(), snap(base), nil, "")
	RunAlertingEngine(context.Background(), snap(base.Add(1*time.Minute)), nil, "") // within cooldown → suppressed
	if n := countMetric("capacity_saturation", "dedup-profile"); n != 1 {
		t.Fatalf("dedup should keep exactly 1 alert within cooldown, got %d", n)
	}
	// after the cooldown window, it may fire again
	RunAlertingEngine(context.Background(), snap(base.Add(AlertCooldown+time.Minute)), nil, "")
	if n := countMetric("capacity_saturation", "dedup-profile"); n != 2 {
		t.Fatalf("after cooldown the alert should fire again (total 2), got %d", n)
	}
}

func TestAlertingEvidenceBridge(t *testing.T) {
	ClearAlerts()
	ClearAlertDedup()
	curr := &Snapshot{
		ProfileID: "ev-profile",
		Evidence: []Evidence{
			{Code: "REPLICATION_LAG_HIGH", Severity: "critical", Summary: "replica lag 900s"},
			{Code: "INFO_ONLY", Severity: "info", Summary: "nothing to see"},
		},
		CollectedAt: time.Now(),
	}
	RunAlertingEngine(context.Background(), curr, nil, "")
	if countMetric("replication_lag_high", "ev-profile") != 1 {
		t.Fatalf("critical evidence must bridge to an alert")
	}
	if countMetric("info_only", "ev-profile") != 0 {
		t.Fatalf("info evidence must not raise an alert")
	}
}

func countMetric(metric, profile string) int {
	n := 0
	for _, a := range GetAlerts() {
		if a.MetricName == metric && a.ProfileID == profile {
			n++
		}
	}
	return n
}
