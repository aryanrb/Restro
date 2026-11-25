package live

import (
	"context"

	"connectrpc.com/connect"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
)

// Handler implements LiveServiceHandler. Read-only: no route here changes anything.
type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) GetSnapshot(ctx context.Context, _ *connect.Request[restaurantv1.GetLiveSnapshotRequest]) (*connect.Response[restaurantv1.LiveSnapshot], error) {
	snap, err := h.svc.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(snap), nil
}

func (h *Handler) GetOrder(ctx context.Context, req *connect.Request[restaurantv1.GetLiveOrderRequest]) (*connect.Response[restaurantv1.LiveOrderCard], error) {
	card, err := h.svc.Order(ctx, req.Msg.GetOrderId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(card), nil
}
