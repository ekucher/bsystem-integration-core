package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// openAPIDocument is the subset of docs/openapi.yaml the contract test reads.
type openAPIDocument struct {
	OpenAPI  string                                `yaml:"openapi"`
	Security []map[string][]string                 `yaml:"security"`
	Paths    map[string]map[string]openAPIOperator `yaml:"paths"`
}

type openAPIOperator struct {
	OperationID string                 `yaml:"operationId"`
	Summary     string                 `yaml:"summary"`
	Tags        []string               `yaml:"tags"`
	Security    *[]map[string][]string `yaml:"security"`
	Responses   map[string]yaml.Node   `yaml:"responses"`
}

var httpMethods = map[string]string{
	"get": "GET", "post": "POST", "put": "PUT", "patch": "PATCH", "delete": "DELETE", "head": "HEAD", "options": "OPTIONS",
}

func loadOpenAPI(t *testing.T) openAPIDocument {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "docs", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read docs/openapi.yaml: %v", err)
	}
	var document openAPIDocument
	if err := yaml.Unmarshal(body, &document); err != nil {
		t.Fatalf("parse docs/openapi.yaml: %v", err)
	}
	if document.OpenAPI == "" || len(document.Paths) == 0 {
		t.Fatal("docs/openapi.yaml has no version or no paths")
	}
	return document
}

// documentedOperations returns every documented operation keyed by the same
// "METHOD /path" pattern the router uses.
func documentedOperations(t *testing.T, document openAPIDocument) map[string]openAPIOperator {
	t.Helper()
	operations := map[string]openAPIOperator{}
	for path, item := range document.Paths {
		for method, operation := range item {
			upper, ok := httpMethods[strings.ToLower(method)]
			if !ok {
				continue
			}
			operations[upper+" "+path] = operation
		}
	}
	return operations
}

// The documented contract and the served routes must be the same set. An
// endpoint that ships undocumented is invisible to the HUB and to any
// consumer; a documented endpoint that does not exist is a promise the
// platform does not keep.
func TestOpenAPICoversExactlyTheServedRoutes(t *testing.T) {
	operations := documentedOperations(t, loadOpenAPI(t))

	served := map[string]bool{}
	for _, pattern := range routePatterns() {
		served[pattern] = true
	}

	var undocumented, unimplemented []string
	for pattern := range served {
		if _, ok := operations[pattern]; !ok {
			undocumented = append(undocumented, pattern)
		}
	}
	for pattern := range operations {
		if !served[pattern] {
			unimplemented = append(unimplemented, pattern)
		}
	}
	sort.Strings(undocumented)
	sort.Strings(unimplemented)

	if len(undocumented) > 0 {
		t.Errorf("served but missing from docs/openapi.yaml:\n  %s", strings.Join(undocumented, "\n  "))
	}
	if len(unimplemented) > 0 {
		t.Errorf("documented in docs/openapi.yaml but not served:\n  %s", strings.Join(unimplemented, "\n  "))
	}
}

// The authentication boundary a route declares in code must be the one the
// contract advertises, so a reader of the API documentation cannot conclude
// an endpoint is public when it is not, or the reverse.
func TestOpenAPISecurityMatchesTheDeclaredBoundary(t *testing.T) {
	document := loadOpenAPI(t)
	operations := documentedOperations(t, document)

	// The document-level default must be the human scheme, so that an
	// operation which says nothing is documented as user-authenticated.
	if len(document.Security) != 1 {
		t.Fatalf("document-level security = %v, want exactly one scheme", document.Security)
	}
	if _, ok := document.Security[0]["bearerAuth"]; !ok {
		t.Fatalf("document-level security = %v, want bearerAuth", document.Security[0])
	}

	for _, route := range routes() {
		t.Run(route.Pattern(), func(t *testing.T) {
			operation, ok := operations[route.Pattern()]
			if !ok {
				t.Skip("covered by TestOpenAPICoversExactlyTheServedRoutes")
			}
			switch route.Auth {
			case authNone:
				if operation.Security == nil || len(*operation.Security) != 0 {
					t.Fatalf("unauthenticated route must document `security: []`, got %v", operation.Security)
				}
			case authHuman:
				if operation.Security != nil {
					t.Fatalf("human route must inherit the document-level bearerAuth, got %v", *operation.Security)
				}
			case authService:
				if operation.Security == nil || len(*operation.Security) != 1 {
					t.Fatalf("service route must document exactly one scheme, got %v", operation.Security)
				}
				if _, ok := (*operation.Security)[0]["serviceAuth"]; !ok {
					t.Fatalf("service route must document serviceAuth, got %v", (*operation.Security)[0])
				}
			default:
				t.Fatalf("route declares an unknown authentication boundary %q", route.Auth)
			}
		})
	}
}

