package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ekucher/bsystem-integration-core/internal/authentikadmin"
	"github.com/ekucher/bsystem-integration-core/internal/platformdb"
)

const userAdminAdvisoryLock int64 = 0x4253595354454d

var humanUsername = regexp.MustCompile(`^[A-Za-z0-9._@-]+$`)

var humanRoleGroups = map[string]string{
	"admin":     "BSYSTEM-Admins",
	"manager":   "BSYSTEM-Managers",
	"developer": "BSYSTEM-Developers",
	"qa":        "BSYSTEM-QA",
	"support":   "BSYSTEM-Support",
	"devops":    "BSYSTEM-DevOps",
	"customer":  "BSYSTEM-Customers",
}

var humanGroupRoles = func() map[string]string {
	result := make(map[string]string, len(humanRoleGroups))
	for role, group := range humanRoleGroups {
		result[group] = role
	}
	return result
}()

type adminAccount struct {
	AuthentikID        *int       `json:"authentik_id,omitempty"`
	GlobalID           *string    `json:"global_id,omitempty"`
	Username           string     `json:"username"`
	Name               string     `json:"name"`
	Email              string     `json:"email"`
	Active             *bool      `json:"active,omitempty"`
	Roles              []string   `json:"roles"`
	Groups             []string   `json:"groups"`
	FirstSeenAt        *time.Time `json:"first_seen_at,omitempty"`
	LastSeenAt         *time.Time `json:"last_seen_at,omitempty"`
	Manageable         bool       `json:"manageable"`
	PasswordManageable bool       `json:"password_manageable"`
}

type adminAccountsResponse struct {
	ManagementAvailable bool           `json:"management_available"`
	Accounts            []adminAccount `json:"accounts"`
}

