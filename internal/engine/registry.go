package engine

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Adapter is an engine declaration and optional role-provider bundle.
type Adapter struct {
	Name         string
	Capabilities CapabilitySet
	Connector    Connector
	Metadata     MetadataProvider
	Sessions     SessionProvider
	Locks        LockProvider
	Dialect      DialectProvider
	QueryGuard   QueryGuard
	Workload     WorkloadProvider
	Explain      ExplainProvider
	Storage      StorageProvider
	Replication  ReplicationProvider
	Backup       BackupProvider
	Security     SecurityProvider
	Admin        AdminProvider
	Maintenance  MaintenanceProvider
}

// Registry centralizes adapter lookup. It rejects duplicate names so a build
// cannot silently replace a production engine implementation.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

func NewRegistry() *Registry { return &Registry{adapters: make(map[string]Adapter)} }

// NewDefaultRegistry is the only product-level engine-name declaration.
// Service packages consume capabilities from this registry and never switch
// on postgres/mysql/mariadb/oracle themselves.
func NewDefaultRegistry() *Registry {
	r := NewRegistry()
	declarations := []Adapter{
		{Name: "postgres", Capabilities: CapabilitySet{Sessions: true, LockTree: true, Workload: true, QueryPlans: true, Storage: true, UserManagement: true, Maintenance: true, Replication: true}},
		{Name: "mysql", Capabilities: CapabilitySet{Sessions: true, LockTree: true, Workload: true, QueryPlans: true, Storage: true, UserManagement: true, Maintenance: true, Replication: true}},
		{Name: "mariadb", Capabilities: CapabilitySet{Sessions: true, LockTree: true, Workload: true, QueryPlans: true, Storage: true, UserManagement: true, Maintenance: true, Replication: true}},
		{Name: "oracle", Capabilities: CapabilitySet{Sessions: true, LockTree: true, Workload: true, QueryPlans: true, Storage: true, UserManagement: true, Maintenance: true, Replication: true, Multitenant: true, RAC: true}},
	}
	for _, declaration := range declarations {
		if err := r.Register(declaration); err != nil {
			panic(err)
		}
	}
	return r
}

func (r *Registry) Register(a Adapter) error {
	name := strings.ToLower(strings.TrimSpace(a.Name))
	if name == "" {
		return fmt.Errorf("engine name is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.adapters[name]; exists {
		return fmt.Errorf("engine %q is already registered", name)
	}
	a.Name = name
	r.adapters[name] = a
	return nil
}

func (r *Registry) Get(name string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[strings.ToLower(strings.TrimSpace(name))]
	return a, ok
}

func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.adapters))
	for name := range r.adapters {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
