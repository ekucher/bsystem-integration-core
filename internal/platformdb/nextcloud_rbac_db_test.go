package platformdb

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestNextcloudInitialRBACPolicy(t *testing.T) {
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	dsn := migrationDatabase(ctx, t, adminDSN)

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open freshly migrated database: %v", err)
	}
	defer db.Close()

	admin, err := db.ResolveAccess(ctx, []string{"BSYSTEM-Admins"}, "human")
	if err != nil {
		t.Fatalf("resolve administrator access: %v", err)
	}
	if !hasValue(admin.Modules, "nextcloud") {
		t.Fatalf("administrator modules = %v, want nextcloud", admin.Modules)
	}

	denied := []struct {
		name  string
		group string
	}{
		{"manager", "BSYSTEM-Managers"},
		{"developer", "BSYSTEM-Developers"},
		{"qa", "BSYSTEM-QA"},
		{"devops", "BSYSTEM-DevOps"},
		{"support", "BSYSTEM-Support"},
		{"customer", "BSYSTEM-Customers"},
	}

	for _, tc := range denied {
		t.Run(tc.name, func(t *testing.T) {
			access, err := db.ResolveAccess(ctx, []string{tc.group}, "human")
			if err != nil {
				t.Fatalf("resolve access: %v", err)
			}
			if hasValue(access.Modules, "nextcloud") {
				t.Fatalf("%s modules = %v, nextcloud must remain fail-closed", tc.name, access.Modules)
			}
		})
	}
}
