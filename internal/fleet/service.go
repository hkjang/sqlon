// Package fleet builds evidence-bearing, read-only fleet snapshots from the
// shared profile and connection services. Each target is probed independently;
// one slow or failed database cannot suppress results for other instances.
package fleet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"sqlon/internal/collector"
	"sqlon/internal/dbconn"
	"sqlon/internal/engine"
	"sqlon/internal/observability"
)

type Status string

const (
	StatusHealthy  Status = "healthy"
	StatusWarning  Status = "warning"
	StatusCritical Status = "critical"
	StatusFailed   Status = "failed"
	StatusUnknown  Status = "unknown"
)

type Evidence struct {
	Code        string    `json:"code"`
	Severity    string    `json:"severity"`
	Summary     string    `json:"summary"`
	Observed    any       `json:"observed,omitempty"`
	CollectedAt time.Time `json:"collected_at"`
}

type Failure struct {
	Code      string   `json:"code,omitempty"`
	Category  string   `json:"category,omitempty"`
	Message   string   `json:"message,omitempty"`
	Hint      string   `json:"hint,omitempty"`
	NextSteps []string `json:"next_steps,omitempty"`
}

type Instance struct {
	ID                string          `json:"id"`
	Name              string          `json:"name"`
	ServiceName       string          `json:"service_name,omitempty"`
	Environment       string          `json:"environment"`
	Criticality       string          `json:"criticality"`
	Engine            string          `json:"engine"`
	Role              string          `json:"role"`
	OwnerTeam         string          `json:"owner_team,omitempty"`
	Location          string          `json:"location,omitempty"`
	MaintenanceWindow string          `json:"maintenance_window,omitempty"`
	Tags              []string        `json:"tags,omitempty"`
	Status            Status          `json:"status"`
	RiskScore         int             `json:"risk_score"`
	RiskLevel         string          `json:"risk_level"`
	Evidence          []Evidence      `json:"evidence"`
	CollectionStatus  string          `json:"collection_status"`
	CollectedAt       time.Time       `json:"collected_at"`
	ElapsedMS         int64           `json:"elapsed_ms,omitempty"`
	Failure           *Failure        `json:"failure,omitempty"`
	Capabilities      map[string]bool `json:"capabilities"`
	DriverAvailable   bool            `json:"driver_available"`
	OperationalStatus string          `json:"operational_status"`
	OperationalAt     *time.Time      `json:"operational_collected_at,omitempty"`
}

type Summary struct {
	Total              int            `json:"total"`
	Healthy            int            `json:"healthy"`
	Warning            int            `json:"warning"`
	Critical           int            `json:"critical"`
	Failed             int            `json:"failed"`
	Unknown            int            `json:"unknown"`
	EngineDistribution map[string]int `json:"engine_distribution"`
}

type Health struct {
	Status      string     `json:"status"`
	Data        []Instance `json:"data"`
	Summary     Summary    `json:"summary"`
	Warnings    []string   `json:"warnings"`
	Limitations []string   `json:"limitations"`
	CollectedAt time.Time  `json:"collected_at"`
	TraceID     string     `json:"trace_id"`
}

type profileSource interface {
	Profiles(context.Context) ([]dbconn.Profile, error)
}

type prober interface {
	Ping(context.Context, string) dbconn.PingResult
	DriverCapabilities() map[string]bool
}

type Service struct {
	Source      profileSource
	Prober      prober
	Engines     *engine.Registry
	Now         func() time.Time
	Concurrency int
	Operations  operationalHistory
	Replication replicationObserver
	Backup      backupObserver
}

type operationalHistory interface {
	History(context.Context, string, time.Time, int) ([]collector.Snapshot, []string, error)
	FreshnessThreshold() time.Duration
}

// replicationObserver and backupObserver are the live observation
// dependencies (normally *observability.Service).
type replicationObserver interface {
	Replication(context.Context, dbconn.Profile) observability.Response[observability.ReplicationData]
}

