package observability

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"sqlon/internal/dbconn"
)

// SystemQueryer is deliberately narrower than the normal query executor. A
// provider supplies a compile-time constant system query; callers cannot pass
// arbitrary SQL through this interface.
type SystemQueryer interface {
	SystemQuery(context.Context, string, string, ...any) ([]map[string]any, error)
}

// Provider is the session/lock observation role. Engine implementations live
// in internal/engine/<engine> and are wired by internal/engine/adapters; this
// package owns the result model, protection policy, and service logic only.
type Provider interface {
	Sessions(context.Context, SystemQueryer, dbconn.Profile) ([]Session, error)
	Locks(context.Context, SystemQueryer, dbconn.Profile) ([]LockEdge, error)
}

// ReplicationProvider is the replication-topology observation role. It is a
// separate interface (not a Provider method) so an engine can ship
// session/lock support before replication support, and vice versa.
type ReplicationProvider interface {
	Replication(context.Context, SystemQueryer, dbconn.Profile) (ReplicationData, error)
}

// BackupProvider is the backup-status observation role. SQLON does not run
// backups; it observes what the server itself can report (archiver/binlog/
// RMAN catalog) and marks external-tool integration as a limitation.
type BackupProvider interface {
	Backup(context.Context, SystemQueryer, dbconn.Profile) (BackupData, error)
}

// SecurityProvider is the privilege-posture observation role: users/roles
// with elevated or dangerous privileges, wildcard hosts, expired passwords.
// Read-only diagnosis — remediation always goes through a change plan.
type SecurityProvider interface {
	Security(context.Context, SystemQueryer, dbconn.Profile) (SecurityData, error)
}

// MaintenanceProvider is the proactive-maintenance observation role: latent
// risks that surface no error until they cause an outage — transaction-ID
// wraparound, table/index bloat, and WAL-retaining inactive replication slots.
// Read-only diagnosis — remediation always goes through a change plan.
type MaintenanceProvider interface {
	Maintenance(context.Context, SystemQueryer, dbconn.Profile) (MaintenanceData, error)
}

// ConfigProvider reads live server parameters as name→value (plus a
// pending-restart set where the engine reports it) so the service can compare
// them against the operator-declared baseline (configuration drift).
type ConfigProvider interface {
	Config(context.Context, SystemQueryer, dbconn.Profile) (values map[string]string, pendingRestart map[string]bool, err error)
}

// SnapshotRowLimit is a hard protection against unbounded operational views.
// Reaching it is reported as a limitation instead of silently looking whole.
const SnapshotRowLimit = 10_000

type Registry struct{ providers map[string]Provider }

// NewRegistry builds a provider registry from an engine-name→implementation
// map (normally adapters.ObservabilityProviders()). A nil map yields an empty
// registry, which reports every engine as unsupported.
func NewRegistry(providers map[string]Provider) *Registry {
	normalized := make(map[string]Provider, len(providers))
	for name, provider := range providers {
		normalized[strings.ToLower(strings.TrimSpace(name))] = provider
	}
	return &Registry{providers: normalized}
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

// Text and Int read a column case-insensitively from a system-query row,
// trying the given names in order. Exported for the engine adapter packages.
func Text(row map[string]any, names ...string) string {
	v := rowValue(row, names...)
	if v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func Number(row map[string]any, names ...string) float64 {
	v := rowValue(row, names...)
	switch n := v.(type) {
	case int:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	case float32:
		return float64(n)
	case float64:
		return n
	}
	n, _ := strconv.ParseFloat(strings.TrimSpace(fmt.Sprint(v)), 64)
	return n
}

func Int(row map[string]any, names ...string) int64 {
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

// SessionKey joins the non-empty parts into the engine-agnostic session key
// (Oracle uses INST_ID:SID:SERIAL# so a recycled SID can never be confused
// with the session that previously held it).
func SessionKey(parts ...string) string {
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			clean = append(clean, strings.TrimSpace(part))
		}
	}
	return strings.Join(clean, ":")
}

// ProtectSession is the product-wide policy marking sessions that must never
// be cancelled or terminated. It is deliberately owned here — engine adapters
// classify their sessions through this single policy.
func ProtectSession(engine, user, state, application string) (bool, string) {
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

// SortSessions orders sessions longest-running first for operator triage.
func SortSessions(items []Session) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].DurationSeconds != items[j].DurationSeconds {
			return items[i].DurationSeconds > items[j].DurationSeconds
		}
		return items[i].SessionKey < items[j].SessionKey
	})
}
