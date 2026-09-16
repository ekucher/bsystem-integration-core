-- Indexes for query paths that had none.
--
-- This is the result of reading every query in internal/platformdb against
-- the indexes that already exist, rather than adding indexes that sound
-- useful. Two candidates were dropped during that pass because the index
-- already existed under another name, and one because the column was already
-- a primary key — a redundant index is not free: it costs a write on every
-- insert and disk forever, in exchange for nothing.
--
-- What was checked and found already covered:
--   audit_events(occurred_at DESC)                  — 001
--   audit_events(subject, occurred_at DESC)         — 001
--   audit_events(resource_type, resource_id, ...)   — 001
--   service_identities(subject)                     — primary key
--   identities(subject)                             — primary key
--   global_entities(source, entity_type, source_id) — unique constraint
--   notifications, operations_events, servers       — added with their tables

-- The audit trail is indexed by authentik subject but not by Global user ID,
-- and the platform's own question is the second one: "what did USR-000004
-- do". The subject is what authentik calls them; the Global ID is what every
-- other record in the platform references.
CREATE INDEX IF NOT EXISTS idx_audit_events_actor
    ON audit_events(global_user_id, id DESC);

-- ListSupportRecords filters on kind and severity, which nothing covered.
CREATE INDEX IF NOT EXISTS idx_support_records_kind_severity
    ON support_records(kind, severity);

-- The open listing is the common read: a support queue is looked at to find
-- what is still being worked. A partial index keeps it small — it does not
-- grow with the closed history, which is the part that grows forever.
CREATE INDEX IF NOT EXISTS idx_support_records_open
    ON support_records(global_id) WHERE status NOT IN ('resolved','closed');

-- notification_reads is joined on both columns for every notification read,
-- but its primary key is (notification_id, global_user_id) and cannot serve a
-- lookup by reader alone. "My unread notifications" is exactly that lookup,
-- and it runs on every page load of the HUB's badge.
CREATE INDEX IF NOT EXISTS idx_notification_reads_reader
    ON notification_reads(global_user_id, notification_id);
