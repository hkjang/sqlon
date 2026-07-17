package metasync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// SystemQuerier is the minimal surface metasync needs from dbconn.Manager: a
// trusted read-only query against a profile's pool and its dialect. Keeping
// it an interface avoids a hard import cycle and makes the collector unit-
// testable with a fake.
type SystemQuerier interface {
	SystemQuery(ctx context.Context, profileID, query string, args ...any) ([]map[string]any, error)
	ProfileDialect(ctx context.Context, profileID string) (string, error)
}

// CollectRequest scopes a collection run.
type CollectRequest struct {
	SourceID     string   // db profile id
	Schemas      []string // empty → all non-system schemas
	IncludeViews bool
}

// Collector reads physical metadata from a source database into the common
// asset model (FR-META-001/002). One Collector serves all dialects by
// dispatching to dialect-specific system-catalog queries; per-dialect logic
// lives in collector_pg.go / collector_mysql.go.
type Collector struct {
	q SystemQuerier
}

func NewCollector(q SystemQuerier) *Collector { return &Collector{q: q} }

// systemSchemas are never collected.
var systemSchemas = map[string]bool{
	"pg_catalog": true, "information_schema": true, "pg_toast": true,
	"mysql": true, "performance_schema": true, "sys": true,
}

// Collect reads the full physical model for the requested schemas.
func (c *Collector) Collect(ctx context.Context, req CollectRequest) (*RawSnapshot, error) {
	dialect, err := c.q.ProfileDialect(ctx, req.SourceID)
	if err != nil {
		return nil, err
	}
	snap := &RawSnapshot{
		SourceID:         req.SourceID,
		Dialect:          dialect,
		CollectorVersion: CollectorVersion,
		Status:           "success",
	}

	var tables []TableAsset
	switch dialect {
	case "postgres":
		tables, err = c.collectPostgres(ctx, req)
	case "mysql", "mariadb":
		tables, err = c.collectMySQL(ctx, req, dialect)
	default:
		return nil, fmt.Errorf("unsupported dialect for metadata collection: %s", dialect)
	}
	if err != nil {
		return nil, err
	}

	// stable ordering + per-table structural hash
	sort.Slice(tables, func(i, j int) bool { return tables[i].FQN() < tables[j].FQN() })
	schemas := map[string]bool{}
	for i := range tables {
		sort.Slice(tables[i].Columns, func(a, b int) bool { return tables[i].Columns[a].Ordinal < tables[i].Columns[b].Ordinal })
		tables[i].StructHash = structHash(tables[i])
		schemas[tables[i].Schema] = true
		snap.ObjectCount.Columns += len(tables[i].Columns)
		snap.ObjectCount.Constraints += len(tables[i].Constraints)
		snap.ObjectCount.Indexes += len(tables[i].Indexes)
		if tables[i].Kind == "view" || tables[i].Kind == "materialized_view" {
			snap.ObjectCount.Views++
		} else {
			snap.ObjectCount.Tables++
		}
	}
	snap.ObjectCount.Schemas = len(schemas)
	snap.Tables = tables
	snap.SchemaHash = schemaHash(tables)
	snap.Checkpoint = Checkpoint{SchemaHash: snap.SchemaHash}
	return snap, nil
}

// DiscoverSchemas lists the non-system schemas available on the source.
func (c *Collector) DiscoverSchemas(ctx context.Context, sourceID string) ([]DatabaseAsset, error) {
	dialect, err := c.q.ProfileDialect(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	var query string
	switch dialect {
	case "postgres":
		query = `SELECT schema_name FROM information_schema.schemata ORDER BY schema_name`
	case "mysql", "mariadb":
		query = `SELECT schema_name FROM information_schema.schemata ORDER BY schema_name`
	default:
		return nil, fmt.Errorf("unsupported dialect: %s", dialect)
	}
	rows, err := c.q.SystemQuery(ctx, sourceID, query)
	if err != nil {
		return nil, err
	}
	var out []DatabaseAsset
	for _, r := range rows {
		s := asString(r["schema_name"])
		if s == "" || systemSchemas[strings.ToLower(s)] {
			continue
		}
		out = append(out, DatabaseAsset{Schema: s, Kind: "schema"})
	}
	return out, nil
}

// ---- hashing ----

// structHash hashes only the structural definition of a table (schema, name,
// kind, ordered columns with type/nullability/keys, constraints, indexes) so
// that comment or row-count churn does NOT count as a structural change.
func structHash(t TableAsset) string {
	type colSig struct {
		N, T         string
		Null, PK, FK bool
		Ord          int
		Def, Gen     string
	}
	sig := struct {
		Schema, Name, Kind, View string
		Cols                     []colSig
		Cons                     []ConstraintAsset
		Idx                      []IndexAsset
	}{Schema: t.Schema, Name: t.Name, Kind: t.Kind, View: normalizeSQL(t.ViewSQL)}
	for _, col := range t.Columns {
		sig.Cols = append(sig.Cols, colSig{
			N: col.Name, T: strings.ToLower(col.FullType), Null: col.Nullable,
			PK: col.IsPrimaryKey, FK: col.IsForeignKey, Ord: col.Ordinal,
			Def: col.Default, Gen: col.Generated,
		})
	}
	cons := append([]ConstraintAsset(nil), t.Constraints...)
	sort.Slice(cons, func(i, j int) bool { return cons[i].Name < cons[j].Name })
	sig.Cons = cons
	idx := append([]IndexAsset(nil), t.Indexes...)
	sort.Slice(idx, func(i, j int) bool { return idx[i].Name < idx[j].Name })
	sig.Idx = idx
	b, _ := json.Marshal(sig)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// schemaHash combines all table struct hashes into a single source-level hash.
func schemaHash(tables []TableAsset) string {
	h := sha256.New()
	for _, t := range tables {
		h.Write([]byte(t.FQN()))
		h.Write([]byte(t.StructHash))
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func normalizeSQL(s string) string { return strings.Join(strings.Fields(strings.ToLower(s)), " ") }

func asString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

func asInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case string:
		var n int64
		fmt.Sscan(x, &n)
		return n
	case []byte:
		var n int64
		fmt.Sscan(string(x), &n)
		return n
	default:
		return 0
	}
}

func asBool(v any, trueVals ...string) bool {
	s := strings.ToLower(strings.TrimSpace(asString(v)))
	for _, t := range trueVals {
		if s == t {
			return true
		}
	}
	return s == "yes" || s == "true" || s == "1"
}
