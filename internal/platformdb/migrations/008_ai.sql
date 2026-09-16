-- AI gateway.
--
-- ai.query is granted to no role here. Who may spend money on a model, and
-- whose data may be put in front of one, is an owner decision about cost and
-- exposure rather than a technical default. Administrators reach it through
-- the wildcard; everyone else needs an explicit grant, which is what
-- deny-by-default means when the question has not been asked yet.
INSERT INTO permissions (id,description) VALUES
('ai.query','Ask the AI gateway a question over authorized context')
ON CONFLICT (id) DO UPDATE SET description=EXCLUDED.description;

-- Every AI request is audited, answered or refused.
--
-- The prompt is deliberately absent. An audit trail is read by more people
-- than the request was, and storing the assembled context would recreate, in
-- one searchable table, exactly the aggregation the authorization rules exist
-- to prevent. What is stored is enough to answer "who asked what about which
-- records, and what did the platform send it to" — the questions an audit is
-- for — without storing the content itself.
CREATE TABLE IF NOT EXISTS ai_requests (
    id BIGSERIAL PRIMARY KEY,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor_id TEXT NOT NULL,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    -- The Global IDs the caller asked about, and the entity types they named.
    requested_sources JSONB NOT NULL DEFAULT '[]'::jsonb,
    entity_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
    -- The shape of what was sent: the overall classification and a count per
    -- level. Never the content.
    classification JSONB NOT NULL DEFAULT '{}'::jsonb,
    request_id TEXT NOT NULL DEFAULT '',
    -- answered, refused_credential, refused_unauthorized, failed.
    result TEXT NOT NULL,
    -- Sizes rather than text, so cost and growth are answerable.
    prompt_bytes INTEGER NOT NULL DEFAULT 0,
    answer_bytes INTEGER NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_ai_requests_actor ON ai_requests(actor_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_ai_requests_time ON ai_requests(occurred_at DESC);
