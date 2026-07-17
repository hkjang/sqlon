package collector

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/engine"
	"sqlon/internal/storage"
)

const SnapshotKind = "workload_capacity"

type profileSource interface {
	Profiles(context.Context) ([]dbconn.Profile, error)
}

type Service struct {
	Queryer     SystemQueryer
	Source      profileSource
	Store       storage.OperationalStore
	Providers   *Registry
	Engines     *engine.Registry
	Now         func() time.Time
	Concurrency int
	// ExpectedInterval is used only to report freshness. The runtime sets it
	// before serving requests; collection scheduling remains owned by the app.
	ExpectedInterval time.Duration
	AlertWebhookURL  string
}

// New builds the collection service. providers is the engine-name→provider
// map, normally adapters.CollectorProviders() — injected so this package
// never depends on concrete engine implementations.
func New(manager *dbconn.Manager, store storage.OperationalStore, providers map[string]Provider) *Service {
	return &Service{Queryer: manager, Source: manager, Store: store, Providers: NewRegistry(providers), Engines: engine.NewDefaultRegistry(), Now: time.Now, Concurrency: 4, ExpectedInterval: time.Minute, AlertWebhookURL: ""}
}

// FreshnessThreshold returns the maximum accepted snapshot age. SQLON's
// freshness SLO is twice the configured collection interval.
func (s *Service) FreshnessThreshold() time.Duration {
	if s == nil || s.ExpectedInterval <= 0 {
		return 2 * time.Minute
	}
	return 2 * s.ExpectedInterval
}

func (s *Service) CollectProfile(ctx context.Context, raw dbconn.Profile, persist bool) ProfileResult {
	p := dbconn.ApplyDefaults(raw)
	now := s.now()
	result := ProfileResult{Status: "ok", Snapshot: emptySnapshot(p, now), CollectedAt: now}
	adapter, ok := s.Engines.Get(p.Type)
	if !ok || (!adapter.Capabilities.Workload && !adapter.Capabilities.Storage) {
		result.Status, result.ErrorCode, result.Error = "unsupported", "CAPABILITY_UNSUPPORTED", "워크로드·용량 Capability가 지원되지 않습니다."
		return result
	}
	provider, ok := s.Providers.Get(p.Type)
	if !ok {
		result.Status, result.ErrorCode, result.Error = "unsupported", "PROVIDER_UNAVAILABLE", "워크로드·용량 Provider가 등록되지 않았습니다."
		return result
	}
	snapshot, err := provider.Collect(ctx, s.Queryer, p)
	snapshot.CollectedAt = s.now()
	snapshot.TraceID = collectorTraceID()
	if snapshot.Counters == nil {
		snapshot.Counters = []Metric{}
	}
	if snapshot.Rates == nil {
		snapshot.Rates = map[string]float64{}
	}
	if snapshot.Waits == nil {
		snapshot.Waits = []Wait{}
	}
	if snapshot.TopSQL == nil {
		snapshot.TopSQL = []SQLStat{}
	}
	if snapshot.Capacity == nil {
		snapshot.Capacity = []Capacity{}
	}
	if snapshot.Evidence == nil {
		snapshot.Evidence = []Evidence{}
	}
	if snapshot.Warnings == nil {
		snapshot.Warnings = []string{}
	}
	if snapshot.Limitations == nil {
		snapshot.Limitations = []string{}
	}
	result.Snapshot, result.CollectedAt = snapshot, snapshot.CollectedAt
	if err != nil {
		result.Status, result.ErrorCode, result.Error = classifyError(err)
		result.Snapshot.Evidence = append(result.Snapshot.Evidence, Evidence{Code: result.ErrorCode, Severity: "warning", Summary: result.Error, CollectedAt: snapshot.CollectedAt})
		return result
	}

	previous, previousWarnings := s.previous(ctx, p.ID)
	result.Snapshot.Warnings = append(result.Snapshot.Warnings, previousWarnings...)
	if previous != nil {
		deriveRates(&result.Snapshot, *previous)
	} else {
		result.Snapshot.Limitations = append(result.Snapshot.Limitations, "첫 스냅숏이므로 QPS/TPS와 용량 증가율은 다음 수집부터 계산됩니다.")
	}
	addEvidence(&result.Snapshot)
	RunAlertingEngine(ctx, &result.Snapshot, previous, s.AlertWebhookURL)
	if len(result.Snapshot.Warnings) > 0 || len(result.Snapshot.Limitations) > 0 {
		result.Status = "partial"
	}
	if persist {
		payload, marshalErr := json.Marshal(result.Snapshot)
		if marshalErr != nil {
			result.Status, result.ErrorCode, result.Error = "error", "SNAPSHOT_ENCODE_FAILED", marshalErr.Error()
			return result
		}
		if appendErr := s.Store.Append(ctx, storage.Record{Kind: SnapshotKind, ProfileID: p.ID, Engine: p.Type, CollectedAt: result.Snapshot.CollectedAt, Data: payload}); appendErr != nil {
			result.Status, result.ErrorCode, result.Error = "partial", "SNAPSHOT_STORE_FAILED", appendErr.Error()
			result.Snapshot.Warnings = append(result.Snapshot.Warnings, "수집에는 성공했지만 운영 저장소 기록에 실패했습니다.")
			return result
		}
		result.Persisted = true
	}
	return result
}

