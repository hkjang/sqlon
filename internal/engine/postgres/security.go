package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"sqlon/internal/dbconn"
	"sqlon/internal/observability"
)

// Security implements observability.SecurityProvider.
type Security struct{}

const rolesSQL = `SELECT rolname AS name,
  rolsuper AS is_superuser, rolcreaterole AS can_create_roles,
  rolcreatedb AS can_create_db, rolcanlogin AS can_login,
  rolbypassrls AS bypass_rls,
  (rolvaliduntil IS NOT NULL AND rolvaliduntil < now()) AS password_expired
FROM pg_catalog.pg_roles
WHERE rolname NOT LIKE 'pg\_%'
ORDER BY rolname
LIMIT 10000`

func (Security) Security(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) (observability.SecurityData, error) {
	now := time.Now().UTC()
	data := observability.SecurityData{ProfileID: p.ID, Engine: "postgres", Findings: []observability.SecurityFinding{}}
	rows, err := q.SystemQuery(ctx, p.ID, rolesSQL)
	if err != nil {
		return data, fmt.Errorf("collect PostgreSQL roles: %w", err)
	}
	data.Principals = len(rows)
	truthy := func(row map[string]any, column string) bool {
		return strings.EqualFold(observability.Text(row, column), "true") || observability.Text(row, column) == "1"
	}
	for _, row := range rows {
		name := observability.Text(row, "name")
		login := truthy(row, "can_login")
		if truthy(row, "is_superuser") && name != "postgres" {
			severity := "warning"
			if login {
				severity = "critical" // 로그인 가능한 비기본 superuser
			}
			data.Findings = append(data.Findings, observability.SecurityFinding{Principal: name, Kind: "superuser", Detail: "SUPERUSER 속성 보유", Severity: severity, CollectedAt: now})
		}
		if truthy(row, "bypass_rls") && login {
			data.Findings = append(data.Findings, observability.SecurityFinding{Principal: name, Kind: "bypass_rls", Detail: "행 수준 보안(RLS)을 우회할 수 있는 로그인 역할", Severity: "warning", CollectedAt: now})
		}
		if truthy(row, "password_expired") && login {
			data.Findings = append(data.Findings, observability.SecurityFinding{Principal: name, Kind: "expired_password", Detail: "비밀번호 유효기간이 만료됨", Severity: "warning", CollectedAt: now})
		}
	}
	return data, nil
}
