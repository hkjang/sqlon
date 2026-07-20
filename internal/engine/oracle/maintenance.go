package oracle

import (
	"context"
	"fmt"
	"strings"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/observability"
)

// Maintenance implements observability.MaintenanceProvider for Oracle using
// base-license DBA_* views only. It surfaces latent risks that report no error
// until they cause an outage: tablespace saturation and accounts whose
// password has expired but which remain unlocked (a login-time surprise).
type Maintenance struct{}

// Tablespace usage against configured thresholds (autoextend-aware via
// DBA_TABLESPACE_USAGE_METRICS, the license-safe source).
const tablespaceUsageSQL = `SELECT tablespace_name AS name,
  ROUND(used_percent, 1) AS used_percent
FROM dba_tablespace_usage_metrics
ORDER BY used_percent DESC`

// Accounts expired but not locked: the next login fails or forces a password
// change unexpectedly. EXPIRED(GRACE) is included.
const expiredUnlockedSQL = `SELECT username, account_status
FROM dba_users
WHERE account_status LIKE '%EXPIRED%'
  AND account_status NOT LIKE '%LOCKED%'
ORDER BY username`

const (
	tablespaceWarnPercent     = 85.0
	tablespaceCriticalPercent = 95.0
)

func (Maintenance) Maintenance(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) (observability.MaintenanceData, error) {
	now := time.Now().UTC()
	data := observability.MaintenanceData{ProfileID: p.ID, Engine: "oracle", Findings: []observability.MaintenanceFinding{}}

	// 1. tablespace saturation
	if rows, err := q.SystemQuery(ctx, p.ID, tablespaceUsageSQL); err != nil {
		data.Limitations = append(data.Limitations, "테이블스페이스 사용률(DBA_TABLESPACE_USAGE_METRICS) 수집을 건너뜀: "+err.Error())
	} else {
		data.Checks++
		for _, row := range rows {
			pct := observability.Number(row, "used_percent")
			severity := ""
			switch {
			case pct >= tablespaceCriticalPercent:
				severity = "critical"
			case pct >= tablespaceWarnPercent:
				severity = "warning"
			}
			if severity == "" {
				continue
			}
			data.Findings = append(data.Findings, observability.MaintenanceFinding{
				Category:       "tablespace_saturation",
				Object:         observability.Text(row, "name"),
				Detail:         fmt.Sprintf("테이블스페이스 사용률 %.1f%%", pct),
				Metric:         "used_percent",
				Value:          pct,
				Threshold:      tablespaceWarnPercent,
				Recommendation: "데이터파일 추가/autoextend 조정 또는 세그먼트 정리를 변경계획으로 수행하세요. 포화 시 확장 실패로 트랜잭션이 중단됩니다.",
				Severity:       severity,
				CollectedAt:    now,
			})
		}
	}

	// 2. expired-but-unlocked accounts
	if rows, err := q.SystemQuery(ctx, p.ID, expiredUnlockedSQL); err != nil {
		data.Limitations = append(data.Limitations, "만료-미잠금 계정(DBA_USERS) 수집을 건너뜀: "+err.Error())
	} else {
		data.Checks++
		for _, row := range rows {
			user := observability.Text(row, "username")
			if strings.EqualFold(user, "") {
				continue
			}
			data.Findings = append(data.Findings, observability.MaintenanceFinding{
				Category:       "expired_account",
				Object:         user,
				Detail:         "계정 상태 " + observability.Text(row, "account_status") + " — 비밀번호 만료됐으나 잠기지 않아 다음 로그인이 예기치 않게 실패할 수 있습니다",
				Recommendation: "비밀번호 갱신 또는 명시적 잠금을 변경계획으로 수행하세요.",
				Severity:       "warning",
				CollectedAt:    now,
			})
		}
	}

	return data, nil
}
