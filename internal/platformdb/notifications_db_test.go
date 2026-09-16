package platformdb

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/notifications"
)

// storeFixture opens a throwaway migrated database for one test.
//
// A database per test rather than a shared one: these tests assert counts over
// "everything visible to this reader", and a row another test left behind
// would make an assertion pass or fail for reasons that have nothing to do
// with the code under test.
func storeFixture(t *testing.T) (context.Context, *DB) {
	t.Helper()
	adminDSN := os.Getenv("TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	db, err := Open(ctx, migrationDatabase(ctx, t, adminDSN))
	if err != nil {
		t.Fatalf("open store fixture: %v", err)
	}
	t.Cleanup(db.Close)
	return ctx, db
}

func raise(t *testing.T, ctx context.Context, db *DB, n notifications.Notification) int64 {
	t.Helper()
	id, err := db.InsertNotification(ctx, n, time.Now().UTC())
	if err != nil {
		t.Fatalf("insert notification: %v", err)
	}
	return id
}

func visibleIDs(t *testing.T, ctx context.Context, db *DB, q NotificationQuery) []int64 {
	t.Helper()
	if q.Limit == 0 {
		q.Limit = 50
	}
	items, _, _, err := db.ListNotifications(ctx, q)
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func contains(ids []int64, wanted int64) bool {
	for _, id := range ids {
		if id == wanted {
			return true
		}
	}
	return false
}

// A notification addressed to one person by name is that person's. This is the
// property the whole visibility predicate exists to hold, and the one whose
// failure would be a disclosure rather than a bug.
func TestPersonalNotificationsAreNotVisibleToOthers(t *testing.T) {
	ctx, db := storeFixture(t)

	mine := raise(t, ctx, db, notifications.Notification{
		Event: "task.completed", Source: "redmine", Severity: "info",
		Title: "yours", RecipientID: "USR-000001",
	})
	theirs := raise(t, ctx, db, notifications.Notification{
		Event: "task.completed", Source: "redmine", Severity: "info",
		Title: "theirs", RecipientID: "USR-000002",
	})

	ids := visibleIDs(t, ctx, db, NotificationQuery{GlobalUserID: "USR-000001"})
	if !contains(ids, mine) {
		t.Fatal("a reader must see a notification addressed to them")
	}
	if contains(ids, theirs) {
		t.Fatal("a reader must not see a notification addressed to someone else")
	}
}

// An administrator reads every audience. That is not the same as reading
// everyone's personal mail, and the predicate distinguishes them.
func TestAdministratorReadsEveryAudienceButNotPersonalNotifications(t *testing.T) {
	ctx, db := storeFixture(t)

	audience := raise(t, ctx, db, notifications.Notification{
		Event: "server.offline", Source: "operations", Severity: "critical",
		Title: "addressed to a role", AudiencePermission: "operations.server.read",
	})
	personal := raise(t, ctx, db, notifications.Notification{
		Event: "task.completed", Source: "redmine", Severity: "info",
		Title: "addressed to a person", RecipientID: "USR-000002",
	})

	ids := visibleIDs(t, ctx, db, NotificationQuery{GlobalUserID: "USR-000001", AllAudiences: true})
	if !contains(ids, audience) {
		t.Fatal("an administrator must read every audience")
	}
	if contains(ids, personal) {
		t.Fatal("reading every audience must not mean reading another person's notifications")
	}
}

// Holding the permission is what makes an audience notification readable.
func TestAudienceNotificationsFollowThePermission(t *testing.T) {
	ctx, db := storeFixture(t)

	id := raise(t, ctx, db, notifications.Notification{
		Event: "test.failed", Source: "qa", Severity: "warning",
		Title: "for QA", AudiencePermission: "qa.testcase.read",
	})

	holder := visibleIDs(t, ctx, db, NotificationQuery{
		GlobalUserID: "USR-000003", AudiencePermissions: []string{"qa.testcase.read"},
	})
	if !contains(holder, id) {
		t.Fatal("a permission holder must see the notification addressed to it")
	}

	other := visibleIDs(t, ctx, db, NotificationQuery{
		GlobalUserID: "USR-000004", AudiencePermissions: []string{"crm.client.read"},
	})
	if contains(other, id) {
		t.Fatal("a different permission must not grant visibility")
	}

	// A principal with no permissions at all — a scope-confined customer —
	// sees only what names them. nil and an empty slice must behave the same:
	// the query rewrites nil, and a difference between them would be an
	// authorization decision made by a nil check.
	for name, permissions := range map[string][]string{"nil": nil, "empty": {}} {
		if got := visibleIDs(t, ctx, db, NotificationQuery{
			GlobalUserID: "USR-000005", AudiencePermissions: permissions,
		}); contains(got, id) {
			t.Fatalf("a principal holding no permissions (%s) must see nothing addressed to a role", name)
		}
	}
}

// The counts describe the visible collection, not the returned page. A badge
// built on a page-sized count is wrong the moment there is a second page.
func TestCountsDescribeTheWholeVisibleCollection(t *testing.T) {
	ctx, db := storeFixture(t)

	for i := 0; i < 5; i++ {
		raise(t, ctx, db, notifications.Notification{
			Event: "task.completed", Source: "redmine", Severity: "info",
			Title: "mine", RecipientID: "USR-000001",
		})
	}
	raise(t, ctx, db, notifications.Notification{
		Event: "task.completed", Source: "redmine", Severity: "info",
		Title: "someone else's", RecipientID: "USR-000002",
	})

	items, total, unread, err := db.ListNotifications(ctx, NotificationQuery{
		GlobalUserID: "USR-000001", Limit: 2,
	})
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected a page of 2, got %d", len(items))
	}
	if total != 5 || unread != 5 {
		t.Fatalf("counts must describe all five visible notifications, got total=%d unread=%d", total, unread)
	}
}

// Keyset paging must neither skip nor repeat a row.
func TestPagingWalksEveryNotificationExactlyOnce(t *testing.T) {
	ctx, db := storeFixture(t)

	const count = 7
	raised := make(map[int64]bool, count)
	for i := 0; i < count; i++ {
		raised[raise(t, ctx, db, notifications.Notification{
			Event: "task.completed", Source: "redmine", Severity: "info",
			Title: "mine", RecipientID: "USR-000001",
		})] = true
	}

	seen := map[int64]int{}
	var after int64
	for page := 0; page < count+2; page++ {
		items, _, _, err := db.ListNotifications(ctx, NotificationQuery{
			GlobalUserID: "USR-000001", AfterID: after, Limit: 3,
		})
		if err != nil {
			t.Fatalf("list notifications: %v", err)
		}
		if len(items) == 0 {
			break
		}
		for _, item := range items {
			seen[item.ID]++
			after = item.ID
		}
	}

	if len(seen) != count {
		t.Fatalf("walked %d notifications, raised %d", len(seen), count)
	}
	for id, times := range seen {
		if times != 1 {
			t.Fatalf("notification %d appeared %d times", id, times)
		}
		if !raised[id] {
			t.Fatalf("notification %d was never raised by this test", id)
		}
	}
}

// Marking read is itself an authorization boundary: an id a reader cannot see
// must be refused, and must stay unread.
func TestMarkingAnInvisibleNotificationIsRefusedAndChangesNothing(t *testing.T) {
	ctx, db := storeFixture(t)

	theirs := raise(t, ctx, db, notifications.Notification{
		Event: "task.completed", Source: "redmine", Severity: "info",
		Title: "theirs", RecipientID: "USR-000002",
	})

	intruder := NotificationQuery{GlobalUserID: "USR-000001"}
	if err := db.MarkNotificationRead(ctx, theirs, intruder); !errors.Is(err, ErrNotificationNotVisible) {
		t.Fatalf("expected ErrNotificationNotVisible, got %v", err)
	}

	// An unknown id must be refused the same way, so that a caller cannot
	// tell an id that exists from one that does not.
	if err := db.MarkNotificationRead(ctx, 999999, intruder); !errors.Is(err, ErrNotificationNotVisible) {
		t.Fatalf("an unknown id must be indistinguishable from an invisible one, got %v", err)
	}

	// And the owner's own view is untouched by the attempt.
	_, _, unread, err := db.ListNotifications(ctx, NotificationQuery{GlobalUserID: "USR-000002", Limit: 10})
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	if unread != 1 {
		t.Fatalf("the refused mark-read must leave the notification unread, unread=%d", unread)
	}
}

func TestMarkingReadIsIdempotentAndFiltersUnreadOnly(t *testing.T) {
	ctx, db := storeFixture(t)

	reader := NotificationQuery{GlobalUserID: "USR-000001", Limit: 10}
	first := raise(t, ctx, db, notifications.Notification{
		Event: "task.completed", Source: "redmine", Severity: "info",
		Title: "one", RecipientID: "USR-000001",
	})
	raise(t, ctx, db, notifications.Notification{
		Event: "task.completed", Source: "redmine", Severity: "info",
		Title: "two", RecipientID: "USR-000001",
	})

	if err := db.MarkNotificationRead(ctx, first, reader); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	// Twice is not an error. A client that retries after a dropped response
	// must not be told the notification vanished.
	if err := db.MarkNotificationRead(ctx, first, reader); err != nil {
		t.Fatalf("marking read twice must be accepted, got %v", err)
	}

	_, total, unread, err := db.ListNotifications(ctx, reader)
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	if total != 2 || unread != 1 {
		t.Fatalf("expected total=2 unread=1, got total=%d unread=%d", total, unread)
	}

	unreadOnly := reader
	unreadOnly.UnreadOnly = true
	if ids := visibleIDs(t, ctx, db, unreadOnly); contains(ids, first) {
		t.Fatal("UnreadOnly must exclude a notification this reader has read")
	}
}

// Read state is per reader. One person reading an audience notification must
// not clear it for everyone holding that permission.
func TestReadStateIsPerReader(t *testing.T) {
	ctx, db := storeFixture(t)

	id := raise(t, ctx, db, notifications.Notification{
		Event: "server.offline", Source: "operations", Severity: "critical",
		Title: "for operations", AudiencePermission: "operations.server.read",
	})
	holders := []string{"USR-000001", "USR-000002"}
	query := func(user string) NotificationQuery {
		return NotificationQuery{
			GlobalUserID: user, AudiencePermissions: []string{"operations.server.read"}, Limit: 10,
		}
	}

	if err := db.MarkNotificationRead(ctx, id, query(holders[0])); err != nil {
		t.Fatalf("mark read: %v", err)
	}

	_, _, unreadFirst, err := db.ListNotifications(ctx, query(holders[0]))
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	_, _, unreadSecond, err := db.ListNotifications(ctx, query(holders[1]))
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	if unreadFirst != 0 {
		t.Fatalf("the reader who marked it read has unread=%d", unreadFirst)
	}
	if unreadSecond != 1 {
		t.Fatalf("another holder must still have it unread, got %d", unreadSecond)
	}
}

// Addressing a notification to an identity or permission that does not exist
// makes it unreadable, so the platform refuses it rather than accepting and
// losing it. These two guards are what that refusal is built on.
func TestExistenceGuardsAnswerForKnownAndUnknownSubjects(t *testing.T) {
	ctx, db := storeFixture(t)

	exists, err := db.IdentityExists(ctx, "USR-DOES-NOT-EXIST")
	if err != nil {
		t.Fatalf("identity exists: %v", err)
	}
	if exists {
		t.Fatal("an unknown Global user ID must not report as existing")
	}

	globalUserID, _, err := db.EnsureIdentity(ctx, Identity{
		Subject: "subject-" + strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", ""),
		Email:   "reader@example.invalid", Username: "reader",
	})
	if err != nil {
		t.Fatalf("ensure identity: %v", err)
	}
	if exists, err = db.IdentityExists(ctx, globalUserID); err != nil || !exists {
		t.Fatalf("a known identity must report as existing (exists=%v err=%v)", exists, err)
	}

	if exists, err = db.PermissionExists(ctx, "crm.client.read"); err != nil || !exists {
		t.Fatalf("a seeded permission must report as existing (exists=%v err=%v)", exists, err)
	}
	if exists, err = db.PermissionExists(ctx, "not.a.permission"); err != nil || exists {
		t.Fatalf("an undefined permission must not report as existing (exists=%v err=%v)", exists, err)
	}
}
