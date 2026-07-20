package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/engine"
)

type Service struct {
	Queryer              SystemQueryer
	Providers            *Registry
	ReplicationProviders map[string]ReplicationProvider
	BackupProviders      map[string]BackupProvider
	SecurityProviders    map[string]SecurityProvider
	ConfigProviders      map[string]ConfigProvider
	MaintenanceProviders map[string]MaintenanceProvider
	Engines              *engine.Registry
	Now                  func() time.Time
}

// New builds the observation service. The provider maps come from
// adapters.ObservabilityProviders()/ReplicationProviders()/BackupProviders()
// — injected so this package never depends on concrete engine
// implementations.
func New(queryer SystemQueryer, providers map[string]Provider, replication map[string]ReplicationProvider, backup map[string]BackupProvider, security map[string]SecurityProvider, config map[string]ConfigProvider, maintenance map[string]MaintenanceProvider) *Service {
	replicationNormalized := make(map[string]ReplicationProvider, len(replication))
	for name, provider := range replication {
		replicationNormalized[strings.ToLower(strings.TrimSpace(name))] = provider
	}
	backupNormalized := make(map[string]BackupProvider, len(backup))
	for name, provider := range backup {
		backupNormalized[strings.ToLower(strings.TrimSpace(name))] = provider
	}
	securityNormalized := make(map[string]SecurityProvider, len(security))
	for name, provider := range security {
		securityNormalized[strings.ToLower(strings.TrimSpace(name))] = provider
	}
	configNormalized := make(map[string]ConfigProvider, len(config))
	for name, provider := range config {
		configNormalized[strings.ToLower(strings.TrimSpace(name))] = provider
	}
	maintenanceNormalized := make(map[string]MaintenanceProvider, len(maintenance))
	for name, provider := range maintenance {
		maintenanceNormalized[strings.ToLower(strings.TrimSpace(name))] = provider
	}
	return &Service{Queryer: queryer, Providers: NewRegistry(providers), ReplicationProviders: replicationNormalized, BackupProviders: backupNormalized, SecurityProviders: securityNormalized, ConfigProviders: configNormalized, MaintenanceProviders: maintenanceNormalized, Engines: engine.NewDefaultRegistry(), Now: time.Now}
}

func (s *Service) Sessions(ctx context.Context, raw dbconn.Profile) Response[SessionData] {
	p := dbconn.ApplyDefaults(raw)
	now := s.now()
	data := SessionData{ProfileID: p.ID, Engine: p.Type, Sessions: []Session{}}
	response := Response[SessionData]{Status: "ok", Data: data, Evidence: []Evidence{}, Warnings: []string{},
		Limitations: []string{"SQL 본문과 bind 값은 민감정보 노출을 막기 위해 세션 목록에 포함하지 않습니다."}, CollectedAt: now, TraceID: observationTraceID()}
	provider, reason := s.provider(p.Type, "sessions")
	if provider == nil {
		response.Status = "unsupported"
		response.Limitations = append(response.Limitations, reason)
		response.Evidence = append(response.Evidence, Evidence{Code: "SESSIONS_UNSUPPORTED", Severity: "warning", Summary: reason, CollectedAt: now})
		return response
	}
	items, err := provider.Sessions(ctx, s.Queryer, p)
	response.CollectedAt = s.now()
	if err != nil {
		return sessionError(response, err)
	}
	response.Data.Sessions = items
	response.Data.Total = len(items)
	if len(items) >= SnapshotRowLimit {
		response.Status = "warning"
		response.Limitations = append(response.Limitations, "세션 결과가 안전 상한 10,000행에 도달하여 일부 세션이 생략되었을 수 있습니다.")
		response.Evidence = append(response.Evidence, Evidence{Code: "SESSION_SNAPSHOT_TRUNCATED", Severity: "warning", Summary: "세션 스냅숏이 안전 상한에 도달함", Attributes: map[string]any{"row_limit": SnapshotRowLimit}, CollectedAt: response.CollectedAt})
	}
	for _, item := range items {
		state := strings.ToLower(item.State)
		if state == "active" || state == "query" {
			response.Data.Active++
		}
		if item.WaitEvent != "" && !strings.Contains(strings.ToLower(item.WaitClass), "idle") && !strings.Contains(strings.ToLower(item.WaitEvent), "client") {
			response.Data.Waiting++
		}
		if item.DurationSeconds >= 300 {
			response.Data.LongRunning++
		}
	}
	severity := "info"
	if response.Data.LongRunning > 0 || response.Data.Waiting > 0 {
		response.Status = "warning"
		severity = "warning"
	}
	response.Evidence = append(response.Evidence, Evidence{Code: "SESSION_SNAPSHOT_COLLECTED", Severity: severity,
		Summary: "세션 스냅숏 수집 완료", Attributes: map[string]any{"total": response.Data.Total, "active": response.Data.Active,
			"waiting": response.Data.Waiting, "long_running": response.Data.LongRunning, "long_running_threshold_seconds": 300}, CollectedAt: response.CollectedAt})
	return response
}

