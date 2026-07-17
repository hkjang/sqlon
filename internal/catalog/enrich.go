package catalog

import (
	"sort"
	"strings"
)

// Rule-based semantic metadata enrichment (FR-META-009/010/011/012). This
// engine produces REVIEWABLE candidates for logical names, semantic types, and
// descriptions — with evidence and a confidence score — and NEVER writes them
// into the operational catalog. It is fully deterministic and offline (no
// external API), satisfying the "규칙 엔진으로 동작" requirement; an LLM MCP
// client can refine the candidates, but the server yields useful defaults on
// its own. Every suggestion carries generator/confidence/evidence so it is
// explainable (FR-META-012), and only surfaces where a value is missing.

// SemanticSuggestion is one reviewable metadata candidate.
type SemanticSuggestion struct {
	Kind       string   `json:"kind"` // logical_name | semantic_type | description
	Table      string   `json:"table"`
	Column     string   `json:"column,omitempty"`
	Current    string   `json:"current,omitempty"`  // existing value being filled
	Suggested  string   `json:"suggested"`          //
	Confidence float64  `json:"confidence"`         // 0..1
	Evidence   []string `json:"evidence,omitempty"` //
	Generator  string   `json:"generator"`          // always "rule" here
	Status     string   `json:"review_status"`      // "suggested" — never applied
}

// abbrevExpansions maps common column-name tokens to Korean business words for
// logical-name generation (FR-META-009 약어 분리). Operator glossary terms take
// priority over this built-in list.
var abbrevExpansions = map[string]string{
	"NO": "번호", "NM": "명", "CD": "코드", "AMT": "금액", "AMOUNT": "금액",
	"DT": "일자", "DATE": "일자", "TM": "시각", "TIME": "시각", "YN": "여부",
	"CNT": "건수", "COUNT": "건수", "QTY": "수량", "RT": "율", "RATE": "율",
	"SCORE": "점수", "SCR": "점수", "GRAD": "등급", "GRADE": "등급", "BAL": "잔액",
	"ID": "식별자", "SEQ": "순번", "ORD": "순서", "STAT": "상태", "STATUS": "상태",
	"TP": "유형", "TYPE": "유형", "DIV": "구분", "REG": "등록", "MOD": "수정",
	"CUST": "고객", "ACCT": "계좌", "ACCOUNT": "계좌", "PROD": "상품", "USR": "사용자",
	"USER": "사용자", "ADDR": "주소", "TEL": "전화", "EMAIL": "이메일", "YR": "연",
	"MON": "월", "DAY": "일", "DESC": "설명", "DESCRIPTION": "설명",
}

// SuggestSemanticMetadata generates logical-name / semantic-type / description
// candidates for the given tables (all tables if empty), restricted to the
// requested kinds (all if empty).
func (c *Catalog) SuggestSemanticMetadata(tables []string, kinds []string) map[string]any {
	wantKind := map[string]bool{}
	for _, k := range kinds {
		wantKind[strings.ToLower(strings.TrimSpace(k))] = true
	}
	kindOn := func(k string) bool { return len(wantKind) == 0 || wantKind[k] }

	var targets []*Table
	if len(tables) == 0 {
		for _, t := range c.Tables {
			targets = append(targets, t)
		}
	} else {
		for _, tn := range tables {
			if t, ok := c.ResolveTable(tn); ok {
				targets = append(targets, t)
			}
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].FQN < targets[j].FQN })

	// cross-table logical-name reuse index (FR-META-009 동일 컬럼)
	nameIndex := c.columnLogicalNameIndex()

	var out []SemanticSuggestion
	for _, t := range targets {
		for _, col := range t.Columns {
			if kindOn("logical_name") && col.LogicalName == "" {
				if s, ok := c.suggestLogicalName(t, col, nameIndex); ok {
					out = append(out, s)
				}
			}
			if kindOn("semantic_type") && col.SemanticType == "" {
				if s, ok := suggestSemanticType(t, col); ok {
					out = append(out, s)
				}
			}
			if kindOn("description") && col.Description == "" {
				if s, ok := c.suggestDescription(t, col); ok {
					out = append(out, s)
				}
			}
		}
	}

	// paste-ready overrides.json columns[] snippet for the top-confidence items
	snippet := buildOverridesSnippet(out)
	return map[string]any{
		"suggestions":       out,
		"count":             len(out),
		"overrides_snippet": snippet,
		"how_to_apply":      "검토 후 overrides_snippet을 overrides.json의 columns 배열에 병합하고 서버를 재기동(또는 put_dataset)하세요. 자동 적용되지 않습니다. 규칙 엔진 기본값이므로 업무 담당자 또는 LLM이 다듬어 승인하세요.",
		"generator":         "rule",
		"note":              "모든 후보는 review_status=suggested 상태이며 운영 카탈로그에 자동 반영되지 않습니다.",
	}
}

