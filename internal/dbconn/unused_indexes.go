package dbconn

import (
	"context"
	"fmt"
)

type UnusedIndex struct {
	TableName  string `json:"table_name"`
	IndexName  string `json:"index_name"`
	Columns    string `json:"columns,omitempty"` // ordered CSV; enables a reversible drop ChangePlan
	SizeBytes  int64  `json:"size_bytes"`
	ScanCount  int64  `json:"scan_count"`
	DropScript string `json:"drop_script"`
}

// ListUnusedIndexes returns non-unique indexes with zero read scans.
func (m *Manager) ListUnusedIndexes(ctx context.Context, profileID string) ([]UnusedIndex, error) {
	p, err := m.store.GetProfileByID(ctx, profileID)
	if err != nil {
		return nil, err
	}
	d, err := DialectFor(p.Type)
	if err != nil {
		return nil, err
	}
	db, err := m.db(p)
	if err != nil {
		return nil, err
	}

	var list []UnusedIndex

	if d.Name() == "postgres" {
		query := `SELECT
			schemaname || '.' || relname AS table_name,
			indexrelname AS index_name,
			pg_relation_size(i.indexrelid)::bigint AS size_bytes,
			idx_scan::bigint AS scan_count,
			COALESCE((SELECT string_agg(a.attname, ',' ORDER BY k.ord)
			          FROM unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord)
			          JOIN pg_catalog.pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum), '') AS columns
		FROM pg_catalog.pg_stat_user_indexes ui
		JOIN pg_catalog.pg_index i ON ui.indexrelid = i.indexrelid
		WHERE NOT indisunique
		  AND idx_scan = 0
		  AND i.indpred IS NULL
		  AND i.indexprs IS NULL
		ORDER BY size_bytes DESC LIMIT 100`

		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("query unused indexes failed: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var ui UnusedIndex
			if err := rows.Scan(&ui.TableName, &ui.IndexName, &ui.SizeBytes, &ui.ScanCount, &ui.Columns); err != nil {
				return nil, err
			}
			ui.DropScript = fmt.Sprintf("DROP INDEX %s;", ui.IndexName)
			list = append(list, ui)
		}
		return list, nil
	} else if d.Name() == "mysql" || d.Name() == "mariadb" {
		query := `SELECT 
			OBJECT_SCHEMA AS schema_name,
			OBJECT_NAME AS table_name,
			INDEX_NAME AS index_name
		FROM performance_schema.table_io_waits_summary_by_index_usage
		WHERE INDEX_NAME IS NOT NULL
		  AND INDEX_NAME != 'PRIMARY'
		  AND COUNT_STAR = 0
		ORDER BY OBJECT_SCHEMA, OBJECT_NAME LIMIT 100`

		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			// Return empty list if performance_schema is disabled/unreadable
			return []UnusedIndex{}, nil
		}
		defer rows.Close()

		for rows.Next() {
			var schemaName, tableName, indexName string
			if err := rows.Scan(&schemaName, &tableName, &indexName); err != nil {
				return nil, err
			}
			ui := UnusedIndex{
				TableName:  schemaName + "." + tableName,
				IndexName:  indexName,
				SizeBytes:  0,
				ScanCount:  0,
				DropScript: fmt.Sprintf("ALTER TABLE %s.%s DROP INDEX %s;", schemaName, tableName, indexName),
			}
			list = append(list, ui)
		}
		return list, nil
	}

	return []UnusedIndex{}, nil
}