func (s *Service) Locks(ctx context.Context, raw dbconn.Profile) Response[LockData] {
	p := dbconn.ApplyDefaults(raw)
	now := s.now()
	data := LockData{ProfileID: p.ID, Engine: p.Type, Edges: []LockEdge{}, Roots: []LockRoot{}}
	response := Response[LockData]{Status: "ok", Data: data, Evidence: []Evidence{}, Warnings: []string{}, Limitations: []string{}, CollectedAt: now, TraceID: observationTraceID()}
	provider, reason := s.provider(p.Type, "locks")
	if provider == nil {
		response.Status = "unsupported"
		response.Limitations = append(response.Limitations, reason)
		response.Evidence = append(response.Evidence, Evidence{Code: "LOCKS_UNSUPPORTED", Severity: "warning", Summary: reason, CollectedAt: now})
		return response
	}
	edges, err := provider.Locks(ctx, s.Queryer, p)
	response.CollectedAt = s.now()
	if err != nil {
		return lockError(response, err)
	}
	response.Data.Edges = edges
	if len(edges) >= SnapshotRowLimit {
		response.Status = "warning"
		response.Limitations = append(response.Limitations, "잠금 결과가 안전 상한 10,000행에 도달하여 블로킹 그래프가 불완전할 수 있습니다.")
		response.Evidence = append(response.Evidence, Evidence{Code: "LOCK_SNAPSHOT_TRUNCATED", Severity: "warning", Summary: "잠금 스냅숏이 안전 상한에 도달함", Attributes: map[string]any{"row_limit": SnapshotRowLimit}, CollectedAt: response.CollectedAt})
	}
	response.Data.BlockedSessions = distinctBlocked(edges)
	response.Data.Roots = lockRoots(edges)
	severity := "info"
	code := "NO_LOCK_CONTENTION"
	summary := "현재 블로킹 관계 없음"
	if len(edges) > 0 {
		response.Status = "critical"
		severity, code, summary = "critical", "LOCK_CONTENTION_DETECTED", "블로킹 세션 관계 감지"
	}
	response.Evidence = append(response.Evidence, Evidence{Code: code, Severity: severity, Summary: summary,
		Attributes: map[string]any{"edges": len(edges), "blocked_sessions": response.Data.BlockedSessions, "root_blockers": len(response.Data.Roots)}, CollectedAt: response.CollectedAt})
	return response
}

// ReplicationLagWarnSeconds is the default threshold above which measured
// replication lag is reported as a warning.
const ReplicationLagWarnSeconds = 300