type backupObserver interface {
	Backup(context.Context, dbconn.Profile) observability.Response[observability.BackupData]
}

func New(db *dbconn.Manager) *Service {
	return &Service{Source: db, Prober: db, Engines: engine.NewDefaultRegistry(), Now: time.Now, Concurrency: 8}
}

// NewWithOperations builds the fleet service with the stored-snapshot history
// and live replication/backup observers. observability.Service satisfies both
// observer interfaces.
func NewWithOperations(db *dbconn.Manager, operations operationalHistory, observers interface {
	replicationObserver
	backupObserver
}) *Service {
	s := New(db)
	s.Operations = operations
	if observers != nil {
		s.Replication = observers
		s.Backup = observers
	}
	return s
}

// Health loads all profiles from the configured source. HTTP/MCP callers in
// authenticated mode should use HealthProfiles with their permission-filtered
// profile list.
func (s *Service) Health(ctx context.Context) Health {
	profiles, err := s.Source.Profiles(ctx)
	if err != nil {
		now := s.now()
		return Health{Status: "error", Data: []Instance{}, Summary: newSummary(), Warnings: []string{err.Error()}, Limitations: limitations(), CollectedAt: now, TraceID: traceID()}
	}
	return s.HealthProfiles(ctx, profiles)
}

func (s *Service) HealthProfiles(ctx context.Context, profiles []dbconn.Profile) Health {
	started := s.now()
	out := Health{Status: "ok", Data: []Instance{}, Summary: newSummary(), Warnings: []string{}, Limitations: limitations(), CollectedAt: started, TraceID: traceID()}
	if len(profiles) == 0 {
		return out
	}
	limit := s.Concurrency
	if limit <= 0 {
		limit = 8
	}
	sem := make(chan struct{}, limit)
	results := make(chan Instance, len(profiles))
	var wg sync.WaitGroup
	for _, profile := range profiles {
		p := dbconn.ApplyDefaults(profile)
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results <- s.contextFailure(p, ctx.Err())
				return
			}
			results <- s.probe(ctx, p)
		}()
	}
	wg.Wait()
	close(results)
	for instance := range results {
		out.Data = append(out.Data, instance)
		addSummary(&out.Summary, instance)
		if instance.Status != StatusHealthy {
			out.Status = "degraded"
		}
	}
	sort.Slice(out.Data, func(i, j int) bool {
		if out.Data[i].RiskScore != out.Data[j].RiskScore {
			return out.Data[i].RiskScore > out.Data[j].RiskScore
		}
		return out.Data[i].ID < out.Data[j].ID
	})
	out.CollectedAt = s.now()
	return out
}

// InventoryProfiles returns the registered fleet and declared capabilities
// without touching a target database. It is the backing operation for
// list_database_instances; HealthProfiles is the explicit observation call.
func (s *Service) InventoryProfiles(profiles []dbconn.Profile) Health {
	now := s.now()
	out := Health{Status: "ok", Data: []Instance{}, Summary: newSummary(), Warnings: []string{}, Limitations: []string{"인벤토리 응답은 대상 DB에 연결하지 않습니다. 최신 연결 상태는 get_fleet_health를 사용하세요."}, CollectedAt: now, TraceID: traceID()}
	drivers := s.Prober.DriverCapabilities()
	for _, raw := range profiles {
		p := dbconn.ApplyDefaults(raw)
		i := baseInstance(p, now)
		i.Status = StatusUnknown
		i.CollectionStatus = "not_collected"
		i.OperationalStatus = "not_queried"
		i.DriverAvailable = drivers[strings.ToLower(p.Type)]
		if adapter, ok := s.Engines.Get(p.Type); ok {
			i.Capabilities = adapter.Capabilities.Map()
			if !i.DriverAvailable {
				i.Status, i.RiskScore, i.RiskLevel = StatusFailed, 100, "critical"
				i.CollectionStatus = "unsupported_edition"
				i.Failure = &Failure{Code: "DRIVER_UNAVAILABLE", Category: "runtime", Message: "이 배포판에 필요한 데이터베이스 드라이버가 없습니다", Hint: "엔진을 지원하는 SQLON 배포판을 사용하세요"}
				i.Evidence = append(i.Evidence, Evidence{Code: "DRIVER_UNAVAILABLE", Severity: "critical", Summary: i.Failure.Message, CollectedAt: now})
			}
		} else {
			i.RiskScore, i.RiskLevel = 90, "critical"
			i.Evidence = append(i.Evidence, Evidence{Code: "ENGINE_UNSUPPORTED", Severity: "critical", Summary: "등록되지 않은 데이터베이스 엔진", Observed: p.Type, CollectedAt: now})
		}
		out.Data = append(out.Data, i)
		addSummary(&out.Summary, i)
	}
	sort.Slice(out.Data, func(i, j int) bool { return out.Data[i].ID < out.Data[j].ID })
	return out
}

