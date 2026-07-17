package catalog

import (
	"sort"
	"strconv"
	"strings"
)

// Rule-based model-candidate generation (Phase 6, FR-META-013/014/015). Like
// the semantic enrichment engine, this produces REVIEWABLE candidates only —
// code dictionaries, metric definitions, and table relations inferred from
// naming, profiling, and type compatibility. Nothing is written to the
// operational catalog; every candidate carries evidence + confidence and stays
// review_status=suggested until an operator approves it.

// ModelCandidate is one reviewable code-dictionary / metric / relation proposal.
type ModelCandidate struct {
	Kind       string         `json:"kind"` // code_dict | metric | relation
	Table      string         `json:"table"`
	Column     string         `json:"column,omitempty"`
	Target     string         `json:"target,omitempty"` // referenced table (relation)
	Suggested  map[string]any `json:"suggested"`
	Confidence float64        `json:"confidence"`
	Evidence   []string       `json:"evidence,omitempty"`
	Generator  string         `json:"generator"`     // always "rule"
	Status     string         `json:"review_status"` // "suggested" — never applied
}

// SuggestModelCandidates generates code-dictionary, metric, and relation
// candidates for the given tables (all if empty), restricted to the requested
// kinds (all if empty).
func (c *Catalog) SuggestModelCandidates(tables []string, kinds []string) map[string]any {
	wantKind := map[string]bool{}
	for _, k := range kinds {
		wantKind[strings.ToLower(strings.TrimSpace(k))] = true
	}
	kindOn := func(k string) bool { return len(wantKind) == 0 || wantKind[k] }

	targets := c.resolveTargets(tables)

	var out []ModelCandidate
	if kindOn("code_dict") {
		for _, t := range targets {
			for _, col := range t.Columns {
				if cand, ok := c.suggestCodeDict(t, col); ok {
					out = append(out, cand)
				}
			}
		}
	}
	if kindOn("metric") {
		covered := c.metricColumnCoverage()
		for _, t := range targets {
			for _, col := range t.Columns {
				if cand, ok := c.suggestMetric(t, col, covered); ok {
					out = append(out, cand)
				}
			}
		}
	}
	if kindOn("relation") {
		existing := c.relationCoverage()
		pkIndex := c.pkColumnIndex()
		nameIndex := c.tableStemIndex()
		for _, t := range targets {
			for _, col := range t.Columns {
				if cand, ok := c.suggestRelation(t, col, existing, pkIndex, nameIndex); ok {
					out = append(out, cand)
				}
			}
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Confidence != out[j].Confidence {
			return out[i].Confidence > out[j].Confidence
		}
		return out[i].Table+out[i].Column < out[j].Table+out[j].Column
	})

	byKind := map[string]int{}
	for _, cand := range out {
		byKind[cand.Kind]++
	}
	return map[string]any{
		"candidates":   out,
		"count":        len(out),
		"count_bykind": byKind,
		"generator":    "rule",
		"how_to_apply": "검토 후 code_dict는 code_dictionary.json/컬럼 code_dict, metric은 metrics.json, relation은 relations.json에 반영하고 재기동하세요. 자동 적용되지 않습니다.",
		"note":         "모든 후보는 review_status=suggested 상태이며 운영 카탈로그에 자동 반영되지 않습니다. 규칙 엔진 기본값이므로 담당자/LLM이 라벨·표현식·카디널리티를 확정해야 합니다.",
	}
}

func (c *Catalog) resolveTargets(tables []string) []*Table {
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
	return targets
}

// ---- code dictionary (FR-META-013) ----

