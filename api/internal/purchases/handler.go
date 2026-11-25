package purchases

import (
	"context"

	"connectrpc.com/connect"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
)

// Handler implements SupplierServiceHandler and PurchaseServiceHandler.
type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func wrap[T any](out *T, err error) (*connect.Response[T], error) {
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

// --- SupplierService ---

func (h *Handler) ListSuppliers(ctx context.Context, req *connect.Request[restaurantv1.ListSuppliersRequest]) (*connect.Response[restaurantv1.ListSuppliersResponse], error) {
	return wrap(h.svc.ListSuppliers(ctx, req.Msg))
}

func (h *Handler) GetSupplier(ctx context.Context, req *connect.Request[restaurantv1.GetSupplierRequest]) (*connect.Response[restaurantv1.Supplier], error) {
	return wrap(h.svc.GetSupplier(ctx, req.Msg.GetId()))
}

func (h *Handler) CreateSupplier(ctx context.Context, req *connect.Request[restaurantv1.CreateSupplierRequest]) (*connect.Response[restaurantv1.Supplier], error) {
	return wrap(h.svc.CreateSupplier(ctx, req.Msg))
}

func (h *Handler) UpdateSupplier(ctx context.Context, req *connect.Request[restaurantv1.UpdateSupplierRequest]) (*connect.Response[restaurantv1.Supplier], error) {
	return wrap(h.svc.UpdateSupplier(ctx, req.Msg))
}

func (h *Handler) SetSupplierActive(ctx context.Context, req *connect.Request[restaurantv1.SetSupplierActiveRequest]) (*connect.Response[restaurantv1.Supplier], error) {
	return wrap(h.svc.SetSupplierActive(ctx, req.Msg))
}

func (h *Handler) GetSupplierStatement(ctx context.Context, req *connect.Request[restaurantv1.SupplierStatementRequest]) (*connect.Response[restaurantv1.SupplierStatement], error) {
	return wrap(h.svc.GetSupplierStatement(ctx, req.Msg))
}

// --- PurchaseService ---

func (h *Handler) RecordPurchase(ctx context.Context, req *connect.Request[restaurantv1.RecordPurchaseRequest]) (*connect.Response[restaurantv1.Purchase], error) {
	return wrap(h.svc.RecordPurchase(ctx, req.Msg))
}

func (h *Handler) VoidPurchase(ctx context.Context, req *connect.Request[restaurantv1.VoidPurchaseRequest]) (*connect.Response[restaurantv1.Purchase], error) {
	return wrap(h.svc.VoidPurchase(ctx, req.Msg))
}

func (h *Handler) ReturnPurchase(ctx context.Context, req *connect.Request[restaurantv1.ReturnPurchaseRequest]) (*connect.Response[restaurantv1.PurchaseReturn], error) {
	return wrap(h.svc.ReturnPurchase(ctx, req.Msg))
}

func (h *Handler) ListPurchases(ctx context.Context, req *connect.Request[restaurantv1.ListPurchasesRequest]) (*connect.Response[restaurantv1.ListPurchasesResponse], error) {
	list, err := h.svc.ListPurchases(ctx, req.Msg)
	return wrap(&restaurantv1.ListPurchasesResponse{Purchases: list}, err)
}

func (h *Handler) GetPurchase(ctx context.Context, req *connect.Request[restaurantv1.GetPurchaseRequest]) (*connect.Response[restaurantv1.Purchase], error) {
	return wrap(h.svc.GetPurchase(ctx, req.Msg.GetId()))
}

func (h *Handler) PaySupplier(ctx context.Context, req *connect.Request[restaurantv1.PaySupplierRequest]) (*connect.Response[restaurantv1.SupplierPayment], error) {
	return wrap(h.svc.PaySupplier(ctx, req.Msg))
}

func (h *Handler) VoidSupplierPayment(ctx context.Context, req *connect.Request[restaurantv1.VoidSupplierPaymentRequest]) (*connect.Response[restaurantv1.SupplierPayment], error) {
	return wrap(h.svc.VoidSupplierPayment(ctx, req.Msg))
}

func (h *Handler) ListSupplierPayments(ctx context.Context, req *connect.Request[restaurantv1.ListSupplierPaymentsRequest]) (*connect.Response[restaurantv1.ListSupplierPaymentsResponse], error) {
	list, err := h.svc.ListSupplierPayments(ctx, req.Msg.GetSupplierId())
	return wrap(&restaurantv1.ListSupplierPaymentsResponse{Payments: list}, err)
}

func (h *Handler) ListPurchaseSettings(ctx context.Context, _ *connect.Request[restaurantv1.ListPurchaseSettingsRequest]) (*connect.Response[restaurantv1.ListPurchaseSettingsResponse], error) {
	list, err := h.svc.ListPurchaseSettings(ctx)
	return wrap(&restaurantv1.ListPurchaseSettingsResponse{Settings: list}, err)
}

func (h *Handler) SavePurchaseSettings(ctx context.Context, req *connect.Request[restaurantv1.SavePurchaseSettingsRequest]) (*connect.Response[restaurantv1.ItemPurchaseSettings], error) {
	return wrap(h.svc.SavePurchaseSettings(ctx, req.Msg.GetSettings()))
}

func (h *Handler) SuggestedPurchases(ctx context.Context, _ *connect.Request[restaurantv1.SuggestedPurchasesRequest]) (*connect.Response[restaurantv1.SuggestedPurchasesResponse], error) {
	groups, err := h.svc.SuggestedPurchases(ctx)
	return wrap(&restaurantv1.SuggestedPurchasesResponse{Groups: groups}, err)
}

func (h *Handler) PurchaseBook(ctx context.Context, req *connect.Request[restaurantv1.PurchaseBookRequest]) (*connect.Response[restaurantv1.PurchaseBookResponse], error) {
	rows, err := h.svc.PurchaseBook(ctx, req.Msg)
	return wrap(&restaurantv1.PurchaseBookResponse{Rows: rows}, err)
}

func (h *Handler) Payables(ctx context.Context, _ *connect.Request[restaurantv1.PayablesRequest]) (*connect.Response[restaurantv1.PayablesResponse], error) {
	rows, err := h.svc.Payables(ctx)
	return wrap(&restaurantv1.PayablesResponse{Rows: rows}, err)
}

func (h *Handler) GetPurchasesToday(ctx context.Context, _ *connect.Request[restaurantv1.PurchasesTodayRequest]) (*connect.Response[restaurantv1.PurchasesToday], error) {
	return wrap(h.svc.GetPurchasesToday(ctx))
}
