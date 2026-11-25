package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
)

const (
	sessionCookie = "rm_session"
	deviceCookie  = "rm_device"
	deviceMaxAge  = 365 * 24 * 60 * 60
)

// Handler implements AuthServiceHandler and StaffServiceHandler. It stays thin:
// read the actor from context, call the service, convert to proto, set cookies.
type Handler struct {
	svc    *Service
	secure bool // Secure flag on cookies (prod)
}

func NewHandler(svc *Service, secure bool) *Handler { return &Handler{svc: svc, secure: secure} }

func (h *Handler) setCookie(header http.Header, name, value string, maxAge int) {
	c := &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: h.secure, MaxAge: maxAge}
	header.Add("Set-Cookie", c.String())
}

// --- AuthService ---

func (h *Handler) GetSession(ctx context.Context, _ *connect.Request[restaurantv1.GetSessionRequest]) (*connect.Response[restaurantv1.Session], error) {
	actor := ActorFrom(ctx)
	sess := &restaurantv1.Session{Device: protoDevice(actor.Device)}
	if actor.LoggedIn() {
		sess.User = toProtoUser(*actor.User)
	}
	return connect.NewResponse(sess), nil
}

func (h *Handler) PasswordLogin(ctx context.Context, req *connect.Request[restaurantv1.PasswordLoginRequest]) (*connect.Response[restaurantv1.User], error) {
	u, token, err := h.svc.PasswordLogin(ctx, req.Msg.GetUsername(), req.Msg.GetPassword())
	if err != nil {
		return nil, err
	}
	resp := connect.NewResponse(toProtoUser(u))
	h.setCookie(resp.Header(), sessionCookie, token, int(sessionTTL.Seconds()))
	return resp, nil
}

func (h *Handler) ListPinUsers(ctx context.Context, _ *connect.Request[restaurantv1.ListPinUsersRequest]) (*connect.Response[restaurantv1.ListPinUsersResponse], error) {
	users, err := h.svc.ListPinUsers(ctx, ActorFrom(ctx).Device)
	if err != nil {
		return nil, err
	}
	out := &restaurantv1.ListPinUsersResponse{Users: make([]*restaurantv1.User, 0, len(users))}
	for _, u := range users {
		out.Users = append(out.Users, toProtoUser(u))
	}
	return connect.NewResponse(out), nil
}

func (h *Handler) PinLogin(ctx context.Context, req *connect.Request[restaurantv1.PinLoginRequest]) (*connect.Response[restaurantv1.User], error) {
	id, err := uuid.Parse(req.Msg.GetUserId())
	if err != nil {
		return nil, connecterr.Invalid("user_id", "invalid user id")
	}
	u, token, err := h.svc.PinLogin(ctx, id, req.Msg.GetPin(), ActorFrom(ctx).Device)
	if err != nil {
		return nil, err
	}
	resp := connect.NewResponse(toProtoUser(u))
	h.setCookie(resp.Header(), sessionCookie, token, int(sessionTTL.Seconds()))
	return resp, nil
}

func (h *Handler) Logout(ctx context.Context, _ *connect.Request[restaurantv1.LogoutRequest]) (*connect.Response[restaurantv1.LogoutResponse], error) {
	if err := h.svc.Logout(ctx, ActorFrom(ctx)); err != nil {
		return nil, err
	}
	resp := connect.NewResponse(&restaurantv1.LogoutResponse{})
	h.setCookie(resp.Header(), sessionCookie, "", -1)
	return resp, nil
}

func (h *Handler) SetLanguage(ctx context.Context, req *connect.Request[restaurantv1.SetLanguageRequest]) (*connect.Response[restaurantv1.User], error) {
	u, err := h.svc.SetLanguage(ctx, ActorFrom(ctx), langString(req.Msg.GetLanguage()))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(toProtoUser(u)), nil
}

func (h *Handler) EnrollDevice(ctx context.Context, req *connect.Request[restaurantv1.EnrollDeviceRequest]) (*connect.Response[restaurantv1.Device], error) {
	d, token, err := h.svc.EnrollDevice(ctx, ActorFrom(ctx), req.Msg.GetLabel())
	if err != nil {
		return nil, err
	}
	resp := connect.NewResponse(&restaurantv1.Device{Shared: d.Shared, Label: d.Label})
	h.setCookie(resp.Header(), deviceCookie, token, deviceMaxAge)
	return resp, nil
}

