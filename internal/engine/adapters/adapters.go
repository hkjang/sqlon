// Package adapters is the single wiring point that binds engine names to
// their role-provider implementations. Adding engine support means creating
// an internal/engine/<engine> package and registering it here — services
// never switch on engine names themselves (they check engine.CapabilitySet
// and look implementations up through these maps).
package adapters

import (
	"sqlon/internal/collector"
	"sqlon/internal/engine/mariadb"
	"sqlon/internal/engine/mysql"
	"sqlon/internal/engine/oracle"
	"sqlon/internal/engine/postgres"
	"sqlon/internal/observability"
)

// CollectorProviders returns the workload/capacity implementation for every
// supported engine.
func CollectorProviders() map[string]collector.Provider {
	return map[string]collector.Provider{
		"postgres": postgres.Workload{},
		"mysql":    mysql.Workload{},
		"mariadb":  mariadb.Workload{},
		"oracle":   oracle.Workload{},
	}
}

// ObservabilityProviders returns the session/lock implementation for every
// supported engine.
func ObservabilityProviders() map[string]observability.Provider {
	return map[string]observability.Provider{
		"postgres": postgres.Observability{},
		"mysql":    mysql.Observability{},
		"mariadb":  mariadb.Observability{},
		"oracle":   oracle.Observability{},
	}
}
