package platformdb

import (
	"context"
	"sort"
)

type AccessProfile struct {
	Roles       []string `json:"roles"`
	Permissions []string `json:"permissions"`
	Modules     []string `json:"modules"`
}

type RoleView struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Description string   `json:"description"`
	Enabled     bool     `json:"enabled"`
	Permissions []string `json:"permissions"`
	Modules     []string `json:"modules"`
}

type ScopeGrant struct {
	PrincipalType string `json:"principal_type"`
	PrincipalID   string `json:"principal_id"`
	ScopeType     string `json:"scope_type"`
	ScopeID       string `json:"scope_id"`
	PermissionID  string `json:"permission_id"`
}

func (db *DB) ResolveAccess(ctx context.Context, groups []string, kind string) (AccessProfile, error) {
	if len(groups) == 0 {
		return AccessProfile{}, nil
	}

	rows, err := db.pool.Query(ctx, `
SELECT DISTINCT r.name
FROM group_role_mappings grm
JOIN roles r ON r.id = grm.role_id
WHERE grm.group_name = ANY($1) AND r.enabled = TRUE AND r.kind = $2
ORDER BY r.name`, groups, kind)
	if err != nil {
		return AccessProfile{}, err
	}
	defer rows.Close()

	roleNames := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return AccessProfile{}, err
		}
		roleNames = append(roleNames, name)
	}
	if err := rows.Err(); err != nil {
		return AccessProfile{}, err
	}

	permRows, err := db.pool.Query(ctx, `
SELECT DISTINCT rp.permission_id
FROM group_role_mappings grm
JOIN roles r ON r.id = grm.role_id
JOIN role_permissions rp ON rp.role_id = r.id
WHERE grm.group_name = ANY($1) AND r.enabled = TRUE AND r.kind = $2
ORDER BY rp.permission_id`, groups, kind)
	if err != nil {
		return AccessProfile{}, err
	}
	defer permRows.Close()

	permissions := []string{}
	for permRows.Next() {
		var id string
		if err := permRows.Scan(&id); err != nil {
			return AccessProfile{}, err
		}
		permissions = append(permissions, id)
	}
	if err := permRows.Err(); err != nil {
		return AccessProfile{}, err
	}

	moduleRows, err := db.pool.Query(ctx, `
SELECT DISTINCT rm.module_id
FROM group_role_mappings grm
JOIN roles r ON r.id = grm.role_id
JOIN role_modules rm ON rm.role_id = r.id
WHERE grm.group_name = ANY($1) AND r.enabled = TRUE AND r.kind = $2
ORDER BY rm.module_id`, groups, kind)
	if err != nil {
		return AccessProfile{}, err
	}
	defer moduleRows.Close()

	modules := []string{}
	for moduleRows.Next() {
		var id string
		if err := moduleRows.Scan(&id); err != nil {
			return AccessProfile{}, err
		}
		modules = append(modules, id)
	}
	if err := moduleRows.Err(); err != nil {
		return AccessProfile{}, err
	}

	sort.Strings(roleNames)
	sort.Strings(permissions)
	sort.Strings(modules)
	return AccessProfile{Roles: roleNames, Permissions: permissions, Modules: modules}, nil
}

func (db *DB) ListRoles(ctx context.Context) ([]RoleView, error) {
	rows, err := db.pool.Query(ctx, `
SELECT id,name,kind,description,enabled
FROM roles
ORDER BY kind,name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	roles := []RoleView{}
	for rows.Next() {
		var role RoleView
		if err := rows.Scan(&role.ID, &role.Name, &role.Kind, &role.Description, &role.Enabled); err != nil {
			return nil, err
		}
		roles = append(roles, role)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range roles {
		permRows, err := db.pool.Query(ctx, `SELECT permission_id FROM role_permissions WHERE role_id=$1 ORDER BY permission_id`, roles[i].ID)
		if err != nil {
			return nil, err
		}
		for permRows.Next() {
			var id string
			if err := permRows.Scan(&id); err != nil {
				permRows.Close()
				return nil, err
			}
			roles[i].Permissions = append(roles[i].Permissions, id)
		}
		permRows.Close()

		moduleRows, err := db.pool.Query(ctx, `SELECT module_id FROM role_modules WHERE role_id=$1 ORDER BY module_id`, roles[i].ID)
		if err != nil {
			return nil, err
		}
		for moduleRows.Next() {
			var id string
			if err := moduleRows.Scan(&id); err != nil {
				moduleRows.Close()
				return nil, err
			}
			roles[i].Modules = append(roles[i].Modules, id)
		}
		moduleRows.Close()
	}
	return roles, nil
}

func (db *DB) AddScopeGrant(ctx context.Context, grant ScopeGrant) error {
	_, err := db.pool.Exec(ctx, `
INSERT INTO principal_scopes (principal_type,principal_id,scope_type,scope_id,permission_id)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT DO NOTHING`, grant.PrincipalType, grant.PrincipalID, grant.ScopeType, grant.ScopeID, grant.PermissionID)
	return err
}

func (db *DB) DeleteScopeGrant(ctx context.Context, grant ScopeGrant) error {
	_, err := db.pool.Exec(ctx, `
DELETE FROM principal_scopes
WHERE principal_type=$1 AND principal_id=$2 AND scope_type=$3 AND scope_id=$4 AND permission_id=$5`,
		grant.PrincipalType, grant.PrincipalID, grant.ScopeType, grant.ScopeID, grant.PermissionID)
	return err
}

func (db *DB) ListScopeGrants(ctx context.Context, principalType, principalID string) ([]ScopeGrant, error) {
	rows, err := db.pool.Query(ctx, `
SELECT principal_type,principal_id,scope_type,scope_id,permission_id
FROM principal_scopes
WHERE principal_type=$1 AND principal_id=$2
ORDER BY scope_type,scope_id,permission_id`, principalType, principalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := []ScopeGrant{}
	for rows.Next() {
		var grant ScopeGrant
		if err := rows.Scan(&grant.PrincipalType, &grant.PrincipalID, &grant.ScopeType, &grant.ScopeID, &grant.PermissionID); err != nil {
			return nil, err
		}
		result = append(result, grant)
	}
	return result, rows.Err()
}

func (db *DB) HasScopedPermission(ctx context.Context, principalType, principalID, scopeType, scopeID, permissionID string) (bool, error) {
	var allowed bool
	err := db.pool.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1
  FROM principal_scopes
  WHERE principal_type=$1
    AND principal_id=$2
    AND permission_id IN ($5, '*')
    AND (
      (scope_type='global' AND scope_id='*') OR
      (scope_type=$3 AND scope_id IN ($4, '*'))
    )
)`, principalType, principalID, scopeType, scopeID, permissionID).Scan(&allowed)
	return allowed, err
}
