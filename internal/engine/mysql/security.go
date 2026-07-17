package mysql

import (
	"context"
	"fmt"
	"strings"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/observability"
)

// Security implements observability.SecurityProvider using
// information_schema.USER_PRIVILEGES — readable without SELECT on mysql.*.
type Security struct{}

const dangerousPrivilegesSQL = `SELECT GRANTEE AS grantee,
  GROUP_CONCAT(PRIVILEGE_TYPE ORDER BY PRIVILEGE_TYPE SEPARATOR ', ') AS privileges,
  MAX(IS_GRANTABLE = 'YES') AS grantable
FROM information_schema.USER_PRIVILEGES
WHERE PRIVILEGE_TYPE IN ('SUPER', 'FILE', 'PROCESS', 'SHUTDOWN', 'CREATE USER', 'REPLICATION SLAVE ADMIN', 'SYSTEM_USER')
GROUP BY GRANTEE`

const principalCountSQL = `SELECT COUNT(DISTINCT GRANTEE) AS principals FROM information_schema.USER_PRIVILEGES`

func (Security) Security(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) (observability.SecurityData, error) {
	return CollectSecurity(ctx, q, p, "mysql")
}

// CollectSecurity runs the MySQL-family privilege-posture collection labeled
// with the given engine name.
func CollectSecurity(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile, engine string) (observability.SecurityData, error) {
	now := time.Now().UTC()
	data := observability.SecurityData{ProfileID: p.ID, Engine: engine, Findings: []observability.SecurityFinding{}}
	if rows, countErr := q.SystemQuery(ctx, p.ID, principalCountSQL); countErr != nil {
		data.Warnings = append(data.Warnings, "계정 수 조회 실패: "+countErr.Error())
	} else if len(rows) > 0 {
		data.Principals = int(observability.Int(rows[0], "principals"))
	}
	rows, err := q.SystemQuery(ctx, p.ID, dangerousPrivilegesSQL)
	if err != nil {
		return data, fmt.Errorf("collect %s privilege posture: %w", engine, err)
	}
	for _, row := range rows {
		grantee := observability.Text(row, "grantee")
		privileges := observability.Text(row, "privileges")
		// GRANTEE is 'user'@'host' — a wildcard host widens the attack surface.
		wildcard := strings.Contains(grantee, "@'%'") || strings.Contains(grantee, `@"%"`)
		severity := "warning"
		if strings.Contains(privileges, "SUPER") || strings.Contains(privileges, "SYSTEM_USER") {
			severity = "critical"
		}
		detail := "위험 권한: " + privileges
		if observability.Int(row, "grantable") > 0 {
			detail += " (GRANT OPTION 포함)"
		}
		data.Findings = append(data.Findings, observability.SecurityFinding{Principal: grantee, Kind: "dangerous_privilege", Detail: detail, Severity: severity, CollectedAt: now})
		if wildcard {
			data.Findings = append(data.Findings, observability.SecurityFinding{Principal: grantee, Kind: "wildcard_host", Detail: "모든 호스트(%)에서 접속 가능한 고권한 계정", Severity: "warning", CollectedAt: now})
		}
	}
	data.Limitations = append(data.Limitations, "정확한 진단은 information_schema.USER_PRIVILEGES 가시 범위에 한정됩니다 — 모니터링 계정의 권한에 따라 다른 계정이 보이지 않을 수 있습니다.")
	return data, nil
}
