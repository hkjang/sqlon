package catalog

import (
	"os"
	"path/filepath"
	"strings"
)

// QueryPattern is a named multi-step SQL shape (two-stage aggregation, top-N
// per group, month-over-month, ratio...). Detected patterns hand the LLM a
// vetted CTE template so it fills slots instead of inventing structure.
type QueryPattern struct {
	Name        string   `json:"name"`
	Keywords    []string `json:"keywords"`
	Description string   `json:"description"`
	Template    string   `json:"template"`
	Slots       []string `json:"slots,omitempty"`
	Caution     string   `json:"caution,omitempty"`
}

var defaultPatterns = []QueryPattern{
	{
		Name:        "two_stage_agg",
		Keywords:    []string{"인별로 집계", "인별 합계", "고객별로 집계한", "집계한 후", "합산 후", "인별로 sum", "고객별 평균을"},
		Description: "1단계에서 개체(고객/계좌)별로 집계한 뒤 2단계에서 그 결과를 다시 집계 (예: 인별 합계의 평균)",
		Template:    "WITH per_entity AS (\n  SELECT {entity_key}, {stage1_agg} AS agg_val\n  FROM {table} {alias}\n  WHERE {filters}\n  GROUP BY {entity_key}\n)\nSELECT {stage2_agg}(agg_val)\nFROM per_entity",
		Slots:       []string{"entity_key", "stage1_agg", "stage2_agg", "table", "filters"},
		Caution:     "1단계 집계 키(고객번호 등)와 2단계 집계 함수를 혼동하지 마세요. AVG(SUM(x)) 같은 중첩 집계는 불가하며 반드시 CTE로 분리해야 합니다.",
	},
	{
		Name:        "top_n_per_group",
		Keywords:    []string{"그룹별 상위", "별 상위", "각각 상위", "별로 가장", "기관별 top", "별 최고"},
		Description: "그룹(기관/등급/지역)마다 상위 N개 행을 선택",
		Template:    "SELECT *\nFROM (\n  SELECT {columns},\n         ROW_NUMBER() OVER (PARTITION BY {group_key} ORDER BY {rank_expr} DESC) AS RN\n  FROM {table} {alias}\n  WHERE {filters}\n) ranked\nWHERE RN <= {n}",
		Slots:       []string{"columns", "group_key", "rank_expr", "table", "filters", "n"},
		Caution:     "전체 상위 N(LIMIT)과 그룹별 상위 N(ROW_NUMBER)을 구분하세요. 동점 처리 필요 시 RANK()를 사용합니다.",
	},
	{
		Name:        "mom_change",
		Keywords:    []string{"전월 대비", "전월대비", "전달 대비", "증감"},
		Description: "월별 집계 후 전월 값과 비교 (증감/증감률)",
		Template:    "WITH monthly AS (\n  SELECT {month_col} AS YM, {agg} AS VAL\n  FROM {table} {alias}\n  WHERE {filters}\n  GROUP BY {month_col}\n)\nSELECT YM, VAL,\n       VAL - LAG(VAL) OVER (ORDER BY YM) AS DIFF,\n       ROUND((VAL - LAG(VAL) OVER (ORDER BY YM)) / NULLIF(LAG(VAL) OVER (ORDER BY YM), 0) * 100, 2) AS PCT_CHANGE\nFROM monthly\nORDER BY YM",
		Slots:       []string{"month_col", "agg", "table", "filters"},
		Caution:     "비교 대상 월이 모두 조회 범위에 포함되도록 기간 조건을 비교월까지 확장해야 합니다 (예: 6월 vs 5월 비교면 5월부터 조회).",
	},
	{
		Name:        "yoy_change",
		Keywords:    []string{"전년 동월", "전년동월", "작년 같은", "전년 대비"},
		Description: "전년 동월 값과 비교",
		Template:    "WITH monthly AS (\n  SELECT {month_col} AS YM, {agg} AS VAL\n  FROM {table} {alias}\n  WHERE {filters}\n  GROUP BY {month_col}\n)\nSELECT cur.YM, cur.VAL, prev.VAL AS PREV_YEAR_VAL,\n       ROUND((cur.VAL - prev.VAL) / NULLIF(prev.VAL, 0) * 100, 2) AS YOY_PCT\nFROM monthly cur\nLEFT JOIN monthly prev\n  ON prev.YM = TO_CHAR(TO_DATE(cur.YM, 'YYYYMM') - INTERVAL '1 year', 'YYYYMM')",
		Slots:       []string{"month_col", "agg", "table", "filters"},
		Caution:     "기간 조건이 전년 동월까지 포함해야 LEFT JOIN 대상이 존재합니다.",
	},
	{
		Name:        "ratio",
		Keywords:    []string{"비율", "비중", "율을", "퍼센트", "%로"},
		Description: "조건부 집계로 분자/분모를 한 번의 스캔으로 계산",
		Template:    "SELECT COUNT(DISTINCT CASE WHEN {numerator_cond} THEN {key} END)\n       / NULLIF(COUNT(DISTINCT {key}), 0) AS RATIO\nFROM {table} {alias}\nWHERE {filters}",
		Slots:       []string{"numerator_cond", "key", "table", "filters"},
		Caution:     "분모가 0이 될 수 있으면 반드시 NULLIF를 사용하세요. 분자/분모의 중복 제거 기준(dedup key)이 같아야 합니다.",
	},
	{
		Name:        "distribution",
		Keywords:    []string{"분포", "구간별", "구간 별", "히스토그램", "대별"},
		Description: "연속값을 구간(bucket)으로 나눠 분포 집계",
		Template:    "SELECT {bucket_expr} AS BUCKET, COUNT(*) AS CNT\nFROM {table} {alias}\nWHERE {filters}\nGROUP BY {bucket_expr}\nORDER BY BUCKET",
		Slots:       []string{"bucket_expr", "table", "filters"},
		Caution:     "구간 정의(CASE, FLOOR(x/100)*100, postgres는 WIDTH_BUCKET 등)를 명시하고 경계값 포함 여부를 밝히세요.",
	},
}

