// Package espocrm adapts the EspoCRM REST API to BSYSTEM.
//
// It isolates HUB and the normalized API from EspoCRM's conventions: nothing
// outside this package sees an EspoCRM field name, status code or envelope.
package espocrm

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/adapters"
	"github.com/ekucher/bsystem-integration-core/internal/adapters/httpx"
)

// Client reads clients and contacts from EspoCRM.
type Client struct {
	http   *httpx.Client
	header http.Header
}

// Account is an EspoCRM account, which BSYSTEM normalizes to a client.
type Account struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Website string `json:"website,omitempty"`
	Email   string `json:"emailAddress,omitempty"`
	Phone   string `json:"phoneNumber,omitempty"`
}

// Contact is an EspoCRM contact.
type Contact struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	AccountID string `json:"accountId,omitempty"`
	Email     string `json:"emailAddress,omitempty"`
	Phone     string `json:"phoneNumber,omitempty"`
}

// listResponse is EspoCRM's collection envelope: a total across the whole
// collection plus the requested window.
type listResponse[T any] struct {
	Total int `json:"total"`
	List  []T `json:"list"`
}

// Collection bounds. EspoCRM accepts larger pages, but the platform caps what
// it will ask for so that one request cannot pull an unbounded amount of
// upstream data through the adapter.
const (
	defaultPageSize = 50
	maxPageSize     = 200
)

// New returns a client for the EspoCRM instance at rawURL.
func New(rawURL, apiKey string, timeout time.Duration) (*Client, error) {
	client, err := httpx.New(httpx.Options{Adapter: "espocrm", BaseURL: rawURL, Timeout: timeout})
	if err != nil {
		return nil, err
	}
	header := http.Header{}
	if key := strings.TrimSpace(apiKey); key != "" {
		header.Set("X-Api-Key", key)
	}
	return &Client{http: client, header: header}, nil
}

func (c *Client) Info() adapters.Info {
	return adapters.Info{ID: "espocrm", Name: "EspoCRM", Version: "1", Status: adapters.StatusReady, Capabilities: []string{"clients.read", "contacts.read"}}
}

// Health probes the endpoint EspoCRM uses to describe the calling API user,
// which exercises both reachability and the credential.
func (c *Client) Health(ctx context.Context) adapters.Health {
	var out map[string]any
	if err := c.get(ctx, "/api/v1/App/user", nil, &out); err != nil {
		return adapters.Health{Status: adapters.StatusDegraded, Message: err.Error()}
	}
	return adapters.Health{Status: adapters.StatusReady}
}

// BreakerState reports the adapter's circuit state.
func (c *Client) BreakerState() string { return string(c.http.BreakerState()) }

// ListAccounts returns one page of accounts.
func (c *Client) ListAccounts(ctx context.Context, page adapters.Page) ([]Account, adapters.PageInfo, error) {
	return listCollection[Account](ctx, c, "/api/v1/Account", page)
}

// ListContacts returns one page of contacts.
func (c *Client) ListContacts(ctx context.Context, page adapters.Page) ([]Contact, adapters.PageInfo, error) {
	return listCollection[Contact](ctx, c, "/api/v1/Contact", page)
}

func listCollection[T any](ctx context.Context, c *Client, path string, page adapters.Page) ([]T, adapters.PageInfo, error) {
	offset, limit, err := page.Resolve(defaultPageSize, maxPageSize)
	if err != nil {
		return nil, adapters.PageInfo{}, err
	}
	query := url.Values{
		"maxSize": {strconv.Itoa(limit)},
		"offset":  {strconv.Itoa(offset)},
		"orderBy": {"name"},
		"order":   {"asc"},
	}
	var out listResponse[T]
	if err := c.get(ctx, path, query, &out); err != nil {
		return nil, adapters.PageInfo{}, err
	}
	return out.List, adapters.NewPageInfo(out.Total, offset, len(out.List)), nil
}

// GetAccount reads one account. It returns adapters.ErrNotFound when the
// upstream has no such record.
func (c *Client) GetAccount(ctx context.Context, id string) (Account, error) {
	var account Account
	if err := c.getByID(ctx, "/api/v1/Account/", id, &account); err != nil {
		return Account{}, err
	}
	return account, nil
}

// GetContact reads one contact. It returns adapters.ErrNotFound when the
// upstream has no such record.
func (c *Client) GetContact(ctx context.Context, id string) (Contact, error) {
	var contact Contact
	if err := c.getByID(ctx, "/api/v1/Contact/", id, &contact); err != nil {
		return Contact{}, err
	}
	return contact, nil
}

func (c *Client) getByID(ctx context.Context, prefix, id string, out any) error {
	id = strings.TrimSpace(id)
	if id == "" {
		// An empty identifier cannot name a record, and sending it would make
		// the collection endpoint answer instead of the detail one.
		return adapters.ErrNotFound
	}
	return c.get(ctx, prefix+url.PathEscape(id), nil, out)
}

// get issues an idempotent read. Every EspoCRM call BSYSTEM makes is a read,
// so all of them may be retried.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	return c.http.Do(ctx, httpx.Request{
		Method:     http.MethodGet,
		Path:       path,
		Query:      query,
		Header:     c.header,
		Idempotent: true,
	}, out)
}
