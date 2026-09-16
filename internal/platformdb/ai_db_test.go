package platformdb

import (
	"strings"
	"testing"
)

// The AI audit records the shape of a call, never its content. That is the
// whole reason the table has prompt_bytes rather than prompt: a CREDENTIAL
// must never reach a model, and an audit trail that stored prompts would
// become the place those credentials ended up instead.
func TestAIAuditStoresNoColumnThatCouldHoldContent(t *testing.T) {
	ctx, db := storeFixture(t)

	rows, err := db.pool.Query(ctx, `
SELECT column_name, data_type FROM information_schema.columns
WHERE table_name = 'ai_requests' ORDER BY ordinal_position`)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	defer rows.Close()

	forbidden := []string{"prompt", "answer", "response", "content", "body", "text", "message"}
	allowed := map[string]bool{"prompt_bytes": true, "answer_bytes": true}

	var columns []string
	for rows.Next() {
		var name, dataType string
		if err := rows.Scan(&name, &dataType); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		columns = append(columns, name)
		if allowed[name] {
			continue
		}
		for _, word := range forbidden {
			if strings.Contains(name, word) {
				t.Errorf("ai_requests.%s (%s) looks like it could hold model content; the audit records shape, not content", name, dataType)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read schema: %v", err)
	}
	if len(columns) == 0 {
		t.Fatal("ai_requests has no columns; the migration did not run")
	}
}

func TestAIRequestsRoundTripNewestFirst(t *testing.T) {
	ctx, db := storeFixture(t)

	for _, request := range []AIRequest{
		{
			ActorID: "USR-000001", Provider: "fake", Model: "fake-1",
			RequestedSources: []string{"clients"}, EntityIDs: []string{"CL-000001"},
			Classification: map[string]any{"overall": "INTERNAL"},
			RequestID:      "req-1", Result: "answered",
			PromptBytes: 120, AnswerBytes: 340, DurationMS: 42,
		},
		{
			ActorID: "USR-000002", Provider: "fake", Model: "fake-1",
			Classification: map[string]any{"overall": "CREDENTIAL"},
			RequestID:      "req-2", Result: "refused_credential",
		},
	} {
		if err := db.InsertAIRequest(ctx, request); err != nil {
			t.Fatalf("insert AI request: %v", err)
		}
	}

	items, err := db.ListAIRequests(ctx, 10)
	if err != nil {
		t.Fatalf("list AI requests: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected two records, got %d", len(items))
	}
	if items[0].RequestID != "req-2" {
		t.Fatalf("records must come newest first, got %q first", items[0].RequestID)
	}

	newest := items[0]
	if newest.Result != "refused_credential" {
		t.Fatalf("a refusal must be recorded as one, got %q", newest.Result)
	}
	// A refusal is exactly the case worth auditing, and its classification is
	// the reason it was refused.
	if got, _ := newest.Classification["overall"].(string); got != "CREDENTIAL" {
		t.Fatalf("classification did not round-trip, got %v", newest.Classification)
	}

	oldest := items[1]
	if len(oldest.RequestedSources) != 1 || oldest.RequestedSources[0] != "clients" {
		t.Fatalf("requested sources did not round-trip: %+v", oldest.RequestedSources)
	}
	if len(oldest.EntityIDs) != 1 || oldest.EntityIDs[0] != "CL-000001" {
		t.Fatalf("entity ids did not round-trip: %+v", oldest.EntityIDs)
	}
	if oldest.PromptBytes != 120 || oldest.AnswerBytes != 340 || oldest.DurationMS != 42 {
		t.Fatalf("sizes did not round-trip: %+v", oldest)
	}
	if oldest.OccurredAt.IsZero() {
		t.Fatal("every audited call must carry when it happened")
	}
	if oldest.OccurredAt.Location().String() != "UTC" {
		t.Fatalf("audit timestamps must be UTC, got %s", oldest.OccurredAt.Location())
	}
}

// A nil slice must be stored as an empty array rather than JSON null, so that
// every reader gets the same shape back and none has to special-case it.
func TestAbsentListsRoundTripAsEmptyRatherThanNull(t *testing.T) {
	ctx, db := storeFixture(t)

	if err := db.InsertAIRequest(ctx, AIRequest{
		ActorID: "USR-000003", Provider: "fake", Model: "fake-1", Result: "failed",
	}); err != nil {
		t.Fatalf("insert AI request: %v", err)
	}

	items, err := db.ListAIRequests(ctx, 1)
	if err != nil {
		t.Fatalf("list AI requests: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one record, got %d", len(items))
	}
	if items[0].RequestedSources == nil || len(items[0].RequestedSources) != 0 {
		t.Fatalf("requested sources must come back as an empty list, got %#v", items[0].RequestedSources)
	}
	if items[0].EntityIDs == nil || len(items[0].EntityIDs) != 0 {
		t.Fatalf("entity ids must come back as an empty list, got %#v", items[0].EntityIDs)
	}
}

func TestAIAuditListingRespectsItsLimit(t *testing.T) {
	ctx, db := storeFixture(t)

	for i := 0; i < 5; i++ {
		if err := db.InsertAIRequest(ctx, AIRequest{
			ActorID: "USR-000004", Provider: "fake", Model: "fake-1", Result: "answered",
		}); err != nil {
			t.Fatalf("insert AI request: %v", err)
		}
	}

	items, err := db.ListAIRequests(ctx, 2)
	if err != nil {
		t.Fatalf("list AI requests: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("the limit must bound the page, got %d", len(items))
	}
}
