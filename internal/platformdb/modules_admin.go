package platformdb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AdminModule is the module-administration view: everything the public
// Module carries, plus fields only an administrator should see — who last
// changed it, when, and which roles currently see it. Deliberately its own
// type rather than added fields on Module: /api/v1/modules serializes
// Module directly, so extending that struct would leak allowed_roles and
// updated_by to every ordinary launcher request.
type AdminModule struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"`
	LaunchURL   string `json:"launch_url,omitempty"`
	Icon        string `json:"icon,omitempty"`
	// AllowedRoles is never nil — an admin needs to see "no roles assigned"
	// as an empty list, not a missing field.
	AllowedRoles []string  `json:"allowed_roles"`
	UpdatedBy    string    `json:"updated_by,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// AdminModuleInput is what a create request contributes. RoleIDs are already
// the platform's internal role ids (e.g. "administrator"), translated from
// the wire-level HumanRole vocabulary ("admin") by the HTTP layer — this
// package only ever deals in the ids role_modules and roles already use.
type AdminModuleInput struct {
	ID           string
	Name         string
	Description  string
	LaunchURL    string
	AllowedRoles []string
}

// AdminModulePatch is a PATCH's fields. A nil pointer means the request did
// not mention the field at all and it must not change — an explicit
// distinction from "mentioned it as empty", which matters most for
// AllowedRoles: a nil *[]string leaves existing role grants untouched, a
// non-nil pointer to an empty slice clears them.
type AdminModulePatch struct {
	Name         *string
	Description  *string
	LaunchURL    *string
	Status       *string
	AllowedRoles *[]string
}

var (
	// ErrModuleNotFound means the id addressed no row.
	ErrModuleNotFound = errors.New("module not found")
	// ErrModuleConflict means the id is already taken.
	ErrModuleConflict = errors.New("a module with this id already exists")
	// ErrUnknownRole means a role id role_modules' foreign key rejected —
	// the caller asked to grant a role that does not exist (or is disabled,
	// via the same FK, since a disabled role is still a row and would NOT be
	// caught here; kind/enabled filtering is the HTTP layer's job before
	// this is ever called, same division as ErrUnknownPermission in rbac.go).
	ErrUnknownRole = errors.New("unknown role")
	// ErrModuleNeedsRoles means the request would leave the module active
	// with no one able to see it — the invariant HUB PR #27 exists to
	// enforce, enforced here too so it cannot be bypassed by any caller that
	// is not the HUB.
	ErrModuleNeedsRoles = errors.New("a module cannot be active with no allowed_roles")
)

// ModuleStatuses is the closed vocabulary this package writes. Enforced here
// rather than by a CHECK constraint — see migration 018's comment for why a
// constraint was tried and reverted (it broke idempotent migration replay
// against 001_init.sql's immutable legacy seed data).
var ModuleStatuses = map[string]bool{"active": true, "maintenance": true, "disabled": true}

// ListAdminModules returns every module regardless of status or enabled —
// the admin catalog view, unlike ListModules' launcher-visible, RBAC-filtered
// one.
func (db *DB) ListAdminModules(ctx context.Context) ([]AdminModule, error) {
	rows, err := db.pool.Query(ctx, `
SELECT id, name, description, status, COALESCE(launch_url,''), COALESCE(icon,''), COALESCE(updated_by,''), updated_at
FROM modules
ORDER BY sort_order, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := []AdminModule{}
	for rows.Next() {
		var m AdminModule
		if err := rows.Scan(&m.ID, &m.Name, &m.Description, &m.Status, &m.LaunchURL, &m.Icon, &m.UpdatedBy, &m.UpdatedAt); err != nil {
			return nil, err
		}
		m.AllowedRoles = []string{}
		result = append(result, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return result, nil
	}

	byID := make(map[string]*AdminModule, len(result))
	ids := make([]string, 0, len(result))
	for i := range result {
		byID[result[i].ID] = &result[i]
		ids = append(ids, result[i].ID)
	}

	roleRows, err := db.pool.Query(ctx, `SELECT module_id, role_id FROM role_modules WHERE module_id = ANY($1) ORDER BY module_id, role_id`, ids)
	if err != nil {
		return nil, err
	}
	defer roleRows.Close()
	for roleRows.Next() {
		var moduleID, roleID string
		if err := roleRows.Scan(&moduleID, &roleID); err != nil {
			return nil, err
		}
		if m := byID[moduleID]; m != nil {
			m.AllowedRoles = append(m.AllowedRoles, roleID)
		}
	}
	return result, roleRows.Err()
}

// setModuleRoles replaces a module's entire role_modules row set. Shared by
// create and update so the two paths cannot drift on how "assign these
// roles" is written.
func setModuleRoles(ctx context.Context, tx pgx.Tx, moduleID string, roleIDs []string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM role_modules WHERE module_id = $1`, moduleID); err != nil {
		return err
	}
	for _, roleID := range roleIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO role_modules (role_id, module_id) VALUES ($1,$2)`, roleID, moduleID); err != nil {
			if isForeignKeyViolation(err) {
				return fmt.Errorf("%w: %q", ErrUnknownRole, roleID)
			}
			return err
		}
	}
	return nil
}

