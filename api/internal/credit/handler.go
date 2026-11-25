package credit

import (
	"context"

	"connectrpc.com/connect"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
)

// Handler implements CustomerServiceHandler and CreditServiceHandler.
type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) ListCustomers(ctx context.Context, req *connect.Request[restaurantv1.ListCustomersRequest]) (*connect.Response[restaurantv1.ListCustomersResponse], error) {
	return wrap(h.svc.ListCustomers(ctx, req.Msg))
}

func (h *Handler) GetCustomer(ctx context.Context, req *connect.Request[restaurantv1.GetCustomerRequest]) (*connect.Response[restaurantv1.Customer], error) {
	return wrap(h.svc.GetCustomer(ctx, req.Msg))
}

func (h *Handler) CreateCustomer(ctx context.Context, req *connect.Request[restaurantv1.CreateCustomerRequest]) (*connect.Response[restaurantv1.Customer], error) {
	return wrap(h.svc.CreateCustomer(ctx, req.Msg))
}

func (h *Handler) UpdateCustomer(ctx context.Context, req *connect.Request[restaurantv1.UpdateCustomerRequest]) (*connect.Response[restaurantv1.Customer], error) {
	return wrap(h.svc.UpdateCustomer(ctx, req.Msg))
}

func (h *Handler) SetCustomerActive(ctx context.Context, req *connect.Request[restaurantv1.SetCustomerActiveRequest]) (*connect.Response[restaurantv1.Customer], error) {
	return wrap(h.svc.SetCustomerActive(ctx, req.Msg))
}

func (h *Handler) Repay(ctx context.Context, req *connect.Request[restaurantv1.RepayRequest]) (*connect.Response[restaurantv1.Repayment], error) {
	return wrap(h.svc.Repay(ctx, req.Msg))
}

func (h *Handler) VoidRepayment(ctx context.Context, req *connect.Request[restaurantv1.VoidRepaymentRequest]) (*connect.Response[restaurantv1.Repayment], error) {
	return wrap(h.svc.VoidRepayment(ctx, req.Msg))
}

func (h *Handler) ListRepayments(ctx context.Context, req *connect.Request[restaurantv1.ListRepaymentsRequest]) (*connect.Response[restaurantv1.ListRepaymentsResponse], error) {
	return wrap(h.svc.ListRepayments(ctx, req.Msg))
}

func (h *Handler) GetStatement(ctx context.Context, req *connect.Request[restaurantv1.StatementRequest]) (*connect.Response[restaurantv1.Statement], error) {
	return wrap(h.svc.GetStatement(ctx, req.Msg))
}

func wrap[T any](v *T, err error) (*connect.Response[T], error) {
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(v), nil
}
