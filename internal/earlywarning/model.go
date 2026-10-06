// Package earlywarning turns SQLON's read-only observations into advance
// warnings. It forecasts when storage fills, watches what fills it (WAL a
// slot or a failing archiver keeps, temp-file spills, runaway tables),
// notices schema changes nobody planned, and runs every finding through one
// alert lifecycle — fire, escalate, remind, resolve — delivered to a webhook.
//
// It never queries a database itself. Every input is something another
// SQLON service already collected with fixed read-only queries: workload and
// capacity snapshots (collector), maintenance findings (observability), and
// physical schema snapshots (metasync).
package earlywarning

import "time"

const (
	SevInfo     = "info"
	SevWarning  = "warning"
	SevCritical = "critical"
)

// Rank orders severities; unknown values rank below info.
func Rank(severity string) int {
	switch severity {
	case SevCritical:
		return 3
	case SevWarning:
		return 2
	case SevInfo:
		return 1
	}
	return 0
}

// Every condition belongs to the check that produced it. A firing alert is
// resolved only by a later run of the same check that no longer reports it,
// so a check that could not run (DB unreachable, permission denied) never
// silently clears an alert it did not re-examine.
const (
	CheckCollection  = "collection"
	CheckCapacity    = "capacity"
	CheckTables      = "tables"
	CheckMaintenance = "maintenance"
	CheckSchema      = "schema"
	CheckWorkload    = "workload"
	CheckHostDisk    = "host_disk"
	// CheckHostDiskAgent owns only the "reports stopped" warning, so a dead
	// agent never counts as a re-examination of the volume alerts.
	CheckHostDiskAgent = "host_disk_agent"
)

// Rules — the stable machine names of what an alert is about.
const (
	RuleCollectionDown   = "collection_down"
	RuleCapacityUsage    = "capacity_usage"
	RuleCapacityForecast = "capacity_forecast"
	RuleGrowthSurge      = "capacity_growth_surge"
	RuleLimitUndeclared  = "capacity_limit_undeclared"
	RuleMonitorPrivilege = "monitor_privilege"
	RuleTempSpill        = "temp_spill"
	RuleTableSurge       = "table_growth_surge"
	RuleTableShrink      = "table_shrink"
	RuleSchemaChange     = "schema_change"
	RuleSchemaBaseline   = "schema_baseline"
	RuleHostDiskStale    = "host_disk_stale"
	RuleMaintenance      = "maint_" // + finding category
)

// Condition is one thing a check found wrong in the current cycle.
type Condition struct {
	Key            string         `json:"key"`
	ProfileID      string         `json:"profile_id"`
	Check          string         `json:"check"`
	Rule           string         `json:"rule"`
	Severity       string         `json:"severity"`
	Object         string         `json:"object,omitempty"`
	Title          string         `json:"title"`
	Detail         string         `json:"detail,omitempty"`
	Recommendation string         `json:"recommendation,omitempty"`
	Value          float64        `json:"value,omitempty"`
	Threshold      float64        `json:"threshold,omitempty"`
	Attributes     map[string]any `json:"attributes,omitempty"`
	// Event marks a one-shot occurrence (a schema change happened) rather
	// than a standing state. Events notify once and stay firing until
	// acknowledged or EventTTL passes; their absence resolves nothing.
	Event bool `json:"event,omitempty"`
	// QuietResolve suppresses the "resolved" notification for standing
	// conditions that clear by aging out of their window (table shrink).
	QuietResolve bool `json:"quiet_resolve,omitempty"`
}