func (s *Service) Replication(ctx context.Context, raw dbconn.Profile) Response[ReplicationData] {
	p := dbconn.ApplyDefaults(raw)
	now := s.now()
	data := ReplicationData{ProfileID: p.ID, Engine: p.Type, Role: "unknown", Nodes: []ReplicationNode{}}
	response := Response[ReplicationData]{Status: "ok", Data: data, Evidence: []Evidence{}, Warnings: []string{}, Limitations: []string{}, CollectedAt: now, TraceID: observationTraceID()}
	provider, reason := s.replicationProvider(p.Type)
	if provider == nil {
		response.Status = "unsupported"
		response.Limitations = append(response.Limitations, reason)
		response.Evidence = append(response.Evidence, Evidence{Code: "REPLICATION_UNSUPPORTED", Severity: "warning", Summary: reason, CollectedAt: now})
		return response
	}
	result, err := provider.Replication(ctx, s.Queryer, p)
	response.CollectedAt = s.now()
	if err != nil {
		return replicationError(response, err)
	}
	if result.Nodes == nil {
		result.Nodes = []ReplicationNode{}
	}
	response.Warnings = append(response.Warnings, result.Warnings...)
	response.Limitations = append(response.Limitations, result.Limitations...)
	result.Warnings, result.Limitations = nil, nil
	response.Data = result

	unhealthy, lagging, lagUnknown := 0, 0, 0
	worstLag := 0.0
	for _, node := range result.Nodes {
		if !node.Healthy {
			unhealthy++
		}
		switch {
		case node.LagSeconds == LagUnknown:
			lagUnknown++
		case node.LagSeconds >= ReplicationLagWarnSeconds:
			lagging++
		}
		if node.LagSeconds > worstLag {
			worstLag = node.LagSeconds
		}
	}
	summaryAttrs := map[string]any{"role": result.Role, "nodes": len(result.Nodes), "unhealthy": unhealthy, "lagging": lagging, "worst_lag_seconds": worstLag}
	switch {
	case unhealthy > 0:
		response.Status = "critical"
		response.Evidence = append(response.Evidence, Evidence{Code: "REPLICATION_BROKEN", Severity: "critical", Summary: "복제 구성 요소가 비정상 상태입니다", Attributes: summaryAttrs, CollectedAt: response.CollectedAt})
	case lagging > 0:
		response.Status = "warning"
		response.Evidence = append(response.Evidence, Evidence{Code: "REPLICATION_LAG_HIGH", Severity: "warning", Summary: "복제 지연이 경고 임계값을 초과했습니다", Attributes: summaryAttrs, CollectedAt: response.CollectedAt})
	case result.Role == "standalone":
		response.Evidence = append(response.Evidence, Evidence{Code: "REPLICATION_NOT_CONFIGURED", Severity: "info", Summary: "복제 구성이 감지되지 않았습니다", Attributes: summaryAttrs, CollectedAt: response.CollectedAt})
	default:
		response.Evidence = append(response.Evidence, Evidence{Code: "REPLICATION_STATUS_COLLECTED", Severity: "info", Summary: "복제 상태 수집 완료", Attributes: summaryAttrs, CollectedAt: response.CollectedAt})
	}
	if lagUnknown > 0 {
		response.Limitations = append(response.Limitations, "일부 노드의 복제 지연을 측정할 수 없습니다 (lag_seconds = -1).")
	}
	if len(response.Warnings) > 0 && response.Status == "ok" {
		response.Status = "partial"
	}
	return response
}

