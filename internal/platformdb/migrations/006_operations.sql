-- Operations: servers and what is reported about them.
--
-- The platform mirrors infrastructure it does not own. An operations toolkit
-- reports; the platform normalizes, stores, publishes and notifies. There is
-- deliberately no way to reach a server from these tables — no hostname, no
-- address — because the platform never connects to one, and an inventory of
-- reachable addresses is internal topology that should not accumulate
-- somewhere more people can read than can log in.
CREATE TABLE IF NOT EXISTS servers (
    global_id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    environment TEXT NOT NULL DEFAULT 'unknown'
        CHECK (environment IN ('dev','stage','prod','unknown')),
    status TEXT NOT NULL DEFAULT 'unknown'
        CHECK (status IN ('ok','warning','error','maintenance','offline','unknown')),
    -- Optional relations. They are Global IDs rather than upstream keys, and
    -- are verified to resolve before a server is stored: a server attached to
    -- the wrong customer is worse than one attached to none.
    client_id TEXT,
    project_id TEXT,
    source TEXT NOT NULL,
    source_id TEXT NOT NULL,
    last_event_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source, source_id)
);

CREATE INDEX IF NOT EXISTS idx_servers_client ON servers(client_id) WHERE client_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_servers_status ON servers(status);

CREATE TABLE IF NOT EXISTS operations_events (
    id BIGSERIAL PRIMARY KEY,
    server_id TEXT NOT NULL REFERENCES servers(global_id) ON DELETE CASCADE,
    event TEXT NOT NULL,
    severity TEXT NOT NULL CHECK (severity IN ('debug','info','warning','error','critical')),
    -- Only a short human summary is stored. A reporter's full payload can
    -- carry credentials, addresses and command output, and this table is read
    -- by everyone holding operations.server.read.
    summary TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL,
    correlation_id TEXT NOT NULL DEFAULT '',
    occurred_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Both indexes are ordered by id descending, which is the order the API
-- serves and the direction its keyset cursor walks.
CREATE INDEX IF NOT EXISTS idx_operations_events_server ON operations_events(server_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_operations_events_event ON operations_events(event, id DESC);

-- Reporting is a machine capability. No human role receives it: a person does
-- not assert that a backup failed, a reporter does.
INSERT INTO permissions (id,description) VALUES
('operations.report','Report server registrations and operations events')
ON CONFLICT (id) DO UPDATE SET description=EXCLUDED.description;

INSERT INTO role_permissions (role_id,permission_id) VALUES
('service-core','operations.report')
ON CONFLICT DO NOTHING;
