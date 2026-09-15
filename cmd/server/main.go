package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

type healthResponse struct {
	Status    string `json:"status"`
	Service   string `json:"service"`
	Version   string `json:"version"`
	Timestamp string `json:"timestamp"`
}

type userInfo struct {
	Sub               string   `json:"sub"`
	Email             string   `json:"email"`
	Name              string   `json:"name"`
	PreferredUsername string   `json:"preferred_username"`
	Groups            []string `json:"groups"`
}

type meResponse struct {
	ID          string   `json:"id"`
	Subject     string   `json:"subject"`
	Email       string   `json:"email"`
	Name        string   `json:"name"`
	Username    string   `json:"username"`
	Groups      []string `json:"groups"`
	Roles       []string `json:"roles"`
	Permissions []string `json:"permissions"`
	Modules     []string `json:"modules"`
}

type module struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"`
}

var allModules = []module{
	{ID: "crm", Name: "BSYSTEM CRM", Description: "Клієнти, контакти, договори та сервіси", Status: "planned"},
	{ID: "projects", Name: "BSYSTEM Projects", Description: "Проєкти й задачі Redmine", Status: "planned"},
	{ID: "qa", Name: "BSYSTEM QA", Description: "Тест-кейси, запуски та дефекти", Status: "planned"},
	{ID: "development", Name: "BSYSTEM Development", Description: "Репозиторії, CI/CD та релізи", Status: "planned"},
	{ID: "wiki", Name: "BSYSTEM Wiki", Description: "Документація та Runbooks", Status: "planned"},
	{ID: "operations", Name: "BSYSTEM Operations", Description: "BRAVO, сервери, backup та події", Status: "planned"},
	{ID: "support", Name: "BSYSTEM Support", Description: "Звернення, інциденти та SLA", Status: "planned"},
}

var groupRoles = map[string]string{
	"BSYSTEM-Admins":     "Administrator",
	"BSYSTEM-Managers":   "Manager",
	"BSYSTEM-Developers": "Developer",
	"BSYSTEM-QA":         "QA",
	"BSYSTEM-Support":    "Support",
	"BSYSTEM-DevOps":     "DevOps",
	"BSYSTEM-Customers":  "Customer",
}

var rolePermissions = map[string][]string{
	"Administrator": {"*"},
	"Manager":       {"crm.client.read", "projects.task.read", "qa.report.read", "wiki.document.read", "operations.server.read", "support.incident.read"},
	"Developer":     {"projects.task.read", "projects.task.edit", "development.repo.read", "development.pr.write", "qa.testcase.read", "wiki.document.read", "wiki.document.edit", "operations.server.read"},
	"QA":            {"projects.task.read", "qa.testcase.read", "qa.testcase.execute", "qa.bug.write", "wiki.document.read"},
	"Support":       {"crm.client.read", "projects.task.read", "wiki.document.read", "operations.server.read", "support.incident.read", "support.incident.write"},
	"DevOps":        {"development.repo.read", "wiki.document.read", "wiki.document.edit", "operations.server.read", "operations.server.manage"},
	"Customer":      {"portal.read", "wiki.document.read", "support.incident.read"},
}

var roleModules = map[string][]string{
	"Administrator": {"crm", "projects", "qa", "development", "wiki", "operations", "support"},
	"Manager":       {"crm", "projects", "qa", "wiki", "operations", "support"},
	"Developer":     {"projects", "qa", "development", "wiki", "operations"},
	"QA":            {"projects", "qa", "wiki"},
	"Support":       {"crm", "projects", "wiki", "operations", "support"},
	"DevOps":        {"development", "wiki", "operations"},
	"Customer":      {"wiki", "support"},
}

type contextKey string

const userContextKey contextKey = "user"

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = time.Now().UTC().Format("20060102T150405.000000000Z07:00")
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r)
	})
}

func fetchUserInfo(ctx context.Context, token string) (userInfo, error) {
	endpoint := os.Getenv("AUTHENTIK_USERINFO_URL")
	if endpoint == "" {
		return userInfo{}, errors.New("AUTHENTIK_USERINFO_URL is not configured")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return userInfo{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return userInfo{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return userInfo{}, errors.New("token rejected by authentik")
	}

	var info userInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return userInfo{}, err
	}
	if info.Sub == "" {
		return userInfo{}, errors.New("userinfo response has no subject")
	}
	return info, nil
}

func authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing bearer token"})
			return
		}

		info, err := fetchUserInfo(r.Context(), strings.TrimPrefix(header, "Bearer "))
		if err != nil {
			log.Printf("authentication failed: %v", err)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or expired token"})
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userContextKey, info)))
	})
}

func unique(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func resolveAccess(info userInfo) meResponse {
	roles := []string{}
	permissions := []string{}
	modules := []string{}

	for _, group := range info.Groups {
		role, ok := groupRoles[group]
		if !ok {
			continue
		}
		roles = append(roles, role)
		permissions = append(permissions, rolePermissions[role]...)
		modules = append(modules, roleModules[role]...)
	}

	username := info.PreferredUsername
	if username == "" {
		username = info.Email
	}

	return meResponse{
		ID:          "USR-" + info.Sub,
		Subject:     info.Sub,
		Email:       info.Email,
		Name:        info.Name,
		Username:    username,
		Groups:      unique(info.Groups),
		Roles:       unique(roles),
		Permissions: unique(permissions),
		Modules:     unique(modules),
	}
}

func main() {
	publicMux := http.NewServeMux()
	publicMux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, healthResponse{
			Status:    "ok",
			Service:   "bsystem-integration-core",
			Version:   "0.2.0",
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		})
	})

	protectedMux := http.NewServeMux()
	protectedMux.HandleFunc("GET /api/v1/me", func(w http.ResponseWriter, r *http.Request) {
		info := r.Context().Value(userContextKey).(userInfo)
		writeJSON(w, http.StatusOK, resolveAccess(info))
	})
	protectedMux.HandleFunc("GET /api/v1/modules", func(w http.ResponseWriter, r *http.Request) {
		info := r.Context().Value(userContextKey).(userInfo)
		access := resolveAccess(info)
		allowed := map[string]bool{}
		for _, id := range access.Modules {
			allowed[id] = true
		}
		modules := []module{}
		for _, item := range allModules {
			if allowed[item.ID] {
				modules = append(modules, item)
			}
		}
		writeJSON(w, http.StatusOK, modules)
	})

	root := http.NewServeMux()
	root.Handle("/api/", authenticate(protectedMux))
	root.Handle("/health", publicMux)

	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           requestID(root),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("bsystem-integration-core listening on %s", addr)
	log.Fatal(server.ListenAndServe())
}