type adminCreateAccountRequest struct {
	Username string `json:"username"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	Role     string `json:"role"`
	Password string `json:"password"`
}

type adminUpdateAccountRequest struct {
	Role   *string `json:"role,omitempty"`
	Active *bool   `json:"active,omitempty"`
}

type adminPasswordRequest struct {
	Password string `json:"password"`
}

func (a *app) adminAccounts(w http.ResponseWriter, r *http.Request) {
	identities, err := a.db.ListIdentities(r.Context())
	if err != nil {
		logger.ErrorContext(r.Context(), "identity directory read failed", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error":      "identity directory unavailable",
			"code":       "identity_store_unavailable",
			"request_id": requestIDFrom(r.Context()),
		})
		return
	}

	if a.userAdmin == nil || !a.userAdmin.Configured() {
		writeJSON(w, http.StatusOK, adminAccountsResponse{
			ManagementAvailable: false,
			Accounts:            accountsFromIdentities(identities),
		})
		return
	}

	users, err := a.userAdmin.ListUsers(r.Context())
	if err != nil {
		a.writeUserAdminDependencyError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, adminAccountsResponse{
		ManagementAvailable: true,
		Accounts:            mergeAdminAccounts(users, identities),
	})
}

func (a *app) adminCreateAccount(w http.ResponseWriter, r *http.Request) {
	if !a.requireUserAdmin(w, r) {
		return
	}

	var input adminCreateAccountRequest
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body", "code": "invalid_user_input"})
		return
	}
	input.Username = strings.TrimSpace(input.Username)
	input.Name = strings.TrimSpace(input.Name)
	input.Email = strings.TrimSpace(input.Email)
	input.Role = strings.ToLower(strings.TrimSpace(input.Role))

	groupName, ok := humanRoleGroups[input.Role]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown human role", "code": "invalid_role"})
		return
	}
	if input.Role == "admin" && !callerIsAdministrator(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "administrator access required for administrator-class user management",
			"code":  "permission_required",
		})
		return
	}
	if input.Username == "" || !humanUsername.MatchString(input.Username) || input.Name == "" || input.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "username, name and password are required and username contains invalid characters",
			"code":  "invalid_user_input",
		})
		return
	}

	var created authentikadmin.User
	err := a.db.WithAdvisoryLock(r.Context(), userAdminAdvisoryLock, func(ctx context.Context) error {
		users, err := a.userAdmin.ListUsers(ctx)
		if err != nil {
			return err
		}
		for _, user := range users {
			if strings.EqualFold(user.Username, input.Username) {
				return errUsernameConflict
			}
		}

		groups, err := a.userAdmin.ListGroups(ctx)
		if err != nil {
			return err
		}
		groupPK, ok := groupPKByName(groups, groupName)
		if !ok {
			return errHumanGroupMissing
		}

		created, err = a.userAdmin.CreateUser(ctx, authentikadmin.CreateUserInput{
			Username: input.Username,
			Name:     input.Name,
			Email:    input.Email,
			GroupPK:  groupPK,
		})
		if err != nil {
			return err
		}
		// New accounts are deliberately created inactive. Password setup must
		// succeed before activation so a partial failure leaves a locked account.
		if err := a.userAdmin.SetPassword(ctx, created.PK, input.Password); err != nil {
			return err
		}
		active := true
		created, err = a.userAdmin.UpdateUser(ctx, created.PK, nil, &active)
		return err
	})
	// Drop the raw password reference before any error handling or audit work.
	input.Password = ""
	if err != nil {
		a.writeUserAdminMutationError(w, r, err)
		return
	}

	a.audit(r, accessFrom(r.Context()), "identity.user.created", "user", input.Username, map[string]any{
		"role": input.Role,
	})
	account := accountFromAuthentik(created, nil)
	writeJSON(w, http.StatusCreated, account)
}

var (
	errUsernameConflict    = errors.New("username already exists")
	errHumanGroupMissing   = errors.New("required BSYSTEM group is missing")
	errLastAdmin           = errors.New("final active administrator cannot be removed")
	errServiceIdentity     = errors.New("service identity cannot be managed as a human")
	errNotHumanAccount     = errors.New("account has no BSYSTEM human role")
	errPasswordUnsupported = errors.New("password reset is only supported for internal users")
	errAdminTarget         = errors.New("administrator-class user management requires administrator access")
)

func (a *app) adminUpdateAccount(w http.ResponseWriter, r *http.Request) {
	if !a.requireUserAdmin(w, r) {
		return
	}
	pk, ok := parseAuthentikID(w, r)
	if !ok {
		return
	}

	var input adminUpdateAccountRequest
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body", "code": "invalid_user_input"})
		return
	}
	if input.Role == nil && input.Active == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "role or active is required", "code": "invalid_user_input"})
		return
	}

	var requestedRole string
	if input.Role != nil {
		requestedRole = strings.ToLower(strings.TrimSpace(*input.Role))
		if _, ok := humanRoleGroups[requestedRole]; !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown human role", "code": "invalid_role"})
			return
		}
	}

	var before authentikadmin.User
	var updated authentikadmin.User
	err := a.db.WithAdvisoryLock(r.Context(), userAdminAdvisoryLock, func(ctx context.Context) error {
		var err error
		before, err = a.userAdmin.GetUser(ctx, pk)
		if err != nil {
			return err
		}
		if hasGroup(before.GroupsObj, "BSYSTEM-Services") ||
			before.Type == "service_account" ||
			before.Type == "internal_service_account" {
			return errServiceIdentity
		}
		currentRoles := rolesFromGroups(groupNames(before.GroupsObj))
		if len(currentRoles) == 0 {
			return errNotHumanAccount
		}
		if (hasString(currentRoles, "admin") || requestedRole == "admin") && !callerIsAdministrator(r) {
			return errAdminTarget
		}

		groups, err := a.userAdmin.ListGroups(ctx)
		if err != nil {
			return err
		}
		humanPKs := map[string]bool{}
		for _, group := range groups {
			if _, isHuman := humanGroupRoles[group.Name]; isHuman {
				humanPKs[group.PK] = true
			}
		}

		var nextGroups *[]string
		resultGroups := append([]string(nil), before.Groups...)
		if input.Role != nil {
			targetPK, ok := groupPKByName(groups, humanRoleGroups[requestedRole])
			if !ok {
				return errHumanGroupMissing
			}
			filtered := make([]string, 0, len(resultGroups)+1)
			for _, groupPK := range resultGroups {
				if !humanPKs[groupPK] {
					filtered = append(filtered, groupPK)
				}
			}
			filtered = append(filtered, targetPK)
			resultGroups = uniqueStrings(filtered)
			nextGroups = &resultGroups
		}

		resultActive := before.IsActive
		if input.Active != nil {
			resultActive = *input.Active
		}
		resultAdmin := hasString(currentRoles, "admin")
		if input.Role != nil {
			resultAdmin = requestedRole == "admin"
		}

		if before.IsActive && hasString(currentRoles, "admin") && (!resultActive || !resultAdmin) {
			users, err := a.userAdmin.ListUsers(ctx)
			if err != nil {
				return err
			}
			if activeAdminCount(users) <= 1 {
				return errLastAdmin
			}
		}

		updated, err = a.userAdmin.UpdateUser(ctx, pk, nextGroups, input.Active)
		return err
	})
	if err != nil {
		a.writeUserAdminMutationError(w, r, err)
		return
	}

	beforeRoles := rolesFromGroups(groupNames(before.GroupsObj))
	afterRoles := rolesFromGroups(groupNames(updated.GroupsObj))
	a.audit(r, accessFrom(r.Context()), "identity.user.updated", "user", updated.Username, map[string]any{
		"roles_before":  beforeRoles,
		"roles_after":   afterRoles,
		"active_before": before.IsActive,
		"active_after":  updated.IsActive,
	})
	identity := a.identityByUsername(r, updated.Username)
	writeJSON(w, http.StatusOK, accountFromAuthentik(updated, identity))
}

func (a *app) adminSetAccountPassword(w http.ResponseWriter, r *http.Request) {
	if !a.requireUserAdmin(w, r) {
		return
	}
	pk, ok := parseAuthentikID(w, r)
	if !ok {
		return
	}

	var input adminPasswordRequest
	if err := decodeAdminJSON(w, r, &input); err != nil || input.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "password is required", "code": "invalid_user_input"})
		return
	}

	var username string
	err := a.db.WithAdvisoryLock(r.Context(), userAdminAdvisoryLock, func(ctx context.Context) error {
		user, err := a.userAdmin.GetUser(ctx, pk)
		if err != nil {
			return err
		}
		if hasGroup(user.GroupsObj, "BSYSTEM-Services") || user.Type == "service_account" || user.Type == "internal_service_account" {
			return errServiceIdentity
		}
		targetRoles := rolesFromGroups(groupNames(user.GroupsObj))
		if len(targetRoles) == 0 {
			return errNotHumanAccount
		}
		if hasString(targetRoles, "admin") && !callerIsAdministrator(r) {
			return errAdminTarget
		}
		if user.Type != "internal" {
			return errPasswordUnsupported
		}
		username = user.Username
		return a.userAdmin.SetPassword(ctx, pk, input.Password)
	})
	// Clear the only local reference as soon as the downstream call returns.
	input.Password = ""
	if err != nil {
		a.writeUserAdminMutationError(w, r, err)
		return
	}
	a.audit(r, accessFrom(r.Context()), "identity.password.reset", "user", username, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) identityByUsername(r *http.Request, username string) *platformdb.IdentityView {
	identities, err := a.db.ListIdentities(r.Context())
	if err != nil {
		return nil
	}
	for i := range identities {
		if identities[i].Username == username {
			return &identities[i]
		}
	}
	return nil
}

func (a *app) requireUserAdmin(w http.ResponseWriter, r *http.Request) bool {
	if a.userAdmin != nil && a.userAdmin.Configured() {
		return true
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{
		"error":      "user administration is not configured",
		"code":       "user_admin_not_configured",
		"request_id": requestIDFrom(r.Context()),
	})
	return false
}

func (a *app) writeUserAdminDependencyError(w http.ResponseWriter, r *http.Request, err error) {
	logger.ErrorContext(r.Context(), "authentik administration request failed", "error", err.Error())
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{
		"error":      "user administration dependency unavailable",
		"code":       "user_admin_unavailable",
		"request_id": requestIDFrom(r.Context()),
	})
}

func (a *app) writeUserAdminMutationError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errUsernameConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "username already exists", "code": "username_conflict"})
	case errors.Is(err, errLastAdmin):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "final active administrator cannot be removed", "code": "last_admin_guard"})
	case errors.Is(err, errServiceIdentity):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "service identity cannot be managed as a human", "code": "service_identity"})
	case errors.Is(err, errNotHumanAccount):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "account has no BSYSTEM human role", "code": "role_conflict"})
	case errors.Is(err, errPasswordUnsupported):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "password reset is only supported for internal users", "code": "password_unsupported"})
	case errors.Is(err, errAdminTarget):
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "administrator access required for administrator-class user management",
			"code":  "permission_required",
		})
	case errors.Is(err, errHumanGroupMissing):
		logger.ErrorContext(r.Context(), "required BSYSTEM human group is missing", "error", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "user administration configuration is incomplete", "code": "user_admin_unavailable", "request_id": requestIDFrom(r.Context())})
	default:
		var apiErr *authentikadmin.APIError
		if errors.As(err, &apiErr) {
			switch apiErr.Status {
			case http.StatusBadRequest:
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": "authentik rejected the user input",
					"code":  "user_admin_rejected",
				})
				return
			case http.StatusNotFound:
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found", "code": "not_found"})
				return
			}
		}
		a.writeUserAdminDependencyError(w, r, err)
	}
}

func parseAuthentikID(w http.ResponseWriter, r *http.Request) (int, bool) {
	pk, err := strconv.Atoi(strings.TrimSpace(r.PathValue("id")))
	if err != nil || pk <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid authentik user id", "code": "invalid_user_input"})
		return 0, false
	}
	return pk, true
}

func decodeAdminJSON(w http.ResponseWriter, r *http.Request, out any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func callerIsAdministrator(r *http.Request) bool {
	access := accessFrom(r.Context())
	return hasString(access.Roles, "Administrator") || hasString(access.Permissions, "*") || hasString(access.Permissions, "identity.user.admin")
}

func mergeAdminAccounts(users []authentikadmin.User, identities []platformdb.IdentityView) []adminAccount {
	byUsername := make(map[string]*platformdb.IdentityView, len(identities))
	for i := range identities {
		byUsername[identities[i].Username] = &identities[i]
	}

	result := make([]adminAccount, 0, len(users)+len(identities))
	seen := map[string]bool{}
	for _, user := range users {
		names := groupNames(user.GroupsObj)
		if hasString(names, "BSYSTEM-Services") ||
			user.Type == "service_account" ||
			user.Type == "internal_service_account" {
			continue
		}
		if len(rolesFromGroups(names)) == 0 {
			continue
		}
		identity := byUsername[user.Username]
		result = append(result, accountFromAuthentik(user, identity))
		seen[user.Username] = true
	}
	for i := range identities {
		if !seen[identities[i].Username] {
			result = append(result, accountFromIdentity(identities[i]))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Username == result[j].Username {
			left, right := "", ""
			if result[i].GlobalID != nil {
				left = *result[i].GlobalID
			}
			if result[j].GlobalID != nil {
				right = *result[j].GlobalID
			}
			return left < right
		}
		return result[i].Username < result[j].Username
	})
	return result
}

func accountsFromIdentities(identities []platformdb.IdentityView) []adminAccount {
	result := make([]adminAccount, 0, len(identities))
	for _, identity := range identities {
		result = append(result, accountFromIdentity(identity))
	}
	return result
}

func accountFromIdentity(identity platformdb.IdentityView) adminAccount {
	id := identity.ID
	firstSeen := identity.FirstSeenAt
	lastSeen := identity.LastSeenAt
	return adminAccount{
		GlobalID:    &id,
		Username:    identity.Username,
		Name:        identity.DisplayName,
		Email:       identity.Email,
		Roles:       rolesFromGroups(identity.Groups),
		Groups:      append([]string(nil), identity.Groups...),
		FirstSeenAt: &firstSeen,
		LastSeenAt:  &lastSeen,
	}
}

func accountFromAuthentik(user authentikadmin.User, identity *platformdb.IdentityView) adminAccount {
	pk := user.PK
	active := user.IsActive
	names := groupNames(user.GroupsObj)
	roles := rolesFromGroups(names)
	service := hasString(names, "BSYSTEM-Services") || user.Type == "service_account" || user.Type == "internal_service_account"
	account := adminAccount{
		AuthentikID:        &pk,
		Username:           user.Username,
		Name:               user.Name,
		Email:              user.Email,
		Active:             &active,
		Roles:              roles,
		Groups:             names,
		Manageable:         !service && len(roles) > 0,
		PasswordManageable: !service && len(roles) > 0 && user.Type == "internal",
	}
	if identity != nil {
		id := identity.ID
		firstSeen := identity.FirstSeenAt
		lastSeen := identity.LastSeenAt
		account.GlobalID = &id
		account.FirstSeenAt = &firstSeen
		account.LastSeenAt = &lastSeen
	}
	return account
}

func rolesFromGroups(groups []string) []string {
	roles := []string{}
	for _, group := range groups {
		if role, ok := humanGroupRoles[group]; ok {
			roles = append(roles, role)
		}
	}
	sort.Strings(roles)
	return roles
}

func groupNames(groups []authentikadmin.Group) []string {
	result := make([]string, 0, len(groups))
	for _, group := range groups {
		result = append(result, group.Name)
	}
	sort.Strings(result)
	return result
}

func groupPKByName(groups []authentikadmin.Group, name string) (string, bool) {
	for _, group := range groups {
		if group.Name == name {
			return group.PK, true
		}
	}
	return "", false
}

func hasGroup(groups []authentikadmin.Group, name string) bool {
	for _, group := range groups {
		if group.Name == name {
			return true
		}
	}
	return false
}

func activeAdminCount(users []authentikadmin.User) int {
	count := 0
	for _, user := range users {
		service := hasGroup(user.GroupsObj, "BSYSTEM-Services") ||
			user.Type == "service_account" ||
			user.Type == "internal_service_account"
		if user.IsActive && !service && hasGroup(user.GroupsObj, "BSYSTEM-Admins") {
			count++
		}
	}
	return count
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}
