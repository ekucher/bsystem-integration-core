ALTER TABLE modules
    ADD COLUMN IF NOT EXISTS launch_url TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS icon TEXT NOT NULL DEFAULT '';

UPDATE modules
SET launch_url = '/modules/redmine/', icon = 'redmine', updated_at = now()
WHERE id = 'redmine';

UPDATE modules
SET launch_url = '/modules/outline/', icon = 'outline', updated_at = now()
WHERE id = 'outline';
