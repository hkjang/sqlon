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
}

// Registry centralizes adapter lookup. It rejects duplicate names so a build
// cannot silently replace a production engine implementation.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

func NewRegistry() *Registry { return &Registry{adapters: make(map[string]Adapter)} }

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
