package catalog

import (
	"os"
	"path/filepath"
	"strings"
)

// Overrides lets operators patch the compiled catalog without touching the
// source metadata exports: descriptions, synonyms, PII flags, join policy,
// default filters, dialect, and domain-specific column/schema conventions.
//
// The domain-policy fields below (ValidityFlagColumns..SchemaHints) exist so
// that operator-specific naming conventions (e.g. a bank's "USE_YN" validity
// flag or "DWMST"/"DWSVC" schema split) are configured per deployment rather
// than hardcoded in Go source — every one of them defaults to empty, which
// means zero behavior change for a dataset that doesn't configure them.
type Overrides struct {
	Dialect        string           `json:"dialect,omitempty"`
	Tables         []TableOverride  `json:"tables,omitempty"`
	Columns        []ColumnOverride `json:"columns,omitempty"`
	PIIColumns     []string         `json:"pii_columns,omitempty"` // "SCHEMA.TABLE.COLUMN" or "*.COLUMN"
	ForbiddenJoins []ForbiddenJoin  `json:"forbidden_joins,omitempty"`
	PreferredJoins []PreferredJoin  `json:"preferred_joins,omitempty"`
	DefaultFilters []DefaultFilter  `json:"default_filters,omitempty"`

	// ValidityFlagColumns: column names meaning "row is active" (e.g.
	// "USE_YN"). When present on a table, BuildSQLSkeleton auto-injects
	// COALESCE(col,'Y') <> 'N' and validate_sql warns if the generated SQL
	// omits it.
	ValidityFlagColumns []string `json:"validity_flag_columns,omitempty"`
	// SoftDeleteColumns: column names meaning "row is soft-deleted when
	// non-null" (e.g. "DEL_CAUS_CD"). Auto-injects "col IS NULL".
	SoftDeleteColumns []string `json:"soft_delete_columns,omitempty"`
	// ExclusionColumnPrefixes: any column whose name starts with one of
	// these prefixes is treated like a SoftDeleteColumns entry.
	ExclusionColumnPrefixes []string `json:"exclusion_column_prefixes,omitempty"`
	// SegmentHistoryColumnPairs: {start,end} column pairs representing a
	// point-in-time validity window (e.g. B_ST_DT/B_END_DT).
	SegmentHistoryColumnPairs []SegmentHistoryPair `json:"segment_history_column_pairs,omitempty"`
	// JoinKeyCandidateColumns: column names treated as likely natural join
	// keys shared across tables.
	JoinKeyCandidateColumns []string `json:"join_key_candidate_columns,omitempty"`
	// EntityKeyColumns: column names treated as unique-entity identifiers,
	// so a metric mentioning them defaults to COUNT(DISTINCT col).
	EntityKeyColumns []string `json:"entity_key_columns,omitempty"`
	// AuditColumnNames: extra system/audit columns (beyond the built-in
	// created_at/updated_at/version-style defaults) excluded from join-key
	// suggestion scoring.
	AuditColumnNames []string `json:"audit_column_names,omitempty"`
	// WellKnownDateColumns: exact column names recognized as date-like even
	// without the generic *_DT/*_MON/*_DT_TM suffix, and preferred as the
	// time-predicate target column in BuildSQLSkeleton.
	WellKnownDateColumns []string `json:"well_known_date_columns,omitempty"`
	// DateColumnExcludePrefixes/Names/Substrings: naming patterns that must
	// never be chosen as a time-predicate target even when semantically
	// typed as a date (e.g. history/audit/attribute-date columns).
	DateColumnExcludePrefixes   []string `json:"date_column_exclude_prefixes,omitempty"`
	DateColumnExcludeNames      []string `json:"date_column_exclude_names,omitempty"`
	DateColumnExcludeSubstrings []string `json:"date_column_exclude_substrings,omitempty"`
	// DateColumnEligibleSuffixes: column-name suffixes (beyond
	// WellKnownDateColumns) eligible as a time-predicate target without
	// needing golden-example usage history.
	DateColumnEligibleSuffixes []string `json:"date_column_eligible_suffixes,omitempty"`
	// SchemaHints: keyword-driven schema-relevance rules (e.g. route
	// score/rating questions toward an analytics schema). Optional.
	SchemaHints []SchemaHintRule `json:"schema_hints,omitempty"`
	// PreferredSchemaOrder: when a bare table name exists identically in more
	// than one schema, ResolveTable picks the first schema in this list that
	// has a match. With no match (or this unset), an ambiguous bare name
	// resolves to not-found rather than silently guessing — the caller must
	// re-specify a schema-qualified name.
	PreferredSchemaOrder []string `json:"preferred_schema_order,omitempty"`
}

// SegmentHistoryPair names a point-in-time validity window column pair.
type SegmentHistoryPair struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// SchemaHintRule maps a set of question keywords to relevant schemas, with an
// optional guidance note surfaced when more than one rule group matches.
type SchemaHintRule struct {
	Keywords []string `json:"keywords"`
	Schemas  []string `json:"schemas"`
	Note     string   `json:"note,omitempty"`
}

