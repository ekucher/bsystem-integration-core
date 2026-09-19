-- Grant initial BSYSTEM platform access to Nextcloud.
--
-- Nextcloud does not currently have a corresponding BSYSTEM domain module
-- from which access can be derived, unlike Redmine (projects) and Outline
-- (wiki).
--
-- Keep the initial policy deliberately fail-closed for non-administrative
-- roles. Additional human-role grants require an explicit business policy.

INSERT INTO role_modules (role_id, module_id)
VALUES ('administrator', 'nextcloud')
ON CONFLICT (role_id, module_id) DO NOTHING;