func (s *Service) CollectAll(ctx context.Context, profiles []dbconn.Profile, persist bool) BatchResult {
	now := s.now()
	batch := BatchResult{Status: "ok", Results: []ProfileResult{}, CollectedAt: now, TraceID: collectorTraceID()}
	if profiles == nil {
		loaded, err := s.Source.Profiles(ctx)
		if err != nil {
			batch.Status, batch.Failed = "error", 1
			batch.Results = append(batch.Results, ProfileResult{Status: "error", ErrorCode: "PROFILE_LOAD_FAILED", Error: err.Error(), CollectedAt: now})
			return batch
		}
		profiles = loaded
	}
	limit := s.Concurrency
	if limit <= 0 {
		limit = 4
	}
	sem := make(chan struct{}, limit)
	results := make(chan ProfileResult, len(profiles))
	var wg sync.WaitGroup
	for _, profile := range profiles {
		p := profile
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results <- ProfileResult{Status: "error", ErrorCode: "COLLECTION_CANCELLED", Error: ctx.Err().Error(), CollectedAt: s.now()}
				return
			}
			results <- s.CollectProfile(ctx, p, persist)
		}()
	}
	wg.Wait()
	close(results)
	for result := range results {
		batch.Results = append(batch.Results, result)
		if result.Status == "ok" || result.Status == "partial" {
			batch.Succeeded++
		} else {
			batch.Failed++
		}
	}
	sort.Slice(batch.Results, func(i, j int) bool { return batch.Results[i].Snapshot.ProfileID < batch.Results[j].Snapshot.ProfileID })
	if batch.Failed > 0 {
		batch.Status = "degraded"
	}
	batch.CollectedAt = s.now()
	return batch
}

func (s *Service) History(ctx context.Context, profileID string, since time.Time, limit int) ([]Snapshot, []string, error) {
	result, err := s.Store.Query(ctx, storage.Query{Kind: SnapshotKind, ProfileID: profileID, Since: since, Limit: limit})
	if err != nil {
		return nil, nil, err
	}
	out := make([]Snapshot, 0, len(result.Records))
	for _, record := range result.Records {
		var snapshot Snapshot
		if json.Unmarshal(record.Data, &snapshot) != nil {
			result.Warnings = append(result.Warnings, "스냅숏 payload를 해석하지 못했습니다: "+record.CollectedAt.Format(time.RFC3339))
			continue
		}
		out = append(out, snapshot)
	}
	return out, result.Warnings, nil
}

func (s *Service) previous(ctx context.Context, profileID string) (*Snapshot, []string) {
	history, warnings, err := s.History(ctx, profileID, time.Time{}, 1)
	if err != nil {
		return nil, []string{"이전 스냅숏 조회 실패: " + err.Error()}
	}
	if len(history) == 0 {
		return nil, warnings
	}
	return &history[0], warnings
}

func deriveRates(current *Snapshot, previous Snapshot) {
	seconds := current.CollectedAt.Sub(previous.CollectedAt).Seconds()
	if seconds <= 0 {
		current.Limitations = append(current.Limitations, "이전 스냅숏과 시간 순서가 역전되어 변화율을 계산하지 않았습니다.")
		return
	}
	prior := map[string]Metric{}
	for _, metric := range previous.Counters {
		prior[metric.Name] = metric
	}
	for _, metric := range current.Counters {
		old, ok := prior[metric.Name]
		if !ok || !metric.Cumulative || metric.Value < old.Value {
			continue
		}
		name := metric.Name + "_per_second"
		switch metric.Name {
		case "queries":
			name = "qps"
		case "transactions":
			name = "tps"
		}
		current.Rates[name] = round((metric.Value - old.Value) / seconds)
	}
	if _, ok := current.Rates["tps"]; !ok {
		current.Rates["tps"] = sumRate(current.Rates, "commits_per_second", "rollbacks_per_second")
	}
	priorCapacity := map[string]Capacity{}
	for _, capacity := range previous.Capacity {
		priorCapacity[capacity.Scope+"\x00"+capacity.Name] = capacity
	}
	for _, capacity := range current.Capacity {
		old, ok := priorCapacity[capacity.Scope+"\x00"+capacity.Name]
		if !ok {
			continue
		}
		current.Rates["capacity_growth_bytes_per_day:"+capacity.Scope+":"+capacity.Name] = round((capacity.UsedBytes - old.UsedBytes) / seconds * 86400)
	}
}

