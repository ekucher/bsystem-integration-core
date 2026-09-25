package main

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/ekucher/bsystem-integration-core/internal/platformdb"
)

// moduleHumanRoleToID and moduleRoleIDToHumanRole bridge the wire-level
// HumanRole vocabulary the HUB module-administration contract uses (lowercase
// "admin", already established by /api/v1/admin/accounts' humanRoleGroups)
// and the platform's own role catalogue ids (role_modules.role_id, "roles"
// table) that already back GET /api/v1/modules' RBAC filtering via
// role_modules. The two vocabularies differ only for one entry
// ("admin" vs "administrator") but the module-visibility grant this endpoint
// writes needs to land in the SAME table ResolveAccess already reads — the
// only mechanism that actually enforces launcher visibility — so a bridge
// belongs here rather than a second, parallel notion of "role". The key set
// is derived from humanRoleGroups (admin_accounts.go) — the one place this
// platform already enumerates the wire-level HumanRole vocabulary — rather
// than re-listing the same seven names a second time: adding an eighth human
// role only ever means editing humanRoleGroups.
var moduleHumanRoleToID = func() map[string]string {
	result := make(map[string]string, len(humanRoleGroups))
	for humanRole := range humanRoleGroups {
		result[humanRole] = humanRole
	}
	result["admin"] = "administrator"
	return result
}()

var moduleRoleIDToHumanRole = func() map[string]string {
	result := make(map[string]string, len(moduleHumanRoleToID))
	for humanRole, roleID := range moduleHumanRoleToID {
		result[roleID] = humanRole
	}
	return result
}()

// rolesToWire translates internal role ids back to the wire vocabulary for a
// response. A role id with no wire representation (a service role somehow
// present in role_modules) is dropped rather than surfaced as-is: the
// consumer's type is a closed HumanRole union, and handing back a value
// outside it would be a value the consumer's own type system says cannot
// exist.
func rolesToWire(roleIDs []string) []string {
	result := make([]string, 0, len(roleIDs))
	for _, id := range roleIDs {
		if role, ok := moduleRoleIDToHumanRole[id]; ok {
			result = append(result, role)
		}
	}
	return result
}

// rolesFromWire translates the wire vocabulary to internal role ids,
// rejecting anything outside the closed set before it ever reaches
// platformdb — the same "caller's mistake, not the database's" split
// admin_accounts.go already draws for human roles.
func rolesFromWire(roles []string) ([]string, bool) {
	result := make([]string, 0, len(roles))
	for _, role := range roles {
		id, ok := moduleHumanRoleToID[strings.ToLower(strings.TrimSpace(role))]
		if !ok {
			return nil, false
		}
		result = append(result, id)
	}
	return result, true
}

// adminModuleView is the wire shape: platformdb.AdminModule with its role ids
// translated. A distinct type rather than mutating AdminModule in place, so
// the platformdb value (internal role ids) is never accidentally serialized
// as-is from a code path that forgets the translation.
type adminModuleView struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Status       string   `json:"status"`
	LaunchURL    string   `json:"launch_url,omitempty"`
	Icon         string   `json:"icon,omitempty"`
	AllowedRoles []string `json:"allowed_roles"`
	UpdatedBy    string   `json:"updated_by,omitempty"`
	UpdatedAt    string   `json:"updated_at"`
}

func moduleForWire(m platformdb.AdminModule) adminModuleView {
	return adminModuleView{
		ID:           m.ID,
		Name:         m.Name,
		Description:  m.Description,
		Status:       m.Status,
		LaunchURL:    m.LaunchURL,
		Icon:         m.Icon,
		AllowedRoles: rolesToWire(m.AllowedRoles),
		UpdatedBy:    m.UpdatedBy,
		UpdatedAt:    m.UpdatedAt.UTC().Format(rfc3339Milli),
	}
}

const rfc3339Milli = "2006-01-02T15:04:05.000Z07:00"