func (s *Service) probe(ctx context.Context, p dbconn.Profile) Instance {
	now := s.now()
	i := baseInstance(p, now)
	adapter, supported := s.Engines.Get(p.Type)
	if supported {
		i.Capabilities = adapter.Capabilities.Map()
	} else {
		i.Status, i.RiskScore, i.RiskLevel = StatusUnknown, 90, "critical"
		i.CollectionStatus = "unsupported"
		i.Evidence = append(i.Evidence, Evidence{Code: "ENGINE_UNSUPPORTED", Severity: "critical", Summary: "등록되지 않은 데이터베이스 엔진", Observed: p.Type, CollectedAt: now})
		return i
	}
	drivers := s.Prober.DriverCapabilities()
	i.DriverAvailable = drivers[strings.ToLower(p.Type)]
	if !i.DriverAvailable {
		i.Status, i.RiskScore, i.RiskLevel = StatusFailed, 100, "critical"
		i.CollectionStatus = "unsupported_edition"
		i.Failure = &Failure{Code: "DRIVER_UNAVAILABLE", Category: "runtime", Message: "이 배포판에 필요한 데이터베이스 드라이버가 없습니다", Hint: "엔진을 지원하는 SQLON 배포판을 사용하세요"}
		i.Evidence = append(i.Evidence, Evidence{Code: "DRIVER_UNAVAILABLE", Severity: "critical", Summary: i.Failure.Message, CollectedAt: now})
		return i
	}
	ping := s.Prober.Ping(ctx, p.ID)
	i.ElapsedMS = ping.ElapsedMs
	i.CollectedAt = s.now()
	if !ping.OK {
		i.Status = StatusFailed
		i.CollectionStatus = "failed"
		i.RiskScore = failureRisk(p.Criticality)
		i.RiskLevel = levelForScore(i.RiskScore)
		i.Failure = &Failure{Code: ping.ErrorCode, Category: ping.Category, Message: ping.Error, Hint: ping.Hint, NextSteps: ping.NextSteps}
		i.Evidence = append(i.Evidence, Evidence{Code: "CONNECTION_FAILED", Severity: i.RiskLevel, Summary: "데이터베이스 연결 점검 실패", Observed: map[string]any{"error_code": ping.ErrorCode, "category": ping.Category}, CollectedAt: i.CollectedAt})
		return i
	}
	i.CollectionStatus = "current"
	i.Evidence = append(i.Evidence, Evidence{Code: "CONNECTION_OK", Severity: "info", Summary: "읽기 전용 연결 점검 성공", Observed: map[string]any{"elapsed_ms": ping.ElapsedMs}, CollectedAt: i.CollectedAt})
	if ping.ElapsedMs >= 2000 {
		i.Status, i.RiskScore, i.RiskLevel = StatusWarning, 40, "medium"
		i.Evidence = append(i.Evidence, Evidence{Code: "CONNECTION_SLOW", Severity: "medium", Summary: "연결 응답이 기준선보다 느림", Observed: map[string]any{"elapsed_ms": ping.ElapsedMs, "threshold_ms": 2000}, CollectedAt: i.CollectedAt})
	}
	if strings.HasPrefix(strings.ToLower(p.PasswordRef), "plain:") {
		i.Status, i.RiskScore, i.RiskLevel = StatusCritical, 85, "critical"
		i.Evidence = append(i.Evidence, Evidence{Code: "PLAINTEXT_SECRET_REF", Severity: "critical", Summary: "평문 자격증명 참조가 구성됨", CollectedAt: i.CollectedAt})
	}
	s.applyOperationalEvidence(ctx, p, &i)
	s.applyReplicationEvidence(ctx, p, adapter, &i)
	s.applyBackupEvidence(ctx, p, adapter, &i)
	return i
}

