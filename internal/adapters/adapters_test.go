package adapters

import (
	"context"
	"testing"
	"time"
)

func TestRegistry(t *testing.T) {
	registry := NewRegistry()
	adapter := Mock{
		AdapterInfo:   Info{ID: "test", Name: "Test Adapter", Version: "1.0.0", Status: StatusReady, Capabilities: []string{"items.read"}},
		AdapterHealth: Health{Status: StatusReady},
	}
	if err := registry.Register(adapter); err != nil {
		t.Fatalf("register adapter: %v", err)
	}
	if err := registry.Register(adapter); err == nil {
		t.Fatal("expected duplicate adapter registration to fail")
	}
	items := registry.List()
	if len(items) != 1 || items[0].ID != "test" {
		t.Fatalf("unexpected registry contents: %#v", items)
	}
	health := registry.Health(context.Background())
	if health["test"].Status != StatusReady {
		t.Fatalf("unexpected health result: %#v", health)
	}
}

// slowAdapter blocks until its release channel is closed, so a test can prove
// probes overlap rather than queue.
type slowAdapter struct {
	Mock
	started chan struct{}
	release chan struct{}
}

func (s *slowAdapter) Health(ctx context.Context) Health {
	close(s.started)
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	return Health{Status: StatusReady}
}

// Probes must overlap. Sequentially a readiness check costs the sum of every
// adapter's timeout, so three slow upstreams make a health endpoint that
// times out in a load balancer exactly when something is wrong.
func TestHealthProbesRunConcurrently(t *testing.T) {
	registry := NewRegistry()
	slow := make([]*slowAdapter, 0, 3)
	for i, id := range []string{"one", "two", "three"} {
		adapter := &slowAdapter{
			Mock:    Mock{AdapterInfo: Info{ID: id, Name: id, Status: StatusReady}},
			started: make(chan struct{}),
			release: make(chan struct{}),
		}
		_ = i
		if err := registry.Register(adapter); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
		slow = append(slow, adapter)
	}

	done := make(chan map[string]Health, 1)
	go func() { done <- registry.Health(context.Background()) }()

	// Every probe must have started before any is allowed to finish. If they
	// ran in sequence, the second would never start.
	for _, adapter := range slow {
		select {
		case <-adapter.started:
		case <-time.After(2 * time.Second):
			t.Fatal("a health probe never started; probes are running in sequence")
		}
	}
	for _, adapter := range slow {
		close(adapter.release)
	}

	select {
	case result := <-done:
		if len(result) != 3 {
			t.Errorf("health returned %d adapters, want 3", len(result))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Health did not return")
	}
}

// Holding the registry lock across a network call would block every other
// reader for as long as an unreachable upstream's timeout.
func TestHealthDoesNotHoldTheRegistryLock(t *testing.T) {
	registry := NewRegistry()
	blocker := &slowAdapter{
		Mock:    Mock{AdapterInfo: Info{ID: "slow", Name: "slow", Status: StatusReady}},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	if err := registry.Register(blocker); err != nil {
		t.Fatalf("register: %v", err)
	}

	go registry.Health(context.Background())
	<-blocker.started

	listed := make(chan int, 1)
	go func() { listed <- len(registry.List()) }()

	select {
	case count := <-listed:
		if count != 1 {
			t.Errorf("List returned %d adapters", count)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("List blocked while a health probe was in flight")
	}
	close(blocker.release)
}
