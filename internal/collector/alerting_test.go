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
