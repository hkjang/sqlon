package dbconn

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Live execution-plan protection: runs the dialect's JSON EXPLAIN for the
// (already guarded) SQL and analyzes the plan tree for risk. EXPLAIN never
// executes the query on any of the three engines, so a SELECT-only account
// is sufficient and no server-side state is created.

type PlanStep struct {
	ID               int    `json:"id"`
	ParentID         int    `json:"parent_id"`
	Depth            int    `json:"depth"`
	Operation        string `json:"operation"`
	Options          string `json:"options,omitempty"`
	ObjectName       string `json:"object_name,omitempty"`
	Cardinality      int64  `json:"cardinality,omitempty"` // 예상 row 수
	Bytes            int64  `json:"bytes,omitempty"`
	Cost             int64  `json:"cost,omitempty"`
	AccessPredicates string `json:"access_predicates,omitempty"`
	FilterPredicates string `json:"filter_predicates,omitempty"`
}

type PlanResult struct {
	ProfileID      string     `json:"profile_id"`
	Dialect        string     `json:"dialect,omitempty"`
	Steps          []PlanStep `json:"steps"`
	TotalCost      int64      `json:"total_cost"`
	MaxCardinality int64      `json:"max_cardinality"`
	Risk           string     `json:"risk"` // low | medium | high
	RiskScore      int        `json:"risk_score"`
	RiskFactors    []string   `json:"risk_factors,omitempty"`
	Suggestions    []string   `json:"suggestions,omitempty"`
	ElapsedMs      int64      `json:"elapsed_ms"`
}

// 위험 임계값 — 실행계획 기반 보호 로직의 판정 기준
const (
	riskRowsHuge      = 1_000_000 // 예상 row 과다
	riskRowsFullScan  = 100_000   // 이 이상을 full scan하면 위험
	riskRowsSort      = 500_000   // 과도한 sort
	riskCostHigh      = 100_000   // 고비용 플랜
	riskNestedLoopMul = 10_000    // NL/조인버퍼 행 수가 이 이상이면 비효율 의심
)

// ExplainPlan runs the dialect-appropriate EXPLAIN and returns the analyzed plan.
func (m *Manager) ExplainPlan(ctx context.Context, profileID, sqlText string) (*PlanResult, error) {
	p, err := m.store.GetProfileByID(ctx, profileID)
	if err != nil {
		return nil, err
	}
	d, err := DialectFor(p.Type)
	if err != nil {
		return nil, err
	}
	// the user SQL must pass the same read-only guard as execution
	if err := ValidateReadOnly(d, sqlText, deniedForProfile(d, p)); err != nil {
		return nil, err
	}
	if err := m.breakerCheck(p.ID); err != nil {
		return nil, err
	}
	db, err := m.db(p)
	if err != nil {
		m.breakerRecord(p.ID, err)
		return nil, err
	}
	qctx, cancel := context.WithTimeout(ctx, durationSeconds(p.Policy.QueryTimeoutSeconds))
	defer cancel()

	cleanSQL := trimSQL(sqlText)
	start := time.Now()
	var res *PlanResult
	if d.Name() == "postgres" {
		var raw []byte
		if err := db.QueryRowContext(qctx, "EXPLAIN (FORMAT JSON) "+cleanSQL).Scan(&raw); err != nil {
			m.breakerRecord(p.ID, err)
			return nil, fmt.Errorf("explain failed: %s", sanitizeDBError(err))
		}
		res, err = AnalyzePostgresPlanJSON(raw)
		if err != nil {
			return nil, err
		}
	} else {
		raw, scanErr := firstColumn(qctx, db, "EXPLAIN FORMAT=JSON "+cleanSQL)
		if scanErr != nil {
			m.breakerRecord(p.ID, scanErr)
			return nil, fmt.Errorf("explain failed: %s", sanitizeDBError(scanErr))
		}
		res, err = AnalyzeMySQLPlanJSON(raw, d.Name())
		if err != nil {
			return nil, err
		}
	}
	m.breakerRecord(p.ID, nil)
	res.ProfileID = p.ID
	res.Dialect = d.Name()
	res.ElapsedMs = time.Since(start).Milliseconds()
	return res, nil
}

