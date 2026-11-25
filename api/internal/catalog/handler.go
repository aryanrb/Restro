package catalog

import (
	"context"

	"connectrpc.com/connect"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
)

// Handler implements SettingsService, FloorService, StationService and MenuService.
type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// --- SettingsService ---

func (h *Handler) GetSettings(ctx context.Context, _ *connect.Request[restaurantv1.GetSettingsRequest]) (*connect.Response[restaurantv1.Settings], error) {
	out, err := h.svc.GetSettings(ctx)
	return resp(out, err)
}

func (h *Handler) UpdateSettings(ctx context.Context, req *connect.Request[restaurantv1.UpdateSettingsRequest]) (*connect.Response[restaurantv1.Settings], error) {
	out, err := h.svc.UpdateSettings(ctx, req.Msg.GetInput(), req.Msg.GetVersion())
	return resp(out, err)
}

// --- FloorService ---

func (h *Handler) GetFloor(ctx context.Context, _ *connect.Request[restaurantv1.GetFloorRequest]) (*connect.Response[restaurantv1.Floor], error) {
	out, err := h.svc.GetFloor(ctx)
	return resp(out, err)
}

func (h *Handler) SaveArea(ctx context.Context, req *connect.Request[restaurantv1.SaveAreaRequest]) (*connect.Response[restaurantv1.Area], error) {
	out, err := h.svc.SaveArea(ctx, req.Msg.GetId(), req.Msg.GetName())
	return resp(out, err)
}

func (h *Handler) DeleteArea(ctx context.Context, req *connect.Request[restaurantv1.DeleteAreaRequest]) (*connect.Response[restaurantv1.DeleteAreaResponse], error) {
	if err := h.svc.DeleteArea(ctx, req.Msg.GetId()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&restaurantv1.DeleteAreaResponse{}), nil
}

func (h *Handler) MoveArea(ctx context.Context, req *connect.Request[restaurantv1.MoveAreaRequest]) (*connect.Response[restaurantv1.MoveAreaResponse], error) {
	if err := h.svc.MoveArea(ctx, req.Msg.GetId(), req.Msg.GetMove()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&restaurantv1.MoveAreaResponse{}), nil
}

func (h *Handler) SaveTable(ctx context.Context, req *connect.Request[restaurantv1.SaveTableRequest]) (*connect.Response[restaurantv1.Table], error) {
	out, err := h.svc.SaveTable(ctx, req.Msg.GetId(), req.Msg.GetAreaId(), req.Msg.GetName(), req.Msg.GetSeats(), req.Msg.GetActive())
	return resp(out, err)
}

func (h *Handler) MoveTable(ctx context.Context, req *connect.Request[restaurantv1.MoveTableRequest]) (*connect.Response[restaurantv1.MoveTableResponse], error) {
	if err := h.svc.MoveTable(ctx, req.Msg.GetId(), req.Msg.GetMove()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&restaurantv1.MoveTableResponse{}), nil
}

// --- StationService ---

func (h *Handler) ListStations(ctx context.Context, _ *connect.Request[restaurantv1.ListStationsRequest]) (*connect.Response[restaurantv1.ListStationsResponse], error) {
	stations, err := h.svc.ListStations(ctx)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&restaurantv1.ListStationsResponse{Stations: stations}), nil
}

func (h *Handler) SaveStation(ctx context.Context, req *connect.Request[restaurantv1.SaveStationRequest]) (*connect.Response[restaurantv1.Station], error) {
	out, err := h.svc.SaveStation(ctx, req.Msg.GetId(), req.Msg.GetName())
	return resp(out, err)
}

func (h *Handler) DeleteStation(ctx context.Context, req *connect.Request[restaurantv1.DeleteStationRequest]) (*connect.Response[restaurantv1.DeleteStationResponse], error) {
	if err := h.svc.DeleteStation(ctx, req.Msg.GetId()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&restaurantv1.DeleteStationResponse{}), nil
}

// --- MenuService ---