// MatchSchemaHints returns the schema hint rules whose keywords appear in the
// (lower-cased) question, plus a de-duplicated list of hinted schemas.
func (c *Catalog) MatchSchemaHints(question string) ([]SchemaHintRule, []string) {
	if c.Overrides == nil || len(c.Overrides.SchemaHints) == 0 {
		return nil, nil
	}
	q := strings.ToLower(question)
	var hits []SchemaHintRule
	var schemas []string
	for _, rule := range c.Overrides.SchemaHints {
		for _, kw := range rule.Keywords {
			if kw != "" && strings.Contains(q, strings.ToLower(kw)) {
				hits = append(hits, rule)
				schemas = append(schemas, rule.Schemas...)
				break
			}
		}
	}
	return hits, unique(schemas)
}

// entityKeyColumns/joinKeyCandidateColumns/... return the configured lists,
// nil-safe for a Catalog with no Overrides loaded.
func (c *Catalog) entityKeyColumns() []string {
	if c.Overrides == nil {
		return nil
	}
	return c.Overrides.EntityKeyColumns
}

type TableOverride struct {
	Table       string `json:"table"`
	LogicalName string `json:"logical_name,omitempty"`
	Description string `json:"description,omitempty"`
	Domain      string `json:"domain,omitempty"`
	Grain       string `json:"grain,omitempty"`
	Freshness   string `json:"freshness,omitempty"`
	RowCount    int64  `json:"row_count,omitempty"`
}

type ColumnOverride struct {
	Table        string   `json:"table"`
	Column       string   `json:"column"`
	LogicalName  string   `json:"logical_name,omitempty"`
	Description  string   `json:"description,omitempty"`
	Synonyms     []string `json:"synonyms,omitempty"`
	SampleValues []string `json:"sample_values,omitempty"`
	PII          *bool    `json:"pii,omitempty"`
	SemanticType string   `json:"semantic_type,omitempty"`
}

type PreferredJoin struct {
	FromTable   string  `json:"from_table"`
	FromColumn  string  `json:"from_column"`
	ToTable     string  `json:"to_table"`
	ToColumn    string  `json:"to_column"`
	Cardinality string  `json:"cardinality,omitempty"`
	JoinType    string  `json:"join_type,omitempty"`
	Description string  `json:"description,omitempty"`
	Caution     string  `json:"caution,omitempty"`
	Confidence  float64 `json:"confidence,omitempty"`
}

type DefaultFilter struct {
	PolicyID    string `json:"policy_id,omitempty"`
	Table       string `json:"table"`
	Condition   string `json:"condition"`
	Reason      string `json:"reason,omitempty"`
	Enforcement string `json:"enforcement,omitempty"` // warn (default) | error
}

func loadOverrides(dataDir string) (*Overrides, []LoadIssue) {
	path := filepath.Join(dataDir, "overrides.json")
	if _, err := os.Stat(path); err != nil {
		return &Overrides{}, nil
	}
	o := &Overrides{}
	if err := readJSON(path, o); err != nil {
		return &Overrides{}, []LoadIssue{{Level: "error", Source: "overrides.json", Message: err.Error()}}
	}
	return o, nil
}