// applyBackupEvidence escalates risk when a backup/archiving component
// failed or continuous archiving (the PITR basis) is disabled. Runs only for
// engines declaring the BackupStatus capability with a live observer wired.
func (s *Service) applyBackupEvidence(ctx context.Context, p dbconn.Profile, adapter engine.Adapter, instance *Instance) {
	if s.Backup == nil || !adapter.Capabilities.BackupStatus {
		return
	}
	res := s.Backup.Backup(ctx, p)
	observed := map[string]any{"archiving": res.Data.Archiving, "items": len(res.Data.Items), "last_success_at": res.Data.LastSuccessAt, "last_failure_at": res.Data.LastFailureAt}
	switch res.Status {
	case "permission_denied", "error":
		promoteRisk(instance, StatusWarning, 35, "medium")
		instance.Evidence = append(instance.Evidence, Evidence{Code: "BACKUP_STATUS_UNAVAILABLE", Severity: "medium", Summary: "백업 상태를 확인할 수 없습니다", Observed: observed, CollectedAt: res.CollectedAt})
		return
	case "unsupported", "policy_blocked":
		instance.Evidence = append(instance.Evidence, Evidence{Code: "BACKUP_STATUS_UNAVAILABLE", Severity: "info", Summary: "이 배포판/정책에서는 백업 상태를 수집하지 않습니다", Observed: observed, CollectedAt: res.CollectedAt})
		return
	}
	escalated := false
	for _, finding := range res.Evidence {
		switch finding.Code {
		case "BACKUP_FAILURE_DETECTED":
			promoteRisk(instance, StatusCritical, 80, "critical")
			instance.Evidence = append(instance.Evidence, Evidence{Code: finding.Code, Severity: "critical", Summary: finding.Summary, Observed: finding.Attributes, CollectedAt: res.CollectedAt})
			escalated = true
		case "BACKUP_PITR_DISABLED":
			promoteRisk(instance, StatusWarning, 50, "high")
			instance.Evidence = append(instance.Evidence, Evidence{Code: finding.Code, Severity: "high", Summary: finding.Summary, Observed: finding.Attributes, CollectedAt: res.CollectedAt})
			escalated = true
		}
	}
	if !escalated {
		instance.Evidence = append(instance.Evidence, Evidence{Code: "BACKUP_STATUS_COLLECTED", Severity: "info", Summary: "백업 상태 수집 완료", Observed: observed, CollectedAt: res.CollectedAt})
	}
}

