package reservations

import (
	"context"

	"connectrpc.com/connect"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
)

// Handler implements restaurantv1connect.ReservationServiceHandler (thin: proto in, proto out).
type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func wrap[T any](v *T, err error) (*connect.Response[T], error) {
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(v), nil
}

func (h *Handler) ListReservations(ctx context.Context, req *connect.Request[restaurantv1.ListReservationsRequest]) (*connect.Response[restaurantv1.ListReservationsResponse], error) {
	return wrap(h.svc.ListReservations(ctx, req.Msg))
}

func (h *Handler) GetReservation(ctx context.Context, req *connect.Request[restaurantv1.GetReservationRequest]) (*connect.Response[restaurantv1.Reservation], error) {
	return wrap(h.svc.GetReservation(ctx, req.Msg))
}

func (h *Handler) CreateReservation(ctx context.Context, req *connect.Request[restaurantv1.CreateReservationRequest]) (*connect.Response[restaurantv1.Reservation], error) {
	return wrap(h.svc.CreateReservation(ctx, req.Msg))
}

func (h *Handler) UpdateReservation(ctx context.Context, req *connect.Request[restaurantv1.UpdateReservationRequest]) (*connect.Response[restaurantv1.Reservation], error) {
	return wrap(h.svc.UpdateReservation(ctx, req.Msg))
}

func (h *Handler) CancelReservation(ctx context.Context, req *connect.Request[restaurantv1.CancelReservationRequest]) (*connect.Response[restaurantv1.Reservation], error) {
	return wrap(h.svc.CancelReservation(ctx, req.Msg))
}

func (h *Handler) MarkNoShow(ctx context.Context, req *connect.Request[restaurantv1.MarkNoShowRequest]) (*connect.Response[restaurantv1.Reservation], error) {
	return wrap(h.svc.MarkNoShow(ctx, req.Msg))
}

func (h *Handler) RestoreReservation(ctx context.Context, req *connect.Request[restaurantv1.RestoreReservationRequest]) (*connect.Response[restaurantv1.Reservation], error) {
	return wrap(h.svc.RestoreReservation(ctx, req.Msg))
}

func (h *Handler) ExtendHold(ctx context.Context, req *connect.Request[restaurantv1.ExtendHoldRequest]) (*connect.Response[restaurantv1.Reservation], error) {
	return wrap(h.svc.ExtendHold(ctx, req.Msg))
}

func (h *Handler) SeatReservation(ctx context.Context, req *connect.Request[restaurantv1.SeatReservationRequest]) (*connect.Response[restaurantv1.Order], error) {
	return wrap(h.svc.SeatReservation(ctx, req.Msg))
}

func (h *Handler) SuggestTables(ctx context.Context, req *connect.Request[restaurantv1.SuggestTablesRequest]) (*connect.Response[restaurantv1.SuggestTablesResponse], error) {
	return wrap(h.svc.SuggestTables(ctx, req.Msg))
}

func (h *Handler) MarkMessaged(ctx context.Context, req *connect.Request[restaurantv1.MarkMessagedRequest]) (*connect.Response[restaurantv1.Reservation], error) {
	return wrap(h.svc.MarkMessaged(ctx, req.Msg))
}

func (h *Handler) FindBookingCustomers(ctx context.Context, req *connect.Request[restaurantv1.FindBookingCustomersRequest]) (*connect.Response[restaurantv1.FindBookingCustomersResponse], error) {
	return wrap(h.svc.FindBookingCustomers(ctx, req.Msg))
}

func (h *Handler) ListCustomerReservations(ctx context.Context, req *connect.Request[restaurantv1.ListCustomerReservationsRequest]) (*connect.Response[restaurantv1.ListReservationsResponse], error) {
	return wrap(h.svc.ListCustomerReservations(ctx, req.Msg))
}

func (h *Handler) GetReservationSettings(ctx context.Context, req *connect.Request[restaurantv1.GetReservationSettingsRequest]) (*connect.Response[restaurantv1.ReservationSettings], error) {
	return wrap(h.svc.GetReservationSettings(ctx, req.Msg))
}

func (h *Handler) SaveReservationSettings(ctx context.Context, req *connect.Request[restaurantv1.ReservationSettings]) (*connect.Response[restaurantv1.ReservationSettings], error) {
	return wrap(h.svc.SaveReservationSettings(ctx, req.Msg))
}

func (h *Handler) ReservationReport(ctx context.Context, req *connect.Request[restaurantv1.ReservationReportRequest]) (*connect.Response[restaurantv1.ReservationReportResponse], error) {
	return wrap(h.svc.ReservationReport(ctx, req.Msg))
}

func (h *Handler) GetReservationsToday(ctx context.Context, req *connect.Request[restaurantv1.ReservationsTodayRequest]) (*connect.Response[restaurantv1.ReservationsToday], error) {
	return wrap(h.svc.GetReservationsToday(ctx, req.Msg))
}
