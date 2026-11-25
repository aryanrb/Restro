package loyalty

import (
	"context"

	"connectrpc.com/connect"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
)

// Handler implements restaurantv1connect.LoyaltyServiceHandler (thin: proto in, proto out).
type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func wrap[T any](v *T, err error) (*connect.Response[T], error) {
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(v), nil
}

func (h *Handler) FindMember(ctx context.Context, req *connect.Request[restaurantv1.FindMemberRequest]) (*connect.Response[restaurantv1.FindMemberResponse], error) {
	return wrap(h.svc.FindMember(ctx, req.Msg))
}

func (h *Handler) JoinLoyalty(ctx context.Context, req *connect.Request[restaurantv1.JoinLoyaltyRequest]) (*connect.Response[restaurantv1.MemberSummary], error) {
	return wrap(h.svc.JoinLoyalty(ctx, req.Msg))
}

func (h *Handler) ListMembers(ctx context.Context, req *connect.Request[restaurantv1.ListMembersRequest]) (*connect.Response[restaurantv1.ListMembersResponse], error) {
	return wrap(h.svc.ListMembers(ctx, req.Msg))
}

func (h *Handler) GetMember(ctx context.Context, req *connect.Request[restaurantv1.GetMemberRequest]) (*connect.Response[restaurantv1.MemberCard], error) {
	return wrap(h.svc.GetMember(ctx, req.Msg))
}

func (h *Handler) AdjustStamps(ctx context.Context, req *connect.Request[restaurantv1.AdjustStampsRequest]) (*connect.Response[restaurantv1.MemberCard], error) {
	return wrap(h.svc.AdjustStamps(ctx, req.Msg))
}

func (h *Handler) LeaveLoyalty(ctx context.Context, req *connect.Request[restaurantv1.LeaveLoyaltyRequest]) (*connect.Response[restaurantv1.LeaveLoyaltyResponse], error) {
	return wrap(h.svc.LeaveLoyalty(ctx, req.Msg))
}

func (h *Handler) NewCardLink(ctx context.Context, req *connect.Request[restaurantv1.NewCardLinkRequest]) (*connect.Response[restaurantv1.MemberCard], error) {
	return wrap(h.svc.NewCardLink(ctx, req.Msg))
}

func (h *Handler) GetLoyaltySettings(ctx context.Context, req *connect.Request[restaurantv1.GetLoyaltySettingsRequest]) (*connect.Response[restaurantv1.LoyaltySettings], error) {
	return wrap(h.svc.GetLoyaltySettings(ctx, req.Msg))
}

func (h *Handler) UpdateLoyaltySettings(ctx context.Context, req *connect.Request[restaurantv1.UpdateLoyaltySettingsRequest]) (*connect.Response[restaurantv1.LoyaltySettings], error) {
	return wrap(h.svc.UpdateLoyaltySettings(ctx, req.Msg))
}

func (h *Handler) GetLoyaltyReport(ctx context.Context, req *connect.Request[restaurantv1.LoyaltyReportRequest]) (*connect.Response[restaurantv1.LoyaltyReport], error) {
	return wrap(h.svc.GetLoyaltyReport(ctx, req.Msg))
}

func (h *Handler) GetCardStatus(ctx context.Context, req *connect.Request[restaurantv1.GetCardStatusRequest]) (*connect.Response[restaurantv1.CardStatus], error) {
	return wrap(h.svc.GetCardStatus(ctx, req.Msg))
}
