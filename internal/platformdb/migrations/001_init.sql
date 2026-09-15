CREATE TABLE IF NOT EXISTS identities (
    subject TEXT PRIMARY KEY,
    global_user_id TEXT NOT NULL UNIQUE,
    email TEXT NOT NULL DEFAULT '',
    display_name TEXT NOT NULL DEFAULT '',
    username TEXT NOT NULL DEFAULT '',
    groups_json JSONB NOT NULL DEFAULT '[]'::jsonb,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS modules (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'planned',
    sort_order INTEGER NOT NULL DEFAULT 100,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS global_id_counters (
    entity_type TEXT PRIMARY KEY,
    prefix TEXT NOT NULL UNIQUE,
    next_value BIGINT NOT NULL DEFAULT 1 CHECK (next_value > 0)
);

CREATE TABLE IF NOT EXISTS global_entities (
    global_id TEXT PRIMARY KEY,
    entity_type TEXT NOT NULL,
    source TEXT NOT NULL,
    source_id TEXT NOT NULL,
    tenant_id TEXT,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source, entity_type, source_id)
);

CREATE INDEX IF NOT EXISTS idx_global_entities_type ON global_entities(entity_type);
CREATE INDEX IF NOT EXISTS idx_global_entities_tenant ON global_entities(tenant_id) WHERE tenant_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS audit_events (
    id BIGSERIAL PRIMARY KEY,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    subject TEXT NOT NULL DEFAULT '',
    global_user_id TEXT NOT NULL DEFAULT '',
    action TEXT NOT NULL,
    resource_type TEXT NOT NULL DEFAULT '',
    resource_id TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL DEFAULT '',
    source_ip TEXT NOT NULL DEFAULT '',
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX IF NOT EXISTS idx_audit_events_time ON audit_events(occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_events_subject ON audit_events(subject, occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_events_resource ON audit_events(resource_type, resource_id, occurred_at DESC);

INSERT INTO modules (id, name, description, status, sort_order) VALUES
('crm', 'BSYSTEM CRM', 'Клієнти, контакти, договори та сервіси', 'planned', 10),
('projects', 'BSYSTEM Projects', 'Проєкти й задачі Redmine', 'planned', 20),
('qa', 'BSYSTEM QA', 'Тест-кейси, запуски та дефекти', 'planned', 30),
('development', 'BSYSTEM Development', 'Репозиторії, CI/CD та релізи', 'planned', 40),
('wiki', 'BSYSTEM Wiki', 'Документація та Runbooks', 'planned', 50),
('operations', 'BSYSTEM Operations', 'BRAVO, сервери, backup та події', 'planned', 60),
('support', 'BSYSTEM Support', 'Звернення, інциденти та SLA', 'planned', 70)
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name,
    description = EXCLUDED.description,
    sort_order = EXCLUDED.sort_order,
    updated_at = now();

INSERT INTO global_id_counters (entity_type, prefix, next_value) VALUES
('user', 'USR', 1),
('client', 'CL', 1),
('contact', 'CT', 1),
('project', 'PR', 1),
('task', 'TSK', 1),
('server', 'SRV', 1),
('application', 'APP', 1),
('incident', 'INC', 1),
('test_case', 'TST', 1),
('bug', 'BUG', 1),
('document', 'DOC', 1),
('release', 'REL', 1),
('repository', 'REP', 1)
ON CONFLICT (entity_type) DO NOTHING;
