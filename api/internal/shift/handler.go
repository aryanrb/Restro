package shift

import (
	"context"

	"connectrpc.com/connect"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
)

// Handler implements ShiftServiceHandler.
type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) OpenShift(ctx context.Context, req *connect.Request[restaurantv1.OpenShiftRequest]) (*connect.Response[restaurantv1.Shift], error) {
	sh, err := h.svc.OpenShift(ctx, req.Msg.GetOpeningFloatPaisa(), req.Msg.GetNote())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(sh), nil
}

func (h *Handler) GetMyOpenShift(ctx context.Context, _ *connect.Request[restaurantv1.GetMyOpenShiftRequest]) (*connect.Response[restaurantv1.MyOpenShiftResponse], error) {
	sh, err := h.svc.GetMyOpenShift(ctx)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&restaurantv1.MyOpenShiftResponse{Shift: sh}), nil
}

func (h *Handler) ListOpenShifts(ctx context.Context, _ *connect.Request[restaurantv1.ListOpenShiftsRequest]) (*connect.Response[restaurantv1.ListOpenShiftsResponse], error) {
	shifts, err := h.svc.ListOpenShifts(ctx)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&restaurantv1.ListOpenShiftsResponse{Shifts: shifts}), nil
}

func (h *Handler) CloseShift(ctx context.Context, req *connect.Request[restaurantv1.CloseShiftRequest]) (*connect.Response[restaurantv1.ShiftReport], error) {
	rep, err := h.svc.CloseShift(ctx, req.Msg.GetShiftId(), req.Msg.GetCountedCashPaisa(), req.Msg.GetNote())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(rep), nil
}

func (h *Handler) GetShiftReport(ctx context.Context, req *connect.Request[restaurantv1.GetShiftReportRequest]) (*connect.Response[restaurantv1.ShiftReport], error) {
	rep, err := h.svc.GetShiftReport(ctx, req.Msg.GetShiftId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(rep), nil
}
