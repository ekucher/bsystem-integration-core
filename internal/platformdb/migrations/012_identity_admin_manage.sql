INSERT INTO permissions (id,description) VALUES
('identity.user.manage','Create and manage human user accounts')
ON CONFLICT (id) DO UPDATE SET description=EXCLUDED.description;

INSERT INTO role_permissions (role_id,permission_id) VALUES
('manager','identity.user.manage')
ON CONFLICT DO NOTHING;