// CreateAdminModule inserts a new module row, always disabled regardless of
// what the caller sent — the spec's safe-initial-lifecycle rule enforced
// here rather than trusted from the request, so no caller can create one
// already active. The row, its role grants, and its audit record share one
// transaction: same fail-closed reasoning as AddScopeGrant (rbac.go) — a
// mutation without a durable audit record is a hole the platform has no
// other way to reconstruct.
func (db *DB) CreateAdminModule(ctx context.Context, input AdminModuleInput, actor string, record AuditEvent) (AdminModule, error) {
	const initialStatus = "disabled"

	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AdminModule{}, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
INSERT INTO modules (id, name, description, status, launch_url, icon, enabled, sort_order, updated_by, updated_at)
VALUES ($1,$2,$3,$4,$5,'',FALSE,100,$6,now())`,
		input.ID, input.Name, input.Description, initialStatus, input.LaunchURL, actor); err != nil {
		if isUniqueViolation(err) {
			return AdminModule{}, ErrModuleConflict
		}
		return AdminModule{}, err
	}

	if err := setModuleRoles(ctx, tx, input.ID, input.AllowedRoles); err != nil {
		return AdminModule{}, err
	}

	if _, err := tx.Exec(ctx, auditInsert, auditArgs(record)...); err != nil {
		return AdminModule{}, fmt.Errorf("%w: %v", ErrAuditNotDurable, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AdminModule{}, err
	}

	roles := input.AllowedRoles
	if roles == nil {
		roles = []string{}
	}
	return AdminModule{
		ID:           input.ID,
		Name:         input.Name,
		Description:  input.Description,
		Status:       initialStatus,
		LaunchURL:    input.LaunchURL,
		AllowedRoles: roles,
		UpdatedBy:    actor,
		UpdatedAt:    time.Now().UTC(),
	}, nil
}

// UpdateAdminModule applies a partial patch. The activation invariant
// (ErrModuleNeedsRoles) is evaluated against the RESULTING state — this
// patch merged onto what is already stored, read with FOR UPDATE inside the
// same transaction — not just the fields this particular request happened
// to touch: activating in a request after roles were assigned in an earlier
// one must succeed, and activating in the same request that clears roles
// must not.
func (db *DB) UpdateAdminModule(ctx context.Context, id string, patch AdminModulePatch, actor string, record AuditEvent) (AdminModule, error) {
	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return AdminModule{}, err
	}
	defer tx.Rollback(ctx)

	var current AdminModule
	err = tx.QueryRow(ctx, `
SELECT id,name,description,status,COALESCE(launch_url,''),COALESCE(icon,''),COALESCE(updated_by,''),updated_at
FROM modules WHERE id=$1 FOR UPDATE`, id).
		Scan(&current.ID, &current.Name, &current.Description, &current.Status, &current.LaunchURL, &current.Icon, &current.UpdatedBy, &current.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminModule{}, ErrModuleNotFound
	}
	if err != nil {
		return AdminModule{}, err
	}

	roleRows, err := tx.Query(ctx, `SELECT role_id FROM role_modules WHERE module_id=$1 ORDER BY role_id`, id)
	if err != nil {
		return AdminModule{}, err
	}
	currentRoles := []string{}
	for roleRows.Next() {
		var roleID string
		if err := roleRows.Scan(&roleID); err != nil {
			roleRows.Close()
			return AdminModule{}, err
		}
		currentRoles = append(currentRoles, roleID)
	}
	roleRows.Close()
	if err := roleRows.Err(); err != nil {
		return AdminModule{}, err
	}

	name, description, launchURL, status := current.Name, current.Description, current.LaunchURL, current.Status
	roles := currentRoles
	if patch.Name != nil {
		name = *patch.Name
	}
	if patch.Description != nil {
		description = *patch.Description
	}
	if patch.LaunchURL != nil {
		launchURL = *patch.LaunchURL
	}
	if patch.Status != nil {
		status = *patch.Status
	}
	if patch.AllowedRoles != nil {
		roles = *patch.AllowedRoles
	}

	if status == "active" && len(roles) == 0 {
		return AdminModule{}, ErrModuleNeedsRoles
	}

	if _, err := tx.Exec(ctx, `
UPDATE modules SET name=$1, description=$2, launch_url=$3, status=$4, enabled=$5, updated_by=$6, updated_at=now()
WHERE id=$7`, name, description, launchURL, status, status != "disabled", actor, id); err != nil {
		return AdminModule{}, err
	}

	if patch.AllowedRoles != nil {
		if err := setModuleRoles(ctx, tx, id, roles); err != nil {
			return AdminModule{}, err
		}
	}

	if _, err := tx.Exec(ctx, auditInsert, auditArgs(record)...); err != nil {
		return AdminModule{}, fmt.Errorf("%w: %v", ErrAuditNotDurable, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AdminModule{}, err
	}

	if roles == nil {
		roles = []string{}
	}
	return AdminModule{
		ID:           id,
		Name:         name,
		Description:  description,
		Status:       status,
		LaunchURL:    launchURL,
		Icon:         current.Icon,
		AllowedRoles: roles,
		UpdatedBy:    actor,
		UpdatedAt:    time.Now().UTC(),
	}, nil
}
