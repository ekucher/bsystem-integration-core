package platformdb

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/notifications"
)

// NotificationQuery is one caller's view of the notification store.
//
// The audience fields are the authorization boundary, so they are named
// rather than derived here: the handler resolves them from the same principal
// the rest of the platform authorizes against, and this file only applies
// them.
type NotificationQuery struct {
	// GlobalUserID is the reader. A notification addressed to them
	// personally is always visible.
	GlobalUserID string
	// AllAudiences is set for an administrator, who may read notifications
	// addressed to any permission.
	AllAudiences bool
	// AudiencePermissions are the permissions the reader holds. Empty means
	// they see only what is addressed to them by name, which is what a
	// scope-confined principal gets.
	AudiencePermissions []string
	// UnreadOnly filters to notifications this reader has not marked read.
	UnreadOnly bool
	// AfterID walks the collection: rows strictly older than it. Zero starts
	// at the newest.
	AfterID int64
	Limit   int
}

// ErrNotificationNotVisible means the notification does not exist, or exists
// and is not addressed to this reader. The two are deliberately the same
// error: distinguishing them would let a caller enumerate notification ids
// belonging to other people.
var ErrNotificationNotVisible = errors.New("notification not visible")

// visibility is the predicate that decides what a reader may see. It appears
// once and is used by every statement in this file, because a listing that
// hides a notification while a mark-read call accepts it is a disclosure.
//
// $1 is the reader, $2 whether every audience is readable, $3 the readable
// permissions.
const visibility = `(
    n.recipient_id = $1
 OR ($2 AND n.audience_permission IS NOT NULL)
 OR (NOT $2 AND n.audience_permission = ANY($3))
)`

const notificationColumns = `
 n.id, n.event, n.source, n.severity, n.title, n.body, n.deep_link,
 n.entity_id, n.tenant_id, COALESCE(n.recipient_id,''),
 COALESCE(n.audience_permission,''), n.correlation_id, n.occurred_at,
 (r.notification_id IS NOT NULL) AS read`

// InsertNotification stores a notification and returns its id.
func (db *DB) InsertNotification(ctx context.Context, n notifications.Notification, occurredAt time.Time) (int64, error) {
	recipient := nullable(n.RecipientID)
	audience := nullable(n.AudiencePermission)
	var id int64
	err := db.pool.QueryRow(ctx, `
INSERT INTO notifications
 (event,source,severity,title,body,deep_link,entity_id,tenant_id,correlation_id,recipient_id,audience_permission,occurred_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
RETURNING id`,
		n.Event, n.Source, n.Severity, n.Title, n.Body, n.DeepLink,
		n.EntityID, n.TenantID, n.CorrelationID, recipient, audience, occurredAt.UTC()).Scan(&id)
	return id, err
}

// ListNotifications returns one page, newest first, with the total and unread
// counts of everything visible to this reader.
//
// The counts describe the whole visible collection rather than the page,
// because that is what a badge needs and what a caller would otherwise have
// to page through the entire history to compute.
func (db *DB) ListNotifications(ctx context.Context, q NotificationQuery) (items []notifications.Notification, total, unread int, err error) {
	permissions := q.AudiencePermissions
	if permissions == nil {
		permissions = []string{}
	}

	err = db.pool.QueryRow(ctx, `
SELECT
 COUNT(*),
 COUNT(*) FILTER (WHERE r.notification_id IS NULL)
FROM notifications n
LEFT JOIN notification_reads r ON r.notification_id = n.id AND r.global_user_id = $1
WHERE `+visibility, q.GlobalUserID, q.AllAudiences, permissions).Scan(&total, &unread)
	if err != nil {
		return nil, 0, 0, err
	}

	rows, err := db.pool.Query(ctx, `
SELECT`+notificationColumns+`
FROM notifications n
LEFT JOIN notification_reads r ON r.notification_id = n.id AND r.global_user_id = $1
WHERE `+visibility+`
  AND ($4 = 0 OR n.id < $4)
  AND (NOT $5 OR r.notification_id IS NULL)
ORDER BY n.id DESC
LIMIT $6`, q.GlobalUserID, q.AllAudiences, permissions, q.AfterID, q.UnreadOnly, q.Limit)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()

	items = []notifications.Notification{}
	for rows.Next() {
		var n notifications.Notification
		var occurredAt time.Time
		if err := rows.Scan(&n.ID, &n.Event, &n.Source, &n.Severity, &n.Title, &n.Body, &n.DeepLink,
			&n.EntityID, &n.TenantID, &n.RecipientID, &n.AudiencePermission, &n.CorrelationID,
			&occurredAt, &n.Read); err != nil {
			return nil, 0, 0, err
		}
		n.OccurredAt = occurredAt.UTC().Format(time.RFC3339)
		items = append(items, n)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, err
	}
	return items, total, unread, nil
}

// MarkNotificationRead records that this reader has read a notification they
// can see. A notification they cannot see reports ErrNotificationNotVisible,
// the same answer an unknown id gets.
//
// The visibility check is inside the INSERT rather than a separate SELECT, so
// there is no window in which a revoked permission is still honoured.
func (db *DB) MarkNotificationRead(ctx context.Context, id int64, q NotificationQuery) error {
	permissions := q.AudiencePermissions
	if permissions == nil {
		permissions = []string{}
	}
	tag, err := db.pool.Exec(ctx, `
INSERT INTO notification_reads (notification_id, global_user_id)
SELECT n.id, $1 FROM notifications n
WHERE n.id = $4 AND `+visibility+`
ON CONFLICT (notification_id, global_user_id) DO NOTHING`,
		q.GlobalUserID, q.AllAudiences, permissions, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Either the notification is not visible, or it was already read.
		// Only the first is an error, so the ambiguity is resolved by asking
		// whether the reader can see it at all.
		var visible bool
		if err := db.pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM notifications n WHERE n.id = $4 AND `+visibility+`)`,
			q.GlobalUserID, q.AllAudiences, permissions, id).Scan(&visible); err != nil {
			return err
		}
		if !visible {
			return ErrNotificationNotVisible
		}
	}
	return nil
}

// IdentityExists reports whether a Global user ID belongs to a known
// identity. A notification addressed to an unknown one would be unreadable,
// so raising it is refused rather than accepted and lost.
func (db *DB) IdentityExists(ctx context.Context, globalUserID string) (bool, error) {
	var exists bool
	err := db.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM identities WHERE global_user_id = $1)`, globalUserID).Scan(&exists)
	return exists, err
}

// PermissionExists reports whether a permission is defined in RBAC.
func (db *DB) PermissionExists(ctx context.Context, permission string) (bool, error) {
	var exists bool
	err := db.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM permissions WHERE id = $1)`, permission).Scan(&exists)
	return exists, err
}

func nullable(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
