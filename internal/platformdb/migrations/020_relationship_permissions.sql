-- Permissions for the HTTP surface over entity_relationships (019). The
-- store itself needs no schema change here: this migration only grants
-- access to what 019 already created.
--
-- relationships.read is granted to the platform's existing staff roles —
-- the same ones already trusted to read across CRM, projects and
-- documentation — but deliberately not to 'customer'. A relationship edge
-- names two Global IDs with no tenant column of its own, so there is
-- nothing here to isolate it to the customer's own client the way every
-- other customer-visible collection is scoped; granting it would let a
-- customer enumerate edges touching Global IDs outside their own data.
--
-- relationships.write is granted only to 'service-core': every mutation
-- goes through a service identity that also carries a verified end-user
-- on-behalf-of token (see cmd/server/relationships.go), never directly from
-- a human token.
INSERT INTO permissions (id, description) VALUES
('relationships.read', 'List the relationships touching a Global ID'),
('relationships.write', 'Create or delete a relationship between two Global IDs')
ON CONFLICT (id) DO UPDATE SET description = EXCLUDED.description;

INSERT INTO role_permissions (role_id, permission_id) VALUES
('manager', 'relationships.read'),
('developer', 'relationships.read'),
('qa', 'relationships.read'),
('support', 'relationships.read'),
('devops', 'relationships.read'),
('service-core', 'relationships.read'),
('service-core', 'relationships.write')
ON CONFLICT DO NOTHING;
