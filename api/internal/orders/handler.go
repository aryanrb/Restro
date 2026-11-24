package orders

import (
	"context"

	"connectrpc.com/connect"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
)

// Handler implements OrderServiceHandler. Thin: pass the command to the service.
type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) ListOpenOrders(ctx context.Context, _ *connect.Request[restaurantv1.ListOpenOrdersRequest]) (*connect.Response[restaurantv1.ListOpenOrdersResponse], error) {
	orders, err := h.svc.ListOpenOrders(ctx)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&restaurantv1.ListOpenOrdersResponse{Orders: orders}), nil
}

func (h *Handler) GetOrder(ctx context.Context, req *connect.Request[restaurantv1.GetOrderRequest]) (*connect.Response[restaurantv1.Order], error) {
	out, err := h.svc.GetOrder(ctx, req.Msg)
	return wrap(out, err)
}

func (h *Handler) CreateOrder(ctx context.Context, req *connect.Request[restaurantv1.CreateOrderRequest]) (*connect.Response[restaurantv1.OrderResult], error) {
	out, err := h.svc.CreateOrder(ctx, req.Msg)
	return wrap(out, err)
}

func (h *Handler) AddItems(ctx context.Context, req *connect.Request[restaurantv1.AddItemsRequest]) (*connect.Response[restaurantv1.AddItemsResult], error) {
	out, err := h.svc.AddItems(ctx, req.Msg)
	return wrap(out, err)
}

func (h *Handler) SendItems(ctx context.Context, req *connect.Request[restaurantv1.SendItemsRequest]) (*connect.Response[restaurantv1.OrderResult], error) {
	out, err := h.svc.SendItems(ctx, req.Msg)
	return wrap(out, err)
}

func (h *Handler) VoidItem(ctx context.Context, req *connect.Request[restaurantv1.VoidItemRequest]) (*connect.Response[restaurantv1.OrderResult], error) {
	out, err := h.svc.VoidItem(ctx, req.Msg)
	return wrap(out, err)
}

func (h *Handler) MarkServed(ctx context.Context, req *connect.Request[restaurantv1.MarkServedRequest]) (*connect.Response[restaurantv1.OrderResult], error) {
	out, err := h.svc.MarkServed(ctx, req.Msg)
	return wrap(out, err)
}

func (h *Handler) TransferTable(ctx context.Context, req *connect.Request[restaurantv1.TransferTableRequest]) (*connect.Response[restaurantv1.OrderResult], error) {
	out, err := h.svc.TransferTable(ctx, req.Msg)
	return wrap(out, err)
}

func (h *Handler) MergeTables(ctx context.Context, req *connect.Request[restaurantv1.MergeTablesRequest]) (*connect.Response[restaurantv1.OrderResult], error) {
	out, err := h.svc.MergeTables(ctx, req.Msg)
	return wrap(out, err)
}

func wrap[T any](v *T, err error) (*connect.Response[T], error) {
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(v), nil
}
