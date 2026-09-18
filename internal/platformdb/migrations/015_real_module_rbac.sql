-- Grant access to the real application modules according to the existing
-- BSYSTEM domain-module RBAC policy.
--
-- Redmine implements the Projects domain, so every role that already has
-- access to "projects" receives access to "redmine".
--
-- Outline implements the Wiki/documentation domain, so every role that already
-- has access to "wiki" receives access to "outline".
--
-- Deriving the grants from the existing mappings keeps this migration aligned
-- with the RBAC policy already stored in PostgreSQL instead of duplicating a
-- hard-coded role list.

INSERT INTO role_modules (role_id, module_id)
SELECT role_id, 'redmine'
FROM role_modules
WHERE module_id = 'projects'
ON CONFLICT (role_id, module_id) DO NOTHING;

INSERT INTO role_modules (role_id, module_id)
SELECT role_id, 'outline'
FROM role_modules
WHERE module_id = 'wiki'
ON CONFLICT (role_id, module_id) DO NOTHING;