func (c *Catalog) applyOverrides() {
	o := c.Overrides
	if o == nil {
		return
	}
	if v := strings.TrimSpace(o.Dialect); v != "" {
		c.Dialect = strings.ToLower(v)
	}
	issue := func(level, msg, table, column string) {
		c.Issues = append(c.Issues, LoadIssue{Level: level, Source: "overrides.json", Message: msg, Table: table, Column: column})
	}
	for _, to := range o.Tables {
		t, ok := c.ResolveTable(to.Table)
		if !ok {
			issue("warning", "table override references unknown table", to.Table, "")
			continue
		}
		if to.LogicalName != "" {
			t.LogicalName = to.LogicalName
		}
		if to.Description != "" {
			t.Description = to.Description
		}
		if to.Domain != "" {
			t.Domain = to.Domain
		}
		if to.Grain != "" {
			t.Grain = to.Grain
		}
		if to.Freshness != "" {
			t.Freshness = to.Freshness
		}
		if to.RowCount > 0 {
			t.RowCount = to.RowCount
		}
	}
	for _, co := range o.Columns {
		t, ok := c.ResolveTable(co.Table)
		if !ok {
			issue("warning", "column override references unknown table", co.Table, co.Column)
			continue
		}
		col := t.ColumnMap[cleanIdent(co.Column)]
		if col == nil {
			issue("warning", "column override references unknown column", t.FQN, co.Column)
			continue
		}
		if co.LogicalName != "" {
			col.LogicalName = co.LogicalName
		}
		if co.Description != "" {
			col.Description = co.Description
		}
		if len(co.Synonyms) > 0 {
			col.Synonyms = unique(append(col.Synonyms, co.Synonyms...))
		}
		if len(co.SampleValues) > 0 {
			col.SampleValues = unique(append(col.SampleValues, co.SampleValues...))
		}
		if co.PII != nil {
			col.PII = *co.PII
		}
		if co.SemanticType != "" {
			col.SemanticType = strings.ToUpper(co.SemanticType)
		}
	}
	for _, spec := range o.PIIColumns {
		c.markPII(spec, issue)
	}
	c.ForbiddenJoins = append(c.ForbiddenJoins, o.ForbiddenJoins...)
	for _, pj := range o.PreferredJoins {
		from, okF := c.ResolveTable(pj.FromTable)
		to, okT := c.ResolveTable(pj.ToTable)
		if !okF || !okT {
			issue("warning", "preferred join references unknown table", pj.FromTable+" -> "+pj.ToTable, "")
			continue
		}
		conf := pj.Confidence
		if conf == 0 {
			conf = 0.95
		}
		r := Relation{
			BaseSchema:      from.Schema,
			BaseTable:       from.Name,
			BaseColumn:      cleanColumnList(pj.FromColumn),
			ReferenceSchema: to.Schema,
			ReferenceTable:  to.Name,
			ReferenceColumn: cleanColumnList(pj.ToColumn),
			Cardinality:     pj.Cardinality,
			JoinType:        strings.ToUpper(nonEmpty(pj.JoinType, "INNER")),
			ProvisionType:   "OPERATOR",
			Description:     pj.Description,
			Caution:         pj.Caution,
			Preferred:       true,
			Confidence:      conf,
		}
		c.Relations = append(c.Relations, r)
		fk := from.FQN
		tk := to.FQN
		c.Adjacency[fk] = append(c.Adjacency[fk], JoinEdge{From: fk, To: tk, Relation: r})
		c.Adjacency[tk] = append(c.Adjacency[tk], JoinEdge{From: tk, To: fk, Relation: r, Reversed: true})
	}
	for _, df := range o.DefaultFilters {
		if strings.TrimSpace(df.Condition) == "" {
			issue("error", "default filter requires a non-empty condition", df.Table, "")
			continue
		}
		if _, ok := c.ResolveTable(df.Table); !ok {
			issue("warning", "default filter references unknown table", df.Table, "")
		}
		switch strings.ToLower(strings.TrimSpace(df.Enforcement)) {
		case "", "warn", "error":
		default:
			issue("error", "default filter enforcement must be 'warn' or 'error'", df.Table, "")
		}
	}
}

func (c *Catalog) markPII(spec string, issue func(level, msg, table, column string)) {
	spec = cleanIdent(spec)
	parts := strings.Split(spec, ".")
	switch len(parts) {
	case 2: // "*.COLUMN" or "TABLE.COLUMN"
		colName := parts[1]
		if parts[0] == "*" {
			for _, t := range c.Tables {
				if col := t.ColumnMap[colName]; col != nil {
					col.PII = true
				}
			}
			return
		}
		if t, ok := c.ResolveTable(parts[0]); ok {
			if col := t.ColumnMap[colName]; col != nil {
				col.PII = true
				return
			}
		}
		issue("warning", "pii_columns entry did not match any column", spec, "")
	case 3:
		t, ok := c.ResolveTable(parts[0] + "." + parts[1])
		if !ok {
			issue("warning", "pii_columns entry references unknown table", spec, "")
			return
		}
		if col := t.ColumnMap[parts[2]]; col != nil {
			col.PII = true
		} else {
			issue("warning", "pii_columns entry references unknown column", t.FQN, parts[2])
		}
	default:
		issue("warning", "pii_columns entry must be TABLE.COLUMN, SCHEMA.TABLE.COLUMN, or *.COLUMN", spec, "")
	}
}

// DefaultFiltersFor returns operator-defined mandatory filters for a table.
func (c *Catalog) DefaultFiltersFor(fqn string) []DefaultFilter {
	if c.Overrides == nil {
		return nil
	}
	var out []DefaultFilter
	for _, df := range c.Overrides.DefaultFilters {
		if t, ok := c.ResolveTable(df.Table); ok && t.FQN == fqn {
			out = append(out, df)
		}
	}
	return out
}

// IsForbiddenJoin reports whether a table pair is blocked by operator policy.
func (c *Catalog) IsForbiddenJoin(a, b string) (ForbiddenJoin, bool) {
	ta, okA := c.ResolveTable(a)
	tb, okB := c.ResolveTable(b)
	if !okA || !okB {
		return ForbiddenJoin{}, false
	}
	for _, fj := range c.ForbiddenJoins {
		f, okF := c.ResolveTable(fj.FromTable)
		t, okT := c.ResolveTable(fj.ToTable)
		if !okF || !okT {
			continue
		}
		if (f.FQN == ta.FQN && t.FQN == tb.FQN) || (f.FQN == tb.FQN && t.FQN == ta.FQN) {
			return fj, true
		}
	}
	return ForbiddenJoin{}, false
}