// applyReplicationEvidence enriches the instance with its observed replication
// role and escalates risk when replication is broken or lagging. It runs only
// for engines that declare the Replication capability and only when a live
// observer is configured.
func (s *Service) applyReplicationEvidence(ctx context.Context, p dbconn.Profile, adapter engine.Adapter, instance *Instance) {
	if s.Replication == nil || !adapter.Capabilities.Replication {
		return
	}
	res := s.Replication.Replication(ctx, p)
	// Enrich the operator-declared role only when none was declared
	// (ApplyDefaults fills an empty role with "unspecified").
	declared := strings.ToLower(strings.TrimSpace(instance.Role))
	if (declared == "" || declared == "unspecified") && res.Data.Role != "" && res.Data.Role != "unknown" {
		instance.Role = res.Data.Role
	}
	observed := map[string]any{"role": res.Data.Role, "nodes": len(res.Data.Nodes)}
	switch res.Status {
	case "permission_denied", "error":
		promoteRisk(instance, StatusWarning, 35, "medium")
		instance.Evidence = append(instance.Evidence, Evidence{Code: "REPLICATION_STATUS_UNAVAILABLE", Severity: "medium", Summary: "복제 상태를 확인할 수 없습니다", Observed: observed, CollectedAt: res.CollectedAt})
		return
	case "unsupported", "policy_blocked":
		instance.Evidence = append(instance.Evidence, Evidence{Code: "REPLICATION_STATUS_UNAVAILABLE", Severity: "info", Summary: "이 배포판/정책에서는 복제 상태를 수집하지 않습니다", Observed: observed, CollectedAt: res.CollectedAt})
		return
	}
	escalated := false
	for _, finding := range res.Evidence {
		switch finding.Code {
		case "REPLICATION_BROKEN":
			promoteRisk(instance, StatusCritical, 88, "critical")
			instance.Evidence = append(instance.Evidence, Evidence{Code: finding.Code, Severity: "critical", Summary: finding.Summary, Observed: finding.Attributes, CollectedAt: res.CollectedAt})
			escalated = true
		case "REPLICATION_LAG_HIGH":
			promoteRisk(instance, StatusWarning, 55, "high")
			instance.Evidence = append(instance.Evidence, Evidence{Code: finding.Code, Severity: "high", Summary: finding.Summary, Observed: finding.Attributes, CollectedAt: res.CollectedAt})
			escalated = true
		}
	}
	if !escalated {
		instance.Evidence = append(instance.Evidence, Evidence{Code: "REPLICATION_STATUS_COLLECTED", Severity: "info", Summary: "복제 상태 수집 완료", Observed: observed, CollectedAt: res.CollectedAt})
	}
}

func (s *Service) applyOperationalEvidence(ctx context.Context, p dbconn.Profile, instance *Instance) {
	if s.Operations == nil {
		instance.OperationalStatus = "not_configured"
		return
	}
	history, warnings, err := s.Operations.History(ctx, p.ID, time.Time{}, 1)
	if err != nil {
		instance.OperationalStatus = "store_error"
		promoteRisk(instance, StatusWarning, 45, "high")
		instance.Evidence = append(instance.Evidence, Evidence{Code: "OPERATIONAL_STORE_ERROR", Severity: "high", Summary: "운영 스냅숏 저장소 조회 실패", CollectedAt: instance.CollectedAt})
		return
	}
	if len(history) == 0 {
		instance.OperationalStatus = "not_collected"
		promoteRisk(instance, StatusWarning, 30, "medium")
		instance.Evidence = append(instance.Evidence, Evidence{Code: "OPERATIONAL_DATA_MISSING", Severity: "medium", Summary: "워크로드·용량 스냅숏이 아직 수집되지 않음", CollectedAt: instance.CollectedAt})
		return
	}
	snapshot := history[0]
	instance.OperationalAt = &snapshot.CollectedAt
	age := s.now().Sub(snapshot.CollectedAt)
	threshold := s.Operations.FreshnessThreshold()
	if threshold <= 0 {
		threshold = 2 * time.Minute
	}
	instance.OperationalStatus = "current"
	if age > threshold {
		instance.OperationalStatus = "stale"
		promoteRisk(instance, StatusWarning, 45, "high")
		instance.Evidence = append(instance.Evidence, Evidence{Code: "OPERATIONAL_DATA_STALE", Severity: "high", Summary: "워크로드·용량 데이터가 설정된 수집주기의 2배보다 오래됨", Observed: map[string]any{"age_seconds": int64(age.Seconds()), "threshold_seconds": int64(threshold.Seconds())}, CollectedAt: instance.CollectedAt})
	}
	if len(warnings) > 0 || len(snapshot.Warnings) > 0 {
		promoteRisk(instance, StatusWarning, 40, "medium")
		instance.Evidence = append(instance.Evidence, Evidence{Code: "OPERATIONAL_DATA_PARTIAL", Severity: "medium", Summary: "워크로드·용량 스냅숏에 부분 수집 경고가 있음", Observed: map[string]any{"warning_count": len(warnings) + len(snapshot.Warnings)}, CollectedAt: instance.CollectedAt})
	}
	for _, finding := range snapshot.Evidence {
		switch finding.Code {
		case "CAPACITY_CRITICAL", "CAPACITY_EXHAUSTION_RISK":
			if finding.Severity == "critical" {
				promoteRisk(instance, StatusCritical, 90, "critical")
			} else {
				promoteRisk(instance, StatusWarning, 60, "high")
			}
			instance.Evidence = append(instance.Evidence, Evidence{Code: finding.Code, Severity: finding.Severity, Summary: finding.Summary, Observed: finding.Attributes, CollectedAt: snapshot.CollectedAt})
		case "CAPACITY_WARNING":
			promoteRisk(instance, StatusWarning, 55, "high")
			instance.Evidence = append(instance.Evidence, Evidence{Code: finding.Code, Severity: finding.Severity, Summary: finding.Summary, Observed: finding.Attributes, CollectedAt: snapshot.CollectedAt})
		}
	}
}