func (s *Service) Backup(ctx context.Context, raw dbconn.Profile) Response[BackupData] {
	p := dbconn.ApplyDefaults(raw)
	now := s.now()
	data := BackupData{ProfileID: p.ID, Engine: p.Type, Archiving: "unknown", Items: []BackupItem{}}
	response := Response[BackupData]{Status: "ok", Data: data, Evidence: []Evidence{}, Warnings: []string{}, Limitations: []string{}, CollectedAt: now, TraceID: observationTraceID()}
	provider, reason := s.backupProvider(p.Type)
	if provider == nil {
		response.Status = "unsupported"
		response.Limitations = append(response.Limitations, reason)
		response.Evidence = append(response.Evidence, Evidence{Code: "BACKUP_UNSUPPORTED", Severity: "warning", Summary: reason, CollectedAt: now})
		return response
	}
	result, err := provider.Backup(ctx, s.Queryer, p)
	response.CollectedAt = s.now()
	if err != nil {
		return backupError(response, err)
	}
	if result.Items == nil {
		result.Items = []BackupItem{}
	}
	response.Warnings = append(response.Warnings, result.Warnings...)
	response.Limitations = append(response.Limitations, result.Limitations...)
	result.Warnings, result.Limitations = nil, nil
	response.Data = result

	unhealthy := 0
	for _, item := range result.Items {
		if !item.Healthy {
			unhealthy++
		}
	}
	attrs := map[string]any{"archiving": result.Archiving, "items": len(result.Items), "unhealthy": unhealthy, "last_success_at": result.LastSuccessAt, "last_failure_at": result.LastFailureAt}
	switch {
	case unhealthy > 0:
		response.Status = "critical"
		response.Evidence = append(response.Evidence, Evidence{Code: "BACKUP_FAILURE_DETECTED", Severity: "critical", Summary: "백업·아카이브 구성 요소가 비정상 상태입니다", Attributes: attrs, CollectedAt: response.CollectedAt})
	case result.Archiving == "disabled":
		response.Status = "warning"
		response.Evidence = append(response.Evidence, Evidence{Code: "BACKUP_PITR_DISABLED", Severity: "warning", Summary: "지속 아카이빙이 비활성화되어 시점 복구(PITR)가 불가능합니다", Attributes: attrs, CollectedAt: response.CollectedAt})
	default:
		response.Evidence = append(response.Evidence, Evidence{Code: "BACKUP_STATUS_COLLECTED", Severity: "info", Summary: "백업 상태 수집 완료", Attributes: attrs, CollectedAt: response.CollectedAt})
	}
	if len(response.Warnings) > 0 && response.Status == "ok" {
		response.Status = "partial"
	}
	return response
}

func (s *Service) Security(ctx context.Context, raw dbconn.Profile) Response[SecurityData] {
	p := dbconn.ApplyDefaults(raw)
	now := s.now()
	data := SecurityData{ProfileID: p.ID, Engine: p.Type, Findings: []SecurityFinding{}}
	response := Response[SecurityData]{Status: "ok", Data: data, Evidence: []Evidence{}, Warnings: []string{}, Limitations: []string{}, CollectedAt: now, TraceID: observationTraceID()}
	provider, reason := s.securityProvider(p.Type)
	if provider == nil {
		response.Status = "unsupported"
		response.Limitations = append(response.Limitations, reason)
		response.Evidence = append(response.Evidence, Evidence{Code: "SECURITY_UNSUPPORTED", Severity: "warning", Summary: reason, CollectedAt: now})
		return response
	}
	result, err := provider.Security(ctx, s.Queryer, p)
	response.CollectedAt = s.now()
	if err != nil {
		return securityError(response, err)
	}
	if result.Findings == nil {
		result.Findings = []SecurityFinding{}
	}
	response.Warnings = append(response.Warnings, result.Warnings...)
	response.Limitations = append(response.Limitations, result.Limitations...)
	result.Warnings, result.Limitations = nil, nil
	response.Data = result

	critical, warning := 0, 0
	for _, finding := range result.Findings {
		switch finding.Severity {
		case "critical":
			critical++
		case "warning":
			warning++
		}
	}
	attrs := map[string]any{"principals": result.Principals, "findings": len(result.Findings), "critical": critical, "warning": warning}
	switch {
	case critical > 0:
		response.Status = "critical"
		response.Evidence = append(response.Evidence, Evidence{Code: "SECURITY_EXCESS_PRIVILEGE", Severity: "critical", Summary: "치명적 권한 과다 항목이 발견되었습니다", Attributes: attrs, CollectedAt: response.CollectedAt})
	case warning > 0:
		response.Status = "warning"
		response.Evidence = append(response.Evidence, Evidence{Code: "SECURITY_EXCESS_PRIVILEGE", Severity: "warning", Summary: "권한 과다 의심 항목이 발견되었습니다", Attributes: attrs, CollectedAt: response.CollectedAt})
	default:
		response.Evidence = append(response.Evidence, Evidence{Code: "SECURITY_POSTURE_COLLECTED", Severity: "info", Summary: "사용자·권한 진단 수집 완료", Attributes: attrs, CollectedAt: response.CollectedAt})
	}
	if len(response.Warnings) > 0 && response.Status == "ok" {
		response.Status = "partial"
	}
	return response
}

