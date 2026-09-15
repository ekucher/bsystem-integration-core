package platformdb

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func hasValue(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func TestPersistenceLifecycle(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()

	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	subject := "test-subject-" + suffix
	globalUserID, created, err := db.EnsureIdentity(ctx, Identity{Subject: subject, Email: "test@example.invalid", Username: "test-user", Groups: []string{"BSYSTEM-Developers"}})
	if err != nil {
		t.Fatalf("ensure identity: %v", err)
	}
	if !created || !strings.HasPrefix(globalUserID, "USR-") {
		t.Fatalf("unexpected identity allocation: id=%q created=%v", globalUserID, created)
	}

	again, createdAgain, err := db.EnsureIdentity(ctx, Identity{Subject: subject, Email: "changed@example.invalid", Username: "test-user", Groups: []string{"BSYSTEM-QA"}})
	if err != nil {
		t.Fatalf("ensure identity second time: %v", err)
	}
	if createdAgain || again != globalUserID {
		t.Fatalf("identity Global ID is not stable: first=%q second=%q created=%v", globalUserID, again, createdAgain)
	}

	serviceSubject := "service-subject-" + suffix
	globalServiceID, serviceCreated, err := db.EnsureServiceIdentity(ctx, ServiceIdentity{Subject: serviceSubject, Name: "CI Service", Username: "svc-ci", Groups: []string{"BSYSTEM-Services"}})
	if err != nil {
		t.Fatalf("ensure service identity: %v", err)
	}
	if !serviceCreated || !strings.HasPrefix(globalServiceID, "SVC-") {
		t.Fatalf("unexpected service identity allocation: id=%q created=%v", globalServiceID, serviceCreated)
	}
	serviceAgain, serviceCreatedAgain, err := db.EnsureServiceIdentity(ctx, ServiceIdentity{Subject: serviceSubject, Name: "CI Service Renamed", Username: "svc-ci", Groups: []string{"BSYSTEM-Services"}})
	if err != nil {
		t.Fatalf("ensure service identity second time: %v", err)
	}
	if serviceCreatedAgain || serviceAgain != globalServiceID {
		t.Fatalf("service Global ID is not stable: first=%q second=%q created=%v", globalServiceID, serviceAgain, serviceCreatedAgain)
	}

	developer, err := db.ResolveAccess(ctx, []string{"BSYSTEM-Developers"}, "human")
	if err != nil {
		t.Fatalf("resolve developer access: %v", err)
	}
	if !hasValue(developer.Roles, "Developer") || !hasValue(developer.Permissions, "development.pr.write") || !hasValue(developer.Modules, "development") {
		t.Fatalf("unexpected developer access: %#v", developer)
	}

	admin, err := db.ResolveAccess(ctx, []string{"BSYSTEM-Admins"}, "human")
	if err != nil {
		t.Fatalf("resolve administrator access: %v", err)
	}
	if !hasValue(admin.Roles, "Administrator") || !hasValue(admin.Permissions, "*") {
		t.Fatalf("unexpected administrator access: %#v", admin)
	}

	serviceAccess, err := db.ResolveAccess(ctx, []string{"BSYSTEM-Services"}, "service")
	if err != nil {
		t.Fatalf("resolve service access: %v", err)
	}
	if !hasValue(serviceAccess.Roles, "Service Core") || !hasValue(serviceAccess.Permissions, "events.publish") {
		t.Fatalf("unexpected service access: %#v", serviceAccess)
	}

	grant := ScopeGrant{PrincipalType: "user", PrincipalID: globalUserID, ScopeType: "project", ScopeID: "PR-000123", PermissionID: "projects.task.read"}
	if err := db.AddScopeGrant(ctx, grant); err != nil {
		t.Fatalf("add scope grant: %v", err)
	}
	grants, err := db.ListScopeGrants(ctx, "user", globalUserID)
	if err != nil {
		t.Fatalf("list scope grants: %v", err)
	}
	if len(grants) == 0 {
		t.Fatal("expected scope grant")
	}
	allowed, err := db.HasScopedPermission(ctx, "user", globalUserID, "project", "PR-000123", "projects.task.read")
	if err != nil {
		t.Fatalf("check scoped permission: %v", err)
	}
	if !allowed {
		t.Fatal("expected scoped permission to be allowed")
	}

	entity, err := db.CreateGlobalEntity(ctx, "client", "test", "source-"+suffix, "", map[string]any{"test": true})
	if err != nil {
		t.Fatalf("create Global ID: %v", err)
	}
	if !strings.HasPrefix(entity.GlobalID, "CL-") {
		t.Fatalf("unexpected client Global ID: %s", entity.GlobalID)
	}
	resolved, err := db.ResolveGlobalEntity(ctx, entity.GlobalID)
	if err != nil {
		t.Fatalf("resolve Global ID: %v", err)
	}
	if resolved.SourceID != entity.SourceID {
		t.Fatalf("resolved wrong entity: %#v", resolved)
	}

	if err := db.InsertAudit(ctx, AuditEvent{Subject: subject, GlobalUserID: globalUserID, Action: "test.event", ResourceType: "client", ResourceID: entity.GlobalID, RequestID: "test-request"}); err != nil {
		t.Fatalf("insert audit: %v", err)
	}
	events, err := db.ListAudit(ctx, 10)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("expected at least one audit event")
	}
}