// suggestLogicalName builds a Korean logical name from (in priority order): a
// DB comment, a glossary term matching the column, cross-table reuse, and
// abbreviation expansion.
func (c *Catalog) suggestLogicalName(t *Table, col *Column, nameIndex map[string]nameVote) (SemanticSuggestion, bool) {
	s := SemanticSuggestion{Kind: "logical_name", Table: t.FQN, Column: col.Name, Generator: "rule", Status: "suggested"}
	name := strings.ToUpper(col.Name)

	// 1) DB comment / description already present on the physical column
	if col.CodeDict != "" && looksLikeLabel(col.Description) {
		s.Suggested = firstSentence(col.Description)
		s.Confidence = 0.85
		s.Evidence = []string{"derived from column comment/description"}
		return s, true
	}
	// 2) glossary: a term whose synonym list contains this column name/token
	if term, hit := c.glossaryTermForColumn(col.Name); hit {
		s.Suggested = term
		s.Confidence = 0.9
		s.Evidence = []string{"business glossary term for '" + col.Name + "'"}
		return s, true
	}
	// 3) cross-table reuse: same column name already has a logical name elsewhere
	if v, ok := nameIndex[strings.ToUpper(col.Name)]; ok && v.count >= 1 {
		s.Suggested = v.name
		s.Confidence = 0.8
		s.Evidence = []string{"same column name is labeled in " + itoa(v.count) + " other table(s)"}
		return s, true
	}
	// 4) abbreviation expansion of the name tokens
	if expanded, evid := expandName(name); expanded != "" {
		s.Suggested = expanded
		s.Confidence = 0.6
		s.Evidence = []string{"abbreviation expansion: " + evid}
		return s, true
	}
	return s, false
}

// suggestSemanticType classifies a column into a business semantic type from
// its name suffix/pattern and data type (FR-META-011). Complements the
// date-focused inferSemanticType used at load time.
func suggestSemanticType(t *Table, col *Column) (SemanticSuggestion, bool) {
	s := SemanticSuggestion{Kind: "semantic_type", Table: t.FQN, Column: col.Name, Generator: "rule", Status: "suggested"}
	n := strings.ToUpper(col.Name)
	dt := strings.ToLower(col.DataType)

	typ, conf, evid := "", 0.0, ""
	switch {
	case col.PII:
		typ, conf, evid = "PII", 0.9, "column flagged pii=true"
	case hasSuffix(n, "_YN") || n == "USE_YN" || strings.HasSuffix(n, "FLAG"):
		typ, conf, evid = "FLAG", 0.85, "Y/N flag naming"
	case hasSuffix(n, "_CD") || hasSuffix(n, "_CODE") || hasSuffix(n, "_TP") || hasSuffix(n, "_TYPE") || hasSuffix(n, "_DIV") || col.CodeDict != "":
		typ, conf, evid = "CODE", 0.85, "code-column naming/dictionary"
	case hasSuffix(n, "_AMT") || hasSuffix(n, "_AMOUNT") || hasSuffix(n, "_BAL"):
		typ, conf, evid = "AMOUNT", 0.85, "amount/balance naming"
	case hasSuffix(n, "_RT") || hasSuffix(n, "_RATE") || strings.Contains(n, "RATIO"):
		typ, conf, evid = "RATIO", 0.8, "rate/ratio naming"
	case strings.Contains(n, "SCORE") || hasSuffix(n, "_SCR"):
		typ, conf, evid = "SCORE", 0.8, "score naming"
	case hasSuffix(n, "_CNT") || hasSuffix(n, "_COUNT") || hasSuffix(n, "_QTY"):
		typ, conf, evid = "COUNT", 0.8, "count/quantity naming"
	case hasSuffix(n, "_NM") || hasSuffix(n, "_NAME") || strings.HasSuffix(n, "명"):
		typ, conf, evid = "NAME", 0.75, "name-column naming"
	case col.IsPK || hasSuffix(n, "_NO") || hasSuffix(n, "_ID") || hasSuffix(n, "_SEQ"):
		typ, conf, evid = "IDENTIFIER", 0.7, "identifier/key naming"
	case strings.Contains(dt, "date") || strings.Contains(dt, "timestamp") || hasSuffix(n, "_DT") || hasSuffix(n, "_DATE"):
		typ, conf, evid = "DATE", 0.7, "date type/naming"
	}
	if typ == "" {
		return s, false
	}
	s.Suggested, s.Confidence, s.Evidence = typ, conf, []string{evid}
	return s, true
}