// moduleAllowedOrigins reads the canonical-origin allowlist (ADR-006, in the
// consumer repository) from configuration. Read per-request rather than
// cached at startup: the existing per-module launch-URL overrides
// (applyModuleLaunchOverride, main.go) use the same live-env-var pattern, and
// this endpoint is not hot enough for the syscall to matter.
//
// A deployment that has not configured this yet returns an empty list rather
// than failing — the admin catalog (list/edit everything except launch_url)
// stays usable, and the HUB's own form already degrades to "launch URL
// unavailable" when the allowlist is empty (see Modules.tsx).
func moduleAllowedOrigins() []string {
	raw := strings.TrimSpace(os.Getenv("MODULE_ALLOWED_ORIGINS"))
	if raw == "" {
		return []string{}
	}
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		origin := strings.TrimRight(strings.TrimSpace(part), "/")
		if origin != "" {
			result = append(result, origin)
		}
	}
	return result
}

var (
	errLaunchURLProtocol = errors.New("launch_url must be an https URL")
	errLaunchURLOrigin   = errors.New("launch_url origin is not on the canonical allowlist")
)

// validateLaunchURL independently re-derives the same guarantee the HUB's own
// safeModuleLaunchUrl/buildLaunchUrl already enforce client-side (ADR-006) —
// protocol restricted to https, origin restricted to the configured
// allowlist — because a frontend `<select>` is a usability aid, not a
// security boundary: any other caller of this API (a script, a future admin
// UI, a compromised or buggy client) must be held to the same rule.
//
// Empty input is valid (a module may have no launch_url yet) and returns "".
func validateLaunchURL(raw string, allowedOrigins []string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return "", errLaunchURLProtocol
	}
	origin := parsed.Scheme + "://" + parsed.Host
	allowed := false
	for _, candidate := range allowedOrigins {
		if candidate == origin {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", errLaunchURLOrigin
	}
	// Userinfo isn't part of the origin check (Host excludes it, so
	// "https://user:pass@allowed.example/" is not an origin bypass) but has
	// no legitimate use in a launcher URL and would otherwise be stored and
	// echoed back verbatim — strip it rather than persist embedded
	// credentials or a display-confusable URL.
	parsed.User = nil
	return parsed.String(), nil
}

func (a *app) adminModules(w http.ResponseWriter, r *http.Request) {
	modules, err := a.db.ListAdminModules(r.Context())
	if err != nil {
		logger.ErrorContext(r.Context(), "admin module list failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error":      "module registry unavailable",
			"code":       "module_store_unavailable",
			"request_id": requestIDFrom(r.Context()),
		})
		return
	}
	views := make([]adminModuleView, 0, len(modules))
	for _, m := range modules {
		views = append(views, moduleForWire(m))
	}
	writeJSON(w, http.StatusOK, views)
}

func (a *app) adminModuleAllowedOrigins(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string][]string{"origins": moduleAllowedOrigins()})
}

type adminCreateModuleRequest struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Status is accepted but not trusted: a created module is always
	// disabled regardless of what this says (see CreateAdminModule) — the
	// field exists on this struct only so a request that includes it (the
	// HUB always sends "disabled") does not fail decodeAdminJSON's
	// DisallowUnknownFields.
	Status       string   `json:"status"`
	LaunchURL    string   `json:"launch_url"`
	AllowedRoles []string `json:"allowed_roles"`
}

var moduleID = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

func (a *app) adminCreateModule(w http.ResponseWriter, r *http.Request) {
	var input adminCreateModuleRequest
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body", "code": "invalid_module_input"})
		return
	}
	input.ID = strings.TrimSpace(input.ID)
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)

	if !moduleID.MatchString(input.ID) || input.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "id must be 1-64 lowercase letters, digits or hyphens, and name is required",
			"code":  "invalid_module_input",
		})
		return
	}

	roleIDs, ok := rolesFromWire(input.AllowedRoles)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown human role", "code": "invalid_role"})
		return
	}

	launchURL, err := validateLaunchURL(input.LaunchURL, moduleAllowedOrigins())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "invalid_launch_url"})
		return
	}

	access := accessFrom(r.Context())
	record := a.auditRecord(r, access, "module.created", "module", input.ID, map[string]any{
		"allowed_roles": input.AllowedRoles,
	})
	created, err := a.db.CreateAdminModule(r.Context(), platformdb.AdminModuleInput{
		ID:           input.ID,
		Name:         input.Name,
		Description:  input.Description,
		LaunchURL:    launchURL,
		AllowedRoles: roleIDs,
	}, moduleActor(access), record)
	if err != nil {
		a.writeModuleMutationError(w, r, err)
		return
	}
	auditWrites.Inc("module.created", "written")
	writeJSON(w, http.StatusCreated, moduleForWire(created))
}