// Maintenance runs proactive-maintenance risk detection: transaction-ID
// wraparound, table/index bloat, and inactive replication slots retaining WAL.
// These are latent risks that report no error until they cause an outage, so
// they are surfaced early with severity and a change-plan recommendation.
func (s *Service) Maintenance(ctx context.Context, raw dbconn.Profile) Response[MaintenanceData] {
	p := dbconn.ApplyDefaults(raw)
	now := s.now()
	data := MaintenanceData{ProfileID: p.ID, Engine: p.Type, Findings: []MaintenanceFinding{}}
	response := Response[MaintenanceData]{Status: "ok", Data: data, Evidence: []Evidence{}, Warnings: []string{}, Limitations: []string{}, CollectedAt: now, TraceID: observationTraceID()}
	provider, reason := s.maintenanceProvider(p.Type)
	if provider == nil {
		response.Status = "unsupported"
		response.Limitations = append(response.Limitations, reason)
		response.Evidence = append(response.Evidence, Evidence{Code: "MAINTENANCE_UNSUPPORTED", Severity: "warning", Summary: reason, CollectedAt: now})
		return response
	}
	result, err := provider.Maintenance(ctx, s.Queryer, p)
	response.CollectedAt = s.now()
	if err != nil {
		status, code, hint := classifyCollectionError(err)
		response.Status = status
		response.Warnings = append(response.Warnings, hint)
		response.Evidence = append(response.Evidence, Evidence{Code: code, Severity: "warning", Summary: hint, CollectedAt: response.CollectedAt})
		return response
	}
	if result.Findings == nil {
		result.Findings = []MaintenanceFinding{}
	}
	response.Warnings = append(response.Warnings, result.Warnings...)
	response.Limitations = append(response.Limitations, result.Limitations...)
	result.Warnings, result.Limitations = nil, nil
	response.Data = result

	critical, warning := 0, 0
	for _, finding := range result.Findings {
		switch finding.Severity {
		case "critical":
			critical++
		case "warning":
			warning++
		}
	}
	attrs := map[string]any{"checks": result.Checks, "findings": len(result.Findings), "critical": critical, "warning": warning}
	switch {
	case critical > 0:
		response.Status = "critical"
		response.Evidence = append(response.Evidence, Evidence{Code: "MAINTENANCE_RISK_CRITICAL", Severity: "critical", Summary: "즉시 조치가 필요한 예방 점검 위험이 있습니다 (wraparound 임박 등)", Attributes: attrs, CollectedAt: response.CollectedAt})
	case warning > 0:
		response.Status = "warning"
		response.Evidence = append(response.Evidence, Evidence{Code: "MAINTENANCE_RISK_WARNING", Severity: "warning", Summary: "예방 점검 대상(블로트·비활성 슬롯·freeze 임박)이 발견되었습니다", Attributes: attrs, CollectedAt: response.CollectedAt})
	default:
		response.Evidence = append(response.Evidence, Evidence{Code: "MAINTENANCE_HEALTHY", Severity: "info", Summary: "예방 점검 위험이 발견되지 않았습니다", Attributes: attrs, CollectedAt: response.CollectedAt})
	}
	if len(response.Warnings) > 0 && response.Status == "ok" {
		response.Status = "partial"
	}
	return response
}

func (s *Service) maintenanceProvider(engineName string) (MaintenanceProvider, string) {
	adapter, ok := s.Engines.Get(engineName)
	if !ok {
		return nil, "등록되지 않은 데이터베이스 엔진입니다."
	}
	if !adapter.Capabilities.Maintenance {
		return nil, "maintenance 기능을 엔진 Capability가 지원하지 않습니다."
	}
	provider, ok := s.MaintenanceProviders[strings.ToLower(strings.TrimSpace(engineName))]
	if !ok {
		return nil, "maintenance Provider가 이 배포판에 등록되지 않았습니다."
	}
	return provider, ""
}

