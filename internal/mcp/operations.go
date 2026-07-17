package mcp

import (
	"context"
	"log"
	"strings"
	"time"

	"sqlon/internal/collector"
	"sqlon/internal/dbconn"
)

func (s *Server) operationalSnapshot(ctx context.Context, profile dbconn.Profile, fresh bool) collector.ProfileResult {
	if !fresh {
		history, warnings, err := s.Collector.History(ctx, profile.ID, time.Time{}, 1)
		if err == nil && len(history) > 0 {
			snapshot := history[0]
			snapshot.Warnings = append(snapshot.Warnings, warnings...)
			status := "ok"
			if len(snapshot.Warnings) > 0 || len(snapshot.Limitations) > 0 {
				status = "partial"
			}
			return collector.ProfileResult{Status: status, Snapshot: snapshot, Persisted: true, CollectedAt: snapshot.CollectedAt}
		}
		now := time.Now().UTC()
		snapshot := collector.Snapshot{ProfileID: profile.ID, Engine: profile.Type, Counters: []collector.Metric{}, Rates: map[string]float64{}, Waits: []collector.Wait{}, TopSQL: []collector.SQLStat{}, Capacity: []collector.Capacity{}, Evidence: []collector.Evidence{}, Warnings: warnings, Limitations: []string{"저장된 운영 스냅숏이 없습니다. 주기 수집을 기다리거나 fresh=true로 명시적으로 수집하세요."}, CollectedAt: now, TraceID: "not-collected-" + profile.ID}
		if err != nil {
			snapshot.Warnings = append(snapshot.Warnings, "운영 저장소 조회 실패: "+err.Error())
		}
		return collector.ProfileResult{Status: "not_collected", Snapshot: snapshot, CollectedAt: now}
	}
	return s.Collector.CollectProfile(ctx, profile, true)
}

func operationalView(kind string, result collector.ProfileResult) map[string]any {
	snapshot := result.Snapshot
	data := map[string]any{"profile_id": snapshot.ProfileID, "engine": snapshot.Engine}
	switch kind {
	case "workload":
		data["counters"], data["rates"], data["waits"] = snapshot.Counters, snapshot.Rates, snapshot.Waits
	case "top_sql":
		data["top_sql"] = snapshot.TopSQL
	case "capacity":
		data["capacity"] = snapshot.Capacity
		growth := map[string]float64{}
		exhaustion := map[string]float64{}
		for key, value := range snapshot.Rates {
			if strings.HasPrefix(key, "capacity_growth_bytes_per_day:") {
				growth[strings.TrimPrefix(key, "capacity_growth_bytes_per_day:")] = value
			}
		}
		for _, capacity := range snapshot.Capacity {
			key := capacity.Scope + ":" + capacity.Name
			if daily := growth[key]; daily > 0 && capacity.MaxBytes > capacity.UsedBytes {
				exhaustion[key] = (capacity.MaxBytes - capacity.UsedBytes) / daily
			}
		}
		data["growth_bytes_per_day"] = growth
		data["days_to_exhaustion"] = exhaustion
	}
	if result.Error != "" {
		data["error_code"], data["error"] = result.ErrorCode, result.Error
	}
	return map[string]any{
		"status": result.Status, "data": data, "evidence": snapshot.Evidence,
		"warnings": snapshot.Warnings, "limitations": snapshot.Limitations,
		"collected_at": snapshot.CollectedAt, "trace_id": snapshot.TraceID,
	}
}

func (s *Server) mcpOperational(ctx context.Context, profileID, kind string, fresh bool) map[string]any {
	profiles, err := s.usableProfiles(ctx)
	if err != nil {
		return map[string]any{"status": "error", "warnings": []string{err.Error()}, "data": map[string]any{}}
	}
	profile, ok := allowedProfile(profiles, profileID)
	if !ok {
		return map[string]any{"status": "not_found", "warnings": []string{"db profile not found or not permitted"}, "data": map[string]any{}}
	}
	return operationalView(kind, s.operationalSnapshot(ctx, profile, fresh))
}

// StartObservationCollector launches the persistent workload/capacity loop.
// Each profile is isolated by collector.Service and all queries are fixed,
// read-only provider queries. The first run waits one interval to avoid
// competing with startup connection checks.
func (s *Server) StartObservationCollector(ctx context.Context, interval time.Duration, retentionDays int) {
	if interval <= 0 {
		return
	}
	if interval < 15*time.Second {
		interval = 15 * time.Second
	}
	if retentionDays <= 0 {
		retentionDays = 30
	}
	s.Collector.ExpectedInterval = interval
	log.Printf("SQLON observation collector: every %s, retention=%d days", interval, retentionDays)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				log.Printf("SQLON observation collector stopped")
				return
			case <-ticker.C:
				runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				batch := s.Collector.CollectAll(runCtx, nil, true)
				removed, pruneErr := s.Collector.Store.Prune(runCtx, time.Now().UTC().AddDate(0, 0, -retentionDays))
				cancel()
				entry := map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "tool": "sqlon:observation_collect", "succeeded": batch.Succeeded, "failed": batch.Failed, "retention_files_removed": removed}
				if batch.Failed > 0 || pruneErr != nil {
					entry["is_error"] = true
					if pruneErr != nil {
						entry["error"] = "retention prune failed: " + pruneErr.Error()
					}
				}
				s.appendAudit(entry)
				log.Printf("SQLON observation collector: succeeded=%d failed=%d pruned=%d", batch.Succeeded, batch.Failed, removed)
			}
		}
	}()
}