// suggestCodeDict proposes a code-dictionary skeleton for a low-cardinality
// code column that has none, seeding entries from profiled top values.
func (c *Catalog) suggestCodeDict(t *Table, col *Column) (ModelCandidate, bool) {
	if col.CodeDict != "" || col.CommonCode != "" {
		return ModelCandidate{}, false // already has a dictionary
	}
	if col.PII {
		return ModelCandidate{}, false // never build a value list for PII
	}
	n := strings.ToUpper(col.Name)
	codeName := col.SemanticType == "CODE" || hasSuffix(n, "_CD") || hasSuffix(n, "_CODE") ||
		hasSuffix(n, "_TP") || hasSuffix(n, "_TYPE") || hasSuffix(n, "_DIV") ||
		hasSuffix(n, "_YN") || hasSuffix(n, "_STATUS") || hasSuffix(n, "_STAT")
	lowCard := col.Stats != nil && col.Stats.DistinctCount > 0 && col.Stats.DistinctCount <= 20
	if !codeName && !lowCard {
		return ModelCandidate{}, false
	}
	if col.Stats == nil || len(col.Stats.TopValues) == 0 {
		return ModelCandidate{}, false // nothing to seed the dictionary with
	}

	entries := make([]map[string]any, 0, len(col.Stats.TopValues))
	for _, tv := range col.Stats.TopValues {
		v := strings.TrimSpace(tv.Value)
		if v == "" {
			continue
		}
		e := map[string]any{"code": v, "label": tv.Label}
		if tv.Ratio > 0 {
			e["ratio"] = tv.Ratio
		}
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return ModelCandidate{}, false
	}

	conf := 0.6
	evid := []string{plural(col.Stats.DistinctCount, "distinct value"), "seeded from profiled top values"}
	switch {
	case col.SemanticType == "CODE":
		conf, evid = 0.75, append([]string{"semantic_type=CODE"}, evid...)
	case codeName:
		conf, evid = 0.7, append([]string{"code-like column name"}, evid...)
	default:
		evid = append([]string{"low cardinality (≤20 distinct)"}, evid...)
	}

	dictName := strings.ToLower(t.Name + "_" + col.Name + "_cd")
	return ModelCandidate{
		Kind: "code_dict", Table: t.FQN, Column: col.Name,
		Suggested: map[string]any{
			"code_dict": dictName,
			"entries":   entries,
			"note":      "label은 비어 있습니다. 각 코드의 업무 의미를 담당자가 채워야 합니다.",
		},
		Confidence: conf, Evidence: evid, Generator: "rule", Status: "suggested",
	}, true
}

// ---- metric (FR-META-014) ----

