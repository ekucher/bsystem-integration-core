package platformdb

import (
	"context"
	"encoding/json"
	"time"
)

// AIRequest is one audited call to the AI gateway.
//
// There is no prompt field and there is not going to be one by default. An
// audit trail is read by more people than the request was, and storing the
// assembled context would recreate in one searchable table exactly the
// aggregation the authorization rules exist to prevent.
type AIRequest struct {
	ID               int64          `json:"id"`
	OccurredAt       time.Time      `json:"occurred_at"`
	ActorID          string         `json:"actor_id"`
	Provider         string         `json:"provider"`
	Model            string         `json:"model"`
	RequestedSources []string       `json:"requested_sources"`
	EntityIDs        []string       `json:"entity_ids"`
	Classification   map[string]any `json:"classification"`
	RequestID        string         `json:"request_id"`
	Result           string         `json:"result"`
	PromptBytes      int            `json:"prompt_bytes"`
	AnswerBytes      int            `json:"answer_bytes"`
	DurationMS       int            `json:"duration_ms"`
}

// InsertAIRequest records one gateway call.
func (db *DB) InsertAIRequest(ctx context.Context, request AIRequest) error {
	sources, _ := json.Marshal(nonNil(request.RequestedSources))
	entities, _ := json.Marshal(nonNil(request.EntityIDs))
	classification, _ := json.Marshal(request.Classification)
	_, err := db.pool.Exec(ctx, `
INSERT INTO ai_requests
 (actor_id,provider,model,requested_sources,entity_ids,classification,request_id,result,prompt_bytes,answer_bytes,duration_ms)
VALUES ($1,$2,$3,$4::jsonb,$5::jsonb,$6::jsonb,$7,$8,$9,$10,$11)`,
		request.ActorID, request.Provider, request.Model, string(sources), string(entities),
		string(classification), request.RequestID, request.Result,
		request.PromptBytes, request.AnswerBytes, request.DurationMS)
	return err
}

// ListAIRequests returns the most recent gateway calls, newest first.
func (db *DB) ListAIRequests(ctx context.Context, limit int) ([]AIRequest, error) {
	rows, err := db.pool.Query(ctx, `
SELECT id, occurred_at, actor_id, provider, model, requested_sources, entity_ids,
       classification, request_id, result, prompt_bytes, answer_bytes, duration_ms
FROM ai_requests ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	requests := []AIRequest{}
	for rows.Next() {
		var request AIRequest
		var sources, entities, classification []byte
		if err := rows.Scan(&request.ID, &request.OccurredAt, &request.ActorID, &request.Provider,
			&request.Model, &sources, &entities, &classification, &request.RequestID,
			&request.Result, &request.PromptBytes, &request.AnswerBytes, &request.DurationMS); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(sources, &request.RequestedSources)
		_ = json.Unmarshal(entities, &request.EntityIDs)
		_ = json.Unmarshal(classification, &request.Classification)
		request.OccurredAt = request.OccurredAt.UTC()
		requests = append(requests, request)
	}
	return requests, rows.Err()
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
