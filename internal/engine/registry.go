package engine

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Adapter declares an engine and its capability set. Role-provider
// implementations live in internal/engine/<engine> packages and are bound to
// the consuming services by internal/engine/adapters.
type Adapter struct {
	Name         string
	Capabilities CapabilitySet
}

// Registry centralizes adapter lookup. It rejects duplicate names so a build
// cannot silently replace a production engine implementation.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

func NewRegistry() *Registry { return &Registry{adapters: make(map[string]Adapter)} }

// NewDefaultRegistry is the product-level capability declaration for every
// supported engine. Service packages consume capabilities from this registry
// and never switch on postgres/mysql/mariadb/oracle themselves. The matching
// role-provider implementations are wired by internal/engine/adapters.
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