// metricColumnCoverage indexes "table.column" strings already used by an
// existing metric so we don't re-propose them.
func (c *Catalog) metricColumnCoverage() map[string]bool {
	covered := map[string]bool{}
	for _, m := range c.Metrics {
		for _, col := range m.Columns {
			covered[strings.ToLower(col)] = true
		}
		// expression columns are harder to parse; index bare table.column tokens
		for _, tok := range strings.FieldsFunc(strings.ToLower(m.Expression), func(r rune) bool {
			return !(r == '.' || r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
		}) {
			if strings.Contains(tok, ".") {
				covered[tok] = true
			}
		}
	}
	return covered
}

// suggestMetric proposes an aggregate metric for a measure-like column
// (AMOUNT/COUNT/RATIO/SCORE) that no existing metric already covers.
func (c *Catalog) suggestMetric(t *Table, col *Column, covered map[string]bool) (ModelCandidate, bool) {
	styp := col.SemanticType
	if styp == "" {
		if s, ok := suggestSemanticType(t, col); ok {
			styp = s.Suggested
		}
	}
	var agg string
	switch styp {
	case "AMOUNT":
		agg = "SUM"
	case "COUNT":
		agg = "SUM"
	case "RATIO", "SCORE":
		agg = "AVG"
	default:
		return ModelCandidate{}, false
	}
	key := strings.ToLower(t.Name + "." + col.Name)
	keyFQN := strings.ToLower(col.Name)
	if covered[key] || covered[keyFQN] || covered[strings.ToLower(t.FQN+"."+col.Name)] {
		return ModelCandidate{}, false
	}

	expr := agg + "(" + t.FQN + "." + col.Name + ")"
	groupBy := c.dimensionColumns(t)
	name := strings.ToLower(agg) + "_" + strings.ToLower(col.Name)
	label := t.LogicalNameOr() + " " + col.LogicalNameOr() + " " + aggKO(agg)
	example := "SELECT " + expr + " FROM " + t.FQN
	if len(groupBy) > 0 {
		example = "SELECT " + groupBy[0] + ", " + expr + " FROM " + t.FQN + " GROUP BY " + groupBy[0]
	}
	return ModelCandidate{
		Kind: "metric", Table: t.FQN, Column: col.Name,
		Suggested: map[string]any{
			"name":                 name,
			"business_name":        strings.TrimSpace(label),
			"aggregation":          agg,
			"expression":           expr,
			"tables":               []string{t.FQN},
			"columns":              []string{col.Name},
			"recommended_group_by": groupBy,
			"example_sql":          example,
		},
		Confidence: 0.7,
		Evidence:   []string{"semantic_type=" + styp + " → " + agg, "not covered by existing metric"},
		Generator:  "rule", Status: "suggested",
	}, true
}

// dimensionColumns returns date/code columns in a table that make natural
// GROUP BY dimensions for a metric.
func (c *Catalog) dimensionColumns(t *Table) []string {
	var dims []string
	for _, col := range t.Columns {
		n := strings.ToUpper(col.Name)
		dt := strings.ToLower(col.DataType)
		isDate := strings.Contains(dt, "date") || strings.Contains(dt, "timestamp") || hasSuffix(n, "_DT") || hasSuffix(n, "_DATE")
		isCode := col.SemanticType == "CODE" || col.CodeDict != "" || hasSuffix(n, "_CD") || hasSuffix(n, "_TYPE") || hasSuffix(n, "_DIV")
		if isDate || isCode {
			dims = append(dims, col.Name)
		}
	}
	return dims
}

// ---- relation (FR-META-015) ----

// relationCoverage indexes existing relations by "baseTable|baseColumn".
func (c *Catalog) relationCoverage() map[string]bool {
	seen := map[string]bool{}
	for _, r := range c.Relations {
		seen[strings.ToLower(r.BaseSchema+"."+r.BaseTable+"|"+r.BaseColumn)] = true
	}
	return seen
}

// pkColumnIndex maps UPPER(pk column name) → tables whose single-column PK has
// that name (for FK-by-name inference).
func (c *Catalog) pkColumnIndex() map[string][]*Table {
	idx := map[string][]*Table{}
	for _, t := range c.Tables {
		if len(t.PrimaryKeys) != 1 {
			continue
		}
		key := strings.ToUpper(t.PrimaryKeys[0])
		idx[key] = append(idx[key], t)
	}
	return idx
}

// tableStemIndex maps UPPER(table stem) → table, so a column like CUSTOMER_ID
// can be matched to the CUSTOMER table by name.
func (c *Catalog) tableStemIndex() map[string]*Table {
	idx := map[string]*Table{}
	for _, t := range c.Tables {
		idx[strings.ToUpper(t.Name)] = t
		idx[strings.ToUpper(singularize(t.Name))] = t
	}
	return idx
}

// suggestRelation infers a foreign-key relation for an identifier column whose
// name references another table (by that table's PK column name, or the table
// name itself), when no relation already covers it and the types are
// compatible.
func (c *Catalog) suggestRelation(t *Table, col *Column, existing map[string]bool, pkIndex map[string][]*Table, nameIndex map[string]*Table) (ModelCandidate, bool) {
	if col.IsPK {
		return ModelCandidate{}, false // a PK is not itself an FK reference
	}
	n := strings.ToUpper(col.Name)
	if !(hasSuffix(n, "_ID") || hasSuffix(n, "_NO") || hasSuffix(n, "_CD") || hasSuffix(n, "_SEQ") || hasSuffix(n, "_KEY")) {
		return ModelCandidate{}, false
	}
	if existing[strings.ToLower(t.FQN+"|"+col.Name)] {
		return ModelCandidate{}, false
	}

	var ref *Table
	var conf float64
	var evid []string

	// 1) exact PK-column-name match on another table (strongest signal)
	if cands := pkIndex[n]; len(cands) > 0 {
		for _, cand := range cands {
			if cand.FQN != t.FQN {
				ref, conf = cand, 0.8
				evid = []string{col.Name + " matches PK of " + cand.FQN}
				break
			}
		}
	}
	// 2) stem (CUSTOMER_ID → CUSTOMER) matches a table name
	if ref == nil {
		stem := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(
			n, "_ID"), "_NO"), "_CD"), "_SEQ"), "_KEY")
		if cand, ok := nameIndex[stem]; ok && cand.FQN != t.FQN && len(cand.PrimaryKeys) == 1 {
			ref, conf = cand, 0.7
			evid = []string{"name stem '" + strings.ToLower(stem) + "' matches table " + cand.FQN}
		}
	}
	if ref == nil {
		return ModelCandidate{}, false
	}

	refCol := ref.PrimaryKeys[0]
	// type compatibility guard: base column and referenced PK should be
	// roughly the same family, else it is likely a false positive.
	if baseCol := ref.ColumnMap[refCol]; baseCol != nil && !typeFamilyCompatible(col.DataType, baseCol.DataType) {
		conf -= 0.2
		evid = append(evid, "type mismatch ("+col.DataType+" vs "+baseCol.DataType+"); verify before applying")
	}
	if existing[strings.ToLower(t.FQN+"|"+col.Name)] {
		return ModelCandidate{}, false
	}

	return ModelCandidate{
		Kind: "relation", Table: t.FQN, Column: col.Name, Target: ref.FQN,
		Suggested: map[string]any{
			"base_table":       t.FQN,
			"base_column":      col.Name,
			"reference_table":  ref.FQN,
			"reference_column": refCol,
			"cardinality":      "many-to-one",
			"join_type":        "inner",
			"provision_type":   "inferred",
			"preferred":        false,
		},
		Confidence: conf, Evidence: evid, Generator: "rule", Status: "suggested",
	}, true
}

