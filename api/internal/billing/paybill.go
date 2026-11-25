package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/audit"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/bsdate"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/credit"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/idem"
	"github.com/oneaccord/restro/api/internal/orders"
	"github.com/oneaccord/restro/api/internal/realtime"
)

// requireBillingRole admits the roles that take money and read invoices.
func requireBillingRole(ctx context.Context) (*auth.Actor, error) {
	return auth.RequireRole(ctx, auth.RoleOwner, auth.RoleManager, auth.RoleCashier)
}

// tender is a validated PaymentInput.
type tender struct {
	method     string
	amount     int64
	ref        string
	provider   PaymentProvider // nil for ON_ACCOUNT (no money moves)
	customerID uuid.UUID       // ON_ACCOUNT only
}

// validateTenders checks the payment list's shape: non-empty, every amount > 0,
// every method known. The sum is checked against the total later.
func (s *Service) validateTenders(in []*restaurantv1.PaymentInput) ([]tender, error) {
	if len(in) == 0 {
		return nil, connecterr.Invalid("payments", "at least one payment is required")
	}
	out := make([]tender, 0, len(in))
	for _, p := range in {
		if p.GetAmountPaisa() <= 0 {
			return nil, connecterr.Invalid("payments", "every payment amount must be more than zero")
		}
		method := paymentMethodStr(p.GetMethod())
		if method == methodOnAccount {
			id, err := uuid.Parse(p.GetCustomerId())
			if err != nil {
				return nil, connecterr.Invalid("customer_id", "choose the customer this bill is on account to")
			}
			out = append(out, tender{method: method, amount: p.GetAmountPaisa(), ref: p.GetReference(), customerID: id})
			continue
		}
		provider, ok := s.providers[method]
		if !ok {
			return nil, connecterr.Invalid("payments", "unsupported payment method")
		}
		out = append(out, tender{method: method, amount: p.GetAmountPaisa(), ref: p.GetReference(), provider: provider})
	}
	return out, nil
}

// lockShift returns the actor's open shift, share-locked for the transaction.
func lockShift(ctx context.Context, q *sqlc.Queries, userID uuid.UUID) (sqlc.Shift, error) {
	sh, err := q.LockOpenShiftForUser(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlc.Shift{}, connecterr.ShiftNotOpen("open a shift before taking payment")
	}
	if err != nil {
		return sqlc.Shift{}, connecterr.Internal(err)
	}
	return sh, nil
}

