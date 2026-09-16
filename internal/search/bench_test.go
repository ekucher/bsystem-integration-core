package search

import (
	"context"
	"fmt"
	"testing"
)

// The in-memory provider scans. That is a deliberate choice for a provider
// whose job is to make the authorization boundary testable, but the cost of
// the scan is what says when a deployment has outgrown it — so it is measured
// rather than assumed.
func benchmarkIndex(b *testing.B, size int) *Memory {
	b.Helper()
	provider := NewMemory()
	documents := make([]Document, 0, size)
	for i := 0; i < size; i++ {
		documents = append(documents, Document{
			ID: fmt.Sprintf("CL-%06d", i), Type: TypeClient,
			Title:       fmt.Sprintf("Northwind Trading %d", i),
			Summary:     "Key account in the Kyiv region",
			Source:      "espocrm",
			Permissions: []string{"crm.client.read"},
			ScopeType:   "client", ScopeID: fmt.Sprintf("CL-%06d", i),
		})
	}
	if err := provider.Index(context.Background(), documents...); err != nil {
		b.Fatal(err)
	}
	return provider
}

func BenchmarkMemorySearch1k(b *testing.B) {
	provider := benchmarkIndex(b, 1000)
	query := Query{Text: "northwind", AllPermissions: true, Limit: DefaultLimit}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := provider.Search(context.Background(), query); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMemorySearch10k(b *testing.B) {
	provider := benchmarkIndex(b, 10000)
	query := Query{Text: "northwind", AllPermissions: true, Limit: DefaultLimit}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := provider.Search(context.Background(), query); err != nil {
			b.Fatal(err)
		}
	}
}

// Cursors are encoded and decoded on every paged request.
func BenchmarkCursorRoundTrip(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := DecodeCursor(EncodeCursor(420)); err != nil {
			b.Fatal(err)
		}
	}
}
