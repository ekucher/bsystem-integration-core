package espocrm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/adapters"
)

type Client struct {
	baseURL *url.URL
	apiKey  string
	http    *http.Client
}

type Account struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Website string `json:"website,omitempty"`
	Email   string `json:"emailAddress,omitempty"`
	Phone   string `json:"phoneNumber,omitempty"`
}

type Contact struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	AccountID string `json:"accountId,omitempty"`
	Email     string `json:"emailAddress,omitempty"`
	Phone     string `json:"phoneNumber,omitempty"`
}

type listResponse[T any] struct {
	Total int `json:"total"`
	List  []T `json:"list"`
}

func New(rawURL, apiKey string, timeout time.Duration) (*Client, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, errors.New("EspoCRM base URL is required")
	}
	u, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("invalid EspoCRM base URL")
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{baseURL: u, apiKey: strings.TrimSpace(apiKey), http: &http.Client{Timeout: timeout}}, nil
}

func (c *Client) Info() adapters.Info {
	return adapters.Info{ID: "espocrm", Name: "EspoCRM", Version: "1", Status: adapters.StatusReady, Capabilities: []string{"clients.read", "contacts.read"}}
}

func (c *Client) Health(ctx context.Context) adapters.Health {
	req, err := c.request(ctx, http.MethodGet, "/api/v1/App/user", nil)
	if err != nil {
		return adapters.Health{Status: adapters.StatusDegraded, Message: err.Error()}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return adapters.Health{Status: adapters.StatusDegraded, Message: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return adapters.Health{Status: adapters.StatusDegraded, Message: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	return adapters.Health{Status: adapters.StatusReady}
}

func (c *Client) ListAccounts(ctx context.Context, maxSize int) ([]Account, error) {
	if maxSize <= 0 || maxSize > 200 {
		maxSize = 50
	}
	q := url.Values{"maxSize": {strconv.Itoa(maxSize)}, "orderBy": {"name"}, "order": {"asc"}}
	var out listResponse[Account]
	if err := c.getJSON(ctx, "/api/v1/Account", q, &out); err != nil {
		return nil, err
	}
	return out.List, nil
}

func (c *Client) ListContacts(ctx context.Context, maxSize int) ([]Contact, error) {
	if maxSize <= 0 || maxSize > 200 {
		maxSize = 50
	}
	q := url.Values{"maxSize": {strconv.Itoa(maxSize)}, "orderBy": {"name"}, "order": {"asc"}}
	var out listResponse[Contact]
	if err := c.getJSON(ctx, "/api/v1/Contact", q, &out); err != nil {
		return nil, err
	}
	return out.List, nil
}

func (c *Client) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	req, err := c.request(ctx, http.MethodGet, path, query)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("EspoCRM request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("EspoCRM returned HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode EspoCRM response: %w", err)
	}
	return nil
}

func (c *Client) request(ctx context.Context, method, path string, query url.Values) (*http.Request, error) {
	u := *c.baseURL
	u.Path = strings.TrimRight(c.baseURL.Path, "/") + path
	if query != nil {
		u.RawQuery = query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("X-Api-Key", c.apiKey)
	}
	return req, nil
}
