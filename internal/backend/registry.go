package backend

import (
	"log"
	"sync"
)

// Registry holds all registered backends and provides lookup.
type Registry struct {
	mu       sync.RWMutex
	backends map[string]Backend
}

// NewRegistry creates an empty backend registry.
func NewRegistry() *Registry {
	return &Registry{
		backends: make(map[string]Backend),
	}
}

// Register adds a backend to the registry.
func (r *Registry) Register(b Backend) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.backends[b.ID()] = b
}

// Get returns the backend with the given ID, or nil if not found.
func (r *Registry) Get(id string) Backend {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.backends[id]
}

// List returns info for all registered backends.
func (r *Registry) List() []BackendInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := make([]BackendInfo, 0, len(r.backends))
	for _, b := range r.backends {
		list = append(list, b.Info())
	}
	return list
}

// RefreshAll calls RefreshProviders on every registered backend.
func (r *Registry) RefreshAll() {
	r.mu.RLock()
	backends := make([]Backend, 0, len(r.backends))
	for _, b := range r.backends {
		backends = append(backends, b)
	}
	r.mu.RUnlock()

	for _, b := range backends {
		if err := b.RefreshProviders(); err != nil {
			log.Printf("[registry] failed to refresh providers for %s: %v", b.ID(), err)
		}
	}
}
