// Package outline adapts the Outline RPC API to BSYSTEM.
//
// It isolates HUB and the normalized API from Outline's conventions: nothing
// outside this package sees an Outline field name, status code or envelope.
package outline

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/ekucher/bsystem-integration-core/internal/adapters"
	"github.com/ekucher/bsystem-integration-core/internal/adapters/httpx"
)

// Client reads documents from Outline.
type Client struct {
	http   *httpx.Client
	header http.Header
}

// Document is an Outline document.
type Document struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Text         string `json:"text,omitempty"`
	URL          string `json:"url,omitempty"`
	CollectionID string `json:"collectionId,omitempty"`
	UpdatedAt    string `json:"updatedAt,omitempty"`
}

// SearchHit is one result from documents.search, which wraps the document in
// a ranked context snippet.
type SearchHit struct {
	Context  string   `json:"context"`
	Ranking  float64  `json:"ranking"`
	Document Document `json:"document"`
}

// rpcResponse is the Outline RPC envelope. Every method returns its result
// under "data"; collection methods return an array there and add "pagination"
// alongside it.
type rpcResponse[T any] struct {
	Data       T          `json:"data"`
	Pagination pagination `json:"pagination"`
}

type pagination struct {
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
	Total  int `json:"total"`
}

// Collection bounds. Outline's own maximum page size is 100.
const (
	defaultPageSize = 50
	maxPageSize     = 100
)

// New returns a client for the Outline instance at rawURL. Outline has no
// anonymous read surface, so an API key is required rather than optional.
func New(config adapters.Config) (*Client, error) {
	key := strings.TrimSpace(config.APIKey)
	if key == "" {
		// lint:ignore ST1005 "Outline" is a proper noun, which Go's error
		// string convention explicitly permits at the start of a message.
		//lint:ignore ST1005 proper noun
		return nil, errors.New("Outline API key is required")
	}
	client, err := httpx.New(httpx.OptionsFor("outline", config))
	if err != nil {
		return nil, err
	}
	return &Client{http: client, header: http.Header{"Authorization": {"Bearer " + key}}}, nil
}

func (c *Client) Info() adapters.Info {
	return adapters.Info{ID: "outline", Name: "Outline", Version: "1", Status: adapters.StatusReady, Capabilities: []string{"documents.read", "documents.search"}}
}

// Health asks for a single document, which exercises both reachability and
// the credential without pulling a page of content.
func (c *Client) Health(ctx context.Context) adapters.Health {
	var out rpcResponse[[]Document]
	if err := c.rpc(ctx, "documents.list", map[string]any{"limit": 1}, &out); err != nil {
		return adapters.Health{Status: adapters.StatusDegraded, Message: err.Error()}
	}
	return adapters.Health{Status: adapters.StatusReady}
}

// BreakerState reports the adapter's circuit state.
func (c *Client) BreakerState() string { return string(c.http.BreakerState()) }

// ListDocuments returns one page of documents.
func (c *Client) ListDocuments(ctx context.Context, page adapters.Page) ([]Document, adapters.PageInfo, error) {
	offset, limit, err := page.Resolve(defaultPageSize, maxPageSize)
	if err != nil {
		return nil, adapters.PageInfo{}, err
	}
	var out rpcResponse[[]Document]
	if err := c.rpc(ctx, "documents.list", map[string]any{"limit": limit, "offset": offset}, &out); err != nil {
		return nil, adapters.PageInfo{}, err
	}
	return out.Data, adapters.NewPageInfo(out.Pagination.Total, offset, len(out.Data)), nil
}

// SearchDocuments returns one page of search results.
func (c *Client) SearchDocuments(ctx context.Context, query string, page adapters.Page) ([]SearchHit, adapters.PageInfo, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, adapters.PageInfo{}, errors.New("search query is required")
	}
	offset, limit, err := page.Resolve(defaultPageSize, maxPageSize)
	if err != nil {
		return nil, adapters.PageInfo{}, err
	}
	var out rpcResponse[[]SearchHit]
	if err := c.rpc(ctx, "documents.search", map[string]any{"query": query, "limit": limit, "offset": offset}, &out); err != nil {
		return nil, adapters.PageInfo{}, err
	}
	return out.Data, adapters.NewPageInfo(out.Pagination.Total, offset, len(out.Data)), nil
}

// GetDocument reads one document. It returns adapters.ErrNotFound when the
// upstream has no such record.
func (c *Client) GetDocument(ctx context.Context, id string) (Document, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Document{}, adapters.ErrNotFound
	}
	var out rpcResponse[Document]
	if err := c.rpc(ctx, "documents.info", map[string]any{"id": id}, &out); err != nil {
		return Document{}, err
	}
	return out.Data, nil
}

// rpc calls one Outline method.
//
// Outline performs reads over POST, so idempotency is asserted here rather
// than inferred from the method: every call this adapter makes is a read and
// may safely be retried.
func (c *Client) rpc(ctx context.Context, method string, payload any, out any) error {
	return c.http.Do(ctx, httpx.Request{
		Method:     http.MethodPost,
		Path:       "/api/" + method,
		Body:       payload,
		Header:     c.header,
		Idempotent: true,
	}, out)
}
