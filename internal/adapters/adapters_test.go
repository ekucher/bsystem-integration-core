package adapters

import (
	"context"
	"testing"
)

func TestRegistry(t *testing.T) {
	registry := NewRegistry()
	adapter := Mock{
		AdapterInfo: Info{ID: "test", Name: "Test Adapter", Version: "1.0.0", Status: StatusReady, Capabilities: []string{"items.read"}},
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
