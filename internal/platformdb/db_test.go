package platformdb

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

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
