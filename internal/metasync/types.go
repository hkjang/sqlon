// Package metasync implements automated source-database metadata collection,
// snapshotting, and incremental change detection (JAMYPG 자동 메타데이터 관리,
// FR-META-001..005). Its guiding principle is that PHYSICAL metadata (schemas,
// tables, columns, constraints, indexes, comments, row-count stats) is
// collected automatically, while BUSINESS meaning (logical names, metrics,
// join policy) is only ever produced as reviewable candidates — never written
// straight into the operational catalog. Every automatically derived value
// therefore carries provenance: how it was generated, from what evidence, and
// with what confidence.
package metasync

import "time"

// Generator identifies how a value was produced (FR-META-012).
type Generator string

const (
	GenSystemCatalog Generator = "system_catalog" // read from the DB itself
	GenRule          Generator = "rule"           // deterministic rule engine
	GenStatistics    Generator = "statistics"     // computed from profiling
	GenLLM           Generator = "llm"            // AI suggestion
	GenUserFeedback  Generator = "user_feedback"  // learned from usage
)

// ReviewStatus tracks a candidate through the approval workflow (FR-META-026).
type ReviewStatus string

const (
	StatusDiscovered ReviewStatus = "discovered"
	StatusSuggested  ReviewStatus = "suggested"
	StatusApproved   ReviewStatus = "approved"
	StatusRejected   ReviewStatus = "rejected"
	StatusRetired    ReviewStatus = "retired"
)

// Provenance is attached to every automatically derived metadata value so it
// is explainable and auditable (FR-META-012). Physical facts read from the
// system catalog carry confidence 1.0 and generator=system_catalog.
type Provenance struct {
	Confidence   float64      `json:"confidence"`         // 0..1
	Evidence     []string     `json:"evidence,omitempty"` // human-readable reasons
	Generator    Generator    `json:"generator"`          // how it was produced
	ModelID      string       `json:"model_id,omitempty"` // LLM model, when applicable
	GeneratedAt  time.Time    `json:"generated_at"`       //
	ReviewStatus ReviewStatus `json:"review_status"`      // approval state
	Reviewer     string       `json:"reviewer,omitempty"` //
}

// ---- collected physical model ----

// DatabaseAsset is a discovered database/schema container.
type DatabaseAsset struct {
	Database string `json:"database,omitempty"`
	Schema   string `json:"schema"`
	Kind     string `json:"kind"` // schema
}

// ColumnAsset is one physical column (FR-META-001 column info).
type ColumnAsset struct {
	Name         string `json:"name"`
	Ordinal      int    `json:"ordinal"`
	DataType     string `json:"data_type"`
	FullType     string `json:"full_type,omitempty"` // with length/precision, e.g. varchar(64)
	Nullable     bool   `json:"nullable"`
	Default      string `json:"default,omitempty"`
	Generated    string `json:"generated,omitempty"` // generation expression, if any
	Comment      string `json:"comment,omitempty"`
	IsPrimaryKey bool   `json:"is_primary_key,omitempty"`
	IsForeignKey bool   `json:"is_foreign_key,omitempty"`
}

// ConstraintAsset is a table constraint (PK/FK/Unique/Check).
type ConstraintAsset struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"` // PRIMARY KEY | FOREIGN KEY | UNIQUE | CHECK
	Columns    []string `json:"columns,omitempty"`
	RefSchema  string   `json:"ref_schema,omitempty"`  // FK target
	RefTable   string   `json:"ref_table,omitempty"`   // FK target
	RefColumns []string `json:"ref_columns,omitempty"` // FK target
	Definition string   `json:"definition,omitempty"`  // CHECK expression
}

// IndexAsset is a table index.
type IndexAsset struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique,omitempty"`
	Primary bool     `json:"primary,omitempty"`
}

// TableAsset is one physical table or view with its structure.
type TableAsset struct {
	Schema      string            `json:"schema"`
	Name        string            `json:"name"`
	Kind        string            `json:"kind"` // table | view | materialized_view
	Comment     string            `json:"comment,omitempty"`
	ViewSQL     string            `json:"view_sql,omitempty"`
	EstRowCount int64             `json:"est_row_count,omitempty"`
	Columns     []ColumnAsset     `json:"columns"`
	Constraints []ConstraintAsset `json:"constraints,omitempty"`
	Indexes     []IndexAsset      `json:"indexes,omitempty"`
	// StructHash is a stable hash of the structural definition (schema, name,
	// kind, columns, constraints, indexes) — NOT comments or row counts — so
	// incremental change detection can skip structurally-unchanged tables.
	StructHash string `json:"struct_hash"`
}

