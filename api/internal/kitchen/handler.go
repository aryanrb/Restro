package kitchen

import (
	"context"

	"connectrpc.com/connect"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
)

// Handler implements KitchenServiceHandler.
type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) ListStationTickets(ctx context.Context, req *connect.Request[restaurantv1.ListStationTicketsRequest]) (*connect.Response[restaurantv1.ListStationTicketsResponse], error) {
	tickets, err := h.svc.ListStationTickets(ctx, req.Msg.GetStationId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&restaurantv1.ListStationTicketsResponse{Tickets: tickets}), nil
}

func (h *Handler) StartItems(ctx context.Context, req *connect.Request[restaurantv1.StartItemsRequest]) (*connect.Response[restaurantv1.KitchenActionResponse], error) {
	out, err := h.svc.StartItems(ctx, req.Msg.GetItemIds())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

func (h *Handler) MarkItemsReady(ctx context.Context, req *connect.Request[restaurantv1.MarkItemsReadyRequest]) (*connect.Response[restaurantv1.KitchenActionResponse], error) {
	out, err := h.svc.MarkItemsReady(ctx, req.Msg.GetItemIds())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

func (h *Handler) BumpTicket(ctx context.Context, req *connect.Request[restaurantv1.BumpTicketRequest]) (*connect.Response[restaurantv1.KitchenActionResponse], error) {
	out, err := h.svc.BumpTicket(ctx, req.Msg.GetOrderId(), req.Msg.GetStationId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}
