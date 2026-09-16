// Package main implements a read-only mapping health diagnostic.
//
// It answers one question before a stage acceptance: does the mapping table
// describe a world that makes sense? It never writes, and it never reaches out
// to a source system — every finding comes from BSYSTEM's own tables.
package main

import (
	"fmt"
	"sort"
	"strings"
)

// Severity orders findings by how much they block an acceptance.
type Severity string

const (
	// SeverityError marks a state the platform cannot be correct in.
	SeverityError Severity = "error"
	// SeverityWarning marks a state that is legal but probably unintended.
	SeverityWarning Severity = "warning"
)

// Finding is one problem, named by identifiers only.
//
// It deliberately has no field for a title, a name or any upstream payload:
// the type itself is the guarantee that running this tool cannot leak
// confidential content into a log, a ticket or a screenshot.
type Finding struct {
	Kind     string   `json:"kind"`
	Severity Severity `json:"severity"`
	// GlobalID is the entity the finding is about, where there is one.
	GlobalID string `json:"global_id,omitempty"`
	// EntityType, Source and SourceID locate the mapping.
	EntityType string `json:"entity_type,omitempty"`
	Source     string `json:"source,omitempty"`
	SourceID   string `json:"source_id,omitempty"`
	// Related names another identifier the finding ties to, such as the
	// client a server points at.
	Related string `json:"related,omitempty"`
	// Detail is a fixed, non-templated explanation. It never interpolates
	// upstream data.
	Detail string `json:"detail"`
}

// Mapping is one row of global_entities.
type Mapping struct {
	GlobalID   string
	EntityType string
	Source     string
	SourceID   string
	TenantID   string
}

// Relation is a Global ID reference held by another table, such as a server's
// client or a support record's client.
type Relation struct {
	// Holder is the entity that holds the reference.
	Holder string
	// Field names the column, for the finding's detail.
	Field string
	// Target is the referenced Global ID, empty when unset.
	Target string
}

// Input is everything the audit reasons over, read in one pass.
type Input struct {
	Mappings []Mapping
	// Prefixes maps an entity type to its registered Global ID prefix, from
	// global_id_counters. A type absent here has no registered prefix.
	Prefixes map[string]string
	// Relations are Global ID references held elsewhere.
	Relations []Relation
	// OwnerlessHolders are entities that should name an owning client but do
	// not, paired with the field that is empty.
	OwnerlessHolders []Relation
}

// Audit returns every finding, ordered so that errors come first and the
// output is stable enough to diff between runs.
func Audit(in Input) []Finding {
	var findings []Finding
	findings = append(findings, invalidPrefixes(in)...)
	findings = append(findings, ambiguousSourceIDs(in)...)
	findings = append(findings, danglingReferences(in)...)
	findings = append(findings, missingOwnership(in)...)

	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Severity != findings[j].Severity {
			return findings[i].Severity == SeverityError
		}
		if findings[i].Kind != findings[j].Kind {
			return findings[i].Kind < findings[j].Kind
		}
		return findings[i].GlobalID < findings[j].GlobalID
	})
	return findings
}

// invalidPrefixes reports Global IDs whose prefix disagrees with the prefix
// registered for their entity type.
//
// A Global ID is immutable and is how every other system refers to the entity,
// so one issued under the wrong prefix is not a cosmetic problem: it will be
// parsed as a different kind of thing by anything that reads the prefix.
func invalidPrefixes(in Input) []Finding {
	var findings []Finding
	for _, m := range in.Mappings {
		prefix, known := in.Prefixes[m.EntityType]
		if !known {
			findings = append(findings, Finding{
				Kind:       "unregistered_entity_type",
				Severity:   SeverityError,
				GlobalID:   m.GlobalID,
				EntityType: m.EntityType,
				Source:     m.Source,
				SourceID:   m.SourceID,
				Detail:     "entity type has no registered Global ID prefix",
			})
			continue
		}
		if !strings.HasPrefix(m.GlobalID, prefix+"-") {
			findings = append(findings, Finding{
				Kind:       "invalid_global_id_prefix",
				Severity:   SeverityError,
				GlobalID:   m.GlobalID,
				EntityType: m.EntityType,
				Source:     m.Source,
				SourceID:   m.SourceID,
				Detail:     "Global ID does not carry the prefix registered for its entity type",
			})
		}
	}
	return findings
}

