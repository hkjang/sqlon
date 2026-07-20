package change

import (
	"regexp"
	"strings"
)

// ImpactPrediction is a static, read-only estimate of what a DDL statement will
// do to a live table: the lock it takes, whether it blocks reads or only
// writes, whether it rewrites/scans the whole table, and the online-DDL option
// that avoids the outage. It is a heuristic over the statement text — it never
// touches a database — so it is advisory and errs toward warning.
type ImpactPrediction struct {
	Engine         string   `json:"engine"`
	Operation      string   `json:"operation"`
	LockLevel      string   `json:"lock_level"`
	Blocking       string   `json:"blocking"` // reads+writes | writes | none | unknown
	Rewrite        bool     `json:"rewrite"`
	OnlineOption   string   `json:"online_option,omitempty"`
	Severity       string   `json:"severity"` // info | warning | critical
	Notes          []string `json:"notes,omitempty"`
	Recommendation string   `json:"recommendation,omitempty"`
}

var wsCollapse = regexp.MustCompile(`\s+`)

// normalizeDDL upper-cases and collapses whitespace for stable matching while
// leaving a leading/trailing trim. Comments are not stripped (a heuristic).
func normalizeDDL(sql string) string {
	return strings.TrimSpace(wsCollapse.ReplaceAllString(strings.ToUpper(sql), " "))
}

// PredictImpact analyzes a single DDL statement for the given engine. Unknown
// or non-DDL statements return an explicit low-severity "unknown" rather than a
// false all-clear.
func PredictImpact(engine, sql string) ImpactPrediction {
	s := normalizeDDL(sql)
	eng := strings.ToLower(strings.TrimSpace(engine))
	p := ImpactPrediction{Engine: eng, Operation: "unknown", LockLevel: "unknown", Blocking: "unknown", Severity: "info"}
	if s == "" {
		p.Notes = append(p.Notes, "빈 문장입니다.")
		return p
	}
	switch eng {
	case "postgres":
		return predictPostgres(s, p)
	case "mysql", "mariadb":
		return predictMySQL(s, p)
	default:
		p.Notes = append(p.Notes, "이 엔진에 대한 영향 예측 규칙이 아직 없습니다. 문장을 수동 검토하세요.")
		return p
	}
}

