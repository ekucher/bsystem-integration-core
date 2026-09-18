package authentikadmin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var ErrNotConfigured = errors.New("authentik administration is not configured")

type APIError struct {
	Status int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("authentik API returned HTTP %d", e.Status)
}

type Group struct {
	PK   string `json:"pk"`
	Name string `json:"name"`
}

type User struct {
	PK        int      `json:"pk"`
	Username  string   `json:"username"`
	Name      string   `json:"name"`
	Email     string   `json:"email"`
	IsActive  bool     `json:"is_active"`
	Type      string   `json:"type"`
	Groups    []string `json:"groups"`
	GroupsObj []Group  `json:"groups_obj"`
}

type CreateUserInput struct {
	Username string
	Name     string
	Email    string
	GroupPK  string
}

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   strings.TrimSpace(token),
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) Configured() bool {
	return c != nil && c.baseURL != "" && c.token != ""
}

func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	result := []User{}
	for page := 1; ; page++ {
		var payload struct {
			Pagination struct {
				Next int `json:"next"`
			} `json:"pagination"`
			Results []User `json:"results"`
		}
		if err := c.get(ctx, "/api/v3/core/users/", map[string]string{
			"include_groups": "true",
			"include_roles":  "false",
			"page_size":      "100",
			"page":           strconv.Itoa(page),
		}, &payload); err != nil {
			return nil, err
		}
		result = append(result, payload.Results...)
		if payload.Pagination.Next == 0 {
			return result, nil
		}
		if payload.Pagination.Next <= page {
			return nil, errors.New("authentik user pagination did not advance")
		}
		page = payload.Pagination.Next - 1
	}
}

func (c *Client) GetUser(ctx context.Context, pk int) (User, error) {
	if !c.Configured() {
		return User{}, ErrNotConfigured
	}
	var user User
	err := c.get(ctx, fmt.Sprintf("/api/v3/core/users/%d/", pk), map[string]string{
		"include_groups": "true",
		"include_roles":  "false",
	}, &user)
	return user, err
}

func (c *Client) ListGroups(ctx context.Context) ([]Group, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	result := []Group{}
	for page := 1; ; page++ {
		var payload struct {
			Pagination struct {
				Next int `json:"next"`
			} `json:"pagination"`
			Results []Group `json:"results"`
		}
		if err := c.get(ctx, "/api/v3/core/groups/", map[string]string{
			"include_users": "false",
			"page_size":     "100",
			"page":          strconv.Itoa(page),
		}, &payload); err != nil {
			return nil, err
		}
		result = append(result, payload.Results...)
		if payload.Pagination.Next == 0 {
			return result, nil
		}
		if payload.Pagination.Next <= page {
			return nil, errors.New("authentik group pagination did not advance")
		}
		page = payload.Pagination.Next - 1
	}
}

func (c *Client) CreateUser(ctx context.Context, input CreateUserInput) (User, error) {
	if !c.Configured() {
		return User{}, ErrNotConfigured
	}
	body := map[string]any{
		"username":  input.Username,
		"name":      input.Name,
		"email":     input.Email,
		"is_active": false,
		"type":      "internal",
		"path":      "users",
		"groups":    []string{input.GroupPK},
	}
	var created User
	if err := c.doJSON(ctx, http.MethodPost, "/api/v3/core/users/", nil, body, http.StatusCreated, &created); err != nil {
		return User{}, err
	}
	return created, nil
}

func (c *Client) UpdateUser(ctx context.Context, pk int, groups *[]string, active *bool, email *string) (User, error) {
	if !c.Configured() {
		return User{}, ErrNotConfigured
	}
	body := map[string]any{}
	if groups != nil {
		body["groups"] = *groups
	}
	if active != nil {
		body["is_active"] = *active
	}
	if email != nil {
		body["email"] = *email
	}
	if len(body) == 0 {
		return c.GetUser(ctx, pk)
	}
	var updated User
	if err := c.doJSON(
		ctx,
		http.MethodPatch,
		fmt.Sprintf("/api/v3/core/users/%d/", pk),
		nil,
		body,
		http.StatusOK,
		&updated,
	); err != nil {
		return User{}, err
	}
	return updated, nil
}

func (c *Client) SetPassword(ctx context.Context, pk int, password string) error {
	if !c.Configured() {
		return ErrNotConfigured
	}
	return c.doJSON(
		ctx,
		http.MethodPost,
		fmt.Sprintf("/api/v3/core/users/%d/set_password/", pk),
		nil,
		map[string]string{"password": password},
		http.StatusNoContent,
		nil,
	)
}

func (c *Client) get(ctx context.Context, path string, query map[string]string, out any) error {
	return c.doJSON(ctx, http.MethodGet, path, query, nil, http.StatusOK, out)
}

func (c *Client) doJSON(
	ctx context.Context,
	method string,
	path string,
	query map[string]string,
	body any,
	wantStatus int,
	out any,
) error {
	if !c.Configured() {
		return ErrNotConfigured
	}
	endpoint, err := url.Parse(c.baseURL + path)
	if err != nil {
		return fmt.Errorf("build authentik URL: %w", err)
	}
	values := endpoint.Query()
	for key, value := range query {
		values.Set(key, value)
	}
	endpoint.RawQuery = values.Encode()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode authentik request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reader)
	if err != nil {
		return fmt.Errorf("create authentik request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call authentik: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != wantStatus {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return &APIError{Status: resp.StatusCode}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 4<<20))
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("decode authentik response: %w", err)
	}
	return nil
}