// ConfigDrift compares live server parameters against the profile's declared
// baseline. Only baseline keys are checked; an empty baseline yields an
// explicit "not configured" response rather than a false all-clear.
func (s *Service) ConfigDrift(ctx context.Context, raw dbconn.Profile) Response[ConfigDriftData] {
	p := dbconn.ApplyDefaults(raw)
	now := s.now()
	data := ConfigDriftData{ProfileID: p.ID, Engine: p.Type, Items: []ConfigDriftItem{}}
	response := Response[ConfigDriftData]{Status: "ok", Data: data, Evidence: []Evidence{}, Warnings: []string{}, Limitations: []string{}, CollectedAt: now, TraceID: observationTraceID()}
	if len(p.ConfigBaseline) == 0 {
		response.Status = "not_configured"
		response.Limitations = append(response.Limitations, "이 프로파일에 config_baseline이 선언되지 않아 드리프트를 검사하지 않습니다.")
		response.Evidence = append(response.Evidence, Evidence{Code: "CONFIG_BASELINE_ABSENT", Severity: "info", Summary: "설정 베이스라인 미선언", CollectedAt: now})
		return response
	}
	provider, ok := s.ConfigProviders[strings.ToLower(strings.TrimSpace(p.Type))]
	if !ok {
		response.Status = "unsupported"
		reason := "config Provider가 이 배포판에 등록되지 않았습니다."
		response.Limitations = append(response.Limitations, reason)
		response.Evidence = append(response.Evidence, Evidence{Code: "CONFIG_UNSUPPORTED", Severity: "warning", Summary: reason, CollectedAt: now})
		return response
	}
	values, pending, err := provider.Config(ctx, s.Queryer, p)
	response.CollectedAt = s.now()
	if err != nil {
		status, code, hint := classifyCollectionError(err)
		response.Status = status
		response.Warnings = append(response.Warnings, hint)
		response.Evidence = append(response.Evidence, Evidence{Code: code, Severity: "warning", Summary: hint, CollectedAt: response.CollectedAt})
		return response
	}
	// Deterministic order for stable output and testing.
	keys := make([]string, 0, len(p.ConfigBaseline))
	for k := range p.ConfigBaseline {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		want := strings.TrimSpace(p.ConfigBaseline[key])
		current, present := lookupFold(values, key)
		status := "match"
		switch {
		case !present:
			status = "unknown"
		case !configValueEqual(current, want):
			status = "drift"
			data.Drifted++
		}
		data.Checked++
		data.Items = append(data.Items, ConfigDriftItem{Parameter: key, Baseline: want, Current: current, Status: status, PendingRestart: pending[strings.ToLower(key)], CollectedAt: response.CollectedAt})
	}
	response.Data = data
	attrs := map[string]any{"checked": data.Checked, "drifted": data.Drifted}
	if data.Drifted > 0 {
		response.Status = "warning"
		response.Evidence = append(response.Evidence, Evidence{Code: "CONFIG_DRIFT_DETECTED", Severity: "warning", Summary: "선언된 베이스라인과 다른 서버 파라미터가 있습니다", Attributes: attrs, CollectedAt: response.CollectedAt})
	} else {
		response.Evidence = append(response.Evidence, Evidence{Code: "CONFIG_IN_SYNC", Severity: "info", Summary: "서버 파라미터가 베이스라인과 일치합니다", Attributes: attrs, CollectedAt: response.CollectedAt})
	}
	return response
}

// configValueEqual compares parameter values tolerantly: case-insensitive and
// with common boolean synonyms normalized (on/off ↔ true/false ↔ 1/0).
func configValueEqual(a, b string) bool {
	na, nb := normalizeConfigValue(a), normalizeConfigValue(b)
	return na == nb
}

func normalizeConfigValue(v string) string {
	s := strings.ToLower(strings.TrimSpace(v))
	switch s {
	case "on", "true", "yes", "1":
		return "true"
	case "off", "false", "no", "0":
		return "false"
	}
	return s
}

