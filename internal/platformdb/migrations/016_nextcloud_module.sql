-- Register Nextcloud as a launchable BSYSTEM application module.
--
-- RBAC grants are intentionally not added here. Module visibility is
-- fail-closed until an explicit BSYSTEM role-to-module policy is defined.

INSERT INTO modules (
    id,
    name,
    description,
    status,
    sort_order,
    enabled,
    launch_url,
    icon
) VALUES (
    'nextcloud',
    'Nextcloud',
    'Файли та спільна робота',
    'ready',
    100,
    TRUE,
    '/modules/nextcloud/',
    'nextcloud'
)
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name,
    description = EXCLUDED.description,
    status = EXCLUDED.status,
    sort_order = EXCLUDED.sort_order,
    enabled = TRUE,
    launch_url = EXCLUDED.launch_url,
    icon = EXCLUDED.icon,
    updated_at = now();
