package catalog

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var codeTailRE = regexp.MustCompile(`([A-Za-z0-9]+)\s*$`)

type columnStatRow struct {
	SchemaName    string     `json:"schema_name"`
	TableName     string     `json:"table_name"`
	ColumnName    string     `json:"column_name"`
	RowCount      int64      `json:"row_count,omitempty"`
	NullRatio     float64    `json:"null_ratio,omitempty"`
	DistinctCount int64      `json:"distinct_count,omitempty"`
	Min           string     `json:"min,omitempty"`
	Max           string     `json:"max,omitempty"`
	TopValues     []TopValue `json:"top_values,omitempty"`
	FormatPattern string     `json:"format_pattern,omitempty"`
	LastUpdated   string     `json:"last_updated,omitempty"`
}

func (c *Catalog) loadColumnStats(dataDir string) {
	path := filepath.Join(dataDir, "column_stats.json")
	if _, err := os.Stat(path); err != nil {
		c.Issues = append(c.Issues, LoadIssue{Level: "warning", Source: "column_stats.json", Message: "column_stats.json not found; get_column_stats serves metadata only and value-based column matching is limited to code dictionaries"})
		return
	}
	var rows []columnStatRow
	if err := readJSON(path, &rows); err != nil {
		c.Issues = append(c.Issues, LoadIssue{Level: "error", Source: "column_stats.json", Message: err.Error()})
		return
	}
	for _, row := range rows {
		t, ok := c.ResolveTable(cleanIdent(row.SchemaName) + "." + cleanIdent(row.TableName))
		if !ok {
			c.Issues = append(c.Issues, LoadIssue{Level: "warning", Source: "column_stats.json", Message: "stats reference unknown table", Table: row.SchemaName + "." + row.TableName})
			continue
		}
		col := t.ColumnMap[cleanIdent(row.ColumnName)]
		if col == nil {
			c.Issues = append(c.Issues, LoadIssue{Level: "warning", Source: "column_stats.json", Message: "stats reference unknown column", Table: t.FQN, Column: row.ColumnName})
			continue
		}
		col.Stats = &ColumnStatData{
			RowCount:      row.RowCount,
			NullRatio:     row.NullRatio,
			DistinctCount: row.DistinctCount,
			Min:           row.Min,
			Max:           row.Max,
			TopValues:     row.TopValues,
			FormatPattern: row.FormatPattern,
			LastUpdated:   row.LastUpdated,
		}
		if row.RowCount > t.RowCount {
			t.RowCount = row.RowCount
		}
		if row.LastUpdated != "" && row.LastUpdated > t.Freshness {
			t.Freshness = row.LastUpdated
		}
		for _, tv := range row.TopValues {
			col.SampleValues = appendUnique(col.SampleValues, tv.Value)
		}
	}
}

// ColumnStats returns everything the catalog knows about one column:
// metadata, code dictionary, profiling stats, and usage in golden examples.
func (c *Catalog) ColumnStats(tableName, columnName string) map[string]any {
	t, ok := c.ResolveTable(tableName)
	if !ok {
		return map[string]any{"error": "table not found", "table": tableName}
	}
	col := t.ColumnMap[cleanIdent(columnName)]
	if col == nil {
		return map[string]any{"error": "column not found", "table": t.FQN, "column": columnName}
	}
	usage := 0
	for _, s := range c.Samples {
		if strings.Contains(strings.ToUpper(s.TargetColumn), t.Name+"."+col.Name) || strings.Contains(strings.ToUpper(s.TargetSQL), col.Name) {
			usage++
		}
	}
	res := map[string]any{
		"table":              t.FQN,
		"column":             col.Name,
		"logical_name":       col.LogicalName,
		"data_type":          col.DataType,
		"length_precision":   col.LengthPrecision,
		"null_constraint":    col.Nullable,
		"is_pk":              col.IsPK,
		"is_fk":              col.IsFK,
		"indexed":            col.Indexed,
		"pii":                col.PII,
		"semantic_type":      col.SemanticType,
		"description":        col.Description,
		"common_code":        col.CommonCode,
		"code_dict":          col.CodeDict,
		"synonyms":           col.Synonyms,
		"sample_values":      col.SampleValues,
		"sample_usage_count": usage,
	}
	if col.Stats != nil {
		res["stats"] = col.Stats
		res["available_stats"] = "profiled"
	} else {
		res["available_stats"] = "metadata_only"
	}
	return res
}