func lookupFold(values map[string]string, key string) (string, bool) {
	if v, ok := values[key]; ok {
		return v, true
	}
	lk := strings.ToLower(strings.TrimSpace(key))
	for k, v := range values {
		if strings.ToLower(strings.TrimSpace(k)) == lk {
			return v, true
		}
	}
	return "", false
}

func (s *Service) securityProvider(engineName string) (SecurityProvider, string) {
	adapter, ok := s.Engines.Get(engineName)
	if !ok {
		return nil, "등록되지 않은 데이터베이스 엔진입니다."
	}
	if !adapter.Capabilities.UserManagement {
		return nil, "user_management 기능을 엔진 Capability가 지원하지 않습니다."
	}
	provider, ok := s.SecurityProviders[strings.ToLower(strings.TrimSpace(engineName))]
	if !ok {
		return nil, "security Provider가 이 배포판에 등록되지 않았습니다."
	}
	return provider, ""
}

func securityError(response Response[SecurityData], err error) Response[SecurityData] {
	status, code, hint := classifyCollectionError(err)
	response.Status = status
	response.Warnings = append(response.Warnings, hint)
	response.Evidence = append(response.Evidence, Evidence{Code: code, Severity: "warning", Summary: hint, CollectedAt: response.CollectedAt})
	return response
}

func (s *Service) backupProvider(engineName string) (BackupProvider, string) {
	adapter, ok := s.Engines.Get(engineName)
	if !ok {
		return nil, "등록되지 않은 데이터베이스 엔진입니다."
	}
	if !adapter.Capabilities.BackupStatus {
		return nil, "backup_status 기능을 엔진 Capability가 지원하지 않습니다."
	}
	provider, ok := s.BackupProviders[strings.ToLower(strings.TrimSpace(engineName))]
	if !ok {
		return nil, "backup Provider가 이 배포판에 등록되지 않았습니다."
	}
	return provider, ""
}

func backupError(response Response[BackupData], err error) Response[BackupData] {
	status, code, hint := classifyCollectionError(err)
	response.Status = status
	response.Warnings = append(response.Warnings, hint)
	response.Evidence = append(response.Evidence, Evidence{Code: code, Severity: "warning", Summary: hint, CollectedAt: response.CollectedAt})
	return response
}

func (s *Service) replicationProvider(engineName string) (ReplicationProvider, string) {
	adapter, ok := s.Engines.Get(engineName)
	if !ok {
		return nil, "등록되지 않은 데이터베이스 엔진입니다."
	}
	if !adapter.Capabilities.Replication {
		return nil, "replication 기능을 엔진 Capability가 지원하지 않습니다."
	}
	provider, ok := s.ReplicationProviders[strings.ToLower(strings.TrimSpace(engineName))]
	if !ok {
		return nil, "replication Provider가 이 배포판에 등록되지 않았습니다."
	}
	return provider, ""
}

func replicationError(response Response[ReplicationData], err error) Response[ReplicationData] {
	status, code, hint := classifyCollectionError(err)
	response.Status = status
	response.Warnings = append(response.Warnings, hint)
	response.Evidence = append(response.Evidence, Evidence{Code: code, Severity: "warning", Summary: hint, CollectedAt: response.CollectedAt})
	return response
}

func (s *Service) provider(engineName, capability string) (Provider, string) {
	adapter, ok := s.Engines.Get(engineName)
	if !ok {
		return nil, "등록되지 않은 데이터베이스 엔진입니다."
	}
	supported := adapter.Capabilities.Sessions
	if capability == "locks" {
		supported = adapter.Capabilities.LockTree
	}
	if !supported {
		return nil, capability + " 기능을 엔진 Capability가 지원하지 않습니다."
	}
	provider, ok := s.Providers.Get(engineName)
	if !ok {
		return nil, capability + " Provider가 이 배포판에 등록되지 않았습니다."
	}
	return provider, ""
}