// ambiguousSourceIDs reports one upstream record mapped to more than one
// entity type, or to more than one Global ID.
//
// The unique constraint on (source, entity_type, source_id) permits both: the
// same Redmine id can legally be mapped once as a project and once as a task.
// That is almost always a mistake made while backfilling, and it makes "which
// BSYSTEM entity is this upstream record?" unanswerable.
func ambiguousSourceIDs(in Input) []Finding {
	type key struct{ source, sourceID string }
	grouped := map[key][]Mapping{}
	for _, m := range in.Mappings {
		k := key{m.Source, m.SourceID}
		grouped[k] = append(grouped[k], m)
	}

	var findings []Finding
	for k, group := range grouped {
		if len(group) < 2 {
			continue
		}
		sort.Slice(group, func(i, j int) bool { return group[i].GlobalID < group[j].GlobalID })
		types := make([]string, 0, len(group))
		ids := make([]string, 0, len(group))
		for _, m := range group {
			types = append(types, m.EntityType)
			ids = append(ids, m.GlobalID)
		}
		findings = append(findings, Finding{
			Kind:       "ambiguous_source_mapping",
			Severity:   SeverityWarning,
			EntityType: strings.Join(dedupe(types), ","),
			Source:     k.source,
			SourceID:   k.sourceID,
			GlobalID:   ids[0],
			Related:    strings.Join(ids[1:], ","),
			Detail:     "one upstream record is mapped to more than one Global ID",
		})
	}
	return findings
}

// danglingReferences reports a Global ID held by another table that no mapping
// resolves.
//
// This is the failure that matters most for authorization: a scope is written
// against a client Global ID, so a server pointing at a client that does not
// exist can never be matched by a grant. It is denied rather than leaked —
// deny by default holds — but it is also invisible to the customer who should
// see it, which looks like data loss.
func danglingReferences(in Input) []Finding {
	known := make(map[string]struct{}, len(in.Mappings))
	for _, m := range in.Mappings {
		known[m.GlobalID] = struct{}{}
	}

	var findings []Finding
	for _, r := range in.Relations {
		if r.Target == "" {
			continue
		}
		if _, ok := known[r.Target]; !ok {
			findings = append(findings, Finding{
				Kind:     "missing_referenced_mapping",
				Severity: SeverityError,
				GlobalID: r.Holder,
				Related:  r.Target,
				Detail:   "references a Global ID with no mapping, via " + r.Field,
			})
		}
	}
	return findings
}

// missingOwnership reports an entity that carries no owning client.
//
// This is a warning rather than an error, and deliberately so. Unknown
// ownership resolves to denial, which is the safe outcome; the platform is
// behaving correctly. What it is not doing is showing the record to the
// customer it belongs to, and nobody finds out until that customer asks.
func missingOwnership(in Input) []Finding {
	var findings []Finding
	for _, r := range in.OwnerlessHolders {
		findings = append(findings, Finding{
			Kind:     "missing_ownership",
			Severity: SeverityWarning,
			GlobalID: r.Holder,
			Detail:   "no owning client in " + r.Field + "; the record is denied to every customer",
		})
	}
	return findings
}

func dedupe(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, v := range values {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// Summary counts findings by severity, for the exit code and the header line.
type Summary struct {
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
}

func Summarize(findings []Finding) Summary {
	var s Summary
	for _, f := range findings {
		switch f.Severity {
		case SeverityError:
			s.Errors++
		case SeverityWarning:
			s.Warnings++
		}
	}
	return s
}

// Text renders findings for a terminal.
func Text(findings []Finding, s Summary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "mapping audit: %d error(s), %d warning(s)\n", s.Errors, s.Warnings)
	if len(findings) == 0 {
		b.WriteString("no findings\n")
		return b.String()
	}
	for _, f := range findings {
		fmt.Fprintf(&b, "%-8s %-28s %-14s %s\n", f.Severity, f.Kind, f.GlobalID, f.Detail)
		if f.Source != "" {
			fmt.Fprintf(&b, "         source=%s source_id=%s type=%s\n", f.Source, f.SourceID, f.EntityType)
		}
		if f.Related != "" {
			fmt.Fprintf(&b, "         related=%s\n", f.Related)
		}
	}
	return b.String()
}
