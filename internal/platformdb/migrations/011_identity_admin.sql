INSERT INTO permissions (id,description) VALUES
('identity.user.read','Read human user directory')
ON CONFLICT (id) DO UPDATE SET description=EXCLUDED.description;

INSERT INTO role_permissions (role_id,permission_id) VALUES
('manager','identity.user.read'),
('support','identity.user.read')
ON CONFLICT DO NOTHING;
