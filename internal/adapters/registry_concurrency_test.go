package adapters

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// blocking is an adapter whose health probe waits until it is released, so a
// test can hold the registry in the middle of a readiness check.
type blocking struct {
	id       string
	released chan struct{}
	probing  chan struct{}
	once     sync.Once
}

func (b *blocking) Info() Info {
	return Info{ID: b.id, Name: b.id, Version: "1", Status: StatusReady}
}

func (b *blocking) Health(ctx context.Context) Health {
	b.once.Do(func() { close(b.probing) })
	select {
	case <-b.released:
	case <-ctx.Done():
	}
	return Health{Status: StatusReady}
}

// Health says the lock is held only long enough to snapshot the registry, and
// says why: holding it across a network call would mean one unreachable
// upstream blocks every other reader for as long as its timeout lasts.
//
// The consequence is real but the mechanism is not quite what that sentence
// suggests, and the first version of this test was written to the sentence
// rather than to the lock. It asserted that Get and List stay responsive while
// a probe stalls — and they do even when the probe is held inside the lock,
// because it is a *read* lock and readers do not exclude each other. The test
// passed against the very simplification it was written to catch.
//
// What actually blocks is a writer. Register waits for the read lock to clear,
// and Go's RWMutex then makes every reader arriving after that writer queue
// behind it. So one stalled upstream would stall registration and, through it,
// every subsequent reader — /adapters, /readyz, and every handler that
// resolves an adapter. Registration is not only a startup activity: an adapter
// that fails to construct is replaced by a disabled placeholder, and readiness
// can be scraped while that happens.
//
// So the assertion is on Register, with the reads kept because a reader
// arriving after the blocked writer is how the stall spreads.
func TestAStalledHealthProbeDoesNotBlockTheRegistry(t *testing.T) {
	registry := NewRegistry()
	stalled := &blocking{id: "stalled", released: make(chan struct{}), probing: make(chan struct{})}
	if err := registry.Register(stalled); err != nil {
		t.Fatalf("register the stalled adapter: %v", err)
	}
	if err := registry.Register(Mock{AdapterInfo: Info{ID: "quick", Name: "Quick", Status: StatusReady}}); err != nil {
		t.Fatalf("register the quick adapter: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		registry.Health(ctx)
	}()

	// Wait until the probe is genuinely in flight. Without this the reads
	// below might run before Health ever took the lock, and the test would
	// pass without the situation it describes ever existing.
	select {
	case <-stalled.probing:
	case <-time.After(5 * time.Second):
		t.Fatal("the health probe never started")
	}

	registered := make(chan struct{})
	go func() {
		defer close(registered)
		if err := registry.Register(Mock{AdapterInfo: Info{
			ID: "arrived-late", Name: "Arrived Late", Status: StatusReady,
		}}); err != nil {
			t.Errorf("registering while a probe was in flight: %v", err)
		}
	}()

	select {
	case <-registered:
	case <-time.After(2 * time.Second):
		t.Fatal("registering an adapter blocked while one upstream's health probe was stalled; the lock is being held across the probe, and every reader arriving after this writer queues behind it")
	}

	read := make(chan struct{})
	go func() {
		defer close(read)
		if _, ok := registry.Get("quick"); !ok {
			t.Error("the quick adapter is registered but Get did not find it")
		}
		registry.List()
		registry.BreakerStates()
	}()

	select {
	case <-read:
	case <-time.After(2 * time.Second):
		t.Fatal("reading the registry blocked while a probe was stalled")
	}

	close(stalled.released)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the health probe did not finish after being released")
	}
}

// The plain data-race question, for the race detector: registration, reads and
// health reporting all happening at once.
//
// Registration is not only a startup activity — an adapter that fails to
// construct is replaced by a disabled placeholder, and the readiness endpoint
// can be scraped while that is happening.
func TestTheRegistryIsSafeUnderConcurrentUse(t *testing.T) {
	registry := NewRegistry()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const workers = 16
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for i := 0; i < workers; i++ {
		done.Add(1)
		go func(index int) {
			defer done.Done()
			start.Wait()
			switch index % 4 {
			case 0:
				_ = registry.Register(Mock{AdapterInfo: Info{
					ID: fmt.Sprintf("adapter-%02d", index), Name: "Adapter", Status: StatusReady,
				}})
			case 1:
				registry.Get(fmt.Sprintf("adapter-%02d", index-1))
			case 2:
				registry.List()
			default:
				registry.Health(ctx)
				registry.BreakerStates()
			}
		}(i)
	}
	start.Done()
	done.Wait()

	// Every registration attempted a distinct id, so each one must be present.
	for i := 0; i < workers; i += 4 {
		if _, ok := registry.Get(fmt.Sprintf("adapter-%02d", i)); !ok {
			t.Errorf("adapter-%02d registered during the concurrent run but is not in the registry", i)
		}
	}
}
