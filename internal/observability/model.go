// Package observability collects evidence-bearing, read-only operational
// snapshots through engine-native providers. It never accepts caller SQL.
package observability

import "time"

type Evidence struct {
	Code        string         `json:"code"`
	Severity    string         `json:"severity"`
	Summary     string         `json:"summary"`
	Attributes  map[string]any `json:"attributes,omitempty"`
	CollectedAt time.Time      `json:"collected_at"`
}

type Session struct {
	ProfileID            string    `json:"profile_id"`
	Engine               string    `json:"engine"`
	SessionKey           string    `json:"session_key"`
	InstanceID           string    `json:"instance_id,omitempty"`
	SessionID            string    `json:"session_id"`
	Serial               string    `json:"serial,omitempty"`
	User                 string    `json:"user,omitempty"`
	State                string    `json:"state,omitempty"`
	Service              string    `json:"service,omitempty"`
	Application          string    `json:"application,omitempty"`
	Client               string    `json:"client,omitempty"`
	SQLID                string    `json:"sql_id,omitempty"`
	WaitClass            string    `json:"wait_class,omitempty"`
	WaitEvent            string    `json:"wait_event,omitempty"`
	QueryStartedAt       string    `json:"query_started_at,omitempty"`
	TransactionStartedAt string    `json:"transaction_started_at,omitempty"`
	DurationSeconds      int64     `json:"duration_seconds,omitempty"`
	TransactionSeconds   int64     `json:"transaction_seconds,omitempty"`
	Protected            bool      `json:"protected"`
	ProtectionReason     string    `json:"protection_reason,omitempty"`
	CollectedAt          time.Time `json:"collected_at"`
}

type LockEdge struct {
	ProfileID    string    `json:"profile_id"`
	Engine       string    `json:"engine"`
	BlockerKey   string    `json:"blocker_key"`
	BlockedKey   string    `json:"blocked_key"`
	BlockerUser  string    `json:"blocker_user,omitempty"`
	BlockedUser  string    `json:"blocked_user,omitempty"`
	LockType     string    `json:"lock_type,omitempty"`
	WaitSeconds  int64     `json:"wait_seconds,omitempty"`
	BlockedSQLID string    `json:"blocked_sql_id,omitempty"`
	CollectedAt  time.Time `json:"collected_at"`
}

type LockRoot struct {
	SessionKey       string `json:"session_key"`
	User             string `json:"user,omitempty"`
	AffectedSessions int    `json:"affected_sessions"`
}

type SessionData struct {
	ProfileID   string    `json:"profile_id"`
	Engine      string    `json:"engine"`
	Sessions    []Session `json:"sessions"`
	Total       int       `json:"total"`
	Active      int       `json:"active"`
	Waiting     int       `json:"waiting"`
	LongRunning int       `json:"long_running"`
}

type LockData struct {
	ProfileID       string     `json:"profile_id"`
	Engine          string     `json:"engine"`
	Edges           []LockEdge `json:"edges"`
	Roots           []LockRoot `json:"roots"`
	BlockedSessions int        `json:"blocked_sessions"`
}

// ReplicationNode is one observed replication participant: a connected
// standby, a replication slot, a WAL receiver, a MySQL channel, or an Oracle
// archive destination / Data Guard lag measurement.
type ReplicationNode struct {
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	Target      string    `json:"target,omitempty"`
	State       string    `json:"state,omitempty"`
	SyncState   string    `json:"sync_state,omitempty"`
	LagSeconds  float64   `json:"lag_seconds"`
	LagBytes    float64   `json:"lag_bytes,omitempty"`
	Error       string    `json:"error,omitempty"`
	Healthy     bool      `json:"healthy"`
	CollectedAt time.Time `json:"collected_at"`
}

// LagUnknown marks a node whose lag could not be measured — distinct from a
// measured lag of zero (No Silent Failure).
const LagUnknown = -1

type ReplicationData struct {
	ProfileID string            `json:"profile_id"`
	Engine    string            `json:"engine"`
	Role      string            `json:"role"` // primary | replica | standby | standalone | unknown
	Details   map[string]string `json:"details,omitempty"`
	Nodes     []ReplicationNode `json:"nodes"`
	// Provider-reported partial-collection notes; the service merges them
	// into the response envelope.
	Warnings    []string `json:"-"`
	Limitations []string `json:"-"`
}

// BackupItem is one observed backup-related component: the WAL archiver, a
// binary log, an RMAN job, or the fast recovery area.
type BackupItem struct {
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	Status      string    `json:"status,omitempty"`
	Detail      string    `json:"detail,omitempty"`
	OccurredAt  string    `json:"occurred_at,omitempty"`
	Error       string    `json:"error,omitempty"`
	Healthy     bool      `json:"healthy"`
	CollectedAt time.Time `json:"collected_at"`
}

type BackupData struct {
	ProfileID string `json:"profile_id"`
	Engine    string `json:"engine"`
	// Archiving reports whether continuous log archiving (the PITR basis) is
	// enabled: enabled | disabled | unknown.
	Archiving     string       `json:"archiving"`
	ArchivingKind string       `json:"archiving_kind,omitempty"` // wal_archive | binlog | archivelog
	Items         []BackupItem `json:"items"`
	LastSuccessAt string       `json:"last_success_at,omitempty"`
	LastFailureAt string       `json:"last_failure_at,omitempty"`
	// Provider-reported partial-collection notes; the service merges them
	// into the response envelope.
	Warnings    []string `json:"-"`
	Limitations []string `json:"-"`
}

type Response[T any] struct {
	Status      string     `json:"status"`
	Data        T          `json:"data"`
	Evidence    []Evidence `json:"evidence"`
	Warnings    []string   `json:"warnings"`
	Limitations []string   `json:"limitations"`
	CollectedAt time.Time  `json:"collected_at"`
	TraceID     string     `json:"trace_id"`
}
