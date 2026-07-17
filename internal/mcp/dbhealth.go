package mcp

import (
	"context"
	"strconv"
	"strings"
)

// DB health report (DBA co-pilot). Runs read-only system-catalog diagnostics
// against a connected profile and flags the classic issues a DBA checks for:
// tables without a primary key, foreign-key columns lacking a supporting index,
// unused indexes, stale planner statistics, and the largest tables (with
// comment coverage). Read-only; touches only pg_catalog / information_schema.

type healthCheck struct {
	ID          string           `json:"id"`
	Title       string           `json:"title"`
	Severity    string           `json:"severity"` // high | medium | low | info
	Count       int              `json:"count"`
	Findings    []map[string]any `json:"findings,omitempty"`
	Unsupported bool             `json:"unsupported,omitempty"`
	Note        string           `json:"note,omitempty"`
}

// pgHealthQueries: PostgreSQL has the richest system views, so all checks run.
var pgHealthQueries = []struct {
	id, title, severity, sql, note string
}{
	{
		id: "tables_without_pk", title: "기본키 없는 테이블", severity: "high",
		note: "PK가 없으면 논리 복제·CDC·중복 방지가 깨지고 행 단위 갱신이 위험합니다.",
		sql: `SELECT n.nspname AS schema, c.relname AS "table"
			FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
			WHERE c.relkind='r' AND n.nspname NOT IN ('pg_catalog','information_schema')
			AND NOT EXISTS (SELECT 1 FROM pg_index i WHERE i.indrelid=c.oid AND i.indisprimary)
			ORDER BY 1,2 LIMIT 200`,
	},
	{
		id: "unindexed_fk", title: "인덱스 없는 외래키 컬럼", severity: "medium",
		note: "FK 컬럼에 인덱스가 없으면 조인·부모행 삭제/갱신이 순차 스캔으로 급격히 느려집니다.",
		sql: `SELECT n.nspname AS schema, cl.relname AS "table", con.conname AS fk, a.attname AS column
			FROM pg_constraint con
			JOIN pg_class cl ON cl.oid=con.conrelid
			JOIN pg_namespace n ON n.oid=cl.relnamespace
			JOIN pg_attribute a ON a.attrelid=cl.oid AND a.attnum=con.conkey[1]
			WHERE con.contype='f' AND n.nspname NOT IN ('pg_catalog','information_schema')
			AND NOT EXISTS (
				SELECT 1 FROM pg_index i WHERE i.indrelid=con.conrelid AND i.indkey[0]=con.conkey[1]
			) ORDER BY 1,2 LIMIT 200`,
	},
	{
		id: "unused_indexes", title: "미사용 인덱스", severity: "low",
		note: "스캔 0회 인덱스는 쓰기 오버헤드·디스크만 차지합니다(통계 리셋 이후 기준).",
		sql: `SELECT s.schemaname AS schema, s.relname AS "table", s.indexrelname AS index,
			s.idx_scan AS scans, pg_size_pretty(pg_relation_size(s.indexrelid)) AS size
			FROM pg_stat_user_indexes s JOIN pg_index i ON i.indexrelid=s.indexrelid
			WHERE i.indisprimary=false AND i.indisunique=false AND s.idx_scan=0
			ORDER BY pg_relation_size(s.indexrelid) DESC LIMIT 50`,
	},
	{
		id: "stale_statistics", title: "통계 오래됨/없음", severity: "medium",
		note: "통계가 오래되면 옵티마이저가 잘못된 실행계획을 골라 성능이 급락합니다. ANALYZE를 검토하세요.",
		sql: `SELECT schemaname AS schema, relname AS "table",
			COALESCE(GREATEST(last_analyze,last_autoanalyze)::text,'never') AS last_analyzed, n_live_tup AS rows
			FROM pg_stat_user_tables
			WHERE (GREATEST(last_analyze,last_autoanalyze) IS NULL
				OR GREATEST(last_analyze,last_autoanalyze) < now()-interval '30 days')
			AND n_live_tup > 1000 ORDER BY n_live_tup DESC LIMIT 50`,
	},
	{
		id: "large_tables", title: "대형 테이블 (코멘트 여부)", severity: "info",
		note: "상위 용량 테이블. 코멘트 없는 대형 테이블은 메타데이터 보강 우선 대상입니다.",
		sql: `SELECT n.nspname AS schema, c.relname AS "table",
			pg_size_pretty(pg_total_relation_size(c.oid)) AS size,
			pg_total_relation_size(c.oid) AS size_bytes,
			(obj_description(c.oid,'pg_class') IS NOT NULL) AS has_comment
			FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
			WHERE c.relkind='r' AND n.nspname NOT IN ('pg_catalog','information_schema')
			ORDER BY pg_total_relation_size(c.oid) DESC LIMIT 20`,
	},
}

