package auth

import (
	"regexp"

	"github.com/oneaccord/restro/api/internal/connecterr"
)

// Roles, as stored in users.role (and the CHECK constraint).
const (
	RoleOwner   = "OWNER"
	RoleManager = "MANAGER"
	RoleCashier = "CASHIER"
	RoleWaiter  = "WAITER"
	RoleKitchen = "KITCHEN"
)

func isPrivileged(role string) bool { return role == RoleOwner || role == RoleManager }

var (
	usernameRe = regexp.MustCompile(`^[a-z0-9._-]{3,30}$`)
	pinRe      = regexp.MustCompile(`^[0-9]{4,6}$`)
)

// StaffInput is the service-layer shape for creating/updating a staff account.
type StaffInput struct {
	ID       string // "" = create
	Name     string
	Role     string
	Active   bool
	Username string // "" = none (non-privileged roles)
	Password string // "" = unchanged
	Pin      string // "" = unchanged
}

func (in StaffInput) isNew() bool { return in.ID == "" }

// validate enforces the field rules that mirror the web's validateStaff. Business
// rules (role hierarchy, last-owner, uniqueness) live in the service.
func (in StaffInput) validate() *connecterr.Error {
	if in.Name == "" {
		return connecterr.Invalid("name", "name is required")
	}
	switch in.Role {
	case RoleOwner, RoleManager, RoleCashier, RoleWaiter, RoleKitchen:
	default:
		return connecterr.Invalid("role", "unknown role")
	}
	if isPrivileged(in.Role) {
		if !usernameRe.MatchString(in.Username) {
			return connecterr.Invalid("username", "username must be 3–30 chars of a–z, 0–9, . _ -")
		}
		if (in.isNew() || in.Password != "") && len(in.Password) < 8 {
			return connecterr.Invalid("password", "password must be at least 8 characters")
		}
	}
	if in.isNew() && in.Pin == "" {
		return connecterr.Invalid("pin", "PIN is required")
	}
	if in.Pin != "" && !pinRe.MatchString(in.Pin) {
		return connecterr.Invalid("pin", "PIN must be 4–6 digits")
	}
	return nil
}
