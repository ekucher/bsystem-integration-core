-- Notifications.
--
-- A notification is addressed either to one named Global user ID or to a
-- permission. The permission form exists because BSYSTEM has no authoritative
-- mapping from an operational event to a person: who should hear that a backup
-- failed is answered by "whoever may read server state", and that set is
-- resolved from RBAC when the notification is read rather than frozen when it
-- is raised. Revoking someone's access therefore also removes the
-- notifications it entitled them to see.
CREATE TABLE IF NOT EXISTS notifications (
    id BIGSERIAL PRIMARY KEY,
    event TEXT NOT NULL,
    source TEXT NOT NULL,
    severity TEXT NOT NULL CHECK (severity IN ('debug','info','warning','error','critical')),
    title TEXT NOT NULL,
    body TEXT NOT NULL DEFAULT '',
    deep_link TEXT NOT NULL DEFAULT '',
    entity_id TEXT NOT NULL DEFAULT '',
    tenant_id TEXT NOT NULL DEFAULT '',
    correlation_id TEXT NOT NULL DEFAULT '',
    recipient_id TEXT,
    -- The foreign key is what stops a notification being addressed to a
    -- permission that does not exist, which would make it unreachable.
    audience_permission TEXT REFERENCES permissions(id) ON DELETE CASCADE,
    occurred_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT notifications_addressed_exactly_once
        CHECK (num_nonnulls(recipient_id, audience_permission) = 1)
);

-- Both indexes are partial and ordered by id descending, which is the order
-- the API serves and the direction its keyset cursor walks.
CREATE INDEX IF NOT EXISTS idx_notifications_audience
    ON notifications(audience_permission, id DESC) WHERE audience_permission IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_notifications_recipient
    ON notifications(recipient_id, id DESC) WHERE recipient_id IS NOT NULL;

-- Read state is per user rather than per notification: an audience
-- notification is read by one person without disappearing for the rest.
CREATE TABLE IF NOT EXISTS notification_reads (
    notification_id BIGINT NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
    global_user_id TEXT NOT NULL,
    read_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (notification_id, global_user_id)
);

INSERT INTO permissions (id,description) VALUES
('notifications.publish','Raise platform notifications')
ON CONFLICT (id) DO UPDATE SET description=EXCLUDED.description;

-- Only a service identity raises notifications. No human role gets it: a
-- notification is a statement by the platform about something that happened,
-- not a message one user sends another.
INSERT INTO role_permissions (role_id,permission_id) VALUES
('service-core','notifications.publish')
ON CONFLICT DO NOTHING;
