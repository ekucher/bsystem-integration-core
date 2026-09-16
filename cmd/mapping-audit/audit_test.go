package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func prefixes() map[string]string {
	return map[string]string{
		"client":  "CL",
		"project": "PR",
		"server":  "SRV",
		"task":    "TSK",
	}
}

func kinds(findings []Finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.Kind)
	}
	return out
}

func TestCleanMappingsProduceNoFindings(t *testing.T) {
	in := Input{
		Prefixes: prefixes(),
		Mappings: []Mapping{
			{GlobalID: "CL-1", EntityType: "client", Source: "espocrm", SourceID: "a1"},
			{GlobalID: "PR-1", EntityType: "project", Source: "redmine", SourceID: "7"},
		},
		Relations: []Relation{
			{Holder: "SRV-1", Field: "servers.client_id", Target: "CL-1"},
			{Holder: "SRV-1", Field: "servers.project_id", Target: ""},
		},
	}
	if findings := Audit(in); len(findings) != 0 {
		t.Fatalf("expected no findings, got %v", kinds(findings))
	}
}

func TestInvalidPrefixIsAnError(t *testing.T) {
	in := Input{
		Prefixes: prefixes(),
		Mappings: []Mapping{{GlobalID: "PR-9", EntityType: "client", Source: "espocrm", SourceID: "a1"}},
	}
	findings := Audit(in)
	if len(findings) != 1 || findings[0].Kind != "invalid_global_id_prefix" {
		t.Fatalf("expected one prefix finding, got %v", kinds(findings))
	}
	if findings[0].Severity != SeverityError {
		t.Fatalf("a wrong prefix must be an error, got %q", findings[0].Severity)
	}
}

// A prefix check that accepted "CL" as a prefix of "CLIENT-1" would pass every
// realistic fixture while still being wrong, so the separator is asserted.
func TestPrefixMatchRequiresTheSeparator(t *testing.T) {
	in := Input{
		Prefixes: map[string]string{"client": "CL"},
		Mappings: []Mapping{{GlobalID: "CLX-1", EntityType: "client", Source: "espocrm", SourceID: "a1"}},
	}
	if findings := Audit(in); len(findings) != 1 {
		t.Fatalf("CLX-1 must not satisfy the CL prefix, got %v", kinds(findings))
	}
}

func TestUnregisteredEntityTypeIsAnError(t *testing.T) {
	in := Input{
		Prefixes: prefixes(),
		Mappings: []Mapping{{GlobalID: "ZZ-1", EntityType: "sasquatch", Source: "espocrm", SourceID: "a1"}},
	}
	findings := Audit(in)
	if len(findings) != 1 || findings[0].Kind != "unregistered_entity_type" {
		t.Fatalf("expected an unregistered type finding, got %v", kinds(findings))
	}
}

func TestOneUpstreamRecordMappedTwiceIsReported(t *testing.T) {
	in := Input{
		Prefixes: prefixes(),
		Mappings: []Mapping{
			{GlobalID: "PR-1", EntityType: "project", Source: "redmine", SourceID: "7"},
			{GlobalID: "TSK-1", EntityType: "task", Source: "redmine", SourceID: "7"},
		},
	}
	findings := Audit(in)
	if len(findings) != 1 || findings[0].Kind != "ambiguous_source_mapping" {
		t.Fatalf("expected one ambiguity finding, got %v", kinds(findings))
	}
	if got := findings[0].EntityType; got != "project,task" {
		t.Fatalf("both types must be named, got %q", got)
	}
	if got := findings[0].Related; got != "TSK-1" {
		t.Fatalf("the other Global ID must be named, got %q", got)
	}
}

// The same upstream record legitimately appears once per source. Grouping by
// source_id alone would report every Redmine id that happens to equal an
// EspoCRM id — noise that would train a reader to ignore the tool.
func TestSameSourceIDInDifferentSourcesIsNotAmbiguous(t *testing.T) {
	in := Input{
		Prefixes: prefixes(),
		Mappings: []Mapping{
			{GlobalID: "CL-1", EntityType: "client", Source: "espocrm", SourceID: "7"},
			{GlobalID: "PR-1", EntityType: "project", Source: "redmine", SourceID: "7"},
		},
	}
	if findings := Audit(in); len(findings) != 0 {
		t.Fatalf("expected no findings, got %v", kinds(findings))
	}
}

func TestDanglingReferenceIsAnError(t *testing.T) {
	in := Input{
		Prefixes:  prefixes(),
		Mappings:  []Mapping{{GlobalID: "SRV-1", EntityType: "server", Source: "manual", SourceID: "s1"}},
		Relations: []Relation{{Holder: "SRV-1", Field: "servers.client_id", Target: "CL-404"}},
	}
	findings := Audit(in)
	if len(findings) != 1 || findings[0].Kind != "missing_referenced_mapping" {
		t.Fatalf("expected a dangling reference finding, got %v", kinds(findings))
	}
	if !strings.Contains(findings[0].Detail, "servers.client_id") {
		t.Fatalf("the detail must name the field, got %q", findings[0].Detail)
	}
}

