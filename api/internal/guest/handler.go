package guest

import (
	"context"
	"net"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
)

// PublicHandler implements restaurantv1connect.GuestServiceHandler. It is a
// separate type from DeskHandler so the public mount can only ever expose the
// four guest RPCs; it never reads the session.
type PublicHandler struct{ svc *Service }

func NewPublicHandler(svc *Service) *PublicHandler { return &PublicHandler{svc: svc} }

// DeskHandler implements restaurantv1connect.GuestDeskServiceHandler (staff).
type DeskHandler struct{ svc *Service }

func NewDeskHandler(svc *Service) *DeskHandler { return &DeskHandler{svc: svc} }

func wrap[T any](v *T, err error) (*connect.Response[T], error) {
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(v), nil
}

// clientAddr is the caller's address for the per-client rate limit: the entry
// the reverse proxy appended to X-Forwarded-For (the rightmost), else the peer.
func clientAddr(h http.Header, peer string) string {
	if xff := h.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
			return ip
		}
	}
	if host, _, err := net.SplitHostPort(peer); err == nil {
		return host
	}
	return peer
}

func (h *PublicHandler) GetGuestMenu(ctx context.Context, req *connect.Request[restaurantv1.GetGuestMenuRequest]) (*connect.Response[restaurantv1.GuestMenu], error) {
	return wrap(h.svc.Menu(ctx, req.Msg.GetTableCode()))
}

func (h *PublicHandler) SubmitGuestOrder(ctx context.Context, req *connect.Request[restaurantv1.SubmitGuestOrderRequest]) (*connect.Response[restaurantv1.GuestRequestView], error) {
	return wrap(h.svc.SubmitOrder(ctx, req.Msg, clientAddr(req.Header(), req.Peer().Addr)))
}

func (h *PublicHandler) CallStaff(ctx context.Context, req *connect.Request[restaurantv1.CallStaffRequest]) (*connect.Response[restaurantv1.GuestRequestView], error) {
	return wrap(h.svc.Call(ctx, req.Msg, clientAddr(req.Header(), req.Peer().Addr)))
}

func (h *PublicHandler) GetGuestStatus(ctx context.Context, req *connect.Request[restaurantv1.GetGuestStatusRequest]) (*connect.Response[restaurantv1.GuestStatus], error) {
	return wrap(h.svc.Status(ctx, req.Msg.GetTableCode()))
}

func (h *DeskHandler) ListGuestRequests(ctx context.Context, _ *connect.Request[restaurantv1.ListGuestRequestsRequest]) (*connect.Response[restaurantv1.ListGuestRequestsResponse], error) {
	return wrap(h.svc.List(ctx))
}

func (h *DeskHandler) AcceptGuestOrder(ctx context.Context, req *connect.Request[restaurantv1.AcceptGuestOrderRequest]) (*connect.Response[restaurantv1.Order], error) {
	return wrap(h.svc.Accept(ctx, req.Msg))
}

func (h *DeskHandler) RejectGuestOrder(ctx context.Context, req *connect.Request[restaurantv1.RejectGuestOrderRequest]) (*connect.Response[restaurantv1.RejectGuestOrderResponse], error) {
	return wrap(h.svc.Reject(ctx, req.Msg))
}

func (h *DeskHandler) ResolveGuestAlert(ctx context.Context, req *connect.Request[restaurantv1.ResolveGuestAlertRequest]) (*connect.Response[restaurantv1.ResolveGuestAlertResponse], error) {
	return wrap(h.svc.Resolve(ctx, req.Msg))
}

func (h *DeskHandler) ListGuestTables(ctx context.Context, _ *connect.Request[restaurantv1.ListGuestTablesRequest]) (*connect.Response[restaurantv1.ListGuestTablesResponse], error) {
	return wrap(h.svc.ListTables(ctx))
}

func (h *DeskHandler) SetGuestOrderingEnabled(ctx context.Context, req *connect.Request[restaurantv1.SetGuestOrderingEnabledRequest]) (*connect.Response[restaurantv1.SetGuestOrderingEnabledResponse], error) {
	return wrap(h.svc.SetEnabled(ctx, req.Msg))
}

func (h *DeskHandler) SetTableGuestOrdering(ctx context.Context, req *connect.Request[restaurantv1.SetTableGuestOrderingRequest]) (*connect.Response[restaurantv1.SetTableGuestOrderingResponse], error) {
	return wrap(h.svc.SetTableOrdering(ctx, req.Msg))
}

func (h *DeskHandler) NewGuestCode(ctx context.Context, req *connect.Request[restaurantv1.NewGuestCodeRequest]) (*connect.Response[restaurantv1.GuestTableConfig], error) {
	return wrap(h.svc.NewCode(ctx, req.Msg))
}