// Alert is a condition with a lifecycle.
type Alert struct {
	ID             string         `json:"id"`
	Key            string         `json:"key"`
	ProfileID      string         `json:"profile_id"`
	Check          string         `json:"check"`
	Rule           string         `json:"rule"`
	Severity       string         `json:"severity"`
	PeakSeverity   string         `json:"peak_severity"`
	State          string         `json:"state"` // firing | resolved
	Object         string         `json:"object,omitempty"`
	Title          string         `json:"title"`
	Detail         string         `json:"detail,omitempty"`
	Recommendation string         `json:"recommendation,omitempty"`
	Value          float64        `json:"value,omitempty"`
	Threshold      float64        `json:"threshold,omitempty"`
	Attributes     map[string]any `json:"attributes,omitempty"`
	Event          bool           `json:"event,omitempty"`
	QuietResolve   bool           `json:"quiet_resolve,omitempty"`
	FirstSeen      time.Time      `json:"first_seen"`
	LastSeen       time.Time      `json:"last_seen"`
	ResolvedAt     *time.Time     `json:"resolved_at,omitempty"`
	ResolveReason  string         `json:"resolve_reason,omitempty"`
	// Delivery bookkeeping. Pending notifications are derived from these on
	// every cycle, so a failed delivery is retried rather than lost.
	LastNotifiedAt   *time.Time `json:"last_notified_at,omitempty"`
	NotifiedSeverity string     `json:"notified_severity,omitempty"`
	NotifyCount      int        `json:"notify_count"`
	ResolveNotified  bool       `json:"resolve_notified,omitempty"`
	// Escalation bookkeeping: when the alert became critical, when the
	// on-call channel was paged, and whether that channel heard it resolve.
	CriticalSince       *time.Time `json:"critical_since,omitempty"`
	PagedAt             *time.Time `json:"paged_at,omitempty"`
	PageResolveNotified bool       `json:"page_resolve_notified,omitempty"`
	AckedBy             string     `json:"acked_by,omitempty"`
	AckedAt             *time.Time `json:"acked_at,omitempty"`
	AckNote             string     `json:"ack_note,omitempty"`
	// SilencedUntil and Flapping are filled in read models only (Board).
	SilencedUntil *time.Time `json:"silenced_until,omitempty"`
	Flapping      bool       `json:"flapping,omitempty"`
}

const (
	StateFiring   = "firing"
	StateResolved = "resolved"
)

// Notification kinds.
const (
	KindFiring    = "firing"
	KindEscalated = "escalated"
	KindReminder  = "reminder"
	KindResolved  = "resolved"
)

type Notification struct {
	Kind  string `json:"kind"`
	Alert Alert  `json:"alert"`
	// Cause names the probable root cause when this alert is a symptom of
	// another firing alert (see correlate).
	Cause string `json:"cause,omitempty"`
}

// Forecast is the storage outlook of one capacity asset.
type Forecast struct {
	Asset        string  `json:"asset"` // scope:name
	Scope        string  `json:"scope"`
	Name         string  `json:"name"`
	Primary      bool    `json:"primary"` // stands for the whole storage volume
	UsedBytes    float64 `json:"used_bytes"`
	LimitBytes   float64 `json:"limit_bytes,omitempty"`
	LimitSource  string  `json:"limit_source,omitempty"` // declared | engine
	UsagePercent float64 `json:"usage_percent,omitempty"`
	// Growth in bytes/day fitted by least squares over a recent window
	// (short, catches a surge) and a long one (the trend).
	GrowthShort *Fit `json:"growth_short,omitempty"`
	GrowthLong  *Fit `json:"growth_long,omitempty"`
	// DaysToFull is the earliest projection among windows with a valid,
	// positive fit; nil when no limit is known or nothing is growing.
	DaysToFull *float64   `json:"days_to_full,omitempty"`
	FullAt     *time.Time `json:"full_at,omitempty"`
	Basis      string     `json:"basis,omitempty"` // window behind DaysToFull
	// Attribution splits the recent growth into what grew.
	Attribution *Attribution `json:"attribution,omitempty"`
	Status      string       `json:"status"` // ok | info | warning | critical | unknown
	Note        string       `json:"note,omitempty"`
}

// Fit is a least-squares line through a series window.
type Fit struct {
	Window       string  `json:"window"`
	BytesPerDay  float64 `json:"bytes_per_day"`
	R2           float64 `json:"r2"`
	Points       int     `json:"points"`
	SpanHours    float64 `json:"span_hours"`
	Valid        bool    `json:"valid"`
	InvalidCause string  `json:"invalid_cause,omitempty"`
}
