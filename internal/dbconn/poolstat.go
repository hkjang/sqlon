package dbconn

import (
	"context"
	"fmt"
)

// PoolStat is a point-in-time view of SQLON's own connection pool to a target
// database, combined with the configured limits. It is SQLON-side telemetry
// (how our pool behaves), not a query against the database server.
type PoolStat struct {
	ProfileID         string `json:"profile_id"`
	Engine            string `json:"engine"`
	MaxOpenConns      int    `json:"max_open_conns"`
	MaxIdleConns      int    `json:"max_idle_conns"`
	OpenConnections   int    `json:"open_connections"`
	InUse             int    `json:"in_use"`
	Idle              int    `json:"idle"`
	WaitCount         int64  `json:"wait_count"`
	WaitDurationMs    int64  `json:"wait_duration_ms"`
	MaxIdleClosed     int64  `json:"max_idle_closed"`
	MaxLifetimeClosed int64  `json:"max_lifetime_closed"`
}

// PoolAdviceLevel is the diagnostic severity.
type PoolAdvice struct {
	Status         string   `json:"status"` // healthy | warning | critical
	Code           string   `json:"code"`
	Summary        string   `json:"summary"`
	AvgWaitMs      float64  `json:"avg_wait_ms"`
	Recommendation string   `json:"recommendation,omitempty"`
	SuggestedMax   int      `json:"suggested_max_open_conns,omitempty"`
	Notes          []string `json:"notes,omitempty"`
}

// Pool-diagnosis thresholds. Deliberately conservative: contention (any
// acquisition wait) is the primary signal, escalated by how long callers
// blocked on average.
const (
	poolAvgWaitWarnMs     = 5.0  // avg ms blocked per acquisition → warning
	poolAvgWaitCriticalMs = 50.0 // → critical
	poolSaturationRatio   = 0.9  // in_use / max_open at/above which the pool is near saturation
)

// PoolStatFor returns the live pool statistics for a profile if a pool exists.
// It does not open a new pool: a diagnosis is only meaningful once the pool
// has served traffic, and forcing one open would report a pristine, useless
// snapshot.
func (m *Manager) PoolStatFor(ctx context.Context, profileID string) (PoolStat, bool, error) {
	p, err := m.store.GetProfileByID(ctx, profileID)
	if err != nil {
		return PoolStat{}, false, err
	}
	m.mu.Lock()
	pooled, ok := m.pools[profileID]
	m.mu.Unlock()
	if !ok {
		return PoolStat{}, false, nil
	}
	s := pooled.db.Stats()
	return PoolStat{
		ProfileID: p.ID, Engine: p.Type,
		MaxOpenConns: p.Pool.MaxOpenConns, MaxIdleConns: p.Pool.MaxIdleConns,
		OpenConnections: s.OpenConnections, InUse: s.InUse, Idle: s.Idle,
		WaitCount: s.WaitCount, WaitDurationMs: s.WaitDuration.Milliseconds(),
		MaxIdleClosed: s.MaxIdleClosed, MaxLifetimeClosed: s.MaxLifetimeClosed,
	}, true, nil
}

// Diagnose evaluates the pool telemetry into an evidence-based recommendation.
// Pure function of the snapshot so it is unit-testable without a live pool.
func (ps PoolStat) Diagnose() PoolAdvice {
	advice := PoolAdvice{Status: "healthy", Code: "POOL_HEALTHY", Summary: "커넥션 풀에서 관찰된 경합이 없습니다"}
	if ps.WaitCount > 0 {
		advice.AvgWaitMs = float64(ps.WaitDurationMs) / float64(ps.WaitCount)
	}

	// Primary signal: callers had to wait to acquire a connection — the pool
	// hit its ceiling. Escalate by average blocked time.
	if ps.WaitCount > 0 {
		advice.Status, advice.Code = "warning", "POOL_CONTENTION"
		advice.Summary = "커넥션 획득 대기가 발생했습니다 — 풀이 상한에 도달했습니다"
		if advice.AvgWaitMs >= poolAvgWaitCriticalMs {
			advice.Status, advice.Code = "critical", "POOL_EXHAUSTION"
			advice.Summary = "커넥션 획득 지연이 큽니다 — 풀 고갈로 애플리케이션이 지연되고 있습니다"
		} else if advice.AvgWaitMs < poolAvgWaitWarnMs && ps.MaxOpenConns > 0 && float64(ps.InUse)/float64(ps.MaxOpenConns) < poolSaturationRatio {
			// Waits occurred but were brief and the pool is not currently
			// saturated — a transient spike, not sustained under-provisioning.
			advice.Status, advice.Code = "warning", "POOL_TRANSIENT_WAIT"
			advice.Summary = "간헐적 커넥션 대기가 있었으나 현재 포화 상태는 아닙니다"
		}
		if ps.MaxOpenConns > 0 {
			advice.SuggestedMax = ps.MaxOpenConns + ps.MaxOpenConns/2 // +50%
			if advice.SuggestedMax == ps.MaxOpenConns {
				advice.SuggestedMax = ps.MaxOpenConns + 1
			}
			advice.Recommendation = fmt.Sprintf("max_open_conns를 %d → %d로 상향하거나 애플리케이션의 커넥션 보유 시간을 줄이는 것을 검토하세요.", ps.MaxOpenConns, advice.SuggestedMax)
		}
	}

	// Secondary: near-saturation even without a recorded wait yet.
	if advice.Status == "healthy" && ps.MaxOpenConns > 0 && float64(ps.InUse)/float64(ps.MaxOpenConns) >= poolSaturationRatio {
		advice.Status, advice.Code = "warning", "POOL_NEAR_SATURATION"
		advice.Summary = "사용 중 커넥션이 상한에 근접했습니다 — 곧 대기가 발생할 수 있습니다"
		advice.SuggestedMax = ps.MaxOpenConns + ps.MaxOpenConns/2
		advice.Recommendation = fmt.Sprintf("트래픽이 늘면 대기가 발생합니다. max_open_conns %d 상향을 검토하세요.", ps.MaxOpenConns)
	}

	// Idle churn: connections repeatedly closed for exceeding max_idle while
	// the pool is actively used suggests max_idle_conns is too low.
	if ps.MaxIdleClosed > 0 && ps.WaitCount == 0 && ps.OpenConnections > ps.MaxIdleConns {
		advice.Notes = append(advice.Notes, fmt.Sprintf("유휴 상한 초과로 커넥션이 %d회 닫혔습니다 — 재연결 오버헤드가 있으면 max_idle_conns(%d) 상향을 검토하세요.", ps.MaxIdleClosed, ps.MaxIdleConns))
	}

	// Over-provisioning: plenty of headroom and no contention. Advisory only.
	if advice.Status == "healthy" && ps.MaxOpenConns >= 20 && ps.WaitCount == 0 && float64(ps.InUse) < float64(ps.MaxOpenConns)*0.25 {
		advice.Notes = append(advice.Notes, "커넥션 여유가 큽니다 — 오버프로비저닝일 수 있으나 경합이 없으므로 조치는 선택입니다.")
	}
	return advice
}
