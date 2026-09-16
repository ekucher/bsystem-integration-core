package search

import (
	"context"
	"errors"
	"testing"
	"time"
)

func document(id, entityType, title, summary string, permissions ...string) Document {
	return Document{
		ID: id, Type: entityType, Title: title, Summary: summary,
		Source: "test", Permissions: permissions,
		ScopeType: "resource", ScopeID: id,
		UpdatedAt: time.Date(2026, 1, 6, 9, 0, 0, 0, time.UTC),
	}
}

func indexed(t *testing.T, documents ...Document) *Memory {
	t.Helper()
	provider := NewMemory()
	if err := provider.Index(context.Background(), documents...); err != nil {
		t.Fatalf("index: %v", err)
	}
	return provider
}

func TestADocumentWithoutAnAudienceIsRefused(t *testing.T) {
	// Indexing a document nobody may read would put a record in the index
	// that is never returned. The publisher should learn now, not when
	// somebody reports that search is missing things.
	cases := map[string]Document{
		"no permissions":    {ID: "CL-1", Type: TypeClient, Title: "x", Source: "s", ScopeType: "client", ScopeID: "CL-1"},
		"blank permissions": {ID: "CL-1", Type: TypeClient, Title: "x", Source: "s", Permissions: []string{"  ", ""}, ScopeType: "client", ScopeID: "CL-1"},
		"no scope":          {ID: "CL-1", Type: TypeClient, Title: "x", Source: "s", Permissions: []string{"crm.client.read"}},
		"no id":             {Type: TypeClient, Title: "x", Source: "s", Permissions: []string{"crm.client.read"}, ScopeType: "client", ScopeID: "CL-1"},
		"no title":          {ID: "CL-1", Type: TypeClient, Source: "s", Permissions: []string{"crm.client.read"}, ScopeType: "client", ScopeID: "CL-1"},
		"unknown type":      {ID: "SRV-1", Type: "server", Title: "x", Source: "s", Permissions: []string{"operations.server.read"}, ScopeType: "resource", ScopeID: "SRV-1"},
	}
	for name, invalid := range cases {
		t.Run(name, func(t *testing.T) {
			provider := NewMemory()
			err := provider.Index(context.Background(), invalid)
			if !errors.Is(err, ErrInvalidDocument) {
				t.Fatalf("err = %v, want ErrInvalidDocument", err)
			}
			if provider.Len() != 0 {
				t.Error("an invalid document was indexed anyway")
			}
		})
	}
}

// A batch must not be half-applied: a caller that cannot say what the index
// now holds has to re-index everything to find out.
func TestAnInvalidDocumentRejectsTheWholeBatch(t *testing.T) {
	provider := NewMemory()
	err := provider.Index(context.Background(),
		document("CL-000001", TypeClient, "Northwind", "", "crm.client.read"),
		Document{ID: "CL-000002", Type: TypeClient, Title: "Globex", Source: "s"},
	)
	if !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("err = %v, want ErrInvalidDocument", err)
	}
	if provider.Len() != 0 {
		t.Errorf("%d documents were indexed from a rejected batch", provider.Len())
	}
}