func (t TableAsset) FQN() string { return t.Schema + "." + t.Name }

// ---- snapshot ----

// RawSnapshot is the full physical metadata collected from one source at one
// point in time (FR-META-003). Snapshots are stored verbatim and never
// applied straight to the operational catalog.
type RawSnapshot struct {
	SnapshotID       string       `json:"snapshot_id"`
	SourceID         string       `json:"source_id"` // db profile id
	Dialect          string       `json:"dialect"`
	CollectorVersion string       `json:"collector_version"`
	DBVersion        string       `json:"db_version,omitempty"`
	CollectedAt      time.Time    `json:"collected_at"`
	SchemaHash       string       `json:"schema_hash"` // hash over all table struct hashes
	ObjectCount      ObjectCount  `json:"object_count"`
	Status           string       `json:"status"` // success | partial | failed
	ErrorSummary     []string     `json:"error_summary,omitempty"`
	Tables           []TableAsset `json:"tables"`
	Checkpoint       Checkpoint   `json:"checkpoint,omitempty"`
}

// ObjectCount summarizes a snapshot's size (FR-META-003).
type ObjectCount struct {
	Schemas     int `json:"schemas"`
	Tables      int `json:"tables"`
	Views       int `json:"views"`
	Columns     int `json:"columns"`
	Constraints int `json:"constraints"`
	Indexes     int `json:"indexes"`
}

// Checkpoint carries the incremental-collection watermark for a source
// (FR-META-005). Postgres/MySQL lack a universal DDL log, so the default
// checkpoint is the schema hash — a cheap "did anything structural change"
// signal — plus the collection time.
type Checkpoint struct {
	SchemaHash  string    `json:"schema_hash,omitempty"`
	CollectedAt time.Time `json:"collected_at,omitempty"`
}

// ---- change detection ----

// ChangeKind classifies a detected change (FR-META-004).
type ChangeKind string

const (
	TableAdded     ChangeKind = "table_added"
	TableRemoved   ChangeKind = "table_removed"
	ColumnAdded    ChangeKind = "column_added"
	ColumnRemoved  ChangeKind = "column_removed"
	TypeChanged    ChangeKind = "type_changed"
	NullChanged    ChangeKind = "nullability_changed"
	KeyChanged     ChangeKind = "key_changed" // PK/FK change
	CommentChanged ChangeKind = "comment_changed"
	IndexChanged   ChangeKind = "index_changed"
	ViewSQLChanged ChangeKind = "view_sql_changed"
)

// Severity guides review: physical-only changes are low, ones that can break
// metrics/joins/SQL are higher.
type Severity string

const (
	SevLow      Severity = "low"
	SevMedium   Severity = "medium"
	SevHigh     Severity = "high"
	SevBreaking Severity = "breaking"
)

// Change is one detected difference between two snapshots.
type Change struct {
	Kind     ChangeKind `json:"kind"`
	Severity Severity   `json:"severity"`
	Table    string     `json:"table"`
	Column   string     `json:"column,omitempty"`
	Before   string     `json:"before,omitempty"`
	After    string     `json:"after,omitempty"`
	Detail   string     `json:"detail,omitempty"`
	// Disposition captures the workflow policy: deletions become "retire
	// candidates" rather than immediate removals (FR-META-004, AC-02).
	Disposition string `json:"disposition,omitempty"`
}

// ChangeSet is the diff between a baseline and a current snapshot.
type ChangeSet struct {
	SourceID       string    `json:"source_id"`
	FromSnapshotID string    `json:"from_snapshot_id"`
	ToSnapshotID   string    `json:"to_snapshot_id"`
	ComputedAt     time.Time `json:"computed_at"`
	Changes        []Change  `json:"changes"`
	// ChangedTables is the set of tables that changed structurally — the
	// input to incremental recompilation (FR-META-005: only re-process
	// changed assets).
	ChangedTables []string           `json:"changed_tables"`
	Summary       map[ChangeKind]int `json:"summary"`
}

const CollectorVersion = "1.0.0"
