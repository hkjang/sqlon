// Package engine defines the product-level database engine contract.
//
// Providers are intentionally small: an adapter implements only the roles it
// can safely support. Services must check CapabilitySet instead of branching
// on an engine name or assuming a database feature exists.
package engine

import "context"

// CapabilitySet is the feature declaration returned for an engine/profile.
// A false value means unsupported or intentionally unavailable by policy.
type CapabilitySet struct {
	Sessions       bool `json:"sessions"`
	LockTree       bool `json:"lock_tree"`
	Workload       bool `json:"workload"`
	QueryPlans     bool `json:"query_plans"`
	Replication    bool `json:"replication"`
	BackupStatus   bool `json:"backup_status"`
	Storage        bool `json:"storage"`
	UserManagement bool `json:"user_management"`
	Maintenance    bool `json:"maintenance"`
	Multitenant    bool `json:"multitenant"`
	RAC            bool `json:"rac"`
}

// Identity is the stable database scope attached to every collected asset.
// Oracle adapters populate container fields; other engines leave them empty.
type Identity struct {
	Engine        string `json:"engine"`
	DBUniqueName  string `json:"db_unique_name,omitempty"`
	InstanceName  string `json:"instance_name,omitempty"`
	ServiceName   string `json:"service_name,omitempty"`
	ContainerID   string `json:"container_id,omitempty"`
	ContainerName string `json:"container_name,omitempty"`
}

// Connector owns connection lifecycle and discovery only.
type Connector interface {
	Ping(context.Context) (Identity, error)
}

// MetadataProvider, SessionProvider and LockProvider deliberately expose
// normalized records. Rich engine-native fields can be held in Details.
type MetadataProvider interface {
	CollectMetadata(context.Context) ([]Asset, error)
}
type SessionProvider interface {
	ListSessions(context.Context) ([]Session, error)
}
type LockProvider interface {
	ListLocks(context.Context) ([]LockEdge, error)
}

type Asset struct {
	Identity          Identity `json:"identity"`
	Owner, Name, Type string
	Details           map[string]any
}
type Session struct {
	Identity                Identity `json:"identity"`
	Key, User, State, SQLID string
	Details                 map[string]any
}
type LockEdge struct {
	Identity                   Identity `json:"identity"`
	Blocker, Blocked, LockType string
	Details                    map[string]any
}