// payBill is the shared issue-and-pay core of IssueBill and CounterSale. It runs
// inside the caller's transaction after loadForBilling locked the order: it
// prices items (with the role's discount limit), requires the tenders to sum to
// the invoice total, issues the invoice, records the payments against shift,
// marks the order BILLED once nothing is left unbilled, and applies the close
// rule. It reports whether the order closed. Any error rolls everything back,
// including the invoice number allocation.
//
// On-account tenders are checked (account active, within the available credit or
// approved by creditApproval / an owner-manager biller) under a lock on each
// customer before the invoice is numbered; a business account fills a blank buyer
// name/PAN. Their ids are returned for the customer.updated event.
func (s *Service) payBill(ctx context.Context, q *sqlc.Queries, actor *auth.Actor, shift sqlc.Shift, order sqlc.Order, items []sqlc.OrderItem, settings sqlc.Setting,
	disc *restaurantv1.DiscountInput, buyerName, buyerPan, label string, tenders []tender, creditApproval *restaurantv1.ManagerApproval) (sqlc.Invoice, bool, []string, error) {
	subtotal, byItem, err := s.subtotalOf(ctx, q, order.ID, items)
	if err != nil {
		return sqlc.Invoice{}, false, nil, err
	}
	discount, reason, approvedBy, err := s.computeDiscount(ctx, disc, subtotal, actor)
	if err != nil {
		return sqlc.Invoice{}, false, nil, err
	}
	total := s.compute(settings, subtotal, discount).Total
	var paid int64
	for _, t := range tenders {
		paid += t.amount
	}
	if paid != total {
		return sqlc.Invoice{}, false, nil, connecterr.Invalid("payments", fmt.Sprintf("payments add up to %d paisa but the bill is %d paisa", paid, total))
	}

	onAccount := map[uuid.UUID]int64{}
	var accountTenders []credit.Tender
	for _, t := range tenders {
		if t.method == methodOnAccount {
			onAccount[t.customerID] += t.amount
			accountTenders = append(accountTenders, credit.Tender{CustomerID: t.customerID, Amount: t.amount, Reference: t.ref})
		}
	}
	var checks []credit.OnAccount
	if len(onAccount) > 0 {
		if checks, err = credit.CheckOnAccount(ctx, q, actor, onAccount, creditApproval, s.auth.VerifyManagerApproval); err != nil {
			return sqlc.Invoice{}, false, nil, err
		}
		if name, pan, ok := credit.BusinessBuyer(checks); ok && buyerName == "" && buyerPan == "" {
			buyerName, buyerPan = name, pan
		}
	}

	inv, err := s.issueInvoiceForItems(ctx, q, order, items, byItem, subtotal, settings, discount, reason, approvedBy, buyerName, buyerPan, actor, label)
	if err != nil {
		return sqlc.Invoice{}, false, nil, err
	}
	for _, t := range tenders {
		if t.method == methodOnAccount {
			continue // recorded below, with the receipt snapshot
		}
		status, ref, e := t.provider.Record(ctx, t.amount, t.ref)
		if e != nil {
			return sqlc.Invoice{}, false, nil, connecterr.Internal(e)
		}
		if _, e := q.CreatePayment(ctx, sqlc.CreatePaymentParams{
			InvoiceID: inv.ID, Method: t.method, AmountPaisa: t.amount, Reference: ref,
			ConfirmedByUserID: actor.User.ID, ShiftID: shift.ID, Status: status,
		}); e != nil {
			return sqlc.Invoice{}, false, nil, connecterr.Internal(e)
		}
		if e := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: "payment.record", Entity: "invoice", EntityID: inv.ID.String(), After: audit.JSON(map[string]any{"method": t.method, "amount": t.amount})}); e != nil {
			return sqlc.Invoice{}, false, nil, e
		}
	}

	if len(checks) > 0 {
		if e := credit.RecordOnAccount(ctx, q, actor, inv.ID, shift.ID, inv.DisplayNumber, checks, accountTenders); e != nil {
			return sqlc.Invoice{}, false, nil, e
		}
	}
	customers := make([]string, 0, len(checks))
	for _, c := range checks {
		customers = append(customers, c.Customer.ID.String())
	}

	remaining, err := q.ListUnbilledOrderItems(ctx, order.ID)
	if err != nil {
		return sqlc.Invoice{}, false, nil, connecterr.Internal(err)
	}
	if len(remaining) == 0 {
		if e := q.SetOrderStatus(ctx, sqlc.SetOrderStatusParams{ID: order.ID, Status: "BILLED"}); e != nil {
			return sqlc.Invoice{}, false, nil, connecterr.Internal(e)
		}
	}
	closed, err := orders.CloseIfDone(ctx, q, order.ID)
	if err != nil {
		return sqlc.Invoice{}, false, nil, err
	}
	return inv, closed, customers, nil
}

// IssueBill issues an invoice for the whole order (item_ids empty) or a subset of
// its unbilled items, and records its payments, in one idempotent transaction.
func (s *Service) IssueBill(ctx context.Context, req *restaurantv1.IssueBillRequest) (*restaurantv1.Invoice, error) {
	actor, err := requireBillingRole(ctx)
	if err != nil {
		return nil, err
	}
	cmdID, err := parseID("command_id", req.GetCommandId())
	if err != nil {
		return nil, err
	}
	ocid, err := parseID("order_client_id", req.GetOrderClientId())
	if err != nil {
		return nil, err
	}

	var freedTable string
	var customers []string
	out := &restaurantv1.Invoice{}
	err = idem.Run(ctx, s.pool, s.q, cmdID, actor.User.ID, out, func(q *sqlc.Queries) error {
		shift, e := lockShift(ctx, q, actor.User.ID)
		if e != nil {
			return e
		}
		tenders, e := s.validateTenders(req.GetPayments())
		if e != nil {
			return e
		}
		order, unbilled, settings, e := s.loadForBilling(ctx, q, ocid)
		if e != nil {
			return e
		}
		items, e := pickItems(unbilled, req.GetItemIds())
		if e != nil {
			return e
		}
		inv, closed, cust, e := s.payBill(ctx, q, actor, shift, order, items, settings, req.GetDiscount(), req.GetBuyerName(), req.GetBuyerPan(), "", tenders, req.GetCreditApproval())
		if e != nil {
			return e
		}
		customers = cust
		if closed {
			freedTable = tableIDStr(order)
		}
		full, e := s.loadInvoice(ctx, q, inv.ID)
		if e != nil {
			return e
		}
		proto.Merge(out, full)
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "order.updated", ID: out.OrderId})
	if freedTable != "" {
		s.emit(realtime.Event{Type: "table.updated", ID: freedTable})
	}
	s.emit(realtime.Event{Type: "invoice.issued", ID: out.Id})
	s.emitCustomers(customers)
	return out, nil
}