// ---- helpers ----

func aggKO(agg string) string {
	switch agg {
	case "SUM":
		return "합계"
	case "AVG":
		return "평균"
	case "COUNT":
		return "건수"
	default:
		return agg
	}
}

func (col *Column) LogicalNameOr() string {
	if col.LogicalName != "" {
		return col.LogicalName
	}
	if exp, _ := expandName(strings.ToUpper(col.Name)); exp != "" {
		return exp
	}
	return col.Name
}

func plural(n int64, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.FormatInt(n, 10) + " " + unit + "s"
}

func singularize(name string) string {
	l := strings.ToLower(name)
	switch {
	case strings.HasSuffix(l, "ies") && len(l) > 3:
		return name[:len(name)-3] + "y"
	case strings.HasSuffix(l, "ses") && len(l) > 3:
		return name[:len(name)-2]
	case strings.HasSuffix(l, "s") && !strings.HasSuffix(l, "ss") && len(l) > 1:
		return name[:len(name)-1]
	}
	return name
}

// typeFamilyCompatible reports whether two SQL type strings belong to the same
// broad family (numeric / text / temporal), for FK inference sanity-checking.
func typeFamilyCompatible(a, b string) bool {
	return typeFamily(a) == typeFamily(b)
}

func typeFamily(dt string) string {
	d := strings.ToLower(dt)
	switch {
	case strings.Contains(d, "int") || strings.Contains(d, "num") || strings.Contains(d, "dec") ||
		strings.Contains(d, "float") || strings.Contains(d, "double") || strings.Contains(d, "real") ||
		strings.Contains(d, "serial"):
		return "numeric"
	case strings.Contains(d, "char") || strings.Contains(d, "text") || strings.Contains(d, "clob") ||
		strings.Contains(d, "string") || strings.Contains(d, "uuid"):
		return "text"
	case strings.Contains(d, "date") || strings.Contains(d, "time"):
		return "temporal"
	default:
		return "other"
	}
}
