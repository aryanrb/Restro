// Package auth owns authentication, sessions, device enrolment, staff accounts and
// the manager-PIN override. Service methods own business rules, DB access and
// errors; the Connect handlers stay thin. Audit rows are written in the same
// transaction as the change.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
)

const (
	sessionTTL   = 12 * time.Hour
	pinLockAfter = 5 // failed PIN attempts
)

// Service holds the connection pool (for transactions) and a non-tx Queries for
// reads.
type Service struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
	log  *zap.Logger
}

func NewService(pool *pgxpool.Pool, log *zap.Logger) *Service {
	return &Service{pool: pool, q: sqlc.New(pool), log: log}
}

// ManagerApproval is an inline manager-PIN override carried by privileged commands.
type ManagerApproval struct {
	ManagerUserID string
	Pin           string
}

// --- pgtype helpers ---

func ptext(s string) pgtype.Text    { return pgtype.Text{String: s, Valid: true} }
func ptextOpt(s string) pgtype.Text { if s == "" { return pgtype.Text{} }; return ptext(s) }
func puuid(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

func auditJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func (s *Service) internal(where string, err error) *connecterr.Error {
	s.log.Error(where, zap.Error(err))
	return connecterr.Internal(err)
}

// ResolveActor is best-effort: it never errors, returning an empty-ish Actor when
// the session/device can't be resolved (treated as logged out / un-enrolled).
func (s *Service) ResolveActor(ctx context.Context, sessionToken, deviceToken string) *Actor {
	a := &Actor{}
	if deviceToken != "" {
		if d, err := s.q.GetDeviceByTokenHash(ctx, hashToken(deviceToken)); err == nil {
			a.Device = &d
		}
	}
	if sessionToken != "" {
		if row, err := s.q.GetActiveSessionByTokenHash(ctx, hashToken(sessionToken)); err == nil {
			u := row.User
			a.User = &u
			a.Token = sessionToken
		}
	}
	return a
}

// issueSession creates a session + a login audit row in one transaction and clears
// any PIN-failure count. Returns the raw token (the handler sets the cookie).
func (s *Service) issueSession(ctx context.Context, userID uuid.UUID, deviceID pgtype.UUID) (string, *connecterr.Error) {
	token, err := newToken()
	if err != nil {
		return "", s.internal("new token", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", s.internal("begin", err)
	}
	defer tx.Rollback(ctx)
	q := s.q.WithTx(tx)

	if _, err := q.CreateSession(ctx, sqlc.CreateSessionParams{
		TokenHash: hashToken(token),
		UserID:    userID,
		DeviceID:  deviceID,
		ExpiresAt: time.Now().Add(sessionTTL),
	}); err != nil {
		return "", s.internal("create session", err)
	}
	if err := q.ResetPinFailures(ctx, userID); err != nil {
		return "", s.internal("reset pin failures", err)
	}
	if err := q.InsertAudit(ctx, sqlc.InsertAuditParams{
		ActorUserID: puuid(userID), Action: "login", Entity: "user", EntityID: ptext(userID.String()),
	}); err != nil {
		return "", s.internal("audit login", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", s.internal("commit", err)
	}
	return token, nil
}

// PasswordLogin validates username+password (owner/manager) and issues a session.
func (s *Service) PasswordLogin(ctx context.Context, username, password string) (sqlc.User, string, error) {
	u, err := s.q.GetUserByUsername(ctx, ptext(username))
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlc.User{}, "", connecterr.InvalidCredentials()
	}
	if err != nil {
		return sqlc.User{}, "", s.internal("get user by username", err)
	}
	if !u.Active || !u.PasswordHash.Valid {
		return sqlc.User{}, "", connecterr.InvalidCredentials()
	}
	ok, err := VerifySecret(password, u.PasswordHash.String)
	if err != nil {
		return sqlc.User{}, "", s.internal("verify password", err)
	}
	if !ok {
		return sqlc.User{}, "", connecterr.InvalidCredentials()
	}
	token, cerr := s.issueSession(ctx, u.ID, pgtype.UUID{})
	if cerr != nil {
		return sqlc.User{}, "", cerr
	}
	return u, token, nil
}

// PinLogin validates a staff PIN on an enrolled shared device and issues a session.
func (s *Service) PinLogin(ctx context.Context, userID uuid.UUID, pin string, device *sqlc.Device) (sqlc.User, string, error) {
	if device == nil || !device.Shared {
		return sqlc.User{}, "", connecterr.DeviceNotShared()
	}
	u, err := s.q.GetUserByID(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlc.User{}, "", connecterr.InvalidCredentials()
	}
	if err != nil {
		return sqlc.User{}, "", s.internal("get user by id", err)
	}
	if u.FailedPinAttempts >= pinLockAfter {
		return sqlc.User{}, "", connecterr.PinLocked()
	}
	if !u.Active || !u.PinHash.Valid {
		return sqlc.User{}, "", connecterr.InvalidCredentials()
	}
	ok, err := VerifySecret(pin, u.PinHash.String)
	if err != nil {
		return sqlc.User{}, "", s.internal("verify pin", err)
	}
	if !ok {
		updated, err := s.q.IncrementPinFailure(ctx, userID)
		if err != nil {
			return sqlc.User{}, "", s.internal("increment pin failure", err)
		}
		if updated.FailedPinAttempts >= pinLockAfter {
			return sqlc.User{}, "", connecterr.PinLocked()
		}
		return sqlc.User{}, "", connecterr.InvalidCredentials()
	}
	token, cerr := s.issueSession(ctx, u.ID, puuid(device.ID))
	if cerr != nil {
		return sqlc.User{}, "", cerr
	}
	return u, token, nil
}

// EnrollDevice marks the caller's device as a shared PIN-login device.
func (s *Service) EnrollDevice(ctx context.Context, actor *Actor, label string) (sqlc.Device, string, error) {
	if !actor.LoggedIn() {
		return sqlc.Device{}, "", connecterr.Unauthenticated("login required")
	}
	if !isPrivileged(actor.User.Role) {
		return sqlc.Device{}, "", connecterr.NotAllowed("only owners and managers can enrol a device")
	}
	token, err := newToken()
	if err != nil {
		return sqlc.Device{}, "", s.internal("new device token", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return sqlc.Device{}, "", s.internal("begin", err)
	}
	defer tx.Rollback(ctx)
	q := s.q.WithTx(tx)

	d, err := q.CreateDevice(ctx, sqlc.CreateDeviceParams{TokenHash: hashToken(token), Label: label})
	if err != nil {
		return sqlc.Device{}, "", s.internal("create device", err)
	}
	if err := q.InsertAudit(ctx, sqlc.InsertAuditParams{
		ActorUserID: puuid(actor.User.ID), Action: "device.enroll", Entity: "device",
		EntityID: ptext(d.ID.String()), After: auditJSON(map[string]any{"label": label}), DeviceLabel: ptext(label),
	}); err != nil {
		return sqlc.Device{}, "", s.internal("audit device enroll", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return sqlc.Device{}, "", s.internal("commit", err)
	}
	return d, token, nil
}

// Logout revokes the current session. Idempotent when already logged out.
func (s *Service) Logout(ctx context.Context, actor *Actor) error {
	if !actor.LoggedIn() {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return s.internal("begin", err)
	}
	defer tx.Rollback(ctx)
	q := s.q.WithTx(tx)
	if err := q.RevokeSession(ctx, hashToken(actor.Token)); err != nil {
		return s.internal("revoke session", err)
	}
	if err := q.InsertAudit(ctx, sqlc.InsertAuditParams{
		ActorUserID: puuid(actor.User.ID), Action: "logout", Entity: "user", EntityID: ptext(actor.User.ID.String()),
	}); err != nil {
		return s.internal("audit logout", err)
	}
	return tx.Commit(ctx)
}

// SetLanguage saves the caller's per-user language preference.
func (s *Service) SetLanguage(ctx context.Context, actor *Actor, language string) (sqlc.User, error) {
	if !actor.LoggedIn() {
		return sqlc.User{}, connecterr.Unauthenticated("login required")
	}
	u, err := s.q.SetUserLanguage(ctx, sqlc.SetUserLanguageParams{ID: actor.User.ID, Language: language})
	if err != nil {
		return sqlc.User{}, s.internal("set language", err)
	}
	return u, nil
}

// ListPinUsers returns the staff picker, only on an enrolled shared device.
func (s *Service) ListPinUsers(ctx context.Context, device *sqlc.Device) ([]sqlc.User, error) {
	if device == nil || !device.Shared {
		return nil, connecterr.DeviceNotShared()
	}
	users, err := s.q.ListActiveUsers(ctx)
	if err != nil {
		return nil, s.internal("list active users", err)
	}
	return users, nil
}

// ListStaff returns all accounts (owner/manager only).
func (s *Service) ListStaff(ctx context.Context, actor *Actor) ([]sqlc.User, error) {
	if !actor.LoggedIn() || !isPrivileged(actor.User.Role) {
		return nil, connecterr.NotAllowed("only owners and managers can view staff")
	}
	staff, err := s.q.ListStaff(ctx)
	if err != nil {
		return nil, s.internal("list staff", err)
	}
	return staff, nil
}

// SaveStaff creates or updates a staff account, enforcing the role hierarchy,
// self-protection and last-owner rules, with validation and username uniqueness.
func (s *Service) SaveStaff(ctx context.Context, actor *Actor, in StaffInput) (sqlc.User, error) {
	if !actor.LoggedIn() || !isPrivileged(actor.User.Role) {
		return sqlc.User{}, connecterr.NotAllowed("only owners and managers can manage staff")
	}
	if verr := in.validate(); verr != nil {
		return sqlc.User{}, verr
	}

	editing := !in.isNew()
	var existing sqlc.User
	if editing {
		id, err := uuid.Parse(in.ID)
		if err != nil {
			return sqlc.User{}, connecterr.Invalid("id", "invalid id")
		}
		existing, err = s.q.GetUserByID(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return sqlc.User{}, connecterr.NotFound("staff not found")
		}
		if err != nil {
			return sqlc.User{}, s.internal("get user by id", err)
		}
	}

	// A manager may not create or edit a privileged account.
	if actor.User.Role == RoleManager && (isPrivileged(in.Role) || (editing && isPrivileged(existing.Role))) {
		return sqlc.User{}, connecterr.NotAllowed("managers cannot manage owner or manager accounts")
	}
	// No changing your own role or deactivating yourself.
	if editing && existing.ID == actor.User.ID && (in.Role != existing.Role || !in.Active) {
		return sqlc.User{}, connecterr.NotAllowed("you cannot change your own role or deactivate yourself")
	}
	// The last active owner cannot be demoted or deactivated.
	if editing && existing.Role == RoleOwner && existing.Active && (in.Role != RoleOwner || !in.Active) {
		n, err := s.q.CountActiveOwners(ctx)
		if err != nil {
			return sqlc.User{}, s.internal("count owners", err)
		}
		if n <= 1 {
			return sqlc.User{}, connecterr.NotAllowed("the last active owner cannot be removed or demoted")
		}
	}

	username := ""
	if isPrivileged(in.Role) {
		username = in.Username
	}
	if username != "" {
		if u, err := s.q.GetUserByUsername(ctx, ptext(username)); err == nil {
			if !editing || u.ID != existing.ID {
				return sqlc.User{}, connecterr.Duplicate("username", "username already taken")
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return sqlc.User{}, s.internal("check username", err)
		}
	}

	// Carry forward existing secrets; hash any provided ones.
	var passwordHash, pinHash pgtype.Text
	if editing {
		passwordHash, pinHash = existing.PasswordHash, existing.PinHash
	}
	if in.Password != "" {
		h, err := HashSecret(in.Password)
		if err != nil {
			return sqlc.User{}, s.internal("hash password", err)
		}
		passwordHash = ptext(h)
	}
	if in.Pin != "" {
		h, err := HashSecret(in.Pin)
		if err != nil {
			return sqlc.User{}, s.internal("hash pin", err)
		}
		pinHash = ptext(h)
	}
	if !isPrivileged(in.Role) { // non-privileged accounts have no username/password
		username = ""
		passwordHash = pgtype.Text{}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return sqlc.User{}, s.internal("begin", err)
	}
	defer tx.Rollback(ctx)
	q := s.q.WithTx(tx)

	var saved sqlc.User
	action := "staff.create"
	if editing {
		action = "staff.update"
		saved, err = q.UpdateUser(ctx, sqlc.UpdateUserParams{
			ID: existing.ID, Name: in.Name, Role: in.Role, Username: ptextOpt(username),
			PasswordHash: passwordHash, PinHash: pinHash, Active: in.Active,
		})
	} else {
		saved, err = q.CreateUser(ctx, sqlc.CreateUserParams{
			Name: in.Name, Role: in.Role, Username: ptextOpt(username),
			PasswordHash: passwordHash, PinHash: pinHash, Language: "en", Active: in.Active,
		})
	}
	if err != nil {
		return sqlc.User{}, s.internal("save staff", err)
	}

	var before []byte
	if editing {
		before = auditJSON(map[string]any{"name": existing.Name, "role": existing.Role, "active": existing.Active})
	}
	if err := q.InsertAudit(ctx, sqlc.InsertAuditParams{
		ActorUserID: puuid(actor.User.ID), Action: action, Entity: "user", EntityID: ptext(saved.ID.String()),
		Before: before, After: auditJSON(map[string]any{"name": saved.Name, "role": saved.Role, "active": saved.Active}),
	}); err != nil {
		return sqlc.User{}, s.internal("audit staff save", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return sqlc.User{}, s.internal("commit", err)
	}
	return saved, nil
}

// ResetPin sets a new PIN and clears any lock (owner/manager only).
func (s *Service) ResetPin(ctx context.Context, actor *Actor, userID uuid.UUID, pin string) error {
	if !actor.LoggedIn() || !isPrivileged(actor.User.Role) {
		return connecterr.NotAllowed("only owners and managers can reset PINs")
	}
	if !pinRe.MatchString(pin) {
		return connecterr.Invalid("pin", "PIN must be 4–6 digits")
	}
	h, err := HashSecret(pin)
	if err != nil {
		return s.internal("hash pin", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return s.internal("begin", err)
	}
	defer tx.Rollback(ctx)
	q := s.q.WithTx(tx)
	if err := q.SetUserPin(ctx, sqlc.SetUserPinParams{ID: userID, PinHash: ptext(h)}); err != nil {
		return s.internal("set pin", err)
	}
	if err := q.InsertAudit(ctx, sqlc.InsertAuditParams{
		ActorUserID: puuid(actor.User.ID), Action: "staff.reset_pin", Entity: "user", EntityID: ptext(userID.String()),
	}); err != nil {
		return s.internal("audit reset pin", err)
	}
	return tx.Commit(ctx)
}

// VerifyManagerApproval checks an inline manager override. Returns the approving
// manager, or ManagerApprovalInvalid. Used by privileged commands in later steps.
func (s *Service) VerifyManagerApproval(ctx context.Context, a ManagerApproval) (sqlc.User, error) {
	id, err := uuid.Parse(a.ManagerUserID)
	if err != nil {
		return sqlc.User{}, connecterr.ManagerApprovalInvalid()
	}
	u, err := s.q.GetUserByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlc.User{}, connecterr.ManagerApprovalInvalid()
	}
	if err != nil {
		return sqlc.User{}, s.internal("get manager", err)
	}
	if !u.Active || !isPrivileged(u.Role) || !u.PinHash.Valid {
		return sqlc.User{}, connecterr.ManagerApprovalInvalid()
	}
	ok, err := VerifySecret(a.Pin, u.PinHash.String)
	if err != nil {
		return sqlc.User{}, s.internal("verify manager pin", err)
	}
	if !ok {
		return sqlc.User{}, connecterr.ManagerApprovalInvalid()
	}
	return u, nil
}

// SeedOwner creates the first owner account, bypassing the actor/role checks. Used
// by boot seeding and tests. No-op (returns the existing count) if any user exists.
func (s *Service) SeedOwner(ctx context.Context, name, username, password, pin string) (created bool, err error) {
	staff, err := s.q.ListStaff(ctx)
	if err != nil {
		return false, err
	}
	if len(staff) > 0 {
		return false, nil
	}
	ph, err := HashSecret(password)
	if err != nil {
		return false, err
	}
	pinH, err := HashSecret(pin)
	if err != nil {
		return false, err
	}
	if _, err := s.q.CreateUser(ctx, sqlc.CreateUserParams{
		Name: name, Role: RoleOwner, Username: ptext(username),
		PasswordHash: ptext(ph), PinHash: ptext(pinH), Language: "en", Active: true,
	}); err != nil {
		return false, err
	}
	return true, nil
}