func promoteRisk(instance *Instance, status Status, score int, level string) {
	if score <= instance.RiskScore {
		return
	}
	instance.Status, instance.RiskScore, instance.RiskLevel = status, score, level
}

func (s *Service) contextFailure(p dbconn.Profile, err error) Instance {
	i := baseInstance(p, s.now())
	i.Status, i.CollectionStatus, i.RiskScore, i.RiskLevel = StatusUnknown, "cancelled", 50, "high"
	i.Failure = &Failure{Code: "COLLECTION_CANCELLED", Category: "collector", Message: err.Error()}
	i.Evidence = append(i.Evidence, Evidence{Code: "COLLECTION_CANCELLED", Severity: "high", Summary: "수집 컨텍스트가 종료됨", CollectedAt: i.CollectedAt})
	return i
}

func baseInstance(p dbconn.Profile, now time.Time) Instance {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = p.ID
	}
	return Instance{ID: p.ID, Name: name, ServiceName: p.ServiceName, Environment: p.Environment, Criticality: p.Criticality, Engine: p.Type, Role: p.Role, OwnerTeam: p.OwnerTeam, Location: p.Location, MaintenanceWindow: p.Maintenance, Tags: append([]string(nil), p.Tags...), Status: StatusHealthy, RiskLevel: "low", Evidence: []Evidence{}, CollectionStatus: "collecting", OperationalStatus: "not_collected", CollectedAt: now, Capabilities: map[string]bool{}}
}

func failureRisk(criticality string) int {
	switch strings.ToLower(criticality) {
	case "critical":
		return 100
	case "high":
		return 95
	case "low":
		return 70
	default:
		return 85
	}
}

func levelForScore(score int) string {
	switch {
	case score >= 80:
		return "critical"
	case score >= 50:
		return "high"
	case score >= 25:
		return "medium"
	default:
		return "low"
	}
}

func newSummary() Summary { return Summary{EngineDistribution: map[string]int{}} }

func addSummary(summary *Summary, i Instance) {
	summary.Total++
	summary.EngineDistribution[i.Engine]++
	switch i.Status {
	case StatusHealthy:
		summary.Healthy++
	case StatusWarning:
		summary.Warning++
	case StatusCritical:
		summary.Critical++
	case StatusFailed:
		summary.Failed++
	default:
		summary.Unknown++
	}
}

func limitations() []string {
	return []string{"플릿 위험도는 현재 연결·구성 상태, 저장된 워크로드·용량 근거, 실시간 복제·백업 상태를 결합합니다. 외부 백업 도구(pgBackRest·XtraBackup 등)의 잡 상태는 DB 서버가 보고하지 못하므로 포함되지 않습니다."}
}

func (s *Service) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

func traceID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("fleet-%d", time.Now().UnixNano())
	}
	return "fleet-" + hex.EncodeToString(b[:])
}
