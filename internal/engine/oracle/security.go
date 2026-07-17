package oracle

import (
	"context"
	"fmt"
	"strings"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/observability"
)

// Security implements observability.SecurityProvider using DBA_* dictionary
// views (the monitoring account needs SELECT ANY DICTIONARY or equivalent;
// a permission error is surfaced, never masked).
type Security struct{}

const dbaRoleSQL = `SELECT grantee AS name FROM dba_role_privs
WHERE granted_role = 'DBA' AND grantee NOT IN ('SYS', 'SYSTEM')`

const dangerousSysPrivsSQL = `SELECT grantee AS name, privilege FROM dba_sys_privs
WHERE privilege IN ('GRANT ANY PRIVILEGE', 'ALTER SYSTEM', 'DROP ANY TABLE', 'ALTER USER', 'BECOME USER', 'EXEMPT ACCESS POLICY')
AND grantee NOT IN ('SYS', 'SYSTEM', 'DBA', 'SYSBACKUP', 'SYSDG', 'SYSKM', 'IMP_FULL_DATABASE', 'DATAPUMP_IMP_FULL_DATABASE', 'EXP_FULL_DATABASE', 'DATAPUMP_EXP_FULL_DATABASE')`

const openUsersSQL = `SELECT COUNT(*) AS principals FROM dba_users WHERE account_status = 'OPEN'`

const expiredUsersSQL = `SELECT username AS name FROM dba_users
WHERE account_status LIKE '%EXPIRED%' AND account_status NOT LIKE '%LOCKED%'`

func (Security) Security(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) (observability.SecurityData, error) {
	now := time.Now().UTC()
	data := observability.SecurityData{ProfileID: p.ID, Engine: "oracle", Findings: []observability.SecurityFinding{}}
	rows, err := q.SystemQuery(ctx, p.ID, dbaRoleSQL)
	if err != nil {
		return data, fmt.Errorf("collect Oracle DBA role grants: %w", err)
	}
	for _, row := range rows {
		data.Findings = append(data.Findings, observability.SecurityFinding{Principal: observability.Text(row, "name"), Kind: "dba_role", Detail: "DBA 역할이 부여됨", Severity: "critical", CollectedAt: now})
	}
	if privRows, privErr := q.SystemQuery(ctx, p.ID, dangerousSysPrivsSQL); privErr != nil {
		data.Warnings = append(data.Warnings, "DBA_SYS_PRIVS 조회 실패: "+privErr.Error())
	} else {
		for _, row := range privRows {
			privilege := observability.Text(row, "privilege")
			severity := "warning"
			if strings.Contains(privilege, "GRANT ANY") || privilege == "BECOME USER" {
				severity = "critical"
			}
			data.Findings = append(data.Findings, observability.SecurityFinding{Principal: observability.Text(row, "name"), Kind: "dangerous_privilege", Detail: "위험 시스템 권한: " + privilege, Severity: severity, CollectedAt: now})
		}
	}
	if countRows, countErr := q.SystemQuery(ctx, p.ID, openUsersSQL); countErr != nil {
		data.Warnings = append(data.Warnings, "DBA_USERS 조회 실패: "+countErr.Error())
	} else if len(countRows) > 0 {
		data.Principals = int(observability.Int(countRows[0], "principals"))
	}
	if expiredRows, expiredErr := q.SystemQuery(ctx, p.ID, expiredUsersSQL); expiredErr == nil {
		for _, row := range expiredRows {
			data.Findings = append(data.Findings, observability.SecurityFinding{Principal: observability.Text(row, "name"), Kind: "expired_password", Detail: "비밀번호가 만료되었지만 계정이 잠기지 않음", Severity: "warning", CollectedAt: now})
		}
	}
	return data, nil
}