func TestIndexingIsAnUpsertKeyedByGlobalID(t *testing.T) {
	provider := indexed(t, document("CL-000001", TypeClient, "Northwind Trading", "", "crm.client.read"))
	if err := provider.Index(context.Background(), document("CL-000001", TypeClient, "Northwind Holdings", "", "crm.client.read")); err != nil {
		t.Fatalf("re-index: %v", err)
	}
	if provider.Len() != 1 {
		t.Fatalf("%d documents indexed, want the original replaced", provider.Len())
	}
	page, err := provider.Search(context.Background(), Query{Text: "northwind", AllPermissions: true, Limit: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(page.Hits) != 1 || page.Hits[0].Title != "Northwind Holdings" {
		t.Errorf("hits = %+v, want the updated document only", page.Hits)
	}
}

// Deleting something absent succeeds: a delete that failed once the record
// was already gone would turn a retry into an error.
func TestDeletingAnAbsentDocumentSucceeds(t *testing.T) {
	provider := indexed(t, document("CL-000001", TypeClient, "Northwind", "", "crm.client.read"))
	if err := provider.Delete(context.Background(), "CL-999999", "CL-000001", "CL-000001"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if provider.Len() != 0 {
		t.Errorf("%d documents remain", provider.Len())
	}
}

// An empty query must match nothing. A search endpoint that returns the whole
// index when asked for nothing is an export endpoint wearing a search
// endpoint's name.
func TestAnEmptyQueryMatchesNothing(t *testing.T) {
	provider := indexed(t,
		document("CL-000001", TypeClient, "Northwind", "", "crm.client.read"),
		document("PR-000001", TypeProject, "Migration", "", "projects.task.read"),
	)
	for _, text := range []string{"", "   ", "!!!", "—"} {
		page, err := provider.Search(context.Background(), Query{Text: text, AllPermissions: true, Limit: 10})
		if err != nil {
			t.Fatalf("search %q: %v", text, err)
		}
		if len(page.Hits) != 0 || page.Total != 0 {
			t.Errorf("query %q returned %d hits", text, len(page.Hits))
		}
	}
}

func TestMatchingRequiresEveryTerm(t *testing.T) {
	provider := indexed(t,
		document("CL-000001", TypeClient, "Northwind Trading", "Key account", "crm.client.read"),
		document("CL-000002", TypeClient, "Globex Industrial", "Northwind competitor", "crm.client.read"),
	)
	cases := []struct {
		query string
		want  []string
	}{
		{"northwind", []string{"CL-000001", "CL-000002"}},
		{"northwind trading", []string{"CL-000001"}},
		// A prefix matches; a suffix does not. Stating it so the behaviour is
		// a decision rather than an accident of the implementation.
		{"north", []string{"CL-000001", "CL-000002"}},
		{"wind", nil},
		{"northwind globex", []string{"CL-000002"}},
		{"nonexistent", nil},
	}
	for _, c := range cases {
		t.Run(c.query, func(t *testing.T) {
			page, err := provider.Search(context.Background(), Query{Text: c.query, AllPermissions: true, Limit: 10})
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			got := ids(page.Hits)
			if len(got) != len(c.want) {
				t.Fatalf("hits = %v, want %v", got, c.want)
			}
			found := map[string]bool{}
			for _, id := range got {
				found[id] = true
			}
			for _, id := range c.want {
				if !found[id] {
					t.Errorf("hits = %v, want %v", got, c.want)
				}
			}
		})
	}
}

// A title match must outrank a summary match, and the order must be total, or
// a caller paging through results sees one document twice and misses another.
func TestRankingIsDeterministicAndPrefersTitles(t *testing.T) {
	provider := indexed(t,
		document("CL-000002", TypeClient, "Globex Industrial", "Northwind competitor", "crm.client.read"),
		document("CL-000001", TypeClient, "Northwind Trading", "Key account", "crm.client.read"),
		document("CL-000003", TypeClient, "Northwind Trading", "Duplicate record", "crm.client.read"),
	)
	var previous []string
	for attempt := 0; attempt < 5; attempt++ {
		page, err := provider.Search(context.Background(), Query{Text: "northwind", AllPermissions: true, Limit: 10})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		got := ids(page.Hits)
		if got[len(got)-1] != "CL-000002" {
			t.Errorf("order = %v, want the summary-only match last", got)
		}
		// Equal scores break on id, so the two identical titles keep a stable
		// relative order.
		if got[0] != "CL-000001" || got[1] != "CL-000003" {
			t.Errorf("order = %v, want equally scored documents ordered by id", got)
		}
		if previous != nil && !equal(previous, got) {
			t.Fatalf("the same query gave a different order: %v then %v", previous, got)
		}
		previous = got
	}
}

func TestTypeFilterRestrictsResults(t *testing.T) {
	provider := indexed(t,
		document("CL-000001", TypeClient, "Migration client", "", "crm.client.read"),
		document("PR-000001", TypeProject, "Migration project", "", "projects.task.read"),
		document("DOC-000001", TypeDocument, "Migration runbook", "", "wiki.document.read"),
	)
	page, err := provider.Search(context.Background(), Query{
		Text: "migration", Types: []string{TypeProject, TypeDocument}, AllPermissions: true, Limit: 10,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, hit := range page.Hits {
		if hit.Type != TypeProject && hit.Type != TypeDocument {
			t.Errorf("type filter returned a %s", hit.Type)
		}
	}
	if len(page.Hits) != 2 {
		t.Errorf("hits = %v, want two", ids(page.Hits))
	}
}

// The provider's permission narrowing is not the authorization decision, but
// it must not be wrong in the permissive direction either.
func TestTheProviderNarrowsByPermission(t *testing.T) {
	provider := indexed(t,
		document("CL-000001", TypeClient, "Migration client", "", "crm.client.read"),
		document("PR-000001", TypeProject, "Migration project", "", "projects.task.read"),
	)
	page, err := provider.Search(context.Background(), Query{
		Text: "migration", Permissions: []string{"projects.task.read"}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if got := ids(page.Hits); len(got) != 1 || got[0] != "PR-000001" {
		t.Errorf("hits = %v, want only the project", got)
	}

	// Holding nothing matches nothing, rather than everything.
	page, err = provider.Search(context.Background(), Query{Text: "migration", Limit: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(page.Hits) != 0 {
		t.Errorf("a principal holding no permission saw %v", ids(page.Hits))
	}
}

func TestPagingWalksEveryResultExactlyOnce(t *testing.T) {
	documents := make([]Document, 0, 7)
	for i := 0; i < 7; i++ {
		documents = append(documents, document(
			"CL-00000"+string(rune('1'+i)), TypeClient, "Migration record", "", "crm.client.read"))
	}
	provider := indexed(t, documents...)

	seen := map[string]bool{}
	offset := 0
	for page := 0; page < 20; page++ {
		result, err := provider.Search(context.Background(), Query{
			Text: "migration", AllPermissions: true, Offset: offset, Limit: 2,
		})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(result.Hits) == 0 {
			break
		}
		for _, hit := range result.Hits {
			if seen[hit.ID] {
				t.Fatalf("%s was returned twice", hit.ID)
			}
			seen[hit.ID] = true
		}
		offset += result.Examined
	}
	if len(seen) != len(documents) {
		t.Errorf("walked %d of %d documents", len(seen), len(documents))
	}
}

func TestCursorsRoundTripAndForgeriesAreRefused(t *testing.T) {
	for _, offset := range []int{1, 20, 5000} {
		cursor := EncodeCursor(offset)
		got, err := DecodeCursor(cursor)
		if err != nil || got != offset {
			t.Errorf("round trip of %d gave (%d,%v)", offset, got, err)
		}
	}
	if cursor := EncodeCursor(0); cursor != "" {
		t.Errorf("EncodeCursor(0) = %q, want empty", cursor)
	}
	for _, cursor := range []string{"not-base64!", "bjoxMA", "czot MQ", "nonsense"} {
		if _, err := DecodeCursor(cursor); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("DecodeCursor(%q) was accepted", cursor)
		}
	}
}

func TestUnknownTypesAreRefusedRatherThanDropped(t *testing.T) {
	if _, err := NormalizeTypes([]string{"client", "server"}); err == nil {
		t.Error("an unknown type was accepted; the caller would read an unfiltered result as filtered")
	}
	types, err := NormalizeTypes([]string{" Client ", "", "DOCUMENT"})
	if err != nil {
		t.Fatalf("NormalizeTypes: %v", err)
	}
	if len(types) != 2 || types[0] != TypeClient || types[1] != TypeDocument {
		t.Errorf("types = %v, want normalized client and document", types)
	}
}

// Platform data is not English-only. Dropping non-ASCII letters would make a
// Ukrainian client name unsearchable.
func TestNonASCIITextIsSearchable(t *testing.T) {
	provider := indexed(t, document("CL-000001", TypeClient, "Північвінд Трейдинг", "Ключовий клієнт", "crm.client.read"))
	page, err := provider.Search(context.Background(), Query{Text: "Північвінд", AllPermissions: true, Limit: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(page.Hits) != 1 {
		t.Errorf("hits = %v, want the Ukrainian title to match", ids(page.Hits))
	}
}

func TestLimitsAreClamped(t *testing.T) {
	for _, c := range []struct{ in, want int }{{0, DefaultLimit}, {-1, DefaultLimit}, {5, 5}, {MaxLimit + 1, MaxLimit}} {
		if got := ClampLimit(c.in); got != c.want {
			t.Errorf("ClampLimit(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func ids(hits []Hit) []string {
	result := make([]string, 0, len(hits))
	for _, hit := range hits {
		result = append(result, hit.ID)
	}
	return result
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
