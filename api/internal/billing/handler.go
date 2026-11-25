package billing

import (
	"context"

	"connectrpc.com/connect"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
)

// Handler implements BillingServiceHandler.
type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) PreviewBill(ctx context.Context, req *connect.Request[restaurantv1.PreviewBillRequest]) (*connect.Response[restaurantv1.BillPreview], error) {
	return wrap(h.svc.PreviewBill(ctx, req.Msg))
}

func (h *Handler) IssueFullBill(ctx context.Context, req *connect.Request[restaurantv1.IssueFullBillRequest]) (*connect.Response[restaurantv1.Invoice], error) {
	return wrap(h.svc.IssueFullBill(ctx, req.Msg))
}

func (h *Handler) SplitByItem(ctx context.Context, req *connect.Request[restaurantv1.SplitByItemRequest]) (*connect.Response[restaurantv1.SplitByItemResponse], error) {
	return wrap(h.svc.SplitByItem(ctx, req.Msg))
}

func (h *Handler) RecordPayment(ctx context.Context, req *connect.Request[restaurantv1.RecordPaymentRequest]) (*connect.Response[restaurantv1.Payment], error) {
	return wrap(h.svc.RecordPayment(ctx, req.Msg))
}

func (h *Handler) CancelInvoice(ctx context.Context, req *connect.Request[restaurantv1.CancelInvoiceRequest]) (*connect.Response[restaurantv1.Invoice], error) {
	return wrap(h.svc.CancelInvoice(ctx, req.Msg))
}

func (h *Handler) MarkPrinted(ctx context.Context, req *connect.Request[restaurantv1.MarkPrintedRequest]) (*connect.Response[restaurantv1.PrintInfo], error) {
	return wrap(h.svc.MarkPrinted(ctx, req.Msg))
}

func (h *Handler) GetInvoice(ctx context.Context, req *connect.Request[restaurantv1.GetInvoiceRequest]) (*connect.Response[restaurantv1.Invoice], error) {
	return wrap(h.svc.GetInvoice(ctx, req.Msg))
}

func (h *Handler) IssueBill(ctx context.Context, req *connect.Request[restaurantv1.IssueBillRequest]) (*connect.Response[restaurantv1.Invoice], error) {
	return wrap(h.svc.IssueBill(ctx, req.Msg))
}

func (h *Handler) CounterSale(ctx context.Context, req *connect.Request[restaurantv1.CounterSaleRequest]) (*connect.Response[restaurantv1.Invoice], error) {
	return wrap(h.svc.CounterSale(ctx, req.Msg))
}

func (h *Handler) ListInvoices(ctx context.Context, req *connect.Request[restaurantv1.ListInvoicesRequest]) (*connect.Response[restaurantv1.ListInvoicesResponse], error) {
	return wrap(h.svc.ListInvoices(ctx, req.Msg))
}

func wrap[T any](v *T, err error) (*connect.Response[T], error) {
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(v), nil
}
