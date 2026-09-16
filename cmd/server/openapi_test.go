package main

import (
	"os"
	"path/filepath"
	"regexp"
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

// errorCodeDocument reads only the Error schema's code enum.
type errorCodeDocument struct {
	Components struct {
		Schemas struct {
			Error struct {
				Properties struct {
					Code struct {
						Enum []string `yaml:"enum"`
					} `yaml:"code"`
				} `yaml:"properties"`
			} `yaml:"Error"`
		} `yaml:"schemas"`
	} `yaml:"components"`
}

// Every machine-readable code the server emits must be in the contract's
// enum.
//
// A caller is told to branch on `code` rather than on the human-readable
// summary. An undocumented code makes that instruction false: the caller
// writes a switch from the contract, the platform returns something outside
// it, and the default branch handles a failure it has no idea about. Spectral
// catches the case where an example uses an undocumented code; this catches
// the case where no example happens to use it.
func TestEveryEmittedErrorCodeIsDocumented(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "docs", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read docs/openapi.yaml: %v", err)
	}
	var document errorCodeDocument
	if err := yaml.Unmarshal(body, &document); err != nil {
		t.Fatalf("parse docs/openapi.yaml: %v", err)
	}
	documented := map[string]bool{}
	for _, code := range document.Components.Schemas.Error.Properties.Code.Enum {
		documented[code] = true
	}
	if len(documented) == 0 {
		t.Fatal("the Error schema documents no codes at all")
	}

	// The codes are read out of the handlers rather than listed here, so a
	// new one cannot be added without either documenting it or failing.
	emitted := map[string]string{}
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list handler sources: %v", err)
	}
	pattern := regexp.MustCompile(`"code":\s*"([a-z_]+)"`)
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		content, err := os.ReadFile(source)
		if err != nil {
			t.Fatalf("read %s: %v", source, err)
		}
		for _, match := range pattern.FindAllStringSubmatch(string(content), -1) {
			emitted[match[1]] = source
		}
	}
	if len(emitted) == 0 {
		t.Fatal("no error codes were found in the handlers; the scan is not working")
	}

	undocumented := make([]string, 0)
	for code, source := range emitted {
		if !documented[code] {
			undocumented = append(undocumented, code+" (in "+source+")")
		}
	}
	sort.Strings(undocumented)
	if len(undocumented) > 0 {
		t.Errorf("emitted but missing from the Error schema's enum:\n  %s", strings.Join(undocumented, "\n  "))
	}
}

// acceptanceEndpoints are the endpoints a stage acceptance walks through. They
// are the ones an owner will exercise by hand against a real deployment, so a
// missing example here costs someone an afternoon of guessing request shapes.
var acceptanceEndpoints = []string{
	"GET /api/v1/me",
	"GET /api/v1/clients", "GET /api/v1/clients/{id}",
	"GET /api/v1/contacts", "GET /api/v1/contacts/{id}",
	"GET /api/v1/projects", "GET /api/v1/projects/{id}",
	"GET /api/v1/issues", "GET /api/v1/issues/{id}",
	"GET /api/v1/documents", "GET /api/v1/documents/{id}",
	"GET /api/v1/notifications",
	"GET /api/v1/search",
	"GET /api/v1/servers", "GET /api/v1/servers/{id}",
	"GET /api/v1/incidents", "POST /api/v1/incidents",
	"GET /api/v1/incidents/{id}", "PATCH /api/v1/incidents/{id}",
}

