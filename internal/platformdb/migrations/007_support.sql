-- Support: incidents and requests.
--
-- One table for both. An incident is something broken and a request is
-- something wanted, but they share a lifecycle, a severity and an audience;
-- splitting them would duplicate all three to express one word.
CREATE TABLE IF NOT EXISTS support_records (
    global_id TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('incident','request')),
    title TEXT NOT NULL,
    summary TEXT NOT NULL DEFAULT '',
    severity TEXT NOT NULL CHECK (severity IN ('low','medium','high','critical')),
    status TEXT NOT NULL DEFAULT 'new'
        CHECK (status IN ('new','acknowledged','in_progress','resolved','closed')),
    -- The affected customer. This is what a scope grant is written against,
    -- so a customer can see their own incidents and no others.
    client_id TEXT,
    reported_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- The two moments an SLA is measured against. They are recorded when the
    -- status first reaches them and never moved again, so reopening a
    -- resolved record does not erase that it was once resolved on time.
    acknowledged_at TIMESTAMPTZ,
    resolved_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_support_records_client ON support_records(client_id) WHERE client_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_support_records_status ON support_records(status);

-- Relations are stored as Global IDs, verified to resolve before they are
-- written. A relation to something the platform cannot identify is a dangling
-- pointer that reads as a fact.
CREATE TABLE IF NOT EXISTS support_relations (
    support_id TEXT NOT NULL REFERENCES support_records(global_id) ON DELETE CASCADE,
    entity_type TEXT NOT NULL CHECK (entity_type IN ('client','server','project','task','bug','document')),
    global_id TEXT NOT NULL,
    PRIMARY KEY (support_id, entity_type, global_id)
);

-- SLA targets per severity.
--
-- Seeded empty, deliberately. What the business promises a customer and what
-- it owes when it misses is a commercial decision; plausible-looking defaults
-- committed here would appear in front of customers as a promise nobody made.
-- With no row for a severity, the platform reports the SLA state as "unset"
-- rather than as "on track".
CREATE TABLE IF NOT EXISTS support_sla_policies (
    severity TEXT PRIMARY KEY CHECK (severity IN ('low','medium','high','critical')),
    respond_minutes INTEGER NOT NULL DEFAULT 0 CHECK (respond_minutes >= 0),
    resolve_minutes INTEGER NOT NULL DEFAULT 0 CHECK (resolve_minutes >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