// Every operation needs the metadata a generated client and a human reader
// both depend on.
func TestOpenAPIOperationsAreFullyDescribed(t *testing.T) {
	operations := documentedOperations(t, loadOpenAPI(t))
	seenIDs := map[string]string{}

	patterns := make([]string, 0, len(operations))
	for pattern := range operations {
		patterns = append(patterns, pattern)
	}
	sort.Strings(patterns)

	for _, pattern := range patterns {
		operation := operations[pattern]
		t.Run(pattern, func(t *testing.T) {
			if operation.OperationID == "" {
				t.Error("operationId is required")
			} else if previous, duplicate := seenIDs[operation.OperationID]; duplicate {
				t.Errorf("operationId %q is already used by %s", operation.OperationID, previous)
			} else {
				seenIDs[operation.OperationID] = pattern
			}
			if operation.Summary == "" {
				t.Error("summary is required")
			}
			if len(operation.Tags) == 0 {
				t.Error("at least one tag is required")
			}
			if len(operation.Responses) == 0 {
				t.Fatal("at least one response is required")
			}
			hasSuccess := false
			for status := range operation.Responses {
				if strings.HasPrefix(status, "2") {
					hasSuccess = true
				}
			}
			if !hasSuccess {
				t.Error("a 2xx response is required")
			}
		})
	}
}

// Anything reachable without a token must be an operational endpoint. This
// keeps an unauthenticated business endpoint from being introduced quietly.
func TestOnlyOperationalRoutesAreUnauthenticated(t *testing.T) {
	allowed := map[string]bool{"GET /health": true, "GET /readyz": true, "GET /metrics": true}
	for _, route := range routes() {
		if route.Auth != authNone {
			continue
		}
		if !allowed[route.Pattern()] {
			t.Errorf("%s is served without authentication; only operational endpoints may be", route.Pattern())
		}
	}
}

// Every business and administrative route must sit behind the human boundary,
// and every machine route behind the service boundary. Mixing the two would
// let a human token drive the machine API or the reverse.
func TestAPISurfacesStaySeparated(t *testing.T) {
	for _, route := range routes() {
		t.Run(route.Pattern(), func(t *testing.T) {
			switch {
			case strings.HasPrefix(route.Path, "/api/service/v1/"):
				if route.Auth != authService {
					t.Fatalf("machine API route declares %q, want %q", route.Auth, authService)
				}
			case strings.HasPrefix(route.Path, "/api/v1/"):
				if route.Auth != authHuman {
					t.Fatalf("human API route declares %q, want %q", route.Auth, authHuman)
				}
			default:
				if route.Auth != authNone {
					t.Fatalf("operational route declares %q, want %q", route.Auth, authNone)
				}
			}
		})
	}
}

// Route patterns must be unique: net/http panics on a duplicate registration,
// which would only be discovered at startup.
func TestRoutePatternsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, route := range routes() {
		if seen[route.Pattern()] {
			t.Errorf("duplicate route pattern %q", route.Pattern())
		}
		seen[route.Pattern()] = true
	}
}

// A route that requires a permission must document the denial it can produce,
// and every authenticated route must document the rejection of a bad token.
// Undocumented failure modes are the ones a client never handles.
func TestOpenAPIDocumentsTheFailuresEachRouteCanProduce(t *testing.T) {
	operations := documentedOperations(t, loadOpenAPI(t))
	for _, route := range routes() {
		operation, ok := operations[route.Pattern()]
		if !ok || route.Auth == authNone {
			continue
		}
		t.Run(route.Pattern(), func(t *testing.T) {
			if _, documented := operation.Responses["401"]; !documented {
				t.Error("an authenticated route must document 401")
			}
			if route.Permission == "" {
				return
			}
			if _, documented := operation.Responses["403"]; !documented {
				t.Errorf("a route requiring %q must document 403", route.Permission)
			}
		})
	}
}
