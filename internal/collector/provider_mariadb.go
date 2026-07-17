package collector

import (
	"context"

	"sqlon/internal/dbconn"
)

type mariadbProvider struct{}

func (mariadbProvider) Collect(ctx context.Context, q SystemQueryer, p dbconn.Profile) (Snapshot, error) {
	return collectMySQL(ctx, q, p, "mariadb")
}
