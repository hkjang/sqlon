package mcp

import (
	"regexp"
	"strings"

	"sqlon/internal/dbconn"
)

// dbHints maps common PostgreSQL SQLSTATE codes (PG-xxxxx) and MySQL/MariaDB
// error numbers (MY-nnnn) to actionable Korean guidance so the LLM
// regenerates instead of surfacing a raw driver error to the user. The codes
// match the classifier in dbconn (dbErrCode).
var dbHints = []struct{ code, hint string }{
	// PostgreSQL SQLSTATE
	{"PG-42P01", "테이블/뷰가 없습니다. 스키마 접두어를 확인하고 카탈로그의 테이블명을 그대로 사용하세요."},
	{"PG-42703", "존재하지 않는 컬럼입니다. get_schema_context의 컬럼명만 사용하고 별칭 표기를 확인하세요."},
	{"PG-42601", "SQL 문법 오류 — PostgreSQL 방언(LIMIT, COALESCE, TO_CHAR 등)을 확인하세요. 세미콜론은 제거해야 합니다."},
	{"PG-42702", "컬럼명이 모호합니다. 조인된 테이블의 공통 컬럼에 별칭을 붙이세요."},
	{"PG-42883", "존재하지 않는 함수입니다 — MySQL 전용 함수(IFNULL, DATE_FORMAT 등)를 PostgreSQL 함수로 바꾸세요."},
	{"PG-22P02", "타입 변환 실패 — 문자 컬럼을 숫자/날짜와 비교했을 수 있습니다. 리터럴 타입을 컬럼 타입에 맞추세요."},
	{"PG-22007", "날짜 형식 오류 — TO_DATE/DATE 리터럴 포맷을 확인하세요."},
	{"PG-28P01", "DB 계정 인증 실패 — 프로파일의 사용자/비밀번호 참조를 확인하세요."},
	{"PG-3D000", "데이터베이스가 없습니다 — 프로파일의 connect_string(dbname)을 확인하세요."},
	{"PG-57014", "쿼리가 취소되었습니다(timeout 포함) — 조건을 좁히거나 미리보기로 먼저 확인하세요."},
	{"PG-25006", "읽기 전용 세션에서 쓰기를 시도했습니다 — SELECT만 허용됩니다."},
	// MySQL / MariaDB errno
	{"MY-1146", "테이블이 없습니다. 스키마(데이터베이스) 접두어를 확인하고 카탈로그의 테이블명을 그대로 사용하세요."},
	{"MY-1054", "존재하지 않는 컬럼입니다. get_schema_context의 컬럼명만 사용하고 별칭 표기를 확인하세요."},
	{"MY-1064", "SQL 문법 오류 — MySQL/MariaDB 방언(LIMIT, IFNULL/COALESCE, DATE_FORMAT 등)을 확인하세요. FETCH FIRST와 세미콜론은 제거해야 합니다."},
	{"MY-1052", "컬럼명이 모호합니다. 조인된 테이블의 공통 컬럼에 별칭을 붙이세요."},
	{"MY-1305", "존재하지 않는 함수입니다 — Oracle/PostgreSQL 전용 함수(TO_CHAR, NVL 등)를 MySQL 함수로 바꾸세요."},
	{"MY-1045", "DB 계정 인증 실패 — 프로파일의 사용자/비밀번호 참조를 확인하세요."},
	{"MY-1049", "데이터베이스가 없습니다 — 프로파일의 connect_string(dbname)을 확인하세요."},
	{"MY-1317", "쿼리가 중단되었습니다(취소/타임아웃) — 조건을 좁히거나 미리보기로 먼저 확인하세요."},
	{"MY-3024", "쿼리 실행 시간이 초과되었습니다 — 조건을 좁히거나 미리보기로 먼저 확인하세요."},
	{"MY-1290", "읽기 전용 세션에서 쓰기를 시도했습니다 — SELECT만 허용됩니다."},
	{"MY-1292", "값 형식 오류 — 날짜/숫자 리터럴이 컬럼 타입과 맞는지 확인하세요."},
	// classifier-level codes
	{"TIMEOUT", "쿼리 타임아웃 — 기간 조건을 좁히고 LIMIT을 낮춰 다시 시도하세요."},
	{"CANCELED", "쿼리가 취소되었습니다."},
}

var (
	// pgx messages carry "... (SQLSTATE 42P01)"; go-sql-driver messages start
	// with "Error 1146 (42S02): ...". Normalize both to the dbHints code form.
	pgStateRE = regexp.MustCompile(`(?i)SQLSTATE (\w{5})`)
	myErrnoRE = regexp.MustCompile(`(?i)\bError (\d{3,4})\b`)
)

func dbHint(errMsg string) string {
	code := ""
	if m := pgStateRE.FindStringSubmatch(errMsg); m != nil {
		code = "PG-" + strings.ToUpper(m[1])
	} else if m := myErrnoRE.FindStringSubmatch(errMsg); m != nil {
		code = "MY-" + m[1]
	}
	up := strings.ToUpper(errMsg)
	for _, h := range dbHints {
		if h.code == code || strings.Contains(up, h.code) {
			return h.hint
		}
	}
	return ""
}

// diagnoseResult builds the result_diagnosis block for an executed query:
// zero-row causes (catalog-grounded), NULL-heavy output columns, truncation.
// Nil when there is nothing noteworthy — the happy path stays clean.
func (s *Server) diagnoseResult(sql string, res *dbconn.QueryResult) map[string]any {
	if res == nil {
		return nil
	}
	d := map[string]any{}
	if res.RowCount == 0 {
		d["issue"] = "zero_rows"
		d["hints"] = s.cat().DiagnoseZeroRows(sql)
		d["next_step"] = "힌트를 참고해 조건을 완화한 SQL을 다시 생성/검증한 뒤 재실행하세요. 그래도 0행이면 '해당 조건의 데이터가 없다'고 근거와 함께 답하세요."
		return d
	}
	// NULL-heavy columns: usually a wrong outer-join side or a column that
	// only fills for other grains — worth flagging before the user trusts it.
	nullHeavy := []string{}
	for _, col := range res.Columns {
		nulls := 0
		for _, row := range res.Rows {
			if v, ok := row[col.Name]; !ok || v == nil {
				nulls++
			}
		}
		if len(res.Rows) >= 5 && nulls*100/len(res.Rows) >= 80 {
			nullHeavy = append(nullHeavy, col.Name)
		}
	}
	if len(nullHeavy) > 0 {
		d["issue"] = "null_heavy_columns"
		d["columns"] = nullHeavy
		d["hints"] = []string{"위 컬럼이 80%+ NULL입니다. 조인 방향(LEFT/INNER)이나 해당 컬럼의 적재 조건(grain, 유효기간)을 확인하세요."}
	}
	if res.Truncated {
		d["truncated_note"] = "max_rows에서 잘렸습니다. 집계 질문이면 GROUP BY로 요약하고, 목록 질문이면 정렬 기준을 명확히 해 상위 N만 가져오세요."
	}
	if len(d) == 0 {
		return nil
	}
	return d
}
