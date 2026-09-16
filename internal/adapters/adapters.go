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

// Breakered is implemented by adapters that protect their upstream with a
// circuit breaker. It is optional so that an adapter without one — the
// disabled placeholder, for instance — needs no stub.
type Breakered interface {
	BreakerState() string
}

// BreakerStates returns the circuit state of every adapter that has one,
// keyed by adapter id, for health reporting and metrics.
func (r *Registry) BreakerStates() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := map[string]string{}
	for id, adapter := range r.adapters {
		if breakered, ok := adapter.(Breakered); ok {
			result[id] = breakered.BreakerState()
		}
	}
	return result
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

// healthConcurrency bounds how many upstreams are probed at once.
//
// The bound exists for when the adapter list grows, not for the three there
// are today: a readiness check that opened a connection to every upstream
// simultaneously would be a small thundering herd, arriving exactly when
// something is already struggling.
const healthConcurrency = 8

// Health probes every registered adapter.
//
// The probes run concurrently. Sequentially, a readiness check costs the sum
// of every adapter's timeout — three ten-second adapters make a thirty-second
// health endpoint, which times out in a load balancer precisely when
// something is wrong and the answer matters most.
//
// The lock is held only long enough to snapshot the registry. Holding it
// across a network call would mean one unreachable upstream blocks every
// other reader of the registry for as long as its timeout lasts.
func (r *Registry) Health(ctx context.Context) map[string]Health {
	r.mu.RLock()
	adapters := make(map[string]Adapter, len(r.adapters))
	for id, adapter := range r.adapters {
		adapters[id] = adapter
	}
	r.mu.RUnlock()

	result := make(map[string]Health, len(adapters))
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, healthConcurrency)

	for id, adapter := range adapters {
		wg.Add(1)
		go func(id string, adapter Adapter) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()

			health := adapter.Health(ctx)
			mu.Lock()
			result[id] = health
			mu.Unlock()
		}(id, adapter)
	}
	wg.Wait()
	return result
}