func addEvidence(snapshot *Snapshot) {
	now := snapshot.CollectedAt
	snapshot.Evidence = append(snapshot.Evidence, Evidence{Code: "WORKLOAD_COLLECTED", Severity: "info", Summary: "대상 DB 누적 카운터와 대기 이벤트 수집 완료", Attributes: map[string]any{"counters": len(snapshot.Counters), "waits": len(snapshot.Waits), "top_sql": len(snapshot.TopSQL)}, CollectedAt: now})
	worst := 0.0
	worstName := ""
	for _, capacity := range snapshot.Capacity {
		if capacity.UsagePercent > worst {
			worst, worstName = capacity.UsagePercent, capacity.Name
		}
	}
	severity, code := "info", "CAPACITY_COLLECTED"
	if worst >= 90 {
		severity, code = "critical", "CAPACITY_CRITICAL"
	} else if worst >= 80 {
		severity, code = "warning", "CAPACITY_WARNING"
	}
	snapshot.Evidence = append(snapshot.Evidence, Evidence{Code: code, Severity: severity, Summary: "용량 스냅숏 수집 완료", Attributes: map[string]any{"assets": len(snapshot.Capacity), "highest_usage_percent": worst, "highest_usage_asset": worstName}, CollectedAt: now})
	for _, capacity := range snapshot.Capacity {
		growth := snapshot.Rates["capacity_growth_bytes_per_day:"+capacity.Scope+":"+capacity.Name]
		if growth <= 0 || capacity.MaxBytes <= capacity.UsedBytes {
			continue
		}
		days := (capacity.MaxBytes - capacity.UsedBytes) / growth
		if days <= 30 {
			severity := "warning"
			if days <= 7 {
				severity = "critical"
			}
			snapshot.Evidence = append(snapshot.Evidence, Evidence{Code: "CAPACITY_EXHAUSTION_RISK", Severity: severity, Summary: "현재 증가율 기준 30일 내 용량 고갈 가능", Attributes: map[string]any{"scope": capacity.Scope, "asset": capacity.Name, "days_to_exhaustion": round(days), "growth_bytes_per_day": growth}, CollectedAt: now})
		}
	}
}

func classifyError(err error) (string, string, string) {
	message := strings.ToLower(err.Error())
	for _, token := range []string{"permission denied", "access denied", "ora-00942", "ora-01031", "command denied"} {
		if strings.Contains(message, token) {
			return "permission_denied", "COLLECTION_PERMISSION_DENIED", "워크로드 시스템 뷰 조회 권한이 부족합니다."
		}
	}
	if strings.Contains(message, "license") {
		return "policy_blocked", "LICENSE_POLICY_BLOCKED", "Oracle 라이선스 정책이 수집을 차단했습니다."
	}
	if strings.Contains(message, "driver") && (strings.Contains(message, "not included") || strings.Contains(message, "not compiled")) {
		return "unsupported_edition", "DRIVER_UNAVAILABLE", "현재 배포판에 대상 엔진 드라이버가 없습니다."
	}
	return "error", "COLLECTION_FAILED", err.Error()
}

func emptySnapshot(p dbconn.Profile, now time.Time) Snapshot {
	return Snapshot{ProfileID: p.ID, Engine: p.Type, Counters: []Metric{}, Rates: map[string]float64{}, Waits: []Wait{}, TopSQL: []SQLStat{}, Capacity: []Capacity{}, Evidence: []Evidence{}, Warnings: []string{}, Limitations: []string{}, CollectedAt: now, TraceID: collectorTraceID()}
}

func (s *Service) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

func collectorTraceID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("collect-%d", time.Now().UnixNano())
	}
	return "collect-" + hex.EncodeToString(b[:])
}

func sumRate(rates map[string]float64, names ...string) float64 {
	total := 0.0
	for _, name := range names {
		total += rates[name]
	}
	return round(total)
}

func round(value float64) float64 { return math.Round(value*1000) / 1000 }
