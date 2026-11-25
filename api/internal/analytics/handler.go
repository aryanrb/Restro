package analytics

import (
	"context"

	"connectrpc.com/connect"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
)

// Handler implements AnalyticsServiceHandler and CalendarServiceHandler.
type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func wrap[T any](v *T, err error) (*connect.Response[T], error) {
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(v), nil
}

func (h *Handler) GetMeta(ctx context.Context, _ *connect.Request[restaurantv1.GetAnalyticsMetaRequest]) (*connect.Response[restaurantv1.AnalyticsMeta], error) {
	return wrap(h.svc.Meta(ctx))
}

func (h *Handler) GetTrend(ctx context.Context, req *connect.Request[restaurantv1.GetTrendRequest]) (*connect.Response[restaurantv1.TrendResponse], error) {
	return wrap(h.svc.Trend(ctx, req.Msg))
}

func (h *Handler) GetHours(ctx context.Context, req *connect.Request[restaurantv1.GetHoursRequest]) (*connect.Response[restaurantv1.HoursResponse], error) {
	return wrap(h.svc.Hours(ctx, req.Msg))
}

func (h *Handler) GetMenu(ctx context.Context, req *connect.Request[restaurantv1.GetMenuAnalyticsRequest]) (*connect.Response[restaurantv1.MenuAnalyticsResponse], error) {
	return wrap(h.svc.Menu(ctx, req.Msg))
}

func (h *Handler) GetTables(ctx context.Context, req *connect.Request[restaurantv1.GetTablesAnalyticsRequest]) (*connect.Response[restaurantv1.TablesAnalyticsResponse], error) {
	return wrap(h.svc.Tables(ctx, req.Msg))
}

func (h *Handler) GetStaff(ctx context.Context, req *connect.Request[restaurantv1.GetStaffAnalyticsRequest]) (*connect.Response[restaurantv1.StaffAnalyticsResponse], error) {
	return wrap(h.svc.Staff(ctx, req.Msg))
}

func (h *Handler) GetMoney(ctx context.Context, req *connect.Request[restaurantv1.GetMoneyAnalyticsRequest]) (*connect.Response[restaurantv1.MoneyAnalyticsResponse], error) {
	return wrap(h.svc.Money(ctx, req.Msg))
}

func (h *Handler) GetHighlights(ctx context.Context, req *connect.Request[restaurantv1.GetHighlightsRequest]) (*connect.Response[restaurantv1.HighlightsResponse], error) {
	return wrap(h.svc.Highlights(ctx, req.Msg))
}

func (h *Handler) GetCustomers(ctx context.Context, req *connect.Request[restaurantv1.GetCustomersAnalyticsRequest]) (*connect.Response[restaurantv1.CustomersAnalyticsResponse], error) {
	return wrap(h.svc.Customers(ctx, req.Msg))
}

func (h *Handler) Rebuild(ctx context.Context, req *connect.Request[restaurantv1.RebuildAnalyticsRequest]) (*connect.Response[restaurantv1.RebuildAnalyticsResponse], error) {
	return wrap(h.svc.Rebuild(ctx, req.Msg))
}

func (h *Handler) GetSettings(ctx context.Context, _ *connect.Request[restaurantv1.GetAnalyticsSettingsRequest]) (*connect.Response[restaurantv1.AnalyticsSettings], error) {
	return wrap(h.svc.GetSettings(ctx))
}

func (h *Handler) SaveSettings(ctx context.Context, req *connect.Request[restaurantv1.SaveAnalyticsSettingsRequest]) (*connect.Response[restaurantv1.AnalyticsSettings], error) {
	return wrap(h.svc.SaveSettings(ctx, req.Msg.GetSettings()))
}

func (h *Handler) ListCalendarEvents(ctx context.Context, req *connect.Request[restaurantv1.ListCalendarEventsRequest]) (*connect.Response[restaurantv1.ListCalendarEventsResponse], error) {
	return wrap(h.svc.ListCalendarEvents(ctx, req.Msg))
}

func (h *Handler) SaveCalendarEvent(ctx context.Context, req *connect.Request[restaurantv1.SaveCalendarEventRequest]) (*connect.Response[restaurantv1.CalendarEvent], error) {
	return wrap(h.svc.SaveCalendarEvent(ctx, req.Msg))
}

func (h *Handler) RemoveCalendarEvent(ctx context.Context, req *connect.Request[restaurantv1.RemoveCalendarEventRequest]) (*connect.Response[restaurantv1.RemoveCalendarEventResponse], error) {
	if err := h.svc.RemoveCalendarEvent(ctx, req.Msg.GetId()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&restaurantv1.RemoveCalendarEventResponse{}), nil
}
