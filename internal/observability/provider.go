package observability

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"sqlon/internal/dbconn"
)

// SystemQueryer is deliberately narrower than the normal query executor. A
// provider supplies a compile-time constant system query; callers cannot pass
// arbitrary SQL through this interface.
type SystemQueryer interface {
	SystemQuery(context.Context, string, string, ...any) ([]map[string]any, error)
}

type Provider interface {
	Sessions(context.Context, SystemQueryer, dbconn.Profile) ([]Session, error)
	Locks(context.Context, SystemQueryer, dbconn.Profile) ([]LockEdge, error)
}

// SnapshotRowLimit is a hard protection against unbounded operational views.
// Reaching it is reported as a limitation instead of silently looking whole.
const SnapshotRowLimit = 10_000

type Registry struct{ providers map[string]Provider }

func NewRegistry() *Registry {
	return &Registry{providers: map[string]Provider{
		"postgres": postgresProvider{},
		"mysql":    mysqlProvider{},
		"mariadb":  mariadbProvider{},
		"oracle":   oracleProvider{},
	}}
}

func (r *Registry) Get(engine string) (Provider, bool) {
	p, ok := r.providers[strings.ToLower(strings.TrimSpace(engine))]
	return p, ok
}

func rowValue(row map[string]any, names ...string) any {
	for _, name := range names {
		for key, value := range row {
			if strings.EqualFold(key, name) {
				return value
			}
		}
	}
	return nil
}

func textValue(row map[string]any, names ...string) string {
	v := rowValue(row, names...)
	if v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func intValue(row map[string]any, names ...string) int64 {
	v := rowValue(row, names...)
	switch n := v.(type) {
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(fmt.Sprint(v)), 10, 64)
	return n
}

func sessionKey(parts ...string) string {
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			clean = append(clean, strings.TrimSpace(part))
		}
	}
	return strings.Join(clean, ":")
}

func protectSession(engine, user, state, application string) (bool, string) {
	u := strings.ToUpper(strings.TrimSpace(user))
	a := strings.ToLower(application)
	s := strings.ToLower(state)
	if u == "SYS" || u == "SYSTEM" {
		return true, "Oracle 시스템 계정"
	}
	if u == "SYSTEM USER" || u == "EVENT_SCHEDULER" {
		return true, "데이터베이스 시스템 세션"
	}
	if strings.Contains(a, "walreceiver") || strings.Contains(a, "walsender") || strings.Contains(a, "replication") {
		return true, "복제 세션"
	}
	if strings.Contains(s, "binlog dump") || strings.Contains(s, "replication") {
		return true, "복제 세션"
	}
	if strings.Contains(s, "background") || (engine == "oracle" && u == "") {
		return true, "백그라운드 시스템 세션"
	}
	return false, ""
}

func sortSessions(items []Session) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].DurationSeconds != items[j].DurationSeconds {
			return items[i].DurationSeconds > items[j].DurationSeconds
		}
		return items[i].SessionKey < items[j].SessionKey
	})
}

func nowUTC() time.Time { return time.Now().UTC() }