func has(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func predictPostgres(s string, p ImpactPrediction) ImpactPrediction {
	switch {
	case has(s, "CREATE ", "INDEX", "CONCURRENTLY"):
		p.Operation, p.LockLevel, p.Blocking, p.Severity = "CREATE INDEX CONCURRENTLY", "SHARE UPDATE EXCLUSIVE", "none", "info"
		p.Notes = append(p.Notes, "동시 인덱스 생성은 읽기·쓰기를 차단하지 않지만 느리고 트랜잭션 블록 안에서 실행할 수 없습니다. 실패 시 INVALID 인덱스가 남을 수 있습니다.")
		p.Recommendation = "권장 방식입니다. 완료 후 인덱스 유효성(indisvalid)을 확인하세요."
	case has(s, "CREATE ", "INDEX"):
		p.Operation, p.LockLevel, p.Blocking, p.Severity = "CREATE INDEX", "SHARE", "writes", "warning"
		p.OnlineOption = "CREATE INDEX CONCURRENTLY ..."
		p.Notes = append(p.Notes, "일반 인덱스 생성은 대상 테이블의 쓰기를 차단합니다(읽기는 허용). 대형 테이블에서 오래 걸립니다.")
		p.Recommendation = "운영 트래픽 중에는 CREATE INDEX CONCURRENTLY 사용을 검토하세요."
	case has(s, "ALTER TABLE", "ALTER COLUMN", "TYPE"):
		p.Operation, p.LockLevel, p.Blocking, p.Rewrite, p.Severity = "ALTER COLUMN TYPE", "ACCESS EXCLUSIVE", "reads+writes", true, "critical"
		p.Notes = append(p.Notes, "컬럼 타입 변경은 테이블을 재작성하며 ACCESS EXCLUSIVE 락으로 읽기·쓰기를 모두 차단합니다.")
		p.Recommendation = "유지보수 창에서 수행하거나, 새 컬럼 추가→백필→스왑 패턴으로 무중단 마이그레이션을 검토하세요."
	case has(s, "ALTER TABLE", "ADD COLUMN") && (has(s, "DEFAULT") && !hasConstantDefaultOnly(s) || has(s, "NOT NULL") && !has(s, "DEFAULT")):
		p.Operation, p.LockLevel, p.Blocking, p.Rewrite, p.Severity = "ADD COLUMN (rewrite)", "ACCESS EXCLUSIVE", "reads+writes", true, "warning"
		p.Notes = append(p.Notes, "휘발성 DEFAULT 또는 DEFAULT 없는 NOT NULL 컬럼 추가는 전체 테이블 재작성/스캔을 유발할 수 있습니다(PG 버전에 따라 다름).")
		p.Recommendation = "상수 DEFAULT(PG11+는 즉시) 사용 또는 NULL 허용으로 추가 후 백필·NOT NULL 검증을 분리하세요."
	case has(s, "ALTER TABLE", "ADD COLUMN"):
		p.Operation, p.LockLevel, p.Blocking, p.Severity = "ADD COLUMN", "ACCESS EXCLUSIVE", "reads+writes", "info"
		p.Notes = append(p.Notes, "단순 컬럼 추가는 카탈로그 변경만으로 빠르지만 짧은 ACCESS EXCLUSIVE 락을 잡습니다. 락 대기 중 뒤따르는 쿼리가 큐잉될 수 있습니다.")
		p.Recommendation = "lock_timeout 을 짧게 설정해 락 대기가 트래픽을 막지 않게 하세요."
	case has(s, "ADD CONSTRAINT") && has(s, "FOREIGN KEY") && !has(s, "NOT VALID"):
		p.Operation, p.LockLevel, p.Blocking, p.Rewrite, p.Severity = "ADD FOREIGN KEY (validating)", "SHARE ROW EXCLUSIVE", "writes", true, "warning"
		p.Notes = append(p.Notes, "외래키 추가는 기존 행을 검증하며 양쪽 테이블의 쓰기를 차단합니다.")
		p.Recommendation = "ADD CONSTRAINT ... NOT VALID 로 먼저 추가한 뒤 VALIDATE CONSTRAINT 로 분리 검증하세요."
	case has(s, "VACUUM FULL"):
		p.Operation, p.LockLevel, p.Blocking, p.Rewrite, p.Severity = "VACUUM FULL", "ACCESS EXCLUSIVE", "reads+writes", true, "critical"
		p.Notes = append(p.Notes, "VACUUM FULL 은 테이블을 재작성하며 완료까지 읽기·쓰기를 모두 차단합니다.")
		p.Recommendation = "무중단이 필요하면 pg_repack 을 사용하세요."
	case has(s, "TRUNCATE"):
		p.Operation, p.LockLevel, p.Blocking, p.Severity = "TRUNCATE", "ACCESS EXCLUSIVE", "reads+writes", "critical"
		p.Notes = append(p.Notes, "TRUNCATE 는 ACCESS EXCLUSIVE 락을 잡고 데이터를 즉시 제거합니다(롤백은 트랜잭션 내에서만).")
	case has(s, "DROP INDEX", "CONCURRENTLY"):
		p.Operation, p.LockLevel, p.Blocking, p.Severity = "DROP INDEX CONCURRENTLY", "SHARE UPDATE EXCLUSIVE", "none", "info"
	case has(s, "DROP INDEX"):
		p.Operation, p.LockLevel, p.Blocking, p.Severity = "DROP INDEX", "ACCESS EXCLUSIVE", "reads+writes", "warning"
		p.OnlineOption = "DROP INDEX CONCURRENTLY ..."
	case has(s, "REINDEX") && !has(s, "CONCURRENTLY"):
		p.Operation, p.LockLevel, p.Blocking, p.Rewrite, p.Severity = "REINDEX", "ACCESS EXCLUSIVE", "reads+writes", true, "warning"
		p.OnlineOption = "REINDEX ... CONCURRENTLY (PG12+)"
	default:
		p.Notes = append(p.Notes, "이 문장에 대한 특이 락/재작성 규칙이 없습니다. ACCESS EXCLUSIVE 를 잡는 DDL인지 수동 확인하세요.")
	}
	return p
}

// hasConstantDefaultOnly is a rough check: a DEFAULT that is a literal (number,
// quoted string, TRUE/FALSE/NULL) rather than a function call. Function-call
// defaults (now(), gen_random_uuid()) are volatile and force a rewrite on older
// PG. Conservative: any '(' after DEFAULT is treated as a function call.
func hasConstantDefaultOnly(s string) bool {
	i := strings.Index(s, "DEFAULT ")
	if i < 0 {
		return false
	}
	rest := s[i+len("DEFAULT "):]
	// stop at a comma or closing paren that ends the column definition
	end := strings.IndexAny(rest, ",)")
	if end >= 0 {
		rest = rest[:end]
	}
	return !strings.Contains(rest, "(")
}

func predictMySQL(s string, p ImpactPrediction) ImpactPrediction {
	// Explicit ALGORITHM/LOCK clauses are authoritative when present.
	explicitCopy := has(s, "ALGORITHM=COPY") || has(s, "ALGORITHM = COPY")
	explicitInstant := has(s, "ALGORITHM=INSTANT") || has(s, "ALGORITHM = INSTANT")
	explicitInplace := has(s, "ALGORITHM=INPLACE") || has(s, "ALGORITHM = INPLACE")
	lockNone := has(s, "LOCK=NONE") || has(s, "LOCK = NONE")

	switch {
	case has(s, "CREATE ", "INDEX"), has(s, "ADD INDEX"), has(s, "ADD KEY"):
		p.Operation, p.LockLevel, p.Blocking, p.Severity = "ADD INDEX", "metadata (INPLACE)", "none", "info"
		p.Notes = append(p.Notes, "InnoDB 인덱스 추가는 보통 ONLINE(INPLACE)으로 동작해 읽기·쓰기를 허용합니다. 다만 최종 커밋 시 짧은 메타데이터 락이 있습니다.")
		p.Recommendation = "명시적으로 ALGORITHM=INPLACE, LOCK=NONE 를 지정해 온라인 동작을 보장하세요."
	case explicitCopy || has(s, "MODIFY COLUMN") || has(s, "CHANGE COLUMN") || (has(s, "DROP PRIMARY KEY")):
		p.Operation, p.LockLevel, p.Blocking, p.Rewrite, p.Severity = "ALTER (table copy)", "COPY (table rebuild)", "writes", true, "critical"
		p.Notes = append(p.Notes, "컬럼 타입 변경·기본키 변경 등은 ALGORITHM=COPY 로 강등되어 테이블을 통째로 복사하며 쓰기를 차단합니다.")
		p.OnlineOption = "gh-ost 또는 pt-online-schema-change"
		p.Recommendation = "대형 테이블은 gh-ost/pt-osc 온라인 스키마 변경 도구 사용을 검토하세요."
	case explicitInstant:
		p.Operation, p.LockLevel, p.Blocking, p.Severity = "ALTER (INSTANT)", "metadata only", "none", "info"
		p.Notes = append(p.Notes, "ALGORITHM=INSTANT 는 메타데이터만 변경해 즉시 완료됩니다(MySQL 8.0.12+).")
	case has(s, "ADD COLUMN"):
		p.Operation, p.LockLevel, p.Blocking, p.Severity = "ADD COLUMN", "INSTANT/INPLACE", "none", "info"
		p.Notes = append(p.Notes, "컬럼 추가는 MySQL 8.0.12+ 에서 대개 INSTANT 이거나 INPLACE 입니다. 특정 위치(AFTER col) 지정 시 COPY 로 강등될 수 있습니다.")
		p.Recommendation = "ALGORITHM=INSTANT 를 명시해 즉시 완료를 보장하세요."
	case explicitInplace:
		p.Operation, p.LockLevel, p.Blocking, p.Severity = "ALTER (INPLACE)", "metadata (INPLACE)", "none", "info"
	case has(s, "TRUNCATE"):
		p.Operation, p.LockLevel, p.Blocking, p.Severity = "TRUNCATE", "metadata + drop", "reads+writes", "critical"
		p.Notes = append(p.Notes, "TRUNCATE 는 테이블을 즉시 비우며 롤백할 수 없습니다.")
	case has(s, "DROP INDEX"):
		p.Operation, p.LockLevel, p.Blocking, p.Severity = "DROP INDEX", "metadata (INPLACE)", "none", "info"
	default:
		p.Notes = append(p.Notes, "이 문장에 대한 특이 규칙이 없습니다. ALGORITHM/LOCK 절로 온라인 동작을 명시하세요.")
	}
	if lockNone && p.Blocking == "writes" {
		p.Notes = append(p.Notes, "LOCK=NONE 이 지정됐지만 이 작업은 COPY 로 강등될 수 있어 서버가 거부할 수 있습니다.")
	}
	return p
}
