INSERT INTO modules (id, name, description, status, sort_order, enabled) VALUES
('redmine', 'Redmine', 'Проєкти й задачі Redmine', 'ready', 80, TRUE),
('outline', 'Outline', 'Документація та Runbooks', 'ready', 90, TRUE)
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name,
    description = EXCLUDED.description,
    status = EXCLUDED.status,
    sort_order = EXCLUDED.sort_order,
    enabled = TRUE,
    updated_at = now();

UPDATE modules
SET enabled = FALSE,
    updated_at = now()
WHERE id IN ('demo-a', 'demo-b');
