package collector

import (
	"context"
	"testing"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/storage"
)

type sequenceProvider struct{ calls int }

func (p *sequenceProvider) Collect(_ context.Context, _ SystemQueryer, profile dbconn.Profile) (Snapshot, error) {
	p.calls++
	value := float64(p.calls * 100)
	return Snapshot{ProfileID: profile.ID, Engine: profile.Type,
		Counters: []Metric{{Name: "queries", Value: value, Unit: "count", Cumulative: true}, {Name: "commits", Value: value / 2, Unit: "count", Cumulative: true}},
		Rates:    map[string]float64{}, Waits: []Wait{}, TopSQL: []SQLStat{}, Capacity: []Capacity{{Scope: "database", Name: "app", UsedBytes: value * 10, MaxBytes: 3000}},
		Evidence: []Evidence{}, Warnings: []string{}, Limitations: []string{}}, nil
}

func TestCollectProfilePersistsAndDerivesRatesFromPriorSnapshot(t *testing.T) {
	store := storage.NewFileStore(t.TempDir())
	provider := &sequenceProvider{}
	svc := New(nil, store)
	svc.Queryer = &contractQueryer{}
	svc.Providers = &Registry{providers: map[string]Provider{"postgres": provider}}
	clock := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return clock }
	profile := dbconn.Profile{ID: "p", Type: "postgres"}
	first := svc.CollectProfile(context.Background(), profile, true)
	if !first.Persisted || first.Status != "partial" {
		t.Fatalf("first snapshot: %+v", first)
	}
	clock = clock.Add(10 * time.Second)
	second := svc.CollectProfile(context.Background(), profile, true)
	if !second.Persisted || second.Snapshot.Rates["qps"] != 10 || second.Snapshot.Rates["commits_per_second"] != 5 {
		t.Fatalf("rates not derived: %+v", second)
	}
	if second.Snapshot.Rates["capacity_growth_bytes_per_day:database:app"] != 8640000 {
		t.Fatalf("capacity growth missing: %+v", second.Snapshot.Rates)
	}
	foundExhaustion := false
	for _, evidence := range second.Snapshot.Evidence {
		if evidence.Code == "CAPACITY_EXHAUSTION_RISK" {
			foundExhaustion = true
		}
	}
	if !foundExhaustion {
		t.Fatalf("capacity exhaustion evidence missing: %+v", second.Snapshot.Evidence)
	}
	history, warnings, err := svc.History(context.Background(), "p", time.Time{}, 10)
	if err != nil || len(warnings) != 0 || len(history) != 2 || !history[0].CollectedAt.After(history[1].CollectedAt) {
		t.Fatalf("history: len=%d warnings=%v err=%v", len(history), warnings, err)
	}
}

func TestBatchIsolationKeepsOtherProfiles(t *testing.T) {
	provider := &sequenceProvider{}
	svc := New(nil, storage.NewFileStore(t.TempDir()))
	svc.Queryer = &contractQueryer{}
	svc.Providers = &Registry{providers: map[string]Provider{"postgres": provider}}
	batch := svc.CollectAll(context.Background(), []dbconn.Profile{{ID: "good", Type: "postgres"}, {ID: "unknown", Type: "nope"}}, false)
	if batch.Status != "degraded" || batch.Succeeded != 1 || batch.Failed != 1 || len(batch.Results) != 2 {
		t.Fatalf("batch isolation failed: %+v", batch)
	}
}

func TestFreshnessThresholdTracksCollectionInterval(t *testing.T) {
	svc := New(nil, storage.NewFileStore(t.TempDir()))
	if got := svc.FreshnessThreshold(); got != 2*time.Minute {
		t.Fatalf("default freshness threshold = %s", got)
	}
	svc.ExpectedInterval = 15 * time.Minute
	if got := svc.FreshnessThreshold(); got != 30*time.Minute {
		t.Fatalf("configured freshness threshold = %s", got)
	}
}
