// Package engine defines the product-level database engine contract.
//
// The contract has two halves:
//
//   - Capability declarations (this package). CapabilitySet says what each
//     engine supports; services enable features from these flags instead of
//     branching on postgres/mysql/mariadb/oracle names or assuming a
//     database feature exists.
//
//   - Role-provider implementations (internal/engine/<engine> packages).
//     Each engine package holds that engine's fixed system queries and row
//     normalization, implementing the small role interfaces owned by the
//     consuming service (collector.Provider for workload/capacity,
//     observability.Provider for sessions/locks, ...). A role interface
//     lives with the service that defines its result model, so adding a
//     role never grows a giant shared interface.
//
// internal/engine/adapters is the single wiring point that binds engine
// names to their implementations. New engine support is added by creating a
// new engine package and registering it there — never by adding a name
// switch inside a service.
package engine

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

func (c CapabilitySet) Map() map[string]bool {
	return map[string]bool{
		"sessions": c.Sessions, "lock_tree": c.LockTree, "workload": c.Workload,
		"query_plans": c.QueryPlans, "replication": c.Replication,
		"backup_status": c.BackupStatus, "storage": c.Storage,
		"user_management": c.UserManagement, "maintenance": c.Maintenance,
		"multitenant": c.Multitenant, "rac": c.RAC,
	}
}