func (h *Handler) GetMenu(ctx context.Context, _ *connect.Request[restaurantv1.GetMenuRequest]) (*connect.Response[restaurantv1.Menu], error) {
	out, err := h.svc.GetMenu(ctx)
	return resp(out, err)
}

func (h *Handler) SaveCategory(ctx context.Context, req *connect.Request[restaurantv1.SaveCategoryRequest]) (*connect.Response[restaurantv1.Category], error) {
	out, err := h.svc.SaveCategory(ctx, req.Msg.GetId(), req.Msg.GetName(), req.Msg.GetActive())
	return resp(out, err)
}

func (h *Handler) DeleteCategory(ctx context.Context, req *connect.Request[restaurantv1.DeleteCategoryRequest]) (*connect.Response[restaurantv1.DeleteCategoryResponse], error) {
	if err := h.svc.DeleteCategory(ctx, req.Msg.GetId()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&restaurantv1.DeleteCategoryResponse{}), nil
}

func (h *Handler) MoveCategory(ctx context.Context, req *connect.Request[restaurantv1.MoveCategoryRequest]) (*connect.Response[restaurantv1.MoveCategoryResponse], error) {
	if err := h.svc.MoveCategory(ctx, req.Msg.GetId(), req.Msg.GetMove()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&restaurantv1.MoveCategoryResponse{}), nil
}

func (h *Handler) SaveItem(ctx context.Context, req *connect.Request[restaurantv1.SaveItemRequest]) (*connect.Response[restaurantv1.MenuItem], error) {
	out, err := h.svc.SaveItem(ctx, req.Msg.GetInput(), req.Msg.Version)
	return resp(out, err)
}

func (h *Handler) MoveItem(ctx context.Context, req *connect.Request[restaurantv1.MoveItemRequest]) (*connect.Response[restaurantv1.MoveItemResponse], error) {
	if err := h.svc.MoveItem(ctx, req.Msg.GetId(), req.Msg.GetMove()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&restaurantv1.MoveItemResponse{}), nil
}

func (h *Handler) SetItemImage(ctx context.Context, req *connect.Request[restaurantv1.SetItemImageRequest]) (*connect.Response[restaurantv1.MenuItem], error) {
	out, err := h.svc.SetItemImage(ctx, req.Msg.GetId(), req.Msg.GetImage(), req.Msg.GetContentType())
	return resp(out, err)
}

func (h *Handler) Set86(ctx context.Context, req *connect.Request[restaurantv1.Set86Request]) (*connect.Response[restaurantv1.Set86Response], error) {
	if err := h.svc.Set86(ctx, req.Msg.GetIds(), req.Msg.GetIs_86D()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&restaurantv1.Set86Response{}), nil
}

func (h *Handler) SaveModifierGroup(ctx context.Context, req *connect.Request[restaurantv1.SaveModifierGroupRequest]) (*connect.Response[restaurantv1.ModifierGroup], error) {
	out, err := h.svc.SaveModifierGroup(ctx, req.Msg.GetInput(), req.Msg.Version)
	return resp(out, err)
}

func (h *Handler) DeleteModifierGroup(ctx context.Context, req *connect.Request[restaurantv1.DeleteModifierGroupRequest]) (*connect.Response[restaurantv1.DeleteModifierGroupResponse], error) {
	if err := h.svc.DeleteModifierGroup(ctx, req.Msg.GetId()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&restaurantv1.DeleteModifierGroupResponse{}), nil
}

// resp wraps a value+error into a Connect response.
func resp[T any](v *T, err error) (*connect.Response[T], error) {
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(v), nil
}

func (h *Handler) SetSettingsImage(ctx context.Context, req *connect.Request[restaurantv1.SetSettingsImageRequest]) (*connect.Response[restaurantv1.Settings], error) {
	out, err := h.svc.SetSettingsImage(ctx, req.Msg.GetKind(), req.Msg.GetImage(), req.Msg.GetContentType())
	return resp(out, err)
}