func (h *Handler) CheckManagerPin(ctx context.Context, req *connect.Request[restaurantv1.CheckManagerPinRequest]) (*connect.Response[restaurantv1.CheckManagerPinResponse], error) {
	a := req.Msg.GetApproval()
	mgr, err := h.svc.VerifyManagerApproval(ctx, ManagerApproval{ManagerUserID: a.GetManagerUserId(), Pin: a.GetPin()})
	if err != nil {
		var ae *connecterr.Error
		if errors.As(err, &ae) && ae.Reason == restaurantv1.ErrorReason_ERROR_REASON_MANAGER_APPROVAL_INVALID {
			return connect.NewResponse(&restaurantv1.CheckManagerPinResponse{Ok: false}), nil
		}
		return nil, err
	}
	return connect.NewResponse(&restaurantv1.CheckManagerPinResponse{
		Ok: true, ManagerUserId: mgr.ID.String(), ManagerName: mgr.Name,
	}), nil
}

// --- StaffService ---

func (h *Handler) ListStaff(ctx context.Context, _ *connect.Request[restaurantv1.ListStaffRequest]) (*connect.Response[restaurantv1.ListStaffResponse], error) {
	staff, err := h.svc.ListStaff(ctx, ActorFrom(ctx))
	if err != nil {
		return nil, err
	}
	out := &restaurantv1.ListStaffResponse{Staff: make([]*restaurantv1.StaffMember, 0, len(staff))}
	for _, u := range staff {
		out.Staff = append(out.Staff, toProtoStaff(u))
	}
	return connect.NewResponse(out), nil
}

func (h *Handler) SaveStaff(ctx context.Context, req *connect.Request[restaurantv1.SaveStaffRequest]) (*connect.Response[restaurantv1.StaffMember], error) {
	m := req.Msg.GetInput()
	in := StaffInput{
		ID: m.GetId(), Name: m.GetName(), Role: roleString(m.GetRole()), Active: m.GetActive(),
		Username: m.GetUsername(), Password: m.GetPassword(), Pin: m.GetPin(), Phone: m.Phone,
	}
	saved, err := h.svc.SaveStaff(ctx, ActorFrom(ctx), in)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(toProtoStaff(saved)), nil
}

func (h *Handler) ResetPin(ctx context.Context, req *connect.Request[restaurantv1.ResetPinRequest]) (*connect.Response[restaurantv1.ResetPinResponse], error) {
	id, err := uuid.Parse(req.Msg.GetId())
	if err != nil {
		return nil, connecterr.Invalid("id", "invalid user id")
	}
	if err := h.svc.ResetPin(ctx, ActorFrom(ctx), id, req.Msg.GetPin()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&restaurantv1.ResetPinResponse{}), nil
}

// --- proto converters ---

func toProtoUser(u sqlc.User) *restaurantv1.User {
	return &restaurantv1.User{Id: u.ID.String(), Name: u.Name, Role: protoRole(u.Role), Language: protoLang(u.Language)}
}

func toProtoStaff(u sqlc.User) *restaurantv1.StaffMember {
	sm := &restaurantv1.StaffMember{
		Id: u.ID.String(), Name: u.Name, Role: protoRole(u.Role), Active: u.Active,
		PinLocked: pinLocked(u, time.Now()), HasPassword: u.PasswordHash.Valid && u.PasswordHash.String != "",
	}
	if u.Username.Valid {
		v := u.Username.String
		sm.Username = &v
	}
	if u.Phone != "" {
		v := u.Phone
		sm.Phone = &v
	}
	return sm
}

func protoDevice(d *sqlc.Device) *restaurantv1.Device {
	if d == nil {
		return &restaurantv1.Device{}
	}
	return &restaurantv1.Device{Shared: d.Shared, Label: d.Label}
}

func protoRole(role string) restaurantv1.Role {
	switch role {
	case RoleOwner:
		return restaurantv1.Role_ROLE_OWNER
	case RoleManager:
		return restaurantv1.Role_ROLE_MANAGER
	case RoleCashier:
		return restaurantv1.Role_ROLE_CASHIER
	case RoleWaiter:
		return restaurantv1.Role_ROLE_WAITER
	case RoleKitchen:
		return restaurantv1.Role_ROLE_KITCHEN
	default:
		return restaurantv1.Role_ROLE_UNSPECIFIED
	}
}

func roleString(r restaurantv1.Role) string {
	switch r {
	case restaurantv1.Role_ROLE_OWNER:
		return RoleOwner
	case restaurantv1.Role_ROLE_MANAGER:
		return RoleManager
	case restaurantv1.Role_ROLE_CASHIER:
		return RoleCashier
	case restaurantv1.Role_ROLE_WAITER:
		return RoleWaiter
	case restaurantv1.Role_ROLE_KITCHEN:
		return RoleKitchen
	default:
		return ""
	}
}

func protoLang(lang string) restaurantv1.Language {
	if lang == "ne" {
		return restaurantv1.Language_LANGUAGE_NE
	}
	return restaurantv1.Language_LANGUAGE_EN
}

func langString(l restaurantv1.Language) string {
	if l == restaurantv1.Language_LANGUAGE_NE {
		return "ne"
	}
	return "en"
}