func TestEmptyRelationIsNotDangling(t *testing.T) {
	in := Input{
		Prefixes:  prefixes(),
		Mappings:  []Mapping{{GlobalID: "SRV-1", EntityType: "server", Source: "manual", SourceID: "s1"}},
		Relations: []Relation{{Holder: "SRV-1", Field: "servers.project_id", Target: ""}},
	}
	if findings := Audit(in); len(findings) != 0 {
		t.Fatalf("an unset relation is not a dangling one, got %v", kinds(findings))
	}
}

func TestMissingOwnershipIsAWarningNotAnError(t *testing.T) {
	in := Input{
		Prefixes:         prefixes(),
		OwnerlessHolders: []Relation{{Holder: "INC-1", Field: "support_records.client_id"}},
	}
	findings := Audit(in)
	if len(findings) != 1 || findings[0].Kind != "missing_ownership" {
		t.Fatalf("expected an ownership finding, got %v", kinds(findings))
	}
	// Unknown ownership denies rather than leaks, so it must not be reported
	// with the same weight as a broken reference.
	if findings[0].Severity != SeverityWarning {
		t.Fatalf("missing ownership is a warning, got %q", findings[0].Severity)
	}
}

func TestErrorsSortBeforeWarnings(t *testing.T) {
	in := Input{
		Prefixes:         prefixes(),
		OwnerlessHolders: []Relation{{Holder: "INC-1", Field: "support_records.client_id"}},
		Mappings:         []Mapping{{GlobalID: "PR-9", EntityType: "client", Source: "espocrm", SourceID: "a1"}},
	}
	findings := Audit(in)
	if len(findings) != 2 {
		t.Fatalf("expected two findings, got %v", kinds(findings))
	}
	if findings[0].Severity != SeverityError {
		t.Fatalf("errors must come first, got %v", kinds(findings))
	}
}

func TestSummarizeCountsBySeverity(t *testing.T) {
	got := Summarize([]Finding{
		{Severity: SeverityError},
		{Severity: SeverityWarning},
		{Severity: SeverityWarning},
	})
	if want := (Summary{Errors: 1, Warnings: 2}); !reflect.DeepEqual(got, want) {
		t.Fatalf("summary = %+v, want %+v", got, want)
	}
}

// The value of this tool depends on it being safe to paste its output into a
// ticket. A Finding has no field that can carry a document title, a customer
// name or an upstream payload, and this test fails if one is ever added.
func TestFindingCarriesNoFreeFormUpstreamContent(t *testing.T) {
	allowed := map[string]struct{}{
		"kind": {}, "severity": {}, "global_id": {}, "entity_type": {},
		"source": {}, "source_id": {}, "related": {}, "detail": {},
	}
	encoded, err := json.Marshal(Finding{Kind: "k", Severity: SeverityError, GlobalID: "CL-1", EntityType: "client", Source: "s", SourceID: "1", Related: "r", Detail: "d"})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for name := range fields {
		if _, ok := allowed[name]; !ok {
			t.Fatalf("Finding gained field %q; confirm it cannot carry upstream content before allowing it", name)
		}
	}
}

// Details are fixed strings. If one ever interpolated a value, this is where
// an upstream name would start leaking into output that people paste around.
func TestDetailsDoNotInterpolateMappingValues(t *testing.T) {
	secretish := "Acme Holdings Confidential"
	in := Input{
		Prefixes: map[string]string{"client": "CL"},
		Mappings: []Mapping{{GlobalID: "XX-1", EntityType: "client", Source: secretish, SourceID: secretish}},
	}
	for _, f := range Audit(in) {
		if strings.Contains(f.Detail, secretish) {
			t.Fatalf("detail interpolated a mapping value: %q", f.Detail)
		}
	}
}

func TestRedactDSNRemovesTheConnectionString(t *testing.T) {
	dsn := "postgres://bsystem:hunter2@db:5432/bsystem"
	got := redactDSN("failed to connect to "+dsn+": timeout", dsn)
	if strings.Contains(got, "hunter2") {
		t.Fatalf("the password survived redaction: %q", got)
	}
	if !strings.Contains(got, "[DATABASE_URL]") {
		t.Fatalf("expected a placeholder, got %q", got)
	}
}

func TestTextRendersASummaryEvenWithNoFindings(t *testing.T) {
	out := Text(nil, Summary{})
	if !strings.Contains(out, "0 error(s)") || !strings.Contains(out, "no findings") {
		t.Fatalf("unexpected output: %q", out)
	}
}