func sessionError(response Response[SessionData], err error) Response[SessionData] {
	status, code, hint := classifyCollectionError(err)
	response.Status = status
	response.Warnings = append(response.Warnings, hint)
	response.Evidence = append(response.Evidence, Evidence{Code: code, Severity: "warning", Summary: hint, CollectedAt: response.CollectedAt})
	return response
}

func lockError(response Response[LockData], err error) Response[LockData] {
	status, code, hint := classifyCollectionError(err)
	response.Status = status
	response.Warnings = append(response.Warnings, hint)
	response.Evidence = append(response.Evidence, Evidence{Code: code, Severity: "warning", Summary: hint, CollectedAt: response.CollectedAt})
	return response
}

func classifyCollectionError(err error) (string, string, string) {
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "license policy") || strings.Contains(message, "oracle license") {
		return "policy_blocked", "LICENSE_POLICY_BLOCKED", "라이선스 정책이 이 시스템 뷰 수집을 차단했습니다."
	}
	permissionTokens := []string{"permission denied", "access denied", "insufficient privilege", "ora-00942", "ora-01031", "command denied"}
	for _, token := range permissionTokens {
		if strings.Contains(message, token) {
			return "permission_denied", "COLLECTION_PERMISSION_DENIED", "시스템 뷰 조회 권한이 부족합니다. 모니터링 계정의 최소 조회 권한을 확인하세요."
		}
	}
	if strings.Contains(message, "driver") && (strings.Contains(message, "not included") || strings.Contains(message, "not compiled")) {
		return "unsupported_edition", "DRIVER_UNAVAILABLE", "현재 SQLON 배포판에 대상 엔진 드라이버가 없습니다."
	}
	return "error", "COLLECTION_FAILED", "운영 시스템 뷰 수집에 실패했습니다: " + err.Error()
}

func distinctBlocked(edges []LockEdge) int {
	seen := map[string]struct{}{}
	for _, edge := range edges {
		if edge.BlockedKey != "" {
			seen[edge.BlockedKey] = struct{}{}
		}
	}
	return len(seen)
}

func lockRoots(edges []LockEdge) []LockRoot {
	children := map[string][]string{}
	blocked := map[string]struct{}{}
	users := map[string]string{}
	for _, edge := range edges {
		if edge.BlockerKey == "" || edge.BlockedKey == "" {
			continue
		}
		children[edge.BlockerKey] = append(children[edge.BlockerKey], edge.BlockedKey)
		blocked[edge.BlockedKey] = struct{}{}
		if edge.BlockerUser != "" {
			users[edge.BlockerKey] = edge.BlockerUser
		}
	}
	roots := make([]LockRoot, 0)
	for key := range children {
		if _, isBlocked := blocked[key]; isBlocked {
			continue
		}
		roots = append(roots, LockRoot{SessionKey: key, User: users[key], AffectedSessions: descendantCount(key, children)})
	}
	// Defensive fallback for a malformed/cyclic graph: preserve evidence rather
	// than falsely reporting that no root blocker exists.
	if len(roots) == 0 && len(children) > 0 {
		for key := range children {
			roots = append(roots, LockRoot{SessionKey: key, User: users[key], AffectedSessions: descendantCount(key, children)})
		}
	}
	sort.Slice(roots, func(i, j int) bool {
		if roots[i].AffectedSessions != roots[j].AffectedSessions {
			return roots[i].AffectedSessions > roots[j].AffectedSessions
		}
		return roots[i].SessionKey < roots[j].SessionKey
	})
	return roots
}

func descendantCount(root string, children map[string][]string) int {
	seen := map[string]struct{}{root: {}}
	var visit func(string)
	visit = func(key string) {
		for _, child := range children[key] {
			if _, ok := seen[child]; ok {
				continue
			}
			seen[child] = struct{}{}
			visit(child)
		}
	}
	visit(root)
	return len(seen) - 1
}

func (s *Service) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

func observationTraceID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("observe-%d", time.Now().UnixNano())
	}
	return "observe-" + hex.EncodeToString(b[:])
}
