package adapters

import (
	"context"
	"errors"
	"sort"
	"sync"
)

var ErrNotSupported = errors.New("adapter capability is not supported")

// ErrNotFound means the upstream system has no such record. Adapters return
// it instead of a generic failure so that a detail endpoint can answer 404
// rather than reporting the upstream as unavailable.
var ErrNotFound = errors.New("upstream resource not found")

type Status string

const (
	StatusReady    Status = "ready"
	StatusDegraded Status = "degraded"
	StatusDisabled Status = "disabled"
)

type Info struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Status       Status   `json:"status"`
	Capabilities []string `json:"capabilities"`
}

type Health struct {
	Status  Status `json:"status"`
	Message string `json:"message,omitempty"`
}

type Adapter interface {
	Info() Info
	Health(context.Context) Health
}

type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

func NewRegistry() *Registry {
	return &Registry{adapters: make(map[string]Adapter)}
}

func (r *Registry) Register(adapter Adapter) error {
	info := adapter.Info()
	if info.ID == "" {
		return errors.New("adapter id is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.adapters[info.ID]; exists {
		return errors.New("adapter already registered: " + info.ID)
	}
	r.adapters[info.ID] = adapter
	return nil
}

func (r *Registry) Get(id string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	adapter, ok := r.adapters[id]
	return adapter, ok
}

func (r *Registry) List() []Info {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Info, 0, len(r.adapters))
	for _, adapter := range r.adapters {
		result = append(result, adapter.Info())
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (r *Registry) Health(ctx context.Context) map[string]Health {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make(map[string]Health, len(r.adapters))
	for id, adapter := range r.adapters {
		result[id] = adapter.Health(ctx)
	}
	return result
}
