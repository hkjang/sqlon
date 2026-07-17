package catalog

import "time"

const (
	DefaultLimit = 1000
)

type Catalog struct {
	DataDir   string
	LoadedAt  time.Time
	Dialect   string
	Tables    map[string]*Table
	ByName    map[string][]*Table
	Relations []Relation
	Adjacency map[string][]JoinEdge
	Samples   []Sample
	Subjects  []SubjectArea
	Prompts   []PromptDef
	Databases []DatabaseDef

	Glossary         *Glossary
	Metrics          []MetricDef
	Overrides        *Overrides
	Issues           []LoadIssue
	ForbiddenJoins   []ForbiddenJoin
	FeedbackUsage    map[string]int
	FeedbackPenalty  map[string]int
	FeedbackTenantID string
	LearnedRules     []LearnedRule
	Patterns         []QueryPattern
}

type Table struct {
	Schema      string    `json:"schema"`
	Name        string    `json:"name"`
	FQN         string    `json:"fqn"`
	LogicalName string    `json:"logical_name,omitempty"`
	Description string    `json:"description,omitempty"`
	Domain      string    `json:"domain,omitempty"`
	Grain       string    `json:"grain,omitempty"`
	Freshness   string    `json:"freshness,omitempty"`
	RowCount    int64     `json:"row_count,omitempty"`
	Version     int       `json:"version,omitempty"`
	Columns     []*Column `json:"columns,omitempty"`
	ColumnMap   map[string]*Column
	PrimaryKeys []string `json:"primary_keys,omitempty"`
	ForeignKeys []string `json:"foreign_keys,omitempty"`
	Indexes     []IndexDef
	SearchText  string
}

type Column struct {
	Name            string          `json:"name"`
	LogicalName     string          `json:"logical_name,omitempty"`
	DataType        string          `json:"data_type,omitempty"`
	LengthPrecision string          `json:"length_precision,omitempty"`
	Nullable        string          `json:"nullable,omitempty"`
	IsPK            bool            `json:"is_pk,omitempty"`
	IsFK            bool            `json:"is_fk,omitempty"`
	Description     string          `json:"description,omitempty"`
	Note            string          `json:"note,omitempty"`
	CodeDict        string          `json:"code_dict,omitempty"`
	CommonCode      string          `json:"common_code,omitempty"`
	Order           int             `json:"order,omitempty"`
	Indexed         bool            `json:"indexed,omitempty"`
	Synonyms        []string        `json:"synonyms,omitempty"`
	SampleValues    []string        `json:"sample_values,omitempty"`
	PII             bool            `json:"pii,omitempty"`
	SemanticType    string          `json:"semantic_type,omitempty"`
	Stats           *ColumnStatData `json:"stats,omitempty"`
}

// ColumnStatData holds profiling statistics loaded from column_stats.json.
type ColumnStatData struct {
	RowCount      int64      `json:"row_count,omitempty"`
	NullRatio     float64    `json:"null_ratio,omitempty"`
	DistinctCount int64      `json:"distinct_count,omitempty"`
	Min           string     `json:"min,omitempty"`
	Max           string     `json:"max,omitempty"`
	TopValues     []TopValue `json:"top_values,omitempty"`
	FormatPattern string     `json:"format_pattern,omitempty"`
	LastUpdated   string     `json:"last_updated,omitempty"`
}

type TopValue struct {
	Value string  `json:"value"`
	Ratio float64 `json:"ratio,omitempty"`
	Label string  `json:"label,omitempty"`
}

type LoadIssue struct {
	Level   string `json:"level"` // error | warning
	Source  string `json:"source"`
	Message string `json:"message"`
	Table   string `json:"table,omitempty"`
	Column  string `json:"column,omitempty"`
}

type ForbiddenJoin struct {
	FromTable string `json:"from_table"`
	ToTable   string `json:"to_table"`
	Reason    string `json:"reason,omitempty"`
}

type Relation struct {
	ID              any     `json:"id,omitempty"`
	BaseSchema      string  `json:"base_schema"`
	BaseTable       string  `json:"base_table"`
	BaseColumn      string  `json:"base_column"`
	ReferenceSchema string  `json:"reference_schema"`
	ReferenceTable  string  `json:"reference_table"`
	ReferenceColumn string  `json:"reference_column"`
	Cardinality     string  `json:"cardinality,omitempty"`
	JoinType        string  `json:"join_type,omitempty"`
	ProvisionType   string  `json:"provision_type,omitempty"`
	Description     string  `json:"description,omitempty"`
	Caution         string  `json:"caution,omitempty"`
	Preferred       bool    `json:"preferred,omitempty"`
	Confidence      float64 `json:"confidence,omitempty"`
	MetaVersion     int     `json:"meta_version,omitempty"`
}

type JoinEdge struct {
	From     string
	To       string
	Relation Relation
	Reversed bool
}

type IndexDef struct {
	Schema      string `json:"schema_name"`
	Table       string `json:"table_name"`
	IndexName   string `json:"index_name"`
	ColumnName  string `json:"column_name"`
	Seq         int    `json:"seq"`
	IndexType   string `json:"index_type"`
	Description string `json:"description,omitempty"`
}

type SubjectArea struct {
	Category    string `json:"category"`
	Rule        string `json:"rule"`
	Code        string `json:"code"`
	CodeName    string `json:"code_name"`
	Description string `json:"description"`
	Keywords    string `json:"keywords"`
}

type Sample struct {
	ID               any    `json:"id"`
	Question         string `json:"question"`
	TargetSQL        string `json:"target_sql"`
	TargetDomain     string `json:"target_domain"`
	TargetTable      string `json:"target_table"`
	TargetColumn     string `json:"target_column"`
	TargetIntent     string `json:"target_intent"`
	TargetDifficulty string `json:"target_difficulty"`
}

type PromptDef struct {
	Name        string `json:"name"`
	Role        string `json:"role"`
	Category    string `json:"category"`
	Content     string `json:"content"`
	Description string `json:"description"`
	IsActive    bool   `json:"is_active"`
}

type DatabaseDef struct {
	ID    any    `json:"id"`
	DBMS  string `json:"dbms"`
	Port  int    `json:"port"`
	Name  string `json:"name"`
	Alias string `json:"alias"`
}
