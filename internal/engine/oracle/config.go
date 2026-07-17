package oracle

import (
	"context"

	"sqlon/internal/dbconn"
	"sqlon/internal/observability"
)

// Config implements observability.ConfigProvider using V$PARAMETER (base
// view; no Diagnostics/Tuning Pack). isspecified/ismodified are not restart
// flags, so no pending-restart set is reported.
type Config struct{}

const parameterSQL = `SELECT name, NVL(value, '') AS value FROM v$parameter`

func (Config) Config(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) (map[string]string, map[string]bool, error) {
	rows, err := q.SystemQuery(ctx, p.ID, parameterSQL)
	if err != nil {
		return nil, nil, err
	}
	values := make(map[string]string, len(rows))
	for _, row := range rows {
		name := observability.Text(row, "name")
		if name != "" {
			values[name] = observability.Text(row, "value")
		}
	}
	return values, map[string]bool{}, nil
}