// pickItems returns all unbilled items when ids is empty, else exactly the named
// ones; each must be an unbilled, non-voided item of the order, named once.
func pickItems(unbilled []sqlc.OrderItem, ids []string) ([]sqlc.OrderItem, error) {
	if len(ids) == 0 {
		if len(unbilled) == 0 {
			return nil, connecterr.Invalid("order", "nothing to bill")
		}
		return unbilled, nil
	}
	byID := make(map[uuid.UUID]sqlc.OrderItem, len(unbilled))
	for _, it := range unbilled {
		byID[it.ID] = it
	}
	out := make([]sqlc.OrderItem, 0, len(ids))
	for _, s := range ids {
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, connecterr.Invalid("item_ids", "invalid id")
		}
		it, ok := byID[id]
		if !ok {
			return nil, connecterr.Invalid("item_ids", "item is not billable on this order")
		}
		delete(byID, id) // a repeated id is rejected on its second appearance
		out = append(out, it)
	}
	return out, nil
}

// CounterSale (pay first) creates a TAKEAWAY order with its lines already SENT and
// issues + pays its invoice, in one idempotent transaction. Any rejected line
// rejects the whole sale. The order stays BILLED until the kitchen marks it READY.
//
// The request's label is stored as the order's label and printed as the invoice's place.
func (s *Service) CounterSale(ctx context.Context, req *restaurantv1.CounterSaleRequest) (*restaurantv1.Invoice, error) {
	actor, err := requireBillingRole(ctx)
	if err != nil {
		return nil, err
	}
	cmdID, err := parseID("command_id", req.GetCommandId())
	if err != nil {
		return nil, err
	}
	ocid := uuid.New()
	if req.GetOrderClientId() != "" {
		if ocid, err = parseID("order_client_id", req.GetOrderClientId()); err != nil {
			return nil, err
		}
	}

	var orderVersion int64
	var customers []string
	out := &restaurantv1.Invoice{}
	err = idem.Run(ctx, s.pool, s.q, cmdID, actor.User.ID, out, func(q *sqlc.Queries) error {
		shift, e := lockShift(ctx, q, actor.User.ID)
		if e != nil {
			return e
		}
		if len(req.GetLines()) == 0 {
			return connecterr.Invalid("lines", "add at least one item")
		}
		tenders, e := s.validateTenders(req.GetPayments())
		if e != nil {
			return e
		}
		o, e := q.CreateOrder(ctx, sqlc.CreateOrderParams{ClientID: ocid, Type: "TAKEAWAY", WaiterUserID: actor.User.ID, Label: strings.TrimSpace(req.GetLabel())})
		if errors.Is(e, pgx.ErrNoRows) {
			return connecterr.Invalid("order_client_id", "an order with this id already exists")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		applied, rejected, e := orders.InsertLines(ctx, q, o.ID, req.GetLines(), "SENT")
		if e != nil {
			return e
		}
		if len(rejected) > 0 {
			msgs := make([]string, 0, len(rejected))
			for _, r := range rejected {
				msgs = append(msgs, r.GetMessage())
			}
			return connecterr.Invalid("lines", strings.Join(msgs, "; "))
		}
		if applied != len(req.GetLines()) {
			return connecterr.Invalid("lines", "a line client_id is repeated")
		}
		if orderVersion, e = q.BumpOrderVersion(ctx, o.ID); e != nil {
			return connecterr.Internal(e)
		}
		order, items, settings, e := s.loadForBilling(ctx, q, ocid)
		if e != nil {
			return e
		}
		inv, _, cust, e := s.payBill(ctx, q, actor, shift, order, items, settings, req.GetDiscount(), req.GetBuyerName(), req.GetBuyerPan(), req.GetLabel(), tenders, req.GetCreditApproval())
		if e != nil {
			return e
		}
		customers = cust
		full, e := s.loadInvoice(ctx, q, inv.ID)
		if e != nil {
			return e
		}
		proto.Merge(out, full)
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "ticket.created", ID: out.OrderId})
	s.emit(realtime.Event{Type: "order.updated", ID: out.OrderId, Version: orderVersion})
	s.emit(realtime.Event{Type: "invoice.issued", ID: out.Id})
	s.emitCustomers(customers)
	return out, nil
}

// ListInvoices returns invoices for one order, one BS date, or an AD range,
// newest first, cancelled ones included.
func (s *Service) ListInvoices(ctx context.Context, req *restaurantv1.ListInvoicesRequest) (*restaurantv1.ListInvoicesResponse, error) {
	if _, err := requireBillingRole(ctx); err != nil {
		return nil, err
	}
	var (
		invs []sqlc.Invoice
		err  error
	)
	switch f := req.GetFilter().(type) {
	case *restaurantv1.ListInvoicesRequest_OrderClientId:
		ocid, perr := parseID("order_client_id", f.OrderClientId)
		if perr != nil {
			return nil, perr
		}
		order, oerr := s.q.GetOrderByClientID(ctx, ocid)
		if errors.Is(oerr, pgx.ErrNoRows) {
			return &restaurantv1.ListInvoicesResponse{}, nil
		}
		if oerr != nil {
			return nil, connecterr.Internal(oerr)
		}
		invs, err = s.q.ListInvoicesForOrder(ctx, dbx.UUID(order.ID))
	case *restaurantv1.ListInvoicesRequest_BsDate:
		if !validBsDate(f.BsDate) {
			return nil, connecterr.Invalid("bs_date", "expected a BS date as YYYY.MM.DD")
		}
		invs, err = s.q.ListInvoicesByBsDate(ctx, f.BsDate)
	case *restaurantv1.ListInvoicesRequest_AdRange:
		from, to := f.AdRange.GetFromUnix(), f.AdRange.GetToUnix()
		if to <= from {
			return nil, connecterr.Invalid("ad_range", "to must be after from")
		}
		invs, err = s.q.ListInvoicesByAdRange(ctx, sqlc.ListInvoicesByAdRangeParams{FromTime: time.Unix(from, 0), ToTime: time.Unix(to, 0)})
	default:
		return nil, connecterr.Invalid("filter", "choose an order, a BS date or an AD range")
	}
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := &restaurantv1.ListInvoicesResponse{Invoices: make([]*restaurantv1.Invoice, 0, len(invs))}
	for _, inv := range invs {
		lines, e := s.q.GetInvoiceLines(ctx, inv.ID)
		if e != nil {
			return nil, connecterr.Internal(e)
		}
		pays, e := s.q.ListPaymentsForInvoice(ctx, inv.ID)
		if e != nil {
			return nil, connecterr.Internal(e)
		}
		prints, e := s.q.ListInvoicePrints(ctx, inv.ID)
		if e != nil {
			return nil, connecterr.Internal(e)
		}
		out.Invoices = append(out.Invoices, toInvoice(inv, lines, pays, prints))
	}
	return out, nil
}

// validBsDate accepts a real BS calendar date written YYYY.MM.DD (the stored form).
func validBsDate(s string) bool {
	var d bsdate.Date
	if n, err := fmt.Sscanf(s, "%4d.%2d.%2d", &d.Year, &d.Month, &d.Day); err != nil || n != 3 {
		return false
	}
	if d.String() != s {
		return false
	}
	_, err := d.ToAD()
	return err == nil
}

// methodOnAccount is the stored PAYMENT_METHOD_ON_ACCOUNT.
const methodOnAccount = "ON_ACCOUNT"

// emitCustomers pokes customer.updated for each account a bill changed.
func (s *Service) emitCustomers(ids []string) {
	for _, id := range ids {
		s.emit(realtime.Event{Type: credit.EventCustomerUpdated, ID: id})
	}
}