// firstColumn runs the statement and returns the first column of the first
// row as bytes (MySQL/MariaDB EXPLAIN output shape varies across versions).
func firstColumn(ctx context.Context, db *sql.DB, stmt string) ([]byte, error) {
	rows, err := db.QueryContext(ctx, stmt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("explain returned no rows")
	}
	values := make([]any, len(cols))
	pointers := make([]any, len(cols))
	for i := range values {
		pointers[i] = &values[i]
	}
	if err := rows.Scan(pointers...); err != nil {
		return nil, err
	}
	switch v := values[0].(type) {
	case []byte:
		return v, nil
	case string:
		return []byte(v), nil
	default:
		return nil, fmt.Errorf("unexpected explain column type %T", values[0])
	}
}

// ---- PostgreSQL plan analysis ----

type pgPlanNode struct {
	NodeType     string       `json:"Node Type"`
	RelationName string       `json:"Relation Name"`
	IndexName    string       `json:"Index Name"`
	PlanRows     float64      `json:"Plan Rows"`
	TotalCost    float64      `json:"Total Cost"`
	Filter       string       `json:"Filter"`
	JoinType     string       `json:"Join Type"`
	JoinFilter   string       `json:"Join Filter"`
	HashCond     string       `json:"Hash Cond"`
	MergeCond    string       `json:"Merge Cond"`
	IndexCond    string       `json:"Index Cond"`
	Plans        []pgPlanNode `json:"Plans"`
}

// AnalyzePostgresPlanJSON parses `EXPLAIN (FORMAT JSON)` output and scores it.
// Pure function so it is unit-testable without a database.
func AnalyzePostgresPlanJSON(raw []byte) (*PlanResult, error) {
	var top []struct {
		Plan pgPlanNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("parse postgres plan: %w", err)
	}
	res := &PlanResult{Risk: "low"}
	if len(top) == 0 {
		res.RiskFactors = append(res.RiskFactors, "empty plan (no steps returned)")
		return res, nil
	}
	score := 0
	addRisk := func(pts int, factor, suggestion string) {
		score += pts
		res.RiskFactors = append(res.RiskFactors, factor)
		if suggestion != "" {
			res.Suggestions = appendUniqueStr(res.Suggestions, suggestion)
		}
	}
	id := 0
	var walk func(n pgPlanNode, parent, depth int)
	walk = func(n pgPlanNode, parent, depth int) {
		myID := id
		id++
		rows := int64(n.PlanRows)
		step := PlanStep{
			ID: myID, ParentID: parent, Depth: depth,
			Operation:        n.NodeType,
			Options:          n.JoinType,
			ObjectName:       firstNonEmpty(n.RelationName, n.IndexName),
			Cardinality:      rows,
			Cost:             int64(n.TotalCost),
			AccessPredicates: firstNonEmpty(n.IndexCond, n.HashCond, n.MergeCond, n.JoinFilter),
			FilterPredicates: n.Filter,
		}
		res.Steps = append(res.Steps, step)
		if rows > res.MaxCardinality {
			res.MaxCardinality = rows
		}
		switch {
		case n.NodeType == "Seq Scan":
			if rows >= riskRowsFullScan {
				addRisk(30, fmt.Sprintf("full table scan (Seq Scan) on %s (~%s rows)", n.RelationName, comma(rows)),
					n.RelationName+"에 인덱스 컬럼(기준일/키 컬럼) 조건을 추가하거나 기간을 좁히세요.")
			} else if rows > 0 {
				addRisk(8, fmt.Sprintf("full table scan (Seq Scan) on %s (small, ~%s rows)", n.RelationName, comma(rows)), "")
			}
		case n.NodeType == "Nested Loop":
			joined := n.JoinFilter != "" || anyChildHasCond(n)
			if !joined {
				addRisk(60, "cartesian join suspected (Nested Loop without join condition)",
					"조인 조건이 누락되었습니다. get_join_paths의 ON 조건을 확인하세요.")
			} else if rows >= riskNestedLoopMul {
				addRisk(20, fmt.Sprintf("nested loop join with ~%s rows", comma(rows)),
					"대량 조인은 해시 조인이 유리합니다. 조인 키 인덱스와 통계를 확인하세요.")
			}
		case n.NodeType == "Sort":
			if rows >= riskRowsSort {
				addRisk(15, fmt.Sprintf("large sort (~%s rows)", comma(rows)),
					"정렬 대상 행을 줄이세요 (기간 조건 또는 사전 집계).")
			}
		case strings.Contains(n.NodeType, "Aggregate"):
			if rows >= riskRowsHuge {
				addRisk(15, fmt.Sprintf("large aggregate (~%s rows)", comma(rows)),
					"집계 전 필터를 강화해 입력 행 수를 줄이세요.")
			}
		}
		for _, c := range n.Plans {
			walk(c, myID, depth+1)
		}
	}
	walk(top[0].Plan, -1, 0)
	res.TotalCost = int64(top[0].Plan.TotalCost)
	finishRisk(res, score)
	return res, nil
}

