package fleet

import (
	"context"
	"sync"
	"testing"
	"time"

	"sqlon/internal/collector"
	"sqlon/internal/dbconn"
	"sqlon/internal/engine"
)

type fakeOperationalHistory struct {
	snapshots []collector.Snapshot
	warnings  []string
	err       error
	threshold time.Duration
}

func (f fakeOperationalHistory) History(context.Context, string, time.Time, int) ([]collector.Snapshot, []string, error) {
	return f.snapshots, f.warnings, f.err
}

func (f fakeOperationalHistory) FreshnessThreshold() time.Duration { return f.threshold }

type fakeProber struct {
	results map[string]dbconn.PingResult
	drivers map[string]bool
	started chan string
	release chan struct{}

	mu        sync.Mutex
	active    int
	maxActive int
}

func (f *fakeProber) Ping(_ context.Context, id string) dbconn.PingResult {
	f.mu.Lock()
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	f.mu.Unlock()
	if f.started != nil {
		f.started <- id
	}
	if f.release != nil {
		<-f.release
	}
	f.mu.Lock()
	f.active--
	f.mu.Unlock()
	return f.results[id]
}

func (f *fakeProber) DriverCapabilities() map[string]bool { return f.drivers }

func fixedService(p *fakeProber) *Service {
	now := time.Date(2026, 7, 17, 4, 0, 0, 0, time.UTC)
	return &Service{Prober: p, Engines: engine.NewDefaultRegistry(), Now: func() time.Time { return now }, Concurrency: 8}
}

func TestHealthProfilesRanksFailuresAndReturnsEvidence(t *testing.T) {
	p := &fakeProber{
		drivers: map[string]bool{"postgres": true, "mysql": true},
		results: map[string]dbconn.PingResult{
			"healthy": {ProfileID: "healthy", OK: true, ElapsedMs: 12},
			"failed":  {ProfileID: "failed", Error: "connection refused", ErrorCode: "NETWORK", Category: "network", Hint: "check listener"},
		},
	}
	profiles := []dbconn.Profile{
		{ID: "healthy", Name: "정상 DB", Type: "postgres", Environment: "development", Criticality: "low", ServiceName: "catalog"},
		{ID: "failed", Name: "운영 DB", Type: "mysql", Environment: "production", Criticality: "critical", ServiceName: "payments"},
	}
	h := fixedService(p).HealthProfiles(context.Background(), profiles)
	if h.Status != "degraded" || h.Summary.Total != 2 || h.Summary.Healthy != 1 || h.Summary.Failed != 1 {
		t.Fatalf("unexpected summary: %+v", h)
	}
	if h.Data[0].ID != "failed" || h.Data[0].RiskScore != 100 || h.Data[0].Failure == nil || h.Data[0].Failure.Code != "NETWORK" {
		t.Fatalf("failed critical instance not ranked first: %+v", h.Data)
	}
	if len(h.Data[0].Evidence) == 0 || h.Data[0].Evidence[0].CollectedAt.IsZero() {
		t.Fatalf("failure lacks timestamped evidence: %+v", h.Data[0])
	}
	if h.TraceID == "" || h.CollectedAt.IsZero() || h.Data[1].ServiceName != "catalog" {
		t.Fatalf("common response context missing: %+v", h)
	}
}

func TestHealthProfilesDistinguishesUnsupportedEdition(t *testing.T) {
	p := &fakeProber{drivers: map[string]bool{"oracle": false}, results: map[string]dbconn.PingResult{}}
	h := fixedService(p).HealthProfiles(context.Background(), []dbconn.Profile{{ID: "ora", Type: "oracle", Criticality: "critical"}})
	got := h.Data[0]
	if got.CollectionStatus != "unsupported_edition" || got.Failure == nil || got.Failure.Code != "DRIVER_UNAVAILABLE" {
		t.Fatalf("driver limitation was treated as a connection failure: %+v", got)
	}
}

func TestHealthProfilesFlagsPlaintextCredentialConfiguration(t *testing.T) {
	p := &fakeProber{
		drivers: map[string]bool{"postgres": true},
		results: map[string]dbconn.PingResult{"prod": {ProfileID: "prod", OK: true, ElapsedMs: 5}},
	}
	h := fixedService(p).HealthProfiles(context.Background(), []dbconn.Profile{{ID: "prod", Type: "postgres", Environment: "production", PasswordRef: "plain:secret"}})
	if h.Data[0].Status != StatusCritical || h.Data[0].RiskScore != 85 {
		t.Fatalf("plaintext credential risk not surfaced: %+v", h.Data[0])
	}
	if h.Data[0].Failure != nil {
		t.Fatalf("configuration risk must not be misreported as collection failure: %+v", h.Data[0])
	}
}

