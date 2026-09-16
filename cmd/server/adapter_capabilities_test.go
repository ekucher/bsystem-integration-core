package main

import (
	"sort"
	"testing"

	"github.com/ekucher/bsystem-integration-core/internal/adapters"
	"github.com/ekucher/bsystem-integration-core/internal/adapters/espocrm"
	"github.com/ekucher/bsystem-integration-core/internal/adapters/outline"
	"github.com/ekucher/bsystem-integration-core/internal/adapters/redmine"
)

// An unconfigured integration is registered as a disabled placeholder so that
// /adapters describes the whole intended surface rather than silently omitting
// what is absent. That is only true if the placeholder describes the same
// surface the real adapter would.
//
// The capability list is written twice — once in the adapter, once beside
// disabledAdapter in service.go — and nothing made the two agree. A capability
// added to an adapter and not to its placeholder means the platform answers a
// different question depending on whether the integration happens to be
// configured: a caller reading /adapters on a deployment that does not run
// Outline is told the product cannot search documents, which is a statement
// about the deployment dressed as a statement about the product.
//
// The E2E stack cannot catch this, by construction: it configures every
// adapter, so it only ever sees the real lists.
func TestADisabledAdapterDescribesTheSameSurfaceAsTheRealOne(t *testing.T) {
	// A base URL is required for a constructor to succeed; nothing is
	// requested, so the value only has to be well formed.
	config := adapters.Config{BaseURL: "http://upstream.invalid", APIKey: "unused-in-this-test"}

	espo, err := espocrm.New(config)
	if err != nil {
		t.Fatalf("construct the EspoCRM adapter: %v", err)
	}
	red, err := redmine.New(config)
	if err != nil {
		t.Fatalf("construct the Redmine adapter: %v", err)
	}
	out, err := outline.New(config)
	if err != nil {
		t.Fatalf("construct the Outline adapter: %v", err)
	}

	tests := []struct {
		id          string
		name        string
		real        adapters.Info
		placeholder adapters.Info
	}{
		{id: "espocrm", name: "EspoCRM", real: espo.Info(),
			placeholder: disabledAdapter("espocrm", "EspoCRM", []string{"clients.read", "contacts.read"}).Info()},
		{id: "redmine", name: "Redmine", real: red.Info(),
			placeholder: disabledAdapter("redmine", "Redmine", []string{"projects.read", "issues.read"}).Info()},
		{id: "outline", name: "Outline", real: out.Info(),
			placeholder: disabledAdapter("outline", "Outline", []string{"documents.read", "documents.search"}).Info()},
	}

	// The placeholder lists above are copied from service.go on purpose. If
	// this test built them by calling the same helper with the same arguments
	// from one place, it would compare a value against itself and pass however
	// wrong both were.
	for _, test := range tests {
		t.Run(test.id, func(t *testing.T) {
			if len(test.real.Capabilities) == 0 {
				t.Fatalf("the real %s adapter declares no capability; the comparison below proves nothing", test.id)
			}
			real := append([]string(nil), test.real.Capabilities...)
			placeholder := append([]string(nil), test.placeholder.Capabilities...)
			sort.Strings(real)
			sort.Strings(placeholder)
			if len(real) != len(placeholder) {
				t.Fatalf("capabilities differ: the adapter declares %v, the disabled placeholder %v", real, placeholder)
			}
			for i := range real {
				if real[i] != placeholder[i] {
					t.Errorf("capabilities differ: the adapter declares %v, the disabled placeholder %v", real, placeholder)
					break
				}
			}
			if test.real.ID != test.placeholder.ID {
				t.Errorf("id differs: adapter %q, placeholder %q", test.real.ID, test.placeholder.ID)
			}
			if test.real.Name != test.placeholder.Name {
				t.Errorf("name differs: adapter %q, placeholder %q", test.real.Name, test.placeholder.Name)
			}
		})
	}
}
