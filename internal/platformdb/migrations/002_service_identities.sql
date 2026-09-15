INSERT INTO global_id_counters (entity_type, prefix, next_value)
VALUES ('service', 'SVC', 1)
ON CONFLICT (entity_type) DO NOTHING;

CREATE TABLE IF NOT EXISTS service_identities (
    subject TEXT PRIMARY KEY,
    global_service_id TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    username TEXT NOT NULL DEFAULT '',
    groups_json JSONB NOT NULL DEFAULT '[]'::jsonb,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS adapter_registry (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    version TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'disabled',
    capabilities JSONB NOT NULL DEFAULT '[]'::jsonb,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_service_identities_global_service_id
    ON service_identities(global_service_id);
