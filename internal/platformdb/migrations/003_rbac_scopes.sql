CREATE TABLE IF NOT EXISTS roles (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    kind TEXT NOT NULL DEFAULT 'human' CHECK (kind IN ('human','service')),
    description TEXT NOT NULL DEFAULT '',
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS permissions (
    id TEXT PRIMARY KEY,
    description TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS role_permissions (
    role_id TEXT NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    permission_id TEXT NOT NULL REFERENCES permissions(id) ON DELETE CASCADE,
    PRIMARY KEY (role_id, permission_id)
);

CREATE TABLE IF NOT EXISTS role_modules (
    role_id TEXT NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    module_id TEXT NOT NULL REFERENCES modules(id) ON DELETE CASCADE,
    PRIMARY KEY (role_id, module_id)
);

CREATE TABLE IF NOT EXISTS group_role_mappings (
    group_name TEXT NOT NULL,
    role_id TEXT NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    PRIMARY KEY (group_name, role_id)
);

CREATE TABLE IF NOT EXISTS principal_scopes (
    id BIGSERIAL PRIMARY KEY,
    principal_type TEXT NOT NULL CHECK (principal_type IN ('user','service','group')),
    principal_id TEXT NOT NULL,
    scope_type TEXT NOT NULL CHECK (scope_type IN ('global','tenant','client','project','resource')),
    scope_id TEXT NOT NULL DEFAULT '*',
    permission_id TEXT NOT NULL REFERENCES permissions(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (principal_type, principal_id, scope_type, scope_id, permission_id)
);

CREATE INDEX IF NOT EXISTS idx_principal_scopes_lookup
    ON principal_scopes(principal_type, principal_id, scope_type, scope_id);

INSERT INTO roles (id,name,kind,description) VALUES
('administrator','Administrator','human','Full platform administration'),
('manager','Manager','human','Business and operational management'),
('developer','Developer','human','Development and project work'),
('qa','QA','human','Quality assurance'),
('support','Support','human','Support and incident handling'),
('devops','DevOps','human','Infrastructure and operations'),
('customer','Customer','human','Customer portal access'),
('service-core','Service Core','service','Default machine-to-machine platform role')
ON CONFLICT (id) DO UPDATE SET name=EXCLUDED.name, kind=EXCLUDED.kind, description=EXCLUDED.description, updated_at=now();

INSERT INTO permissions (id,description) VALUES
('*','All permissions'),
('crm.client.read','Read CRM clients'),
('projects.task.read','Read project tasks'),
('projects.task.edit','Edit project tasks'),
('qa.report.read','Read QA reports'),
('qa.testcase.read','Read test cases'),
('qa.testcase.execute','Execute test cases'),
('qa.bug.write','Create and edit bugs'),
('development.repo.read','Read repositories'),
('development.pr.write','Create or edit pull requests'),
('wiki.document.read','Read documentation'),
('wiki.document.edit','Edit documentation'),
('operations.server.read','Read server state'),
('operations.server.manage','Manage server operations'),
('support.incident.read','Read incidents'),
('support.incident.write','Create and edit incidents'),
('portal.read','Read customer portal'),
('adapters.read','Read adapter registry'),
('events.publish','Publish normalized platform events'),
('global_ids.read','Resolve Global IDs')
ON CONFLICT (id) DO UPDATE SET description=EXCLUDED.description;

INSERT INTO group_role_mappings (group_name,role_id) VALUES
('BSYSTEM-Admins','administrator'),
('BSYSTEM-Managers','manager'),
('BSYSTEM-Developers','developer'),
('BSYSTEM-QA','qa'),
('BSYSTEM-Support','support'),
('BSYSTEM-DevOps','devops'),
('BSYSTEM-Customers','customer'),
('BSYSTEM-Services','service-core')
ON CONFLICT DO NOTHING;

INSERT INTO role_permissions (role_id,permission_id) VALUES
('administrator','*'),
('manager','crm.client.read'),('manager','projects.task.read'),('manager','qa.report.read'),('manager','wiki.document.read'),('manager','operations.server.read'),('manager','support.incident.read'),
('developer','projects.task.read'),('developer','projects.task.edit'),('developer','development.repo.read'),('developer','development.pr.write'),('developer','qa.testcase.read'),('developer','wiki.document.read'),('developer','wiki.document.edit'),('developer','operations.server.read'),
('qa','projects.task.read'),('qa','qa.testcase.read'),('qa','qa.testcase.execute'),('qa','qa.bug.write'),('qa','wiki.document.read'),
('support','crm.client.read'),('support','projects.task.read'),('support','wiki.document.read'),('support','operations.server.read'),('support','support.incident.read'),('support','support.incident.write'),
('devops','development.repo.read'),('devops','wiki.document.read'),('devops','wiki.document.edit'),('devops','operations.server.read'),('devops','operations.server.manage'),
('customer','portal.read'),('customer','wiki.document.read'),('customer','support.incident.read'),
('service-core','adapters.read'),('service-core','events.publish'),('service-core','global_ids.read')
ON CONFLICT DO NOTHING;

INSERT INTO role_modules (role_id,module_id) VALUES
('administrator','crm'),('administrator','projects'),('administrator','qa'),('administrator','development'),('administrator','wiki'),('administrator','operations'),('administrator','support'),
('manager','crm'),('manager','projects'),('manager','qa'),('manager','wiki'),('manager','operations'),('manager','support'),
('developer','projects'),('developer','qa'),('developer','development'),('developer','wiki'),('developer','operations'),
('qa','projects'),('qa','qa'),('qa','wiki'),
('support','crm'),('support','projects'),('support','wiki'),('support','operations'),('support','support'),
('devops','development'),('devops','wiki'),('devops','operations'),
('customer','wiki'),('customer','support')
ON CONFLICT DO NOTHING;