func anyChildHasCond(n pgPlanNode) bool {
	for _, c := range n.Plans {
		if c.IndexCond != "" || c.HashCond != "" || c.MergeCond != "" || c.JoinFilter != "" {
			return true
		}
		if anyChildHasCond(c) {
			return true
		}
	}
	return false
}

// ---- MySQL / MariaDB plan analysis ----

// mysqlWalkKeys are the JSON keys that can contain nested table nodes in
// MySQL and MariaDB EXPLAIN FORMAT=JSON trees.
var mysqlWalkKeys = map[string]bool{
	"query_block": true, "table": true, "nested_loop": true,
	"grouping_operation": true, "ordering_operation": true,
	"duplicates_removal": true, "materialized_from_subquery": true,
	"attached_subqueries": true, "union_result": true,
	"query_specifications": true, "query_specification": true,
	"subqueries": true, "temporary_table": true, "read_sorted_file": true,
	"having_subqueries": true, "select_list_subqueries": true,
}

// AnalyzeMySQLPlanJSON parses `EXPLAIN FORMAT=JSON` output from MySQL or
// MariaDB (differently shaped trees, walked generically) and scores it.
// Pure function, unit-testable.
func AnalyzeMySQLPlanJSON(raw []byte, dialect string) (*PlanResult, error) {
	var tree map[string]any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("parse %s plan: %w", dialect, err)
	}
	res := &PlanResult{Risk: "low"}
	score := 0
	addRisk := func(pts int, factor, suggestion string) {
		score += pts
		res.RiskFactors = append(res.RiskFactors, factor)
		if suggestion != "" {
			res.Suggestions = appendUniqueStr(res.Suggestions, suggestion)
		}
	}

	// total cost: MySQL query_block.cost_info.query_cost (string); MariaDB's
	// EXPLAIN JSON has no cost field.
	if qb, ok := tree["query_block"].(map[string]any); ok {
		if ci, ok := qb["cost_info"].(map[string]any); ok {
			if s, ok := ci["query_cost"].(string); ok {
				if f, err := strconv.ParseFloat(s, 64); err == nil {
					res.TotalCost = int64(f)
				}
			}
		}
	}

	id := 0
	joinTables := 0
	joinBuffered := 0
	filesort := false
	var walk func(v any, depth int)
	walk = func(v any, depth int) {
		switch node := v.(type) {
		case map[string]any:
			if name, ok := node["table_name"].(string); ok {
				access, _ := node["access_type"].(string)
				key, _ := node["key"].(string)
				cond, _ := node["attached_condition"].(string)
				rows := mysqlRows(node)
				res.Steps = append(res.Steps, PlanStep{
					ID: id, ParentID: -1, Depth: depth,
					Operation:        "TABLE ACCESS",
					Options:          strings.ToUpper(access),
					ObjectName:       name,
					Cardinality:      rows,
					AccessPredicates: key,
					FilterPredicates: cond,
				})
				id++
				if rows > res.MaxCardinality {
					res.MaxCardinality = rows
				}
				joinTables++
				if jb, ok := node["using_join_buffer"].(string); ok && jb != "" {
					joinBuffered++
				}
				if strings.EqualFold(access, "ALL") {
					if rows >= riskRowsFullScan {
						addRisk(30, fmt.Sprintf("full table scan on %s (~%s rows)", name, comma(rows)),
							name+"에 인덱스 컬럼(기준일/키 컬럼) 조건을 추가하거나 기간을 좁히세요.")
					} else if rows > 0 {
						addRisk(8, fmt.Sprintf("full table scan on %s (small, ~%s rows)", name, comma(rows)), "")
					}
				}
			}
			if fs, ok := node["using_filesort"].(bool); ok && fs {
				filesort = true
			}
			if fs, ok := node["filesort"].(map[string]any); ok { // MariaDB
				filesort = true
				walk(fs, depth+1)
			}
			for k, child := range node {
				if mysqlWalkKeys[k] || strings.HasSuffix(k, "-join") {
					walk(child, depth+1)
				}
			}
		case []any:
			for _, item := range node {
				walk(item, depth)
			}
		}
	}
	walk(tree, 0)

	if filesort && res.MaxCardinality >= riskRowsSort {
		addRisk(15, fmt.Sprintf("filesort over ~%s rows", comma(res.MaxCardinality)),
			"정렬 대상 행을 줄이세요 (기간 조건 또는 사전 집계).")
	}
	// join buffer (block nested loop / hash join) with 2+ tables and a large
	// inner side usually means the join has no usable condition or index.
	if joinTables >= 2 && joinBuffered > 0 && res.MaxCardinality >= riskNestedLoopMul {
		addRisk(40, "join buffer (block nested loop/hash) on a large inner table — join condition or index may be missing",
			"조인 조건이 누락되었거나 조인 키 인덱스가 없습니다. get_join_paths의 ON 조건과 인덱스를 확인하세요.")
	}
	if len(res.Steps) == 0 {
		res.RiskFactors = append(res.RiskFactors, "empty plan (no table steps found)")
	}
	finishRisk(res, score)
	return res, nil
}

