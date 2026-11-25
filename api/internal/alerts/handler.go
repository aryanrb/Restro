package alerts

import (
	"context"
	"encoding/json"

	"connectrpc.com/connect"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
)

// Handler implements restaurantv1connect.AlertServiceHandler.
type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func wrap[T any](v *T, err error) (*connect.Response[T], error) {
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(v), nil
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func (h *Handler) ListAlerts(ctx context.Context, req *connect.Request[restaurantv1.ListAlertsRequest]) (*connect.Response[restaurantv1.ListAlertsResponse], error) {
	return wrap(h.svc.ListAlerts(ctx, req.Msg))
}

func (h *Handler) GetAlert(ctx context.Context, req *connect.Request[restaurantv1.GetAlertRequest]) (*connect.Response[restaurantv1.GetAlertResponse], error) {
	return wrap(h.svc.GetAlert(ctx, req.Msg))
}

func (h *Handler) ResolveAlerts(ctx context.Context, req *connect.Request[restaurantv1.ResolveAlertsRequest]) (*connect.Response[restaurantv1.ResolveAlertsResponse], error) {
	return wrap(h.svc.ResolveAlerts(ctx, req.Msg))
}

func (h *Handler) GetAlertCounts(ctx context.Context, _ *connect.Request[restaurantv1.GetAlertCountsRequest]) (*connect.Response[restaurantv1.GetAlertCountsResponse], error) {
	return wrap(h.svc.GetAlertCounts(ctx))
}

func (h *Handler) GetAlertConfig(ctx context.Context, _ *connect.Request[restaurantv1.GetAlertConfigRequest]) (*connect.Response[restaurantv1.AlertConfig], error) {
	return wrap(h.svc.GetAlertConfig(ctx))
}

func (h *Handler) UpdateAlertConfig(ctx context.Context, req *connect.Request[restaurantv1.UpdateAlertConfigRequest]) (*connect.Response[restaurantv1.AlertConfig], error) {
	return wrap(h.svc.UpdateAlertConfig(ctx, req.Msg))
}

func (h *Handler) GetDaySummary(ctx context.Context, req *connect.Request[restaurantv1.GetDaySummaryRequest]) (*connect.Response[restaurantv1.DaySummary], error) {
	return wrap(h.svc.GetDaySummary(ctx, req.Msg))
}

func (h *Handler) ListNotifications(ctx context.Context, _ *connect.Request[restaurantv1.ListNotificationsRequest]) (*connect.Response[restaurantv1.ListNotificationsResponse], error) {
	return wrap(h.svc.ListNotifications(ctx))
}
