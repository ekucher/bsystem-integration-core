package authentikadmin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientLifecycle(t *testing.T) {
	t.Parallel()

	var sawPassword string
	var sawUpdateEmail string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("authorization header = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/core/users/":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"pagination": map[string]any{"next": 0},
				"results": []map[string]any{{
					"pk": 7, "username": "user.one", "name": "User One", "email": "one@example.invalid",
					"is_active": true, "type": "internal", "groups": []string{"group-1"},
					"groups_obj": []map[string]any{{"pk": "group-1", "name": "BSYSTEM-Support"}},
				}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/core/groups/":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"pagination": map[string]any{"next": 0},
				"results":    []map[string]any{{"pk": "group-1", "name": "BSYSTEM-Support"}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/core/users/":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode create body: %v", err)
			}
			if body["is_active"] != false || body["type"] != "internal" {
				t.Fatalf("unexpected create body: %#v", body)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"pk": 8, "username": body["username"], "name": body["name"], "email": body["email"],
				"is_active": false, "type": "internal", "groups": []string{"group-1"},
				"groups_obj": []map[string]any{{"pk": "group-1", "name": "BSYSTEM-Support"}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/core/users/8/set_password/":
			var body struct {
				Password string `json:"password"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode password body: %v", err)
			}
			sawPassword = body.Password
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v3/core/users/8/":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode update body: %v", err)
			}
			if value, ok := body["email"].(string); ok {
				sawUpdateEmail = value
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"pk": 8, "username": "new.user", "name": "New User", "email": "new@example.invalid",
				"is_active": body["is_active"], "type": "internal", "groups": []string{"group-1"},
				"groups_obj": []map[string]any{{"pk": "group-1", "name": "BSYSTEM-Support"}},
			})
		default:
			http.Error(w, "unexpected route", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := New(server.URL, "test-token")
	ctx := context.Background()

	users, err := client.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 || users[0].Username != "user.one" || len(users[0].GroupsObj) != 1 {
		t.Fatalf("unexpected users: %#v", users)
	}

	groups, err := client.ListGroups(ctx)
	if err != nil {
		t.Fatalf("ListGroups: %v", err)
	}
	if len(groups) != 1 || groups[0].Name != "BSYSTEM-Support" {
		t.Fatalf("unexpected groups: %#v", groups)
	}

	created, err := client.CreateUser(ctx, CreateUserInput{
		Username: "new.user",
		Name:     "New User",
		Email:    "new@example.invalid",
		GroupPK:  "group-1",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if created.PK != 8 || created.IsActive {
		t.Fatalf("unexpected created user: %#v", created)
	}

	if err := client.SetPassword(ctx, 8, "secret-value"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if sawPassword != "secret-value" {
		t.Fatalf("password was not forwarded")
	}

	active := true
	email := "updated@example.invalid"
	updated, err := client.UpdateUser(ctx, 8, nil, &active, &email)
	if err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if !updated.IsActive {
		t.Fatalf("user was not activated: %#v", updated)
	}
	if sawUpdateEmail != email {
		t.Fatalf("email was not forwarded: got %q want %q", sawUpdateEmail, email)
	}
}

func TestClientNotConfigured(t *testing.T) {
	t.Parallel()

	client := New("", "")
	if client.Configured() {
		t.Fatal("empty client must not be configured")
	}
	_, err := client.ListUsers(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("ListUsers error = %v", err)
	}
}

func TestClientDoesNotExposeResponseBodyInAPIError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal upstream secret", http.StatusForbidden)
	}))
	defer server.Close()

	client := New(server.URL, "test-token")
	_, err := client.ListUsers(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "internal upstream secret") {
		t.Fatalf("upstream body leaked through error: %v", err)
	}
	if err.Error() != "authentik API returned HTTP 403" {
		t.Fatalf("unexpected error: %v", err)
	}
}
