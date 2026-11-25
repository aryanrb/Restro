package inventory

import (
	"context"

	"connectrpc.com/connect"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
)

// Handler implements InventoryServiceHandler. Thin: proto in, service, proto out.
type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func wrap[T any](out *T, err error) (*connect.Response[T], error) {
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

func (h *Handler) ListInventoryItems(ctx context.Context, req *connect.Request[restaurantv1.ListInventoryItemsRequest]) (*connect.Response[restaurantv1.ListInventoryItemsResponse], error) {
	items, err := h.svc.ListInventoryItems(ctx, req.Msg.GetIncludeInactive())
	return wrap(&restaurantv1.ListInventoryItemsResponse{Items: items}, err)
}

func (h *Handler) SaveInventoryItem(ctx context.Context, req *connect.Request[restaurantv1.SaveInventoryItemRequest]) (*connect.Response[restaurantv1.InventoryItem], error) {
	return wrap(h.svc.SaveInventoryItem(ctx, req.Msg))
}

func (h *Handler) SetTracked(ctx context.Context, req *connect.Request[restaurantv1.SetTrackedRequest]) (*connect.Response[restaurantv1.InventoryItem], error) {
	return wrap(h.svc.SetTracked(ctx, req.Msg))
}

func (h *Handler) ListMovements(ctx context.Context, req *connect.Request[restaurantv1.ListMovementsRequest]) (*connect.Response[restaurantv1.ListMovementsResponse], error) {
	list, err := h.svc.ListMovements(ctx, req.Msg)
	return wrap(&restaurantv1.ListMovementsResponse{Movements: list}, err)
}

func (h *Handler) LogWaste(ctx context.Context, req *connect.Request[restaurantv1.LogWasteRequest]) (*connect.Response[restaurantv1.WasteEntry], error) {
	return wrap(h.svc.LogWaste(ctx, req.Msg))
}

func (h *Handler) ListWaste(ctx context.Context, req *connect.Request[restaurantv1.WasteRangeRequest]) (*connect.Response[restaurantv1.ListWasteResponse], error) {
	list, err := h.svc.ListWaste(ctx, req.Msg.GetFromUnix(), req.Msg.GetToUnix())
	return wrap(&restaurantv1.ListWasteResponse{Entries: list}, err)
}

func (h *Handler) StartCount(ctx context.Context, req *connect.Request[restaurantv1.StartCountRequest]) (*connect.Response[restaurantv1.StockCount], error) {
	return wrap(h.svc.StartCount(ctx, req.Msg))
}

func (h *Handler) SaveCountLines(ctx context.Context, req *connect.Request[restaurantv1.SaveCountLinesRequest]) (*connect.Response[restaurantv1.StockCount], error) {
	return wrap(h.svc.SaveCountLines(ctx, req.Msg))
}

func (h *Handler) SubmitCount(ctx context.Context, req *connect.Request[restaurantv1.CountIdRequest]) (*connect.Response[restaurantv1.StockCount], error) {
	return wrap(h.svc.SubmitCount(ctx, req.Msg.GetCountId()))
}

func (h *Handler) RecountLines(ctx context.Context, req *connect.Request[restaurantv1.RecountLinesRequest]) (*connect.Response[restaurantv1.StockCount], error) {
	return wrap(h.svc.RecountLines(ctx, req.Msg))
}

func (h *Handler) PostCount(ctx context.Context, req *connect.Request[restaurantv1.CountIdRequest]) (*connect.Response[restaurantv1.StockCount], error) {
	return wrap(h.svc.PostCount(ctx, req.Msg.GetCountId()))
}

func (h *Handler) DiscardCount(ctx context.Context, req *connect.Request[restaurantv1.DiscardCountRequest]) (*connect.Response[restaurantv1.StockCount], error) {
	return wrap(h.svc.DiscardCount(ctx, req.Msg.GetCountId(), req.Msg.GetReason()))
}

func (h *Handler) GetCount(ctx context.Context, req *connect.Request[restaurantv1.CountIdRequest]) (*connect.Response[restaurantv1.StockCount], error) {
	return wrap(h.svc.GetCount(ctx, req.Msg.GetCountId()))
}

func (h *Handler) ListCounts(ctx context.Context, _ *connect.Request[restaurantv1.ListCountsRequest]) (*connect.Response[restaurantv1.ListCountsResponse], error) {
	list, err := h.svc.ListCounts(ctx)
	return wrap(&restaurantv1.ListCountsResponse{Counts: list}, err)
}

func (h *Handler) SaveCountList(ctx context.Context, req *connect.Request[restaurantv1.SaveCountListRequest]) (*connect.Response[restaurantv1.CountList], error) {
	return wrap(h.svc.SaveCountList(ctx, req.Msg))
}

func (h *Handler) ListCountLists(ctx context.Context, _ *connect.Request[restaurantv1.ListCountListsRequest]) (*connect.Response[restaurantv1.ListCountListsResponse], error) {
	list, err := h.svc.ListCountLists(ctx)
	return wrap(&restaurantv1.ListCountListsResponse{Lists: list}, err)
}

func (h *Handler) Variance(ctx context.Context, req *connect.Request[restaurantv1.CountIdRequest]) (*connect.Response[restaurantv1.VarianceReport], error) {
	return wrap(h.svc.Variance(ctx, req.Msg.GetCountId()))
}

func (h *Handler) Wastage(ctx context.Context, req *connect.Request[restaurantv1.WasteRangeRequest]) (*connect.Response[restaurantv1.WastageReport], error) {
	return wrap(h.svc.Wastage(ctx, req.Msg.GetFromUnix(), req.Msg.GetToUnix()))
}

func (h *Handler) LowStock(ctx context.Context, _ *connect.Request[restaurantv1.LowStockRequest]) (*connect.Response[restaurantv1.LowStockResponse], error) {
	rows, err := h.svc.LowStock(ctx)
	return wrap(&restaurantv1.LowStockResponse{Rows: rows}, err)
}

func (h *Handler) ItemsWithoutRecipe(ctx context.Context, _ *connect.Request[restaurantv1.ItemsWithoutRecipeRequest]) (*connect.Response[restaurantv1.ItemsWithoutRecipeResponse], error) {
	rows, err := h.svc.ItemsWithoutRecipe(ctx)
	return wrap(&restaurantv1.ItemsWithoutRecipeResponse{Rows: rows}, err)
}

func (h *Handler) Suggestions86(ctx context.Context, _ *connect.Request[restaurantv1.Suggestions86Request]) (*connect.Response[restaurantv1.Suggestions86Response], error) {
	list, err := h.svc.Suggestions86(ctx)
	return wrap(&restaurantv1.Suggestions86Response{Suggestions: list}, err)
}
