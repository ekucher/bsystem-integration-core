package main

import (
	"testing"

	"github.com/ekucher/bsystem-integration-core/internal/ai"
	"github.com/ekucher/bsystem-integration-core/internal/authz"
)

// Every entity the gateway will accept as context must be authorized with
// exactly the permission its own read endpoint uses.
//
// This is the property the whole gateway rests on: a question must reach the
// same answer as the same person reading the record directly. A gateway that
// asked for a weaker permission would be the bypass the architecture forbids,
// and it would be invisible — the endpoint would work, and only the wrong
// people would be able to use it.
func TestEveryAISourceUsesItsOwnEndpointsPermission(t *testing.T) {
	routePermissions := map[string]string{}
	for _, route := range routes() {
		routePermissions[route.Pattern()] = route.Permission
	}

	cases := map[string]string{
		"client":   "GET /api/v1/clients/{id}",
		"contact":  "GET /api/v1/contacts/{id}",
		"project":  "GET /api/v1/projects/{id}",
		"task":     "GET /api/v1/issues/{id}",
		"document": "GET /api/v1/documents/{id}",
		"server":   "GET /api/v1/servers/{id}",
		"incident": "GET /api/v1/incidents/{id}",
	}
	if len(cases) != len(aiSourceRules) {
		t.Fatalf("the gateway accepts %d source types and this test names %d; a new one needs its endpoint named here",
			len(aiSourceRules), len(cases))
	}

	for entityType, pattern := range cases {
		t.Run(entityType, func(t *testing.T) {
			rule, known := aiSourceRules[entityType]
			if !known {
				t.Fatalf("%s is not an accepted source type", entityType)
			}
			endpoint, served := routePermissions[pattern]
			if !served {
				t.Fatalf("%s is not served; this test needs updating", pattern)
			}
			if rule.Permission != endpoint {
				t.Errorf("the gateway authorizes %s with %q while %s uses %q",
					entityType, rule.Permission, pattern, endpoint)
			}
			if rule.Permission == "" {
				t.Error("a source authorized with no permission would be readable by any authenticated caller")
			}
		})
	}
}

// Customer data must be classified as confidential wherever it enters a
// prompt. Classifying it as internal would let it be sent to a provider a
// policy meant to keep it away from.
func TestCustomerDataIsClassifiedConfidential(t *testing.T) {
	for _, entityType := range []string{"client", "contact"} {
		if got := aiSourceRules[entityType].Classification; got != ai.ClassConfidential {
			t.Errorf("%s is classified %q, want CONFIDENTIAL", entityType, got)
		}
	}
	// Nothing is classified CREDENTIAL, because such a source would always be
	// refused and having one would suggest it might not be.
	for entityType, rule := range aiSourceRules {
		if rule.Classification == ai.ClassCredential {
			t.Errorf("%s is classified CREDENTIAL; the gateway would always refuse it", entityType)
		}
		if rule.Classification == "" {
			t.Errorf("%s has no classification, so it would be treated as PUBLIC", entityType)
		}
	}
}

// A source is authorized in the scope its own endpoint uses, or a grant
// written for one would not be honoured by the other.
func TestAISourceScopesMatchTheirEndpoints(t *testing.T) {
	cases := map[string]string{
		"client":   authz.ScopeClient,
		"contact":  authz.ScopeResource,
		"project":  authz.ScopeProject,
		"task":     authz.ScopeResource,
		"document": authz.ScopeResource,
		"server":   authz.ScopeResource,
		"incident": authz.ScopeClient,
	}
	for entityType, want := range cases {
		if got := aiSourceRules[entityType].ScopeType; got != want {
			t.Errorf("%s is scoped %q, want %q", entityType, got, want)
		}
	}
}

// The gateway must be usable without a model configured, or its
// authorization, classification and audit behaviour would only be exercised
// in deployments that had chosen one.
func TestTheDefaultProviderIsTheFakeOne(t *testing.T) {
	t.Setenv("AI_PROVIDER", "")
	if provider := aiProviderFromEnv(); provider.Name() != "fake" {
		t.Errorf("the default provider is %q, want fake", provider.Name())
	}
	// A misconfigured provider must fall back rather than silently sending
	// prompts somewhere unintended.
	t.Setenv("AI_PROVIDER", "openai")
	t.Setenv("OPENAI_URL", "")
	t.Setenv("OPENAI_API_KEY", "")
	if provider := aiProviderFromEnv(); provider.Name() != "fake" {
		t.Errorf("a misconfigured OpenAI provider resolved to %q", provider.Name())
	}
}