// TestAcceptanceEndpointsDocumentASuccessExample fails when an endpoint on the
// acceptance path describes a success shape without showing one.
func TestAcceptanceEndpointsDocumentASuccessExample(t *testing.T) {
	operations := documentedOperations(t, loadOpenAPI(t))

	for _, key := range acceptanceEndpoints {
		operation, ok := operations[key]
		if !ok {
			t.Errorf("%s is on the acceptance path but is not documented", key)
			continue
		}
		var success *yaml.Node
		for status := range operation.Responses {
			if strings.HasPrefix(status, "2") {
				node := operation.Responses[status]
				success = &node
				break
			}
		}
		if success == nil {
			t.Errorf("%s documents no 2xx response", key)
			continue
		}
		rendered, err := yaml.Marshal(success)
		if err != nil {
			t.Fatalf("%s: re-encode response: %v", key, err)
		}
		if !strings.Contains(string(rendered), "example") {
			t.Errorf("%s documents a 2xx response with no example", key)
		}
	}
}

// TestAcceptanceEndpointsDocumentTheirRejections checks the failures an
// acceptance actually produces.
//
// 401 applies everywhere: every one of these endpoints is authenticated.
// 403 applies only where a permission or a scope can reject the caller, and
// 404 only where a single resource is addressed. Demanding all three
// everywhere would push the document to describe rejections the platform never
// returns, which is worse than silence — a reader would design for them.
func TestAcceptanceEndpointsDocumentTheirRejections(t *testing.T) {
	operations := documentedOperations(t, loadOpenAPI(t))
	routesByKey := map[string]route{}
	for _, r := range routes() {
		routesByKey[r.Method+" "+r.Path] = r
	}

	for _, key := range acceptanceEndpoints {
		operation, ok := operations[key]
		if !ok {
			continue // reported by the test above
		}
		r, known := routesByKey[key]
		if !known {
			t.Errorf("%s is documented but not served", key)
			continue
		}

		if _, ok := operation.Responses["401"]; !ok {
			t.Errorf("%s is authenticated but documents no 401", key)
		}

		// A route that names a permission, or confines a resource to a scope,
		// can answer 403. One that names neither cannot.
		canForbid := r.Permission != "" || r.ResourceScope != ""
		_, has403 := operation.Responses["403"]
		if canForbid && !has403 {
			t.Errorf("%s requires %q but documents no 403", key, r.Permission)
		}
		if !canForbid && has403 {
			t.Errorf("%s documents a 403 it cannot return: it requires no permission and confines no resource", key)
		}

		// Only addressed resources can be absent.
		addressesOne := strings.Contains(key, "{id}")
		_, has404 := operation.Responses["404"]
		if addressesOne && !has404 {
			t.Errorf("%s addresses one resource but documents no 404", key)
		}
		if !addressesOne && has404 {
			t.Errorf("%s documents a 404 for a collection", key)
		}
	}
}

// Examples are published. A real customer name, address or document title in
// one is a disclosure that survives in every generated client and every copy
// of the documentation.
func TestExamplesUseReservedPlaceholderDomains(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "docs", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read docs/openapi.yaml: %v", err)
	}

	// RFC 2606 and RFC 6761 reserve these for documentation. Anything else is
	// either a real domain or one that could become real.
	allowed := regexp.MustCompile(`(?i)\.(example|invalid|test|localhost)\b`)
	host := regexp.MustCompile(`(?i)https?://([a-z0-9.-]+)`)

	for _, match := range host.FindAllStringSubmatch(string(body), -1) {
		candidate := match[1]
		switch {
		case strings.HasPrefix(candidate, "localhost"), strings.HasPrefix(candidate, "127.0.0.1"):
			continue
		// The document's own normative references are real URLs by necessity.
		case strings.Contains(candidate, "spec.openapis.org"),
			strings.Contains(candidate, "opensource.org"),
			strings.Contains(candidate, "github.com"),
			strings.Contains(candidate, "tools.ietf.org"),
			strings.Contains(candidate, "www.rfc-editor.org"):
			continue
		}
		if !allowed.MatchString(candidate) {
			t.Errorf("example host %q is not a reserved documentation domain", candidate)
		}
	}

	for _, banned := range []string{"@gmail.com", "@outlook.com", "@yahoo.com"} {
		if strings.Contains(strings.ToLower(string(body)), banned) {
			t.Errorf("an example uses a real mail provider (%s)", banned)
		}
	}
}
