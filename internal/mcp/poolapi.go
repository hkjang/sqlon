package mcp

import (
	"context"
	"net/http"
	"time"
)

// connectionPoolDiagnosis builds the evidence-bearing response for SQLON's own
// connection pool to a target profile. It queries no target DB — the data is
// sql.DB pool telemetry combined with the pure PoolStat.Diagnose analysis.
func (s *Server) connectionPoolDiagnosis(ctx context.Context, profileID string) map[string]any {
	now := time.Now().UTC()
	stat, ok, err := s.DB.PoolStatFor(ctx, profileID)
	if err != nil {
		return map[string]any{"status": "error", "error": err.Error(), "collected_at": now}
	}
	if !ok {
		return map[string]any{
			"status":       "not_collected",
			"limitations":  []string{"이 프로파일의 커넥션 풀이 아직 생성되지 않았습니다 — 한 번 이상 조회·수집이 실행된 뒤 진단할 수 있습니다."},
			"collected_at": now,
		}
	}
	advice := stat.Diagnose()
	status := "ok"
	if advice.Status == "warning" || advice.Status == "critical" {
		status = advice.Status
	}
	return map[string]any{
		"status":       status,
		"data":         map[string]any{"pool": stat, "advice": advice},
		"limitations":  []string{"풀 통계는 프로세스 시작 이후 누적값입니다. 이 진단은 대상 DB에 쿼리하지 않고 SQLON 측 풀 텔레메트리만 사용합니다."},
		"collected_at": now,
	}
}

func (s *Server) registerPoolAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/observability/pool", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.requireQueryActor(w, r); !ok {
			return
		}
		profiles, ctx, ok := s.fleetProfilesForRequest(w, r)
		if !ok {
			return
		}
		profile, ok := allowedProfile(profiles, r.URL.Query().Get("profile"))
		if !ok {
			writeAPIError(w, http.StatusNotFound, errEmpty("db profile not found or not permitted"))
			return
		}
		writeJSON(w, http.StatusOK, s.connectionPoolDiagnosis(ctx, profile.ID))
	})
}
