package dbconn

import (
	"context"
	"fmt"
)

// System-catalog access for internal metadata collection (metasync). Unlike
// Execute, these queries are NOT subject to the read-only SQL guard, plan
// gate, or the small row cap — they are server-internal queries against
// information_schema / system catalogs, never user SQL. The session is still
// read-only (enforced by the DSN), the connection pool and circuit breaker
// are reused, and a hard row ceiling bounds memory.

// SystemQueryMaxRows caps rows returned by a system-catalog query.
const SystemQueryMaxRows = 500_000

// ProfileDialect returns the resolved dialect name for a profile id, so the
// metadata collector can pick dialect-specific catalog queries.
func (m *Manager) ProfileDialect(ctx context.Context, profileID string) (string, error) {
	p, err := m.store.GetProfileByID(ctx, profileID)
	if err != nil {
		return "", err
	}
	d, err := DialectFor(p.Type)
	if err != nil {
		return "", err
	}
	return d.Name(), nil
}

// SystemQuery runs a trusted read-only query (information_schema / system
// catalog) against a profile's pool and returns rows as generic maps. It is
// for internal metadata collection only — do not route user SQL through it.
func (m *Manager) SystemQuery(ctx context.Context, profileID, query string, args ...any) ([]map[string]any, error) {
	p, err := m.store.GetProfileByID(ctx, profileID)
	if err != nil {
		return nil, err
	}
	if err := m.breakerCheck(p.ID); err != nil {
		return nil, err
	}
	db, err := m.db(p)
	if err != nil {
		m.breakerRecord(p.ID, err)
		return nil, err
	}
	qctx, cancel := context.WithTimeout(ctx, durationSeconds(p.Policy.QueryTimeoutSeconds))
	defer cancel()
	rows, err := db.QueryContext(qctx, query, args...)
	if err != nil {
		m.breakerRecord(p.ID, err)
		return nil, fmt.Errorf("system query failed: %s", sanitizeDBError(err))
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, 256)
	for rows.Next() {
		if len(out) >= SystemQueryMaxRows {
			break
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = normalizeValue(vals[i])
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	m.breakerRecord(p.ID, nil)
	return out, nil
}
