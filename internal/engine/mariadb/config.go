package mariadb

import (
	"context"

	"sqlon/internal/dbconn"
	"sqlon/internal/engine/mysql"
	"sqlon/internal/observability"
)

// Config implements observability.ConfigProvider via the shared MySQL-family
// global-variables collection.
type Config struct{}

func (Config) Config(ctx context.Context, q observability.SystemQueryer, p dbconn.Profile) (map[string]string, map[string]bool, error) {
	return mysql.CollectConfig(ctx, q, p)
}
