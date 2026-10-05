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
	svc := New(nil, store, map[string]Provider{"postgres": provider})
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
	svc := New(nil, storage.NewFileStore(t.TempDir()), map[string]Provider{"postgres": provider})
	batch := svc.CollectAll(context.Background(), []dbconn.Profile{{ID: "good", Type: "postgres"}, {ID: "unknown", Type: "nope"}}, false)
	if batch.Status != "degraded" || batch.Succeeded != 1 || batch.Failed != 1 || len(batch.Results) != 2 {
		t.Fatalf("batch isolation failed: %+v", batch)
	}
}

func TestFreshnessThresholdTracksCollectionInterval(t *testing.T) {
	svc := New(nil, storage.NewFileStore(t.TempDir()), nil)
	if got := svc.FreshnessThreshold(); got != 2*time.Minute {
		t.Fatalf("default freshness threshold = %s", got)
	}
	svc.ExpectedInterval = 15 * time.Minute
	if got := svc.FreshnessThreshold(); got != 30*time.Minute {
		t.Fatalf("configured freshness threshold = %s", got)
	}
}

func TestApplyDeclaredLimitTargetsTheFootprint(t *testing.T) {
	p := dbconn.Profile{ID: "p", Capacity: &dbconn.CapacityConfig{StorageLimit: "100GiB"}}
	snap := Snapshot{Capacity: []Capacity{
		{Scope: "database", Name: "app", UsedBytes: 10 << 30},
		{Scope: "table", Name: "public.t", UsedBytes: 5 << 30},
		{Scope: ScopeStorage, Name: FootprintName, UsedBytes: 40 << 30},
	}}
	ApplyDeclaredLimit(&snap, p)
	if snap.Capacity[2].MaxBytes != 100<<30 || snap.Capacity[2].UsagePercent != 40 || snap.Capacity[0].MaxBytes != 0 {
		t.Fatalf("the limit belongs on the footprint: %+v", snap.Capacity)
	}

	mysql := Snapshot{Capacity: []Capacity{{Scope: "table", Name: "t"}, {Scope: "database", Name: "app", UsedBytes: 50 << 30}}}
	ApplyDeclaredLimit(&mysql, p)
	if mysql.Capacity[1].UsagePercent != 50 {
		t.Fatalf("without a footprint the database row carries the limit: %+v", mysql.Capacity)
	}

	oracle := Snapshot{Capacity: []Capacity{{Scope: "database", Name: "x", UsedBytes: 1, MaxBytes: 7}}}
	ApplyDeclaredLimit(&oracle, p)
	if oracle.Capacity[0].MaxBytes != 7 {
		t.Fatalf("an engine-reported limit must never be overridden")
	}
}

type countingStore struct {
	storage.OperationalStore
	queries int
}

func (c *countingStore) Query(ctx context.Context, q storage.Query) (storage.QueryResult, error) {
	c.queries++
	return c.OperationalStore.Query(ctx, q)
}

func TestLatestSnapshotIsServedFromMemory(t *testing.T) {
	store := &countingStore{OperationalStore: storage.NewFileStore(t.TempDir())}
	svc := New(nil, store, map[string]Provider{"postgres": &sequenceProvider{}})
	clock := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return clock }
	profile := dbconn.Profile{ID: "p", Type: "postgres"}
	svc.CollectProfile(context.Background(), profile, true)
	clock = clock.Add(time.Minute)
	svc.CollectProfile(context.Background(), profile, true)

	before := store.queries
	got, _, err := svc.History(context.Background(), "p", time.Time{}, 1)
	if err != nil || len(got) != 1 || !got[0].CollectedAt.Equal(clock) {
		t.Fatalf("latest: %+v %v", got, err)
	}
	if store.queries != before {
		t.Fatalf("the latest snapshot must come from memory, store queried %d times", store.queries-before)
	}
	got[0].Warnings = append(got[0].Warnings, "caller-owned")
	again, _, _ := svc.History(context.Background(), "p", time.Time{}, 1)
	for _, w := range again[0].Warnings {
		if w == "caller-owned" {
			t.Fatalf("a caller's append leaked into the cache")
		}
	}
}

func TestLatestSnapshotColdStartFindsOldRecords(t *testing.T) {
	dir := t.TempDir()
	old := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	writer := New(nil, storage.NewFileStore(dir), map[string]Provider{"postgres": &sequenceProvider{}})
	writer.Now = func() time.Time { return old }
	writer.CollectProfile(context.Background(), dbconn.Profile{ID: "p", Type: "postgres"}, true)

	reader := New(nil, storage.NewFileStore(dir), nil) // fresh process, empty cache
	reader.Now = func() time.Time { return old.Add(30 * 24 * time.Hour) }
	got, _, err := reader.History(context.Background(), "p", time.Time{}, 1)
	if err != nil || len(got) != 1 || !got[0].CollectedAt.Equal(old) {
		t.Fatalf("a snapshot older than the 48h lookback must still be found: %+v %v", got, err)
	}
}
