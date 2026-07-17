package fleet

import (
	"context"
	"sync"
	"testing"
	"time"

	"sqlon/internal/collector"
	"sqlon/internal/dbconn"
	"sqlon/internal/engine"
	"sqlon/internal/observability"
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

type fakeReplicationObserver struct {
	responses map[string]observability.Response[observability.ReplicationData]
}

func (f fakeReplicationObserver) Replication(_ context.Context, p dbconn.Profile) observability.Response[observability.ReplicationData] {
	return f.responses[p.ID]
}

func TestHealthProfilesEscalatesBrokenReplicationAndEnrichesRole(t *testing.T) {
	now := time.Date(2026, 7, 17, 4, 0, 0, 0, time.UTC)
	p := &fakeProber{
		drivers: map[string]bool{"postgres": true, "mysql": true},
		results: map[string]dbconn.PingResult{
			"replica-broken": {ProfileID: "replica-broken", OK: true, ElapsedMs: 5},
			"primary-ok":     {ProfileID: "primary-ok", OK: true, ElapsedMs: 5},
		},
	}
	s := fixedService(p)
	s.Replication = fakeReplicationObserver{responses: map[string]observability.Response[observability.ReplicationData]{
		"replica-broken": {
			Status:      "critical",
			Data:        observability.ReplicationData{ProfileID: "replica-broken", Engine: "mysql", Role: "replica", Nodes: []observability.ReplicationNode{{Name: "default", Healthy: false}}},
			Evidence:    []observability.Evidence{{Code: "REPLICATION_BROKEN", Severity: "critical", Summary: "복제 구성 요소가 비정상 상태입니다", CollectedAt: now}},
			CollectedAt: now,
		},
		"primary-ok": {
			Status:      "ok",
			Data:        observability.ReplicationData{ProfileID: "primary-ok", Engine: "postgres", Role: "primary", Nodes: []observability.ReplicationNode{{Name: "standby1", Healthy: true}}},
			Evidence:    []observability.Evidence{{Code: "REPLICATION_STATUS_COLLECTED", Severity: "info", CollectedAt: now}},
			CollectedAt: now,
		},
	}}

	h := s.HealthProfiles(context.Background(), []dbconn.Profile{
		{ID: "replica-broken", Type: "mysql", Criticality: "critical"},
		{ID: "primary-ok", Type: "postgres"},
	})
	broken := h.Data[0]
	if broken.ID != "replica-broken" || broken.Status != StatusCritical || broken.RiskScore != 88 || broken.Role != "replica" {
		t.Fatalf("broken replication did not escalate fleet risk: %+v", broken)
	}
	foundBrokenEvidence := false
	for _, evidence := range broken.Evidence {
		if evidence.Code == "REPLICATION_BROKEN" {
			foundBrokenEvidence = true
		}
	}
	if !foundBrokenEvidence {
		t.Fatalf("REPLICATION_BROKEN evidence missing: %+v", broken.Evidence)
	}
	healthy := h.Data[1]
	if healthy.Role != "primary" || healthy.Status != StatusHealthy {
		t.Fatalf("healthy primary misclassified: %+v", healthy)
	}
}

type fakeBackupObserver struct {
	responses map[string]observability.Response[observability.BackupData]
}

func (f fakeBackupObserver) Backup(_ context.Context, p dbconn.Profile) observability.Response[observability.BackupData] {
	return f.responses[p.ID]
}

func TestHealthProfilesEscalatesBackupFailure(t *testing.T) {
	now := time.Date(2026, 7, 17, 4, 0, 0, 0, time.UTC)
	p := &fakeProber{
		drivers: map[string]bool{"oracle": true},
		results: map[string]dbconn.PingResult{"ora": {ProfileID: "ora", OK: true, ElapsedMs: 5}},
	}
	s := fixedService(p)
	s.Backup = fakeBackupObserver{responses: map[string]observability.Response[observability.BackupData]{
		"ora": {
			Status:      "critical",
			Data:        observability.BackupData{ProfileID: "ora", Engine: "oracle", Archiving: "enabled", Items: []observability.BackupItem{{Name: "rman:DB FULL", Kind: "rman_job", Status: "FAILED", Healthy: false}}},
			Evidence:    []observability.Evidence{{Code: "BACKUP_FAILURE_DETECTED", Severity: "critical", Summary: "백업·아카이브 구성 요소가 비정상 상태입니다", CollectedAt: now}},
			CollectedAt: now,
		},
	}}
	h := s.HealthProfiles(context.Background(), []dbconn.Profile{{ID: "ora", Type: "oracle", Criticality: "critical"}})
	got := h.Data[0]
	if got.Status != StatusCritical || got.RiskScore != 80 {
		t.Fatalf("backup failure did not escalate fleet risk: %+v", got)
	}
	found := false
	for _, evidence := range got.Evidence {
		if evidence.Code == "BACKUP_FAILURE_DETECTED" {
			found = true
		}
	}
	if !found {
		t.Fatalf("BACKUP_FAILURE_DETECTED evidence missing: %+v", got.Evidence)
	}
}

type fakeConfigObserver struct {
	responses map[string]observability.Response[observability.ConfigDriftData]
}

func (f fakeConfigObserver) ConfigDrift(_ context.Context, p dbconn.Profile) observability.Response[observability.ConfigDriftData] {
	return f.responses[p.ID]
}

func TestHealthProfilesEscalatesConfigDrift(t *testing.T) {
	now := time.Date(2026, 7, 17, 4, 0, 0, 0, time.UTC)
	p := &fakeProber{
		drivers: map[string]bool{"postgres": true},
		results: map[string]dbconn.PingResult{"prod": {ProfileID: "prod", OK: true, ElapsedMs: 5}},
	}
	s := fixedService(p)
	s.Config = fakeConfigObserver{responses: map[string]observability.Response[observability.ConfigDriftData]{
		"prod": {Status: "warning", Data: observability.ConfigDriftData{Checked: 3, Drifted: 1}, CollectedAt: now},
	}}
	// A declared baseline is required for the fleet to run the check.
	h := s.HealthProfiles(context.Background(), []dbconn.Profile{{ID: "prod", Type: "postgres", ConfigBaseline: map[string]string{"max_connections": "100"}}})
	got := h.Data[0]
	if got.Status != StatusWarning || got.RiskScore != 45 {
		t.Fatalf("config drift did not escalate fleet risk: %+v", got)
	}
	found := false
	for _, e := range got.Evidence {
		if e.Code == "CONFIG_DRIFT_DETECTED" {
			found = true
		}
	}
	if !found {
		t.Fatalf("CONFIG_DRIFT_DETECTED evidence missing: %+v", got.Evidence)
	}
	// Without a baseline the check is intentionally silent.
	s2 := fixedService(p)
	s2.Config = s.Config
	h2 := s2.HealthProfiles(context.Background(), []dbconn.Profile{{ID: "prod", Type: "postgres"}})
	for _, e := range h2.Data[0].Evidence {
		if e.Code == "CONFIG_DRIFT_DETECTED" || e.Code == "CONFIG_IN_SYNC" {
			t.Fatalf("no baseline must skip drift evidence: %+v", h2.Data[0].Evidence)
		}
	}
}

func TestHealthProfilesReplicationCollectionFailureIsVisibleNotFatal(t *testing.T) {
	p := &fakeProber{
		drivers: map[string]bool{"postgres": true},
		results: map[string]dbconn.PingResult{"prod": {ProfileID: "prod", OK: true, ElapsedMs: 5}},
	}
	s := fixedService(p)
	s.Replication = fakeReplicationObserver{responses: map[string]observability.Response[observability.ReplicationData]{
		"prod": {Status: "permission_denied", Data: observability.ReplicationData{Role: "unknown"}},
	}}
	h := s.HealthProfiles(context.Background(), []dbconn.Profile{{ID: "prod", Type: "postgres"}})
	got := h.Data[0]
	found := false
	for _, evidence := range got.Evidence {
		if evidence.Code == "REPLICATION_STATUS_UNAVAILABLE" {
			found = true
		}
	}
	if !found || got.Status == StatusFailed {
		t.Fatalf("replication visibility gap not surfaced correctly: %+v", got)
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
