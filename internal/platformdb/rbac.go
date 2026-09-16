package platformdb

import (
	"context"
	"errors"
	"fmt"
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

	if len(roles) == 0 {
		return roles, nil
	}

	// Permissions and modules are fetched for every role at once rather than
	// per role. The loop this replaces issued two queries per role, so the
	// cost of listing the roles grew with the number of roles — which is
	// exactly the shape that looks fine with eight of them and does not stay
	// fine.
	byID := make(map[string]*RoleView, len(roles))
	ids := make([]string, 0, len(roles))
	for i := range roles {
		byID[roles[i].ID] = &roles[i]
		ids = append(ids, roles[i].ID)
	}

	permRows, err := db.pool.Query(ctx,
		`SELECT role_id, permission_id FROM role_permissions WHERE role_id = ANY($1) ORDER BY role_id, permission_id`, ids)
	if err != nil {
		return nil, err
	}
	for permRows.Next() {
		var roleID, permission string
		if err := permRows.Scan(&roleID, &permission); err != nil {
			permRows.Close()
			return nil, err
		}
		if role := byID[roleID]; role != nil {
			role.Permissions = append(role.Permissions, permission)
		}
	}
	permRows.Close()
	if err := permRows.Err(); err != nil {
		return nil, err
	}

	moduleRows, err := db.pool.Query(ctx,
		`SELECT role_id, module_id FROM role_modules WHERE role_id = ANY($1) ORDER BY role_id, module_id`, ids)
	if err != nil {
		return nil, err
	}
	for moduleRows.Next() {
		var roleID, module string
		if err := moduleRows.Scan(&roleID, &module); err != nil {
			moduleRows.Close()
			return nil, err
		}
		if role := byID[roleID]; role != nil {
			role.Modules = append(role.Modules, module)
		}
	}
	moduleRows.Close()
	if err := moduleRows.Err(); err != nil {
		return nil, err
	}

	return roles, nil
}

// The three ways a scope grant can be the caller's mistake. They are sentinels
// rather than messages, so the HTTP layer can tell them from a database
// failure without matching on text — which is how a raw SQL error comes to be
// answered as a client error.
var (
	// ErrInvalidPrincipalType names a principal kind outside the vocabulary.
	//
	// The vocabulary is small and closed and worth stating precisely, because
	// two different ones live in this schema and are easy to confuse: a role's
	// kind is "human" or "service", while a scope grant's principal is "user",
	// "service" or "group".
	ErrInvalidPrincipalType = errors.New("principal_type must be user, service or group")
	// ErrInvalidScopeType names a scope outside the vocabulary.
	ErrInvalidScopeType = errors.New("scope_type must be global, tenant, client, project or resource")
	// ErrUnknownPermission names a permission the catalogue does not hold.
	ErrUnknownPermission = errors.New("unknown permission")
)

// These mirror the CHECK constraints on principal_scopes. Checking here as
// well is not redundancy for its own sake: the constraint is what guarantees
// the data, and this is what lets the platform say which field was wrong
// instead of handing back the constraint's name.
var (
	principalTypes = map[string]bool{"user": true, "service": true, "group": true}
	scopeTypes     = map[string]bool{"global": true, "tenant": true, "client": true, "project": true, "resource": true}
)

func (db *DB) AddScopeGrant(ctx context.Context, grant ScopeGrant) error {
	if !principalTypes[grant.PrincipalType] {
		return fmt.Errorf("%w: %q", ErrInvalidPrincipalType, grant.PrincipalType)
	}
	if !scopeTypes[grant.ScopeType] {
		return fmt.Errorf("%w: %q", ErrInvalidScopeType, grant.ScopeType)
	}
	_, err := db.pool.Exec(ctx, `
INSERT INTO principal_scopes (principal_type,principal_id,scope_type,scope_id,permission_id)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT DO NOTHING`, grant.PrincipalType, grant.PrincipalID, grant.ScopeType, grant.ScopeID, grant.PermissionID)
	// The permission is a foreign key rather than a vocabulary, so it is
	// caught rather than pre-checked: a separate existence query would be a
	// second round trip and would still be racing the catalogue.
	if isForeignKeyViolation(err) {
		return fmt.Errorf("%w: %q", ErrUnknownPermission, grant.PermissionID)
	}
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