type FilterColumnCandidate struct {
	Value              string  `json:"value"`
	Table              string  `json:"table"`
	Column             string  `json:"column"`
	LogicalName        string  `json:"logical_name,omitempty"`
	DataType           string  `json:"data_type,omitempty"`
	MatchedIn          string  `json:"matched_in"` // code_dict | top_values | sample_values | logical_name | synonym
	MatchedEntry       string  `json:"matched_entry,omitempty"`
	SuggestedPredicate string  `json:"suggested_predicate,omitempty"`
	Score              float64 `json:"score"`
}

// FindFilterColumns maps literal values from the question ("서울", "정상",
// "개인사업자", "연체") onto columns whose code dictionary, top values, or
// sample values contain them, and proposes the concrete predicate.
func (c *Catalog) FindFilterColumns(values []string, tables []string, topK int) map[string]any {
	if topK <= 0 {
		topK = 8
	}
	tableFilter := map[string]bool{}
	for _, tn := range tables {
		if t, ok := c.ResolveTable(tn); ok {
			tableFilter[t.FQN] = true
		}
	}
	var out []FilterColumnCandidate
	for _, raw := range values {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		lv := strings.ToLower(v)
		for _, t := range c.Tables {
			if len(tableFilter) > 0 && !tableFilter[t.FQN] {
				continue
			}
			for _, col := range t.Columns {
				if cand, ok := matchValueToColumn(v, lv, t, col); ok {
					out = append(out, cand)
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score == out[j].Score {
			return out[i].Table+out[i].Column < out[j].Table+out[j].Column
		}
		return out[i].Score > out[j].Score
	})
	if len(out) > topK {
		out = out[:topK]
	}
	return map[string]any{"values": values, "candidates": out}
}

func matchValueToColumn(v, lv string, t *Table, col *Column) (FilterColumnCandidate, bool) {
	mk := func(in, entry, pred string, score float64) (FilterColumnCandidate, bool) {
		return FilterColumnCandidate{
			Value: v, Table: t.FQN, Column: col.Name, LogicalName: col.LogicalName,
			DataType: col.DataType, MatchedIn: in, MatchedEntry: entry,
			SuggestedPredicate: pred, Score: round(score),
		}, true
	}
	// code dictionary: "00:정상, 09:수정" -> exact label match yields code predicate
	if col.CodeDict != "" && strings.Contains(strings.ToLower(col.CodeDict), lv) {
		for _, pair := range strings.Split(col.CodeDict, ",") {
			kv := strings.SplitN(pair, ":", 2)
			if len(kv) == 2 && strings.EqualFold(strings.TrimSpace(kv[1]), v) {
				// the key part may carry leading prose ("구분 1" / "2)=1");
				// the code is the trailing alphanumeric run before ':'
				if m := codeTailRE.FindStringSubmatch(kv[0]); m != nil {
					return mk("code_dict", strings.TrimSpace(m[1]+":"+kv[1]), col.Name+" = '"+m[1]+"'", 12)
				}
			}
		}
		return mk("code_dict", col.CodeDict, "", 6)
	}
	if col.Stats != nil {
		for _, tv := range col.Stats.TopValues {
			if strings.EqualFold(tv.Value, v) || strings.EqualFold(tv.Label, v) {
				return mk("top_values", tv.Value, col.Name+" = '"+tv.Value+"'", 11)
			}
		}
	}
	for _, sv := range col.SampleValues {
		if strings.EqualFold(sv, v) {
			return mk("sample_values", sv, col.Name+" = '"+sv+"'", 9)
		}
	}
	for _, syn := range col.Synonyms {
		if strings.Contains(strings.ToLower(syn), lv) {
			return mk("synonym", syn, "", 5)
		}
	}
	if col.LogicalName != "" && strings.Contains(strings.ToLower(col.LogicalName), lv) {
		return mk("logical_name", col.LogicalName, "", 3)
	}
	return FilterColumnCandidate{}, false
}
