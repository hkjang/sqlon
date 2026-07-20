package mcp

import (
	"context"
	"fmt"
	"sort"
	"time"

	"sqlon/internal/change"
	"sqlon/internal/dbconn"
)

// Incident RCA correlates the read-only observation signals SQLON already
// collects — blocking tree, sessions, configuration drift, proactive
// maintenance risk, recent change plans, connection-pool telemetry, and stored
// workload snapshots — into a single ranked set of root-cause hypotheses for a
// time window. It executes nothing and mutates nothing; every recommendation
// points back to the approval-gated change flow.

type incidentChange struct {
	ID     string    `json:"id"`
	Target string    `json:"target"`
	State  string    `json:"state"`
	Risk   string    `json:"risk"`
	Reason string    `json:"reason"`
	When   time.Time `json:"when"`
}

type incidentSQL struct {
	Fingerprint string  `json:"fingerprint"`
	Calls       float64 `json:"calls"`
	ElapsedMS   float64 `json:"elapsed_ms"`
}

// incidentSignals is the extracted, engine-agnostic evidence the ranking core
// reasons over. Keeping it a plain struct makes rankIncidentHypotheses a pure
// function that is unit-tested without any live service.
type incidentSignals struct {
	LockEdges       int
	RootBlocker     string
	RootAffected    int
	LongRunning     int
	Waiting         int
	Drifted         int
	MaintCritical   int
	MaintWarning    int
	RecentChanges   []incidentChange
	PoolStatus      string
	PoolWaitCount   float64
	LatencyEvidence []string
	TopSQL          []incidentSQL
	Unavailable     []string
}

type incidentHypothesis struct {
	Rank           int      `json:"rank"`
	Category       string   `json:"category"`
	Confidence     string   `json:"confidence"`
	Score          int      `json:"score"`
	Summary        string   `json:"summary"`
	Evidence       []string `json:"evidence"`
	Recommendation string   `json:"recommendation"`
}

func confidenceFor(score int) string {
	switch {
	case score >= 85:
		return "high"
	case score >= 55:
		return "medium"
	default:
		return "low"
	}
}