// suggestDescription builds a short structured description from what is known
// (FR-META-010) — concept from logical name/type, plus null/PII notes.
func (c *Catalog) suggestDescription(t *Table, col *Column) (SemanticSuggestion, bool) {
	s := SemanticSuggestion{Kind: "description", Table: t.FQN, Column: col.Name, Generator: "rule", Status: "suggested"}
	var parts []string
	concept := col.LogicalName
	if concept == "" {
		if exp, _ := expandName(strings.ToUpper(col.Name)); exp != "" {
			concept = exp
		}
	}
	if concept != "" {
		parts = append(parts, t.LogicalNameOr()+"의 "+concept)
	} else {
		return s, false // nothing meaningful to say yet
	}
	if styp, _ := suggestSemanticType(t, col); styp.Suggested != "" {
		parts = append(parts, "("+semanticTypeKO(styp.Suggested)+")")
	}
	if strings.EqualFold(col.Nullable, "Y") || strings.EqualFold(col.Nullable, "YES") {
		parts = append(parts, "NULL 허용")
	}
	if col.PII {
		parts = append(parts, "개인정보")
	}
	s.Suggested = strings.Join(parts, " ")
	s.Confidence = 0.55
	s.Evidence = []string{"rule-based template from logical name / semantic type"}
	return s, true
}

// ---- helpers ----

type nameVote struct {
	name  string
	count int
}

// columnLogicalNameIndex maps UPPER(column name) → the most common existing
// logical name across all tables (for cross-table reuse).
func (c *Catalog) columnLogicalNameIndex() map[string]nameVote {
	votes := map[string]map[string]int{}
	for _, t := range c.Tables {
		for _, col := range t.Columns {
			if col.LogicalName == "" {
				continue
			}
			key := strings.ToUpper(col.Name)
			if votes[key] == nil {
				votes[key] = map[string]int{}
			}
			votes[key][col.LogicalName]++
		}
	}
	out := map[string]nameVote{}
	for key, m := range votes {
		best, bestN := "", 0
		for name, n := range m {
			if n > bestN || (n == bestN && name < best) {
				best, bestN = name, n
			}
		}
		out[key] = nameVote{name: best, count: bestN}
	}
	return out
}

// glossaryTermForColumn returns the glossary term whose synonym list contains
// the column name (case-insensitive), if any.
func (c *Catalog) glossaryTermForColumn(colName string) (string, bool) {
	if c.Glossary == nil {
		return "", false
	}
	lc := strings.ToLower(colName)
	for _, e := range c.Glossary.Entries {
		for _, syn := range e.Synonyms {
			if strings.EqualFold(strings.TrimSpace(syn), lc) {
				return e.Term, true
			}
		}
	}
	return "", false
}

// expandName expands an UPPER_SNAKE column name into a Korean phrase using the
// abbreviation dictionary. Returns "" if no token expands.
func expandName(upper string) (string, string) {
	tokens := strings.FieldsFunc(upper, func(r rune) bool { return r == '_' })
	var parts, matched []string
	for _, tok := range tokens {
		if exp, ok := abbrevExpansions[tok]; ok {
			parts = append(parts, exp)
			matched = append(matched, tok+"→"+exp)
		}
	}
	if len(matched) == 0 {
		return "", ""
	}
	return strings.Join(parts, " "), strings.Join(matched, ", ")
}

func semanticTypeKO(t string) string {
	switch t {
	case "IDENTIFIER":
		return "식별자"
	case "NAME":
		return "명칭"
	case "CODE":
		return "코드"
	case "AMOUNT":
		return "금액"
	case "RATIO":
		return "비율"
	case "SCORE":
		return "점수"
	case "COUNT":
		return "건수"
	case "DATE":
		return "날짜"
	case "FLAG":
		return "플래그"
	case "PII":
		return "개인정보"
	default:
		return t
	}
}

func hasSuffix(name, suffix string) bool { return strings.HasSuffix(name, suffix) }

func looksLikeLabel(s string) bool {
	s = strings.TrimSpace(s)
	return s != "" && len([]rune(s)) <= 40 && !strings.Contains(s, "\n")
}

// buildOverridesSnippet renders the high-confidence suggestions as an
// overrides.json columns[] fragment for operator review.
func buildOverridesSnippet(suggestions []SemanticSuggestion) []map[string]any {
	byCol := map[string]map[string]any{}
	var order []string
	for _, s := range suggestions {
		if s.Column == "" || s.Confidence < 0.7 {
			continue
		}
		key := s.Table + "|" + s.Column
		entry := byCol[key]
		if entry == nil {
			entry = map[string]any{"table": s.Table, "column": s.Column}
			byCol[key] = entry
			order = append(order, key)
		}
		switch s.Kind {
		case "logical_name":
			entry["logical_name"] = s.Suggested
		case "semantic_type":
			entry["semantic_type"] = s.Suggested
		case "description":
			entry["description"] = s.Suggested
		}
	}
	out := make([]map[string]any, 0, len(order))
	for _, k := range order {
		out = append(out, byCol[k])
	}
	return out
}

// LogicalNameOr returns the table logical name or its physical name.
func (t *Table) LogicalNameOr() string {
	if t.LogicalName != "" {
		return t.LogicalName
	}
	return t.Name
}
