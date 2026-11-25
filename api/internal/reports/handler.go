package reports

import (
	"context"

	"connectrpc.com/connect"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
)

// Handler implements ReportServiceHandler.
type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Dashboard(ctx context.Context, _ *connect.Request[restaurantv1.DashboardRequest]) (*connect.Response[restaurantv1.DashboardResponse], error) {
	return wrap(h.svc.Dashboard(ctx))
}

func (h *Handler) SalesBook(ctx context.Context, req *connect.Request[restaurantv1.RangeRequest]) (*connect.Response[restaurantv1.SalesBookResponse], error) {
	return wrap(h.svc.SalesBook(ctx, req.Msg))
}

func (h *Handler) ExportSalesBookCsv(ctx context.Context, req *connect.Request[restaurantv1.RangeRequest]) (*connect.Response[restaurantv1.CsvExport], error) {
	return wrap(h.svc.ExportSalesBookCsv(ctx, req.Msg))
}

func (h *Handler) DailySales(ctx context.Context, req *connect.Request[restaurantv1.RangeRequest]) (*connect.Response[restaurantv1.DailySalesResponse], error) {
	return wrap(h.svc.DailySales(ctx, req.Msg.GetFromUnix(), req.Msg.GetToUnix()))
}

func (h *Handler) CancelledInvoices(ctx context.Context, req *connect.Request[restaurantv1.RangeRequest]) (*connect.Response[restaurantv1.CancelledInvoicesResponse], error) {
	return wrap(h.svc.CancelledInvoices(ctx, req.Msg.GetFromUnix(), req.Msg.GetToUnix()))
}

func (h *Handler) AuditLog(ctx context.Context, req *connect.Request[restaurantv1.AuditLogRequest]) (*connect.Response[restaurantv1.AuditLogResponse], error) {
	return wrap(h.svc.AuditLog(ctx, req.Msg))
}

func (h *Handler) CreditReport(ctx context.Context, _ *connect.Request[restaurantv1.CreditReportRequest]) (*connect.Response[restaurantv1.CreditReportResponse], error) {
	return wrap(h.svc.CreditReport(ctx))
}

func wrap[T any](v *T, err error) (*connect.Response[T], error) {
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(v), nil
}
