-- QA Requirement Global IDs, and a generic relationship store on top of
-- global_entities.
--
-- Registering the prefix is purely additive: global_id_counters has no
-- schema to change, only a row to add, exactly like every prefix seeded in
-- 001_init.sql. No existing row is touched.
INSERT INTO global_id_counters (entity_type, prefix, next_value) VALUES
('requirement', 'REQ', 1)
ON CONFLICT (entity_type) DO NOTHING;

-- entity_relationships records a typed edge between two Global IDs that
-- already exist in global_entities. It does not duplicate anything about
-- those entities beyond the reference: no title, no status, no source
-- payload. The source system stays authoritative; this table only records
-- that a relationship between two already-identified things was asserted.
--
-- Exactly one row per relationship. relation_type is always stored in its
-- canonical forward form (see the vocabulary in relationships.go); the
-- inverse label ("tested-by" for "tests", and so on) is never written here
-- — it is derived at read time from whichever endpoint is being asked
-- about. This is deliberate: a table that stored both directions could
-- disagree with itself after a partial write, and there is nothing to
-- reconcile if only one row can ever exist for a given edge.
--
-- The foreign keys are what keep this from becoming support_relations'
-- problem in generic form: a relationship to a Global ID the platform
-- cannot resolve is refused by the database itself, not left as a row that
-- reads as a fact until someone checks.
CREATE TABLE IF NOT EXISTS entity_relationships (
    id BIGSERIAL PRIMARY KEY,
    from_global_id TEXT NOT NULL REFERENCES global_entities(global_id),
    relation_type TEXT NOT NULL CHECK (relation_type IN (
        'related-to', 'tests', 'validates', 'documents',
        'implements', 'depends-on', 'blocks', 'references'
    )),
    to_global_id TEXT NOT NULL REFERENCES global_entities(global_id),
    -- Provenance is deliberately two nullable columns rather than one. A
    -- human acting directly through the platform sets only
    -- created_by_user_id. A service identity acting on its own behalf (a
    -- sync job, a scheduled reconciliation) sets only
    -- created_by_service_id. A user-triggered action carried out through a
    -- service — the common case for anything that goes through an adapter
    -- or an automation — sets both, so "who asked" and "what actually wrote
    -- the row" are both on the record instead of one overwriting the other.
    created_by_user_id TEXT,
    created_by_service_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A Global ID cannot relate to itself. This is enforced here rather
    -- than only in the service layer because the table is the last line of
    -- defense against any future writer that skips the Go helper.
    CHECK (from_global_id <> to_global_id),
    -- At least one actor must be recorded. A relationship with no
    -- provenance at all is not something this platform can attribute to
    -- anyone.
    CHECK (created_by_user_id IS NOT NULL OR created_by_service_id IS NOT NULL),
    -- The one canonical edge per (from, relation, to) triple. Creating the
    -- same relationship twice hits this and is treated as success rather
    -- than as a new row: see CreateRelationship's ON CONFLICT DO NOTHING.
    UNIQUE (from_global_id, relation_type, to_global_id)
);

CREATE INDEX IF NOT EXISTS idx_entity_relationships_from ON entity_relationships(from_global_id, relation_type);
CREATE INDEX IF NOT EXISTS idx_entity_relationships_to ON entity_relationships(to_global_id, relation_type);