// rankIncidentHypotheses is the pure correlation core. Each active signal
// contributes at most one hypothesis; the list is returned most-likely first.
func rankIncidentHypotheses(sig incidentSignals) []incidentHypothesis {
	var hs []incidentHypothesis
	add := func(score int, category, summary, rec string, evidence ...string) {
		hs = append(hs, incidentHypothesis{Score: score, Category: category, Confidence: confidenceFor(score),
			Summary: summary, Recommendation: rec, Evidence: evidence})
	}

	if sig.MaintCritical > 0 {
		add(95, "maintenance",
			"예방 점검 치명 위험(트랜잭션 wraparound 임박 등)이 감지되었습니다 — 방치 시 DB 정지로 이어질 수 있습니다.",
			"예방 점검 화면에서 대상을 확인하고 VACUUM(FREEZE)/슬롯 정리를 변경계획으로 즉시 진행하세요.",
			fmt.Sprintf("maintenance critical findings=%d", sig.MaintCritical))
	}
	if sig.LockEdges > 0 {
		ev := fmt.Sprintf("blocking edges=%d", sig.LockEdges)
		if sig.RootBlocker != "" {
			ev = fmt.Sprintf("root blocker=%s, affected sessions=%d, edges=%d", sig.RootBlocker, sig.RootAffected, sig.LockEdges)
		}
		add(90, "lock_contention",
			"세션 간 잠금 경합이 진행 중입니다 — 지연·타임아웃의 직접 원인일 가능성이 높습니다.",
			"세션·잠금 화면의 블로킹 트리에서 루트 블로커를 확인하세요. 세션 종료는 승인된 변경계획으로만 수행합니다.",
			ev)
	}
	for _, c := range sig.RecentChanges {
		switch c.State {
		case string(change.Failed), string(change.RolledBack), string(change.RollbackRequired):
			add(88, "recent_change",
				"장애 시간대에 실패/롤백된 변경계획이 있습니다 — 변경이 사건의 원인일 가능성이 큽니다.",
				"해당 변경계획의 단계·검증·보상(compensation)을 검토하고 필요 시 롤백을 완료하세요.",
				fmt.Sprintf("change %s target=%s state=%s risk=%s reason=%q", c.ID, c.Target, c.State, c.Risk, c.Reason))
		case string(change.Executing), string(change.RollingBack):
			add(75, "recent_change",
				"장애 시간대에 실행 중인 변경계획이 있습니다 — 실행이 부하·잠금을 유발했을 수 있습니다.",
				"진행 중 변경의 잠금·영향을 세션 화면과 대조하세요.",
				fmt.Sprintf("change %s target=%s state=%s", c.ID, c.Target, c.State))
		case string(change.Completed):
			add(68, "recent_change",
				"장애 시간대 직전/직후에 완료된 변경계획이 있습니다 — 상관관계를 확인하세요.",
				"변경 전/후 워크로드·플랜을 비교해 회귀 여부를 확인하세요.",
				fmt.Sprintf("change %s target=%s completed", c.ID, c.Target))
		}
	}
	if sig.PoolStatus == "critical" {
		add(80, "connection_pool",
			"커넥션 풀이 포화 상태입니다 — 커넥션 획득 대기가 지연을 유발하고 있습니다.",
			"max_open_conns 상향 또는 장기 점유 쿼리 정리를 검토하세요(커넥션 풀 진단 참조).",
			fmt.Sprintf("pool status=critical acquire_waits=%.0f", sig.PoolWaitCount))
	} else if sig.PoolStatus == "warning" {
		add(60, "connection_pool",
			"커넥션 풀 사용률이 높습니다 — 포화 임박 신호입니다.",
			"커넥션 풀 진단에서 대기·사용률을 확인하세요.",
			fmt.Sprintf("pool status=warning acquire_waits=%.0f", sig.PoolWaitCount))
	}
	if len(sig.LatencyEvidence) > 0 {
		add(65, "workload_regression",
			"워크로드 스냅숏에서 지연 회귀·이상 신호가 관찰됩니다.",
			"Top-SQL과 실행계획(EXPLAIN)을 확인하고 필요 시 인덱스 어드바이저를 실행하세요.",
			sig.LatencyEvidence...)
	}
	if sig.Drifted > 0 {
		add(55, "config_drift",
			"선언된 베이스라인과 다른 서버 파라미터가 있습니다 — 성능·동작 변화의 원인일 수 있습니다.",
			"보안·권한 화면의 설정 드리프트에서 변경된 파라미터를 확인하고 변경계획으로 원복하세요.",
			fmt.Sprintf("config drift parameters=%d", sig.Drifted))
	}
	if sig.MaintWarning > 0 {
		add(50, "maintenance",
			"예방 점검 경고(블로트·비활성 슬롯·freeze 임박)가 있습니다.",
			"예방 점검 화면에서 대상을 확인하고 VACUUM/pg_repack/슬롯 정리를 계획하세요.",
			fmt.Sprintf("maintenance warning findings=%d", sig.MaintWarning))
	}
	if sig.LongRunning > 0 {
		add(45, "long_running_sessions",
			"장기 실행 세션이 있습니다 — 자원 점유·잠금 유발 가능성을 확인하세요.",
			"세션 화면에서 장기 세션의 SQL·트랜잭션 시간을 확인하세요.",
			fmt.Sprintf("long-running sessions=%d", sig.LongRunning))
	} else if sig.Waiting > 0 {
		add(40, "waiting_sessions",
			"대기 중인 세션이 있습니다 — 자원·잠금 대기 여부를 확인하세요.",
			"세션 화면에서 대기 이벤트를 확인하세요.",
			fmt.Sprintf("waiting sessions=%d", sig.Waiting))
	}

	sort.SliceStable(hs, func(i, j int) bool { return hs[i].Score > hs[j].Score })
	for i := range hs {
		hs[i].Rank = i + 1
	}
	return hs
}

func incidentStatus(hs []incidentHypothesis) string {
	if len(hs) == 0 {
		return "ok"
	}
	// Any emitted hypothesis means at least one signal fired, so the incident is
	// never "ok" once hs is non-empty; the lowest-scoring signal (waiting
	// sessions, 40) sets the degraded floor.
	switch top := hs[0].Score; {
	case top >= 85:
		return "critical"
	case top >= 40:
		return "degraded"
	default:
		return "ok"
	}
}

