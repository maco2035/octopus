package agents

import (
	"fmt"
	"sync"

	"octopus/internal/domain"
)

// Factory builds one Agent instance from a node's config. The engine
// injects a "node_id" key into cfg before calling this, so an agent can
// know which node it's executing as without the registry interface itself
// needing to carry that plumbing.
type Factory func(cfg map[string]any) (domain.Agent, error)

// Registry maps an agent_type string to its constructor. The web UI (Phase
// 4) queries ListTypes to populate the drag-and-drop palette; the engine
// uses Create to instantiate nodes at run time.
type Registry struct {
	mu    sync.RWMutex
	types map[string]Factory
}

func NewRegistry() *Registry {
	return &Registry{types: make(map[string]Factory)}
}

func (r *Registry) Register(agentType string, factory Factory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.types[agentType] = factory
}

func (r *Registry) Create(agentType string, cfg map[string]any) (domain.Agent, error) {
	r.mu.RLock()
	factory, ok := r.types[agentType]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown agent type %q", agentType)
	}
	return factory(cfg)
}

func (r *Registry) ListTypes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	types := make([]string, 0, len(r.types))
	for t := range r.types {
		types = append(types, t)
	}
	return types
}