// mysqlRows extracts the best row estimate from a table node: MySQL uses
// rows_examined_per_scan / rows_produced_per_join, MariaDB uses rows.
func mysqlRows(node map[string]any) int64 {
	for _, k := range []string{"rows_examined_per_scan", "rows_produced_per_join", "rows"} {
		if v, ok := node[k]; ok {
			switch n := v.(type) {
			case float64:
				return int64(n)
			case string:
				if f, err := strconv.ParseFloat(n, 64); err == nil {
					return int64(f)
				}
			}
		}
	}
	return 0
}

// ---- shared scoring ----

func finishRisk(res *PlanResult, score int) {
	if res.MaxCardinality >= riskRowsHuge {
		score += 20
		res.RiskFactors = append(res.RiskFactors, fmt.Sprintf("estimated rows too large (max ~%s)", comma(res.MaxCardinality)))
		res.Suggestions = appendUniqueStr(res.Suggestions, "기간 조건(기준월/기준일)이나 파티션 키 조건을 추가하세요.")
	}
	if res.TotalCost >= riskCostHigh {
		score += 15
		res.RiskFactors = append(res.RiskFactors, fmt.Sprintf("high plan cost (%s)", comma(res.TotalCost)))
		res.Suggestions = appendUniqueStr(res.Suggestions, "timeout 가능성이 높습니다. 조건을 좁히고 미리보기로 먼저 확인하세요.")
	}
	res.RiskScore = score
	switch {
	case score >= 60:
		res.Risk = "high"
	case score >= 25:
		res.Risk = "medium"
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func comma(n int64) string {
	s := fmt.Sprint(n)
	out := ""
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out += ","
		}
		out += string(c)
	}
	return out
}

func appendUniqueStr(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}