// diagnoseIncident gathers the live signals for a profile and returns the
// ranked RCA bundle. Every gather step is best-effort: a failed signal is
// recorded in `unavailable` and never aborts the whole diagnosis.
func (s *Server) diagnoseIncident(ctx context.Context, profile dbconn.Profile, windowMinutes int) map[string]any {
	if windowMinutes <= 0 {
		windowMinutes = 30
	}
	now := time.Now().UTC()
	since := now.Add(-time.Duration(windowMinutes) * time.Minute)
	sig := incidentSignals{PoolStatus: "unknown"}

	locks := s.Observability.Locks(ctx, profile)
	if locks.Status == "permission_denied" || locks.Status == "error" || locks.Status == "unsupported" {
		sig.Unavailable = append(sig.Unavailable, "locks: "+locks.Status)
	} else {
		sig.LockEdges = len(locks.Data.Edges)
		if len(locks.Data.Roots) > 0 {
			sig.RootBlocker = locks.Data.Roots[0].SessionKey
			sig.RootAffected = locks.Data.Roots[0].AffectedSessions
		}
	}

	sessions := s.Observability.Sessions(ctx, profile)
	if sessions.Status == "permission_denied" || sessions.Status == "error" || sessions.Status == "unsupported" {
		sig.Unavailable = append(sig.Unavailable, "sessions: "+sessions.Status)
	} else {
		sig.LongRunning = sessions.Data.LongRunning
		sig.Waiting = sessions.Data.Waiting
	}

	if len(profile.ConfigBaseline) > 0 {
		drift := s.Observability.ConfigDrift(ctx, profile)
		if drift.Status == "warning" || drift.Status == "ok" {
			sig.Drifted = drift.Data.Drifted
		} else if drift.Status != "not_configured" {
			sig.Unavailable = append(sig.Unavailable, "config_drift: "+drift.Status)
		}
	}

	maint := s.Observability.Maintenance(ctx, profile)
	if maint.Status == "permission_denied" || maint.Status == "error" || maint.Status == "unsupported" {
		if maint.Status != "unsupported" {
			sig.Unavailable = append(sig.Unavailable, "maintenance: "+maint.Status)
		}
	} else {
		for _, f := range maint.Data.Findings {
			switch f.Severity {
			case "critical":
				sig.MaintCritical++
			case "warning":
				sig.MaintWarning++
			}
		}
	}

	// recent change plans for this profile within the window
	if s.Changes != nil {
		for _, p := range s.Changes.List() {
			if p.ProfileID != profile.ID {
				continue
			}
			if p.UpdatedAt.Before(since) {
				continue
			}
			switch p.State {
			case change.Failed, change.RolledBack, change.RollbackRequired, change.Executing, change.RollingBack, change.Completed:
				sig.RecentChanges = append(sig.RecentChanges, incidentChange{
					ID: p.ID, Target: p.Target, State: string(p.State), Risk: string(p.Risk),
					Reason: p.Reason, When: p.UpdatedAt,
				})
			}
		}
	}

	// connection pool telemetry
	pool := s.connectionPoolDiagnosis(ctx, profile.ID)
	if st, ok := pool["status"].(string); ok {
		sig.PoolStatus = st
		if data, ok := pool["data"].(map[string]any); ok {
			if ps, ok := data["pool"]; ok {
				sig.PoolWaitCount = poolWaitCount(ps)
			}
		}
	}

	// stored workload snapshots: latest snapshot's warning/critical evidence + top SQL
	if s.Collector != nil {
		snaps, _, err := s.Collector.History(ctx, profile.ID, since, 50)
		if err != nil {
			sig.Unavailable = append(sig.Unavailable, "workload: "+err.Error())
		} else if len(snaps) > 0 {
			latest := snaps[0]
			for _, sn := range snaps {
				if sn.CollectedAt.After(latest.CollectedAt) {
					latest = sn
				}
			}
			for _, e := range latest.Evidence {
				if e.Severity == "warning" || e.Severity == "critical" {
					sig.LatencyEvidence = append(sig.LatencyEvidence, e.Summary)
				}
			}
			sqlStats := latest.TopSQL
			sort.SliceStable(sqlStats, func(i, j int) bool { return sqlStats[i].ElapsedMS > sqlStats[j].ElapsedMS })
			for i, st := range sqlStats {
				if i >= 5 {
					break
				}
				sig.TopSQL = append(sig.TopSQL, incidentSQL{Fingerprint: st.Fingerprint, Calls: st.Calls, ElapsedMS: st.ElapsedMS})
			}
		}
	}

	hypotheses := rankIncidentHypotheses(sig)
	status := incidentStatus(hypotheses)
	if hypotheses == nil {
		hypotheses = []incidentHypothesis{}
	}
	summary := "장애 시간대에서 뚜렷한 근본원인 신호가 관찰되지 않았습니다. 창(window)을 넓히거나 fresh 수집 후 다시 진단하세요."
	if len(hypotheses) > 0 {
		summary = hypotheses[0].Summary
	}
	return map[string]any{
		"status":         status,
		"profile_id":     profile.ID,
		"engine":         profile.Type,
		"window_minutes": windowMinutes,
		"collected_at":   now,
		"summary":        summary,
		"hypotheses":     hypotheses,
		"signals": map[string]any{
			"lock_edges":     sig.LockEdges,
			"root_blocker":   sig.RootBlocker,
			"long_running":   sig.LongRunning,
			"waiting":        sig.Waiting,
			"config_drift":   sig.Drifted,
			"maint_critical": sig.MaintCritical,
			"maint_warning":  sig.MaintWarning,
			"recent_changes": sig.RecentChanges,
			"pool_status":    sig.PoolStatus,
			"top_sql":        sig.TopSQL,
		},
		"unavailable": sig.Unavailable,
		"notice":      "읽기 전용 상관분석입니다. 모든 조치는 변경 관리의 승인 흐름으로 수행하세요.",
	}
}

// poolWaitCount extracts the acquire-wait count from the PoolStat value without
// coupling to its concrete type (it is returned as a struct from dbconn).
func poolWaitCount(ps any) float64 {
	switch v := ps.(type) {
	case map[string]any:
		return toFloat(v["wait_count"])
	case dbconn.PoolStat:
		return float64(v.WaitCount)
	}
	return 0
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	}
	return 0
}