// myHealthQueries: MySQL/MariaDB via information_schema (current database).
// InnoDB auto-creates FK indexes and per-index usage needs performance_schema,
// so those checks are marked unsupported on this engine.
var myHealthQueries = []struct {
	id, title, severity, sql, note string
}{
	{
		id: "tables_without_pk", title: "기본키 없는 테이블", severity: "high",
		note: "PK가 없으면 복제·CDC·중복 방지가 깨지고 InnoDB 성능도 저하됩니다.",
		sql: "SELECT t.table_schema AS `schema`, t.table_name AS `table` " +
			"FROM information_schema.tables t WHERE t.table_type='BASE TABLE' AND t.table_schema=DATABASE() " +
			"AND NOT EXISTS (SELECT 1 FROM information_schema.statistics s " +
			"WHERE s.table_schema=t.table_schema AND s.table_name=t.table_name AND s.index_name='PRIMARY') " +
			"ORDER BY 1,2 LIMIT 200",
	},
	{
		id: "large_tables", title: "대형 테이블 (코멘트 여부)", severity: "info",
		note: "상위 용량 테이블(데이터+인덱스). 코멘트 없는 대형 테이블은 메타 보강 우선 대상.",
		sql: "SELECT table_schema AS `schema`, table_name AS `table`, " +
			"ROUND((COALESCE(data_length,0)+COALESCE(index_length,0))/1024/1024,1) AS size_mb, " +
			"(COALESCE(data_length,0)+COALESCE(index_length,0)) AS size_bytes, " +
			"(COALESCE(table_comment,'')<>'') AS has_comment " +
			"FROM information_schema.tables WHERE table_schema=DATABASE() AND table_type='BASE TABLE' " +
			"ORDER BY size_bytes DESC LIMIT 20",
	},
}

var myUnsupported = []healthCheck{
	{ID: "unindexed_fk", Title: "인덱스 없는 외래키 컬럼", Severity: "medium", Unsupported: true,
		Note: "MySQL/MariaDB(InnoDB)는 FK에 인덱스를 자동 생성하므로 해당 위험이 낮습니다."},
	{ID: "unused_indexes", Title: "미사용 인덱스", Severity: "low", Unsupported: true,
		Note: "인덱스별 사용 통계는 performance_schema/sys 스키마 권한이 필요해 이 계정에서 평가하지 않습니다."},
	{ID: "stale_statistics", Title: "통계 오래됨/없음", Severity: "medium", Unsupported: true,
		Note: "InnoDB 통계 신선도는 표준 information_schema로 신뢰성 있게 조회하기 어려워 생략합니다."},
}

// mcpDBHealthReport runs the diagnostics for a profile and summarizes them.
func (s *Server) mcpDBHealthReport(ctx context.Context, profileID string) map[string]any {
	dialect, err := s.DB.ProfileDialect(ctx, profileID)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	checks := []healthCheck{}
	sevCount := map[string]int{"high": 0, "medium": 0, "low": 0, "info": 0}

	run := func(id, title, severity, sql, note string) {
		hc := healthCheck{ID: id, Title: title, Severity: severity, Note: note, Findings: []map[string]any{}}
		rows, qerr := s.DB.SystemQuery(ctx, profileID, sql)
		if qerr != nil {
			hc.Unsupported = true
			hc.Note = "조회 실패(권한/뷰 미지원): " + qerr.Error()
		} else {
			hc.Findings = rows
			hc.Count = len(rows)
			if severity != "info" {
				sevCount[severity] += len(rows)
			}
		}
		checks = append(checks, hc)
	}

	switch dialect {
	case "postgres":
		for _, q := range pgHealthQueries {
			run(q.id, q.title, q.severity, q.sql, q.note)
		}
	case "mysql", "mariadb":
		for _, q := range myHealthQueries {
			run(q.id, q.title, q.severity, q.sql, q.note)
		}
		checks = append(checks, myUnsupported...)
	default:
		return map[string]any{"error": "unsupported dialect: " + dialect}
	}

	issues := sevCount["high"] + sevCount["medium"] + sevCount["low"]
	return map[string]any{
		"profile":     profileID,
		"dialect":     dialect,
		"checks":      checks,
		"issues":      issues,
		"by_severity": sevCount,
		"headline":    dbHealthHeadline(sevCount),
		"note":        "읽기 전용 시스템 카탈로그 진단입니다. 인덱스 추가/ANALYZE 등은 DBA가 검토 후 수행하세요(자동 실행하지 않습니다).",
	}
}

func dbHealthHeadline(sev map[string]int) string {
	if sev["high"] == 0 && sev["medium"] == 0 && sev["low"] == 0 {
		return "심각한 점검 이슈가 없습니다."
	}
	var parts []string
	if sev["high"] > 0 {
		parts = append(parts, strconv.Itoa(sev["high"])+"건 높음")
	}
	if sev["medium"] > 0 {
		parts = append(parts, strconv.Itoa(sev["medium"])+"건 중간")
	}
	if sev["low"] > 0 {
		parts = append(parts, strconv.Itoa(sev["low"])+"건 낮음")
	}
	return strings.Join(parts, " · ")
}