func TestHealthProfilesPromotesStoredCapacityRisk(t *testing.T) {
	now := time.Date(2026, 7, 17, 4, 0, 0, 0, time.UTC)
	p := &fakeProber{
		drivers: map[string]bool{"postgres": true},
		results: map[string]dbconn.PingResult{"prod": {ProfileID: "prod", OK: true, ElapsedMs: 5}},
	}
	s := fixedService(p)
	s.Operations = fakeOperationalHistory{threshold: 10 * time.Minute, snapshots: []collector.Snapshot{{
		ProfileID: "prod", CollectedAt: now.Add(-time.Minute),
		Evidence: []collector.Evidence{{Code: "CAPACITY_EXHAUSTION_RISK", Severity: "critical", Summary: "7일 이내 고갈 예상", Attributes: map[string]any{"days": 4}, CollectedAt: now.Add(-time.Minute)}},
	}}}

	h := s.HealthProfiles(context.Background(), []dbconn.Profile{{ID: "prod", Type: "postgres", Criticality: "critical"}})
	got := h.Data[0]
	if got.Status != StatusCritical || got.RiskScore != 90 || got.OperationalStatus != "current" {
		t.Fatalf("stored capacity evidence did not promote fleet risk: %+v", got)
	}
	if got.OperationalAt == nil || len(got.Evidence) < 2 || h.Status != "degraded" {
		t.Fatalf("operational evidence context missing: %+v", h)
	}
}

func TestHealthProfilesUsesConfiguredFreshnessThreshold(t *testing.T) {
	now := time.Date(2026, 7, 17, 4, 0, 0, 0, time.UTC)
	p := &fakeProber{
		drivers: map[string]bool{"postgres": true},
		results: map[string]dbconn.PingResult{"prod": {ProfileID: "prod", OK: true}},
	}
	s := fixedService(p)
	s.Operations = fakeOperationalHistory{threshold: 30 * time.Minute, snapshots: []collector.Snapshot{{ProfileID: "prod", CollectedAt: now.Add(-10 * time.Minute)}}}
	current := s.HealthProfiles(context.Background(), []dbconn.Profile{{ID: "prod", Type: "postgres"}}).Data[0]
	if current.OperationalStatus != "current" || current.Status != StatusHealthy {
		t.Fatalf("snapshot inside configured threshold marked stale: %+v", current)
	}

	s.Operations = fakeOperationalHistory{threshold: 5 * time.Minute, snapshots: []collector.Snapshot{{ProfileID: "prod", CollectedAt: now.Add(-10 * time.Minute)}}}
	stale := s.HealthProfiles(context.Background(), []dbconn.Profile{{ID: "prod", Type: "postgres"}}).Data[0]
	if stale.OperationalStatus != "stale" || stale.Status != StatusWarning || stale.RiskScore != 45 {
		t.Fatalf("snapshot outside configured threshold not marked stale: %+v", stale)
	}
}

func TestHealthProfilesProbesTargetsConcurrently(t *testing.T) {
	p := &fakeProber{
		drivers: map[string]bool{"postgres": true},
		results: map[string]dbconn.PingResult{
			"a": {ProfileID: "a", OK: true},
			"b": {ProfileID: "b", OK: true},
		},
		started: make(chan string, 2),
		release: make(chan struct{}),
	}
	done := make(chan Health, 1)
	go func() {
		done <- fixedService(p).HealthProfiles(context.Background(), []dbconn.Profile{{ID: "a", Type: "postgres"}, {ID: "b", Type: "postgres"}})
	}()
	for range 2 {
		select {
		case <-p.started:
		case <-time.After(time.Second):
			t.Fatal("a slow target blocked another profile probe")
		}
	}
	close(p.release)
	<-done
	p.mu.Lock()
	maxActive := p.maxActive
	p.mu.Unlock()
	if maxActive < 2 {
		t.Fatalf("profiles were probed serially: max_active=%d", maxActive)
	}
}

func TestHealthReportsUnavailableProfileAsFailed(t *testing.T) {
	dir := t.TempDir()
	if err := dbconn.SaveProfiles(dir, []dbconn.Profile{{ID: "missing", Type: "postgres", ConnectString: "127.0.0.1:1/db", Username: "u", PasswordRef: "plain:p"}}); err != nil {
		t.Fatal(err)
	}
	h := New(dbconn.NewManager(dir)).Health(context.Background())
	if len(h.Data) != 1 || h.Data[0].Status != StatusFailed {
		t.Fatalf("unexpected health: %+v", h)
	}
}
