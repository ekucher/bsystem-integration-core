-- Search indexing is a machine capability.
--
-- No human role receives it: a person searching does not index, and indexing
-- is how the platform mirrors an authoritative system into something more
-- people can read at once.
INSERT INTO permissions (id,description) VALUES
('search.index','Index, update and remove normalized search documents')
ON CONFLICT (id) DO UPDATE SET description=EXCLUDED.description;

INSERT INTO role_permissions (role_id,permission_id) VALUES
('service-core','search.index')
ON CONFLICT DO NOTHING;