type adminUpdateModuleRequest struct {
	Name         *string   `json:"name,omitempty"`
	Description  *string   `json:"description,omitempty"`
	LaunchURL    *string   `json:"launch_url,omitempty"`
	Status       *string   `json:"status,omitempty"`
	AllowedRoles *[]string `json:"allowed_roles,omitempty"`
}

func (a *app) adminUpdateModule(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "module id is required", "code": "invalid_module_input"})
		return
	}

	var input adminUpdateModuleRequest
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body", "code": "invalid_module_input"})
		return
	}
	if input.Name == nil && input.Description == nil && input.LaunchURL == nil && input.Status == nil && input.AllowedRoles == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "at least one field is required", "code": "invalid_module_input"})
		return
	}

	patch := platformdb.AdminModulePatch{}
	metadata := map[string]any{}

	if input.Name != nil {
		name := strings.TrimSpace(*input.Name)
		if name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name cannot be empty", "code": "invalid_module_input"})
			return
		}
		patch.Name = &name
		metadata["name"] = name
	}
	if input.Description != nil {
		description := strings.TrimSpace(*input.Description)
		patch.Description = &description
		metadata["description"] = description
	}
	if input.LaunchURL != nil {
		launchURL, err := validateLaunchURL(*input.LaunchURL, moduleAllowedOrigins())
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "invalid_launch_url"})
			return
		}
		patch.LaunchURL = &launchURL
		metadata["launch_url"] = launchURL
	}
	if input.Status != nil {
		status := strings.ToLower(strings.TrimSpace(*input.Status))
		if !platformdb.ModuleStatuses[status] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status must be active, maintenance or disabled", "code": "invalid_module_input"})
			return
		}
		patch.Status = &status
		metadata["status"] = status
	}
	if input.AllowedRoles != nil {
		roleIDs, ok := rolesFromWire(*input.AllowedRoles)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown human role", "code": "invalid_role"})
			return
		}
		patch.AllowedRoles = &roleIDs
		metadata["allowed_roles"] = *input.AllowedRoles
	}

	access := accessFrom(r.Context())
	record := a.auditRecord(r, access, "module.updated", "module", id, metadata)
	updated, err := a.db.UpdateAdminModule(r.Context(), id, patch, moduleActor(access), record)
	if err != nil {
		a.writeModuleMutationError(w, r, err)
		return
	}
	auditWrites.Inc("module.updated", "written")
	writeJSON(w, http.StatusOK, moduleForWire(updated))
}

// moduleActor is what module.* audit records and modules.updated_by name as
// the acting principal — the platform's own Global User ID when one has been
// resolved for this caller, matching how every other admin audit record in
// this package identifies who acted.
func moduleActor(access meResponse) string {
	if access.ID != "" {
		return access.ID
	}
	return access.Subject
}

func (a *app) writeModuleMutationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, platformdb.ErrModuleNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "module not found", "code": "not_found"})
	case errors.Is(err, platformdb.ErrModuleConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a module with this id already exists", "code": "module_conflict"})
	case errors.Is(err, platformdb.ErrUnknownRole):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown human role", "code": "invalid_role"})
	case errors.Is(err, platformdb.ErrModuleNeedsRoles):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "a module cannot be activated with no roles assigned — assign allowed_roles first",
			"code":  "module_needs_roles",
		})
	case errors.Is(err, platformdb.ErrAuditNotDurable):
		auditWrites.Inc("module.mutation", "refused")
		logger.ErrorContext(r.Context(), "module mutation refused: its audit record could not be written", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error":      "the change was refused because its audit record could not be written",
			"code":       "audit_unavailable",
			"request_id": requestIDFrom(r.Context()),
		})
	default:
		logger.ErrorContext(r.Context(), "module mutation failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error":      "module registry unavailable",
			"code":       "module_store_unavailable",
			"request_id": requestIDFrom(r.Context()),
		})
	}
}
