package search

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/adapters"
	"github.com/ekucher/bsystem-integration-core/internal/adapters/httpx"
)

// OpenSearch talks to an OpenSearch cluster.
//
// This is a skeleton in one specific sense: the request and response shapes,
// the error normalization, the resilience policy and the authorization
// narrowing are all here and tested against a fake cluster, but no BSYSTEM
// deployment runs an OpenSearch cluster yet, so nothing has been validated
// against a real one. Mapping definitions, index lifecycle and reindexing are
// deliberately absent rather than guessed at — they depend on cluster
// decisions an owner has not made.
//
// What it is not is a placeholder that returns stubs. It builds real
// requests, so the first person to point it at a cluster is debugging a
// cluster rather than this file.
type OpenSearch struct {
	client *httpx.Client
	index  string
}

// DefaultIndex is the index the platform reads and writes.
const DefaultIndex = "bsystem-search"

// NewOpenSearch builds a provider from an adapter configuration. The
// credential, if any, is carried as a bearer token and never logged.
func NewOpenSearch(config adapters.Config, index string) (*OpenSearch, error) {
	if strings.TrimSpace(config.BaseURL) == "" {
		return nil, errors.New("search: OpenSearch base URL is required")
	}
	if index = strings.TrimSpace(index); index == "" {
		index = DefaultIndex
	}
	client, err := httpx.New(httpx.OptionsFor("opensearch", config))
	if err != nil {
		return nil, err
	}
	return &OpenSearch{client: client, index: index}, nil
}

func (o *OpenSearch) Name() string { return "opensearch" }

func (o *OpenSearch) header() http.Header {
	header := http.Header{}
	header.Set("Accept", "application/json")
	return header
}

// bulkLine is one action or document in a bulk request body.
type bulkAction struct {
	Index *bulkTarget `json:"index,omitempty"`
	Sent  *bulkTarget `json:"delete,omitempty"`
}

type bulkTarget struct {
	Index string `json:"_index"`
	ID    string `json:"_id"`
}

// Index upserts documents.
//
// Documents are written with the Global ID as the OpenSearch document id, so
// re-indexing an entity replaces it rather than accumulating copies. The
// request is marked idempotent for exactly that reason: a retried bulk index
// converges on the same state.
func (o *OpenSearch) Index(ctx context.Context, documents ...Document) error {
	if len(documents) == 0 {
		return nil
	}
	now := time.Now()
	lines := make([]any, 0, len(documents)*2)
	for i := range documents {
		if err := documents[i].Normalize(now); err != nil {
			return err
		}
		lines = append(lines, bulkAction{Index: &bulkTarget{Index: o.index, ID: documents[i].ID}}, documents[i])
	}
	return o.bulk(ctx, lines)
}

// Delete removes documents. A missing document is not an error: the bulk API
// reports it per item, and a delete that failed when the record was already
// gone would turn a retry into a failure.
func (o *OpenSearch) Delete(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	lines := make([]any, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			lines = append(lines, bulkAction{Sent: &bulkTarget{Index: o.index, ID: id}})
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return o.bulk(ctx, lines)
}

type bulkResponse struct {
	Errors bool `json:"errors"`
	Items  []map[string]struct {
		Status int `json:"status"`
		Error  *struct {
			Type   string `json:"type"`
			Reason string `json:"reason"`
		} `json:"error"`
	} `json:"items"`
}

func (o *OpenSearch) bulk(ctx context.Context, lines []any) error {
	var response bulkResponse
	request := httpx.Request{
		Method:     http.MethodPost,
		Path:       "/_bulk",
		Header:     o.header(),
		Body:       httpx.NDJSON(lines),
		Idempotent: true,
	}
	if err := o.client.Do(ctx, request, &response); err != nil {
		return normalizeProviderError(err)
	}
	if !response.Errors {
		return nil
	}
	// The cluster reports per-item failures inside a 200. Only the item type
	// is surfaced: an OpenSearch reason can quote the document, and the
	// document is exactly what must not travel into a platform error.
	for _, item := range response.Items {
		for action, result := range item {
			if result.Error == nil {
				continue
			}
			if action == "delete" && result.Status == http.StatusNotFound {
				continue
			}
			return fmt.Errorf("%w: bulk %s rejected (%s)", ErrUnavailable, action, result.Error.Type)
		}
	}
	return nil
}

type searchResponse struct {
	Hits struct {
		Total struct {
			Value int `json:"value"`
		} `json:"total"`
		Hits []struct {
			Score  float64  `json:"_score"`
			Source Document `json:"_source"`
		} `json:"hits"`
	} `json:"hits"`
}

func (o *OpenSearch) Search(ctx context.Context, query Query) (Page, error) {
	terms := Tokenize(query.Text)
	if len(terms) == 0 {
		return Page{}, nil
	}

	// The filter clauses are what keep the narrowing in the query rather than
	// in the platform's memory: a caller's page should not be assembled by
	// fetching records they cannot see and discarding them.
	filters := []any{}
	if len(query.Types) > 0 {
		filters = append(filters, map[string]any{"terms": map[string]any{"type": query.Types}})
	}
	if !query.AllPermissions {
		// An empty permission list intersects nothing, which is the correct
		// result for a principal who holds none.
		filters = append(filters, map[string]any{"terms": map[string]any{"permissions": query.Permissions}})
	}

	body := map[string]any{
		"from": query.Offset,
		"size": ClampLimit(query.Limit),
		"query": map[string]any{
			"bool": map[string]any{
				"must": []any{
					map[string]any{
						"multi_match": map[string]any{
							"query": query.Text,
							// Title is weighted above summary, matching the
							// in-memory provider so behaviour does not change
							// when a deployment switches engine.
							"fields":   []string{"title^2", "summary"},
							"operator": "and",
						},
					},
				},
				"filter": filters,
			},
		},
		"sort": []any{"_score", map[string]any{"updated_at": "desc"}, map[string]any{"id": "asc"}},
	}

	var response searchResponse
	request := httpx.Request{
		Method: http.MethodPost,
		Path:   "/" + url.PathEscape(o.index) + "/_search",
		Header: o.header(),
		Body:   body,
		// A search changes nothing, so retrying it cannot duplicate an effect.
		Idempotent: true,
	}
	if err := o.client.Do(ctx, request, &response); err != nil {
		return Page{}, normalizeProviderError(err)
	}

	hits := make([]Hit, 0, len(response.Hits.Hits))
	for _, hit := range response.Hits.Hits {
		hits = append(hits, Hit{Document: hit.Source, Score: hit.Score})
	}
	return Page{Hits: hits, Total: response.Hits.Total.Value, Examined: len(hits)}, nil
}

// normalizeProviderError turns a transport failure into the platform's own
// error, keeping the cluster's status codes, hostnames and payloads out of
// anything a caller or a log line can see.
func normalizeProviderError(err error) error {
	if err == nil {
		return nil
	}
	var adapterErr *httpx.Error
	if errors.As(err, &adapterErr) {
		return fmt.Errorf("%w: %s", ErrUnavailable, adapterErr.Kind)
	}
	return fmt.Errorf("%w: %v", ErrUnavailable, err)
}