func loadPatterns(dataDir string) ([]QueryPattern, []LoadIssue) {
	path := filepath.Join(dataDir, "patterns.json")
	if _, err := os.Stat(path); err != nil {
		return defaultPatterns, nil
	}
	var patterns []QueryPattern
	if err := readJSON(path, &patterns); err != nil {
		return defaultPatterns, []LoadIssue{{Level: "error", Source: "patterns.json", Message: err.Error()}}
	}
	var issues []LoadIssue
	for _, p := range patterns {
		if p.Name == "" || p.Template == "" || len(p.Keywords) == 0 {
			issues = append(issues, LoadIssue{Level: "error", Source: "patterns.json", Message: "pattern requires name, template, and keywords: " + p.Name})
		}
	}
	return patterns, issues
}

// dialectizePatterns rewrites cross-dialect snippets in the built-in
// templates for MySQL/MariaDB targets. Custom patterns from patterns.json are
// the operator's responsibility and pass through untouched apart from these
// exact-string swaps.
func dialectizePatterns(patterns []QueryPattern, dialect string) []QueryPattern {
	if dialect != "mysql" && dialect != "mariadb" {
		return patterns
	}
	swaps := [][2]string{
		{"ON prev.YM = TO_CHAR(TO_DATE(cur.YM, 'YYYYMM') - INTERVAL '1 year', 'YYYYMM')",
			"ON prev.YM = DATE_FORMAT(DATE_SUB(STR_TO_DATE(CONCAT(cur.YM, '01'), '%Y%m%d'), INTERVAL 1 YEAR), '%Y%m')"},
	}
	out := make([]QueryPattern, len(patterns))
	copy(out, patterns)
	for i := range out {
		for _, sw := range swaps {
			out[i].Template = strings.ReplaceAll(out[i].Template, sw[0], sw[1])
		}
	}
	return out
}

// MatchPatterns returns query patterns whose keywords appear in the question.
func (c *Catalog) MatchPatterns(question string) []QueryPattern {
	q := strings.ToLower(question)
	var out []QueryPattern
	for _, p := range c.Patterns {
		for _, kw := range p.Keywords {
			if kw != "" && strings.Contains(q, strings.ToLower(kw)) {
				out = append(out, p)
				break
			}
		}
	}
	return out
}
