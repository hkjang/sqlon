package metasync

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Migration-tool awareness: most applications change their schema through a
// migration tool that records every applied migration in a history table.
// When a schema change coincides with new rows there, it was a deployment,
// not someone altering production by hand. These are read-only lookups of
// tables the physical snapshot already found.

// Migration is one applied migration as its tool recorded it.
type Migration struct {
	Tool        string    `json:"tool"`
	Version     string    `json:"version"`
	Description string    `json:"description,omitempty"`
	AppliedBy   string    `json:"applied_by,omitempty"`
	AppliedAt   time.Time `json:"applied_at"`
	Table       string    `json:"table"`
}

type migrationTool struct {
	name  string
	table string // lower-case history table name
	// columns: version, description, applied_by, applied_at (+ optional success)
	selectList string
	appliedCol string
	success    string // column whose value must be true, "" when none
}

var migrationTools = []migrationTool{
	{"flyway", "flyway_schema_history", "version AS version, description AS description, installed_by AS applied_by, installed_on AS applied_at, success AS ok", "installed_on", "ok"},
	{"liquibase", "databasechangelog", "id AS version, COALESCE(description, filename) AS description, author AS applied_by, dateexecuted AS applied_at", "dateexecuted", ""},
	{"django", "django_migrations", "name AS version, app AS description, '' AS applied_by, applied AS applied_at", "applied", ""},
	{"prisma", "_prisma_migrations", "migration_name AS version, migration_name AS description, '' AS applied_by, finished_at AS applied_at", "finished_at", ""},
	{"knex", "knex_migrations", "name AS version, name AS description, '' AS applied_by, migration_time AS applied_at", "migration_time", ""},
}

// RecentMigrations returns migrations applied at or after since, from every
// known history table present in the snapshot. A table that cannot be read
// is reported in the error but does not hide the others.
func (s *Service) RecentMigrations(ctx context.Context, snap *RawSnapshot, since time.Time) ([]Migration, error) {
	if snap == nil {
		return nil, nil
	}
	dialect := snap.Dialect
	if dialect != "postgres" && dialect != "mysql" && dialect != "mariadb" {
		return nil, nil
	}
	q := newQuoter(dialect)
	placeholder := "?"
	if dialect == "postgres" {
		placeholder = "$1"
	}
	var out []Migration
	var errs []string
	for _, t := range snap.Tables {
		for _, tool := range migrationTools {
			if !strings.EqualFold(t.Name, tool.table) {
				continue
			}
			query := fmt.Sprintf("SELECT %s FROM %s WHERE %s >= %s ORDER BY %s", tool.selectList, q.qualified(t.Schema, t.Name), tool.appliedCol, placeholder, tool.appliedCol)
			rows, err := s.col.q.SystemQuery(ctx, snap.SourceID, query, since.UTC())
			if err != nil {
				errs = append(errs, tool.name+": "+err.Error())
				continue
			}
			for _, row := range rows {
				if tool.success != "" && !truthy(asString(row[tool.success])) {
					continue
				}
				out = append(out, Migration{Tool: tool.name, Version: asString(row["version"]), Description: asString(row["description"]),
					AppliedBy: asString(row["applied_by"]), AppliedAt: parseTime(row["applied_at"]), Table: t.FQN()})
			}
		}
	}
	if len(errs) > 0 {
		return out, fmt.Errorf("migration history: %s", strings.Join(errs, "; "))
	}
	return out, nil
}

func truthy(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "true" || v == "t" || v == "1"
}

func parseTime(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case string:
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999", "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
			if parsed, err := time.Parse(layout, t); err == nil {
				return parsed
			}
		}
	}
	return time.Time{}
}
