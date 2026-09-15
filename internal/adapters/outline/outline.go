package outline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/adapters"
)

type Client struct {
	baseURL *url.URL
	apiKey  string
	http    *http.Client
}

type Document struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Text         string `json:"text,omitempty"`
	URL          string `json:"url,omitempty"`
	CollectionID string `json:"collectionId,omitempty"`
	UpdatedAt    string `json:"updatedAt,omitempty"`
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

func New(rawURL, apiKey string, timeout time.Duration) (*Client, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, errors.New("Outline base URL is required")
	}
	u, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("invalid Outline base URL")
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("Outline API key is required")
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{baseURL: u, apiKey: strings.TrimSpace(apiKey), http: &http.Client{Timeout: timeout}}, nil
}

func (c *Client) Info() adapters.Info {
	return adapters.Info{ID: "outline", Name: "Outline", Version: "1", Status: adapters.StatusReady, Capabilities: []string{"documents.read"}}
}

func (c *Client) Health(ctx context.Context) adapters.Health {
	var out rpcResponse[[]Document]
	if err := c.rpc(ctx, "documents.list", map[string]any{"limit": 1}, &out); err != nil {
		return adapters.Health{Status: adapters.StatusDegraded, Message: err.Error()}
	}
	return adapters.Health{Status: adapters.StatusReady}
}

func (c *Client) ListDocuments(ctx context.Context, limit int) ([]Document, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var out rpcResponse[[]Document]
	if err := c.rpc(ctx, "documents.list", map[string]any{"limit": limit}, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

func (c *Client) GetDocument(ctx context.Context, id string) (Document, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Document{}, errors.New("document id is required")
	}
	var out rpcResponse[Document]
	if err := c.rpc(ctx, "documents.info", map[string]any{"id": id}, &out); err != nil {
		return Document{}, err
	}
	return out.Data, nil
}

func (c *Client) rpc(ctx context.Context, method string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	u := *c.baseURL
	u.Path = strings.TrimRight(c.baseURL.Path, "/") + "/api/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("Outline request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Outline returned HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode Outline response: %w", err)
	}
	return nil
}
