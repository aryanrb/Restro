package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"
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
	"github.com/oneaccord/restro/api/internal/money"
	"github.com/oneaccord/restro/api/internal/orders"
	"github.com/oneaccord/restro/api/internal/realtime"
	"github.com/oneaccord/restro/api/internal/shift"
)

func (s *Service) PreviewBill(ctx context.Context, req *restaurantv1.PreviewBillRequest) (*restaurantv1.BillPreview, error) {
	if _, err := auth.RequireActor(ctx); err != nil {
		return nil, err
	}
	ocid, err := parseID("order_client_id", req.GetOrderClientId())
	if err != nil {
		return nil, err
	}
	order, err := s.q.GetOrderByClientID(ctx, ocid)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, connecterr.NotFound("order not found")
	}
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	items, err := s.q.ListUnbilledOrderItems(ctx, order.ID)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	subtotal, _, err := s.subtotalOf(ctx, s.q, order.ID, items)
	if err != nil {
		return nil, err
	}
	discount := previewDiscount(req.GetDiscount(), subtotal)
	settings, err := s.q.GetSettings(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	r := s.compute(settings, subtotal, discount)
	return &restaurantv1.BillPreview{
		SubtotalPaisa: r.Subtotal, DiscountPaisa: r.Discount, ServiceChargePaisa: r.ServiceCharge,
		TaxablePaisa: r.Taxable, VatPaisa: r.Vat, RoundingPaisa: r.RoundingAdjustment, TotalPaisa: r.Total,
	}, nil
}

func (s *Service) IssueFullBill(ctx context.Context, req *restaurantv1.IssueFullBillRequest) (*restaurantv1.Invoice, error) {
	actor, err := auth.RequireRole(ctx, auth.RoleOwner, auth.RoleManager, auth.RoleCashier)
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
	out := &restaurantv1.Invoice{}
	err = idem.Run(ctx, s.pool, s.q, cmdID, actor.User.ID, out, func(q *sqlc.Queries) error {
		order, items, settings, e := s.loadForBilling(ctx, q, ocid)
		if e != nil {
			return e
		}
		if len(items) == 0 {
			return connecterr.Invalid("order", "nothing to bill")
		}
		subtotal, byItem, e := s.subtotalOf(ctx, q, order.ID, items)
		if e != nil {
			return e
		}
		discount, reason, approvedBy, e := s.computeDiscount(ctx, req.GetDiscount(), subtotal, actor)
		if e != nil {
			return e
		}
		inv, e := s.issueInvoiceForItems(ctx, q, order, items, byItem, subtotal, settings, discount, reason, approvedBy, req.GetBuyerName(), req.GetBuyerPan(), actor, "")
		if e != nil {
			return e
		}
		if e := q.SetOrderStatus(ctx, sqlc.SetOrderStatusParams{ID: order.ID, Status: "BILLED"}); e != nil {
			return connecterr.Internal(e)
		}
		// Everything is billed now; if it is also all handed over, the order is done.
		closed, e := orders.CloseIfDone(ctx, q, order.ID)
		if e != nil {
			return e
		}
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
	return out, nil
}

func (s *Service) SplitByItem(ctx context.Context, req *restaurantv1.SplitByItemRequest) (*restaurantv1.SplitByItemResponse, error) {
	actor, err := auth.RequireRole(ctx, auth.RoleOwner, auth.RoleManager, auth.RoleCashier)
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
	out := &restaurantv1.SplitByItemResponse{}
	err = idem.Run(ctx, s.pool, s.q, cmdID, actor.User.ID, out, func(q *sqlc.Queries) error {
		order, items, settings, e := s.loadForBilling(ctx, q, ocid)
		if e != nil {
			return e
		}
		byID := map[uuid.UUID]sqlc.OrderItem{}
		for _, it := range items {
			byID[it.ID] = it
		}
		for _, g := range req.GetGroups() {
			var groupItems []sqlc.OrderItem
			for _, idStr := range g.GetOrderItemIds() {
				id, perr := parseID("order_item_ids", idStr)
				if perr != nil {
					return perr
				}
				it, ok := byID[id]
				if !ok {
					return connecterr.Invalid("order_item_ids", "item is not billable on this order")
				}
				groupItems = append(groupItems, it)
				delete(byID, id) // prevent the same item in two groups
			}
			subtotal, byItem, e := s.subtotalOf(ctx, q, order.ID, groupItems)
			if e != nil {
				return e
			}
			inv, e := s.issueInvoiceForItems(ctx, q, order, groupItems, byItem, subtotal, settings, 0, "", uuid.Nil, "", "", actor, "")
			if e != nil {
				return e
			}
			full, e := s.loadInvoice(ctx, q, inv.ID)
			if e != nil {
				return e
			}
			out.Invoices = append(out.Invoices, full)
		}
		// If every item is now invoiced, the order is fully billed.
		remaining, e := q.ListUnbilledOrderItems(ctx, order.ID)
		if e != nil {
			return connecterr.Internal(e)
		}
		if len(remaining) == 0 {
			if e := q.SetOrderStatus(ctx, sqlc.SetOrderStatusParams{ID: order.ID, Status: "BILLED"}); e != nil {
				return connecterr.Internal(e)
			}
			closed, e := orders.CloseIfDone(ctx, q, order.ID)
			if e != nil {
				return e
			}
			if closed {
				freedTable = tableIDStr(order)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "order.updated"})
	if freedTable != "" {
		s.emit(realtime.Event{Type: "table.updated", ID: freedTable})
	}
	return out, nil
}

func (s *Service) RecordPayment(ctx context.Context, req *restaurantv1.RecordPaymentRequest) (*restaurantv1.Payment, error) {
	actor, err := auth.RequireRole(ctx, auth.RoleOwner, auth.RoleManager, auth.RoleCashier)
	if err != nil {
		return nil, err
	}
	shift, err := s.q.GetOpenShiftForUser(ctx, actor.User.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, connecterr.ShiftNotOpen("open a shift before taking payment")
	}
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	cmdID, err := parseID("command_id", req.GetCommandId())
	if err != nil {
		return nil, err
	}
	invID, err := parseID("invoice_id", req.GetInvoiceId())
	if err != nil {
		return nil, err
	}
	method := paymentMethodStr(req.GetMethod())
	provider, ok := s.providers[method]
	if !ok {
		return nil, connecterr.Invalid("method", "unsupported payment method")
	}

	out := &restaurantv1.Payment{}
	err = idem.Run(ctx, s.pool, s.q, cmdID, actor.User.ID, out, func(q *sqlc.Queries) error {
		inv, e := q.GetInvoice(ctx, invID)
		if errors.Is(e, pgx.ErrNoRows) {
			return connecterr.NotFound("invoice not found")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		if inv.Status != "ISSUED" {
			return connecterr.Invalid("invoice_id", "invoice is cancelled")
		}
		status, ref, e := provider.Record(ctx, req.GetAmountPaisa(), req.GetReference())
		if e != nil {
			return connecterr.Internal(e)
		}
		pay, e := q.CreatePayment(ctx, sqlc.CreatePaymentParams{
			InvoiceID: invID, Method: method, AmountPaisa: req.GetAmountPaisa(), Reference: ref,
			ConfirmedByUserID: actor.User.ID, ShiftID: shift.ID, Status: status,
		})
		if e != nil {
			return connecterr.Internal(e)
		}
		// Payment is not part of the close rule (billed + handed over); re-evaluate
		// anyway so an order whose state was settled elsewhere is not left open.
		if inv.OrderID.Valid {
			if _, e := orders.CloseIfDone(ctx, q, uuid.UUID(inv.OrderID.Bytes)); e != nil {
				return e
			}
		}
		if e := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: "payment.record", Entity: "invoice", EntityID: invID.String(), After: audit.JSON(map[string]any{"method": method, "amount": req.GetAmountPaisa()})}); e != nil {
			return e
		}
		proto.Merge(out, toPayment(pay))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) CancelInvoice(ctx context.Context, req *restaurantv1.CancelInvoiceRequest) (*restaurantv1.Invoice, error) {
	actor, err := auth.RequireRole(ctx, auth.RoleOwner, auth.RoleManager, auth.RoleCashier)
	if err != nil {
		return nil, err
	}
	if req.Approval == nil {
		return nil, connecterr.ManagerApprovalRequired("cancelling an invoice needs a manager PIN")
	}
	mgr, err := s.auth.VerifyManagerApproval(ctx, auth.ManagerApproval{ManagerUserID: req.Approval.GetManagerUserId(), Pin: req.Approval.GetPin()})
	if err != nil {
		return nil, err
	}
	cmdID, err := parseID("command_id", req.GetCommandId())
	if err != nil {
		return nil, err
	}
	invID, err := parseID("invoice_id", req.GetInvoiceId())
	if err != nil {
		return nil, err
	}

	var reopened []string
	var credited []uuid.UUID
	out := &restaurantv1.Invoice{}
	err = idem.Run(ctx, s.pool, s.q, cmdID, actor.User.ID, out, func(q *sqlc.Queries) error {
		inv, e := q.CancelInvoice(ctx, sqlc.CancelInvoiceParams{
			ID: invID, CancelReason: req.GetReason(),
			CancelledByUserID: dbx.UUID(actor.User.ID), CancelApprovedByUserID: dbx.UUID(mgr.ID),
		})
		if errors.Is(e, pgx.ErrNoRows) {
			return connecterr.Invalid("invoice_id", "invoice is already cancelled or not found")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		// Count the bill back in the canceller's open shift when its payment was
		// taken in a shift that has since closed; refuse the cancel (rolling it
		// back) when that's owed but the canceller has no shift open.
		if e := shift.RecordCancelAdjustments(ctx, q, invID); e != nil {
			return e
		}
		// Its on-account amounts come off the customers' balances by themselves
		// (balances count ISSUED bills only); tell their screens.
		if credited, e = q.ListInvoiceCreditCustomers(ctx, invID); e != nil {
			return connecterr.Internal(e)
		}
		orderIDs, e := q.UnlinkInvoiceItems(ctx, dbx.UUID(invID))
		if e != nil {
			return connecterr.Internal(e)
		}
		seen := map[uuid.UUID]bool{}
		for _, oid := range orderIDs {
			if !seen[oid] {
				seen[oid] = true
				if e := q.SetOrderStatus(ctx, sqlc.SetOrderStatusParams{ID: oid, Status: "OPEN"}); e != nil {
					return connecterr.Internal(e)
				}
				reopened = append(reopened, oid.String())
			}
		}
		if e := q.CreateCbmsOutbox(ctx, sqlc.CreateCbmsOutboxParams{InvoiceID: inv.ID, Kind: "RETURN", Payload: audit.JSON(map[string]any{"invoice": inv.Number}), Status: "DISABLED"}); e != nil {
			return connecterr.Internal(e)
		}
		if e := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: "invoice.cancel", Entity: "invoice", EntityID: invID.String(), Reason: req.GetReason(), ApproverID: mgr.ID}); e != nil {
			return e
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
	for _, oid := range reopened {
		s.emit(realtime.Event{Type: "order.updated", ID: oid})
	}
	s.emit(realtime.Event{Type: "invoice.cancelled", ID: invID.String()})
	for _, c := range credited {
		s.emit(realtime.Event{Type: credit.EventCustomerUpdated, ID: c.String()})
	}
	return out, nil
}

func (s *Service) MarkPrinted(ctx context.Context, req *restaurantv1.MarkPrintedRequest) (*restaurantv1.PrintInfo, error) {
	actor, err := auth.RequireActor(ctx)
	if err != nil {
		return nil, err
	}
	invID, err := parseID("invoice_id", req.GetInvoiceId())
	if err != nil {
		return nil, err
	}
	var count int32
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		c, e := q.IncrementPrintCount(ctx, invID)
		if errors.Is(e, pgx.ErrNoRows) {
			return connecterr.NotFound("invoice not found")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		count = c
		// Every print is logged (the first is the original) for the IRD sales book.
		if e := q.CreateInvoicePrint(ctx, sqlc.CreateInvoicePrintParams{InvoiceID: invID, PrintedByUserID: actor.User.ID}); e != nil {
			return connecterr.Internal(e)
		}
		if c > 1 {
			return audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: "invoice.reprint", Entity: "invoice", EntityID: invID.String(), After: audit.JSON(map[string]int32{"print_count": c})})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &restaurantv1.PrintInfo{IsOriginal: count == 1, CopyNumber: count - 1, PrintCount: count}, nil
}

func (s *Service) GetInvoice(ctx context.Context, req *restaurantv1.GetInvoiceRequest) (*restaurantv1.Invoice, error) {
	if _, err := auth.RequireActor(ctx); err != nil {
		return nil, err
	}
	invID, err := parseID("id", req.GetId())
	if err != nil {
		return nil, err
	}
	inv, err := s.loadInvoice(ctx, s.q, invID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, connecterr.NotFound("invoice not found")
	}
	if err != nil {
		return nil, err
	}
	return inv, nil
}

// --- internals ---

// loadForBilling must run inside the billing transaction: it locks the order row
// (SELECT ... FOR UPDATE) before reading the unbilled items, so two bills of the
// same order under different command ids serialize and the second sees the first's
// result (status BILLED or the items already invoiced) instead of a stale read.
func (s *Service) loadForBilling(ctx context.Context, q *sqlc.Queries, ocid uuid.UUID) (sqlc.Order, []sqlc.OrderItem, sqlc.Setting, error) {
	order, e := q.LockOrderByClientIDForBilling(ctx, ocid)
	if errors.Is(e, pgx.ErrNoRows) {
		return sqlc.Order{}, nil, sqlc.Setting{}, connecterr.NotFound("order not found")
	}
	if e != nil {
		return sqlc.Order{}, nil, sqlc.Setting{}, connecterr.Internal(e)
	}
	if order.Status != "OPEN" {
		return sqlc.Order{}, nil, sqlc.Setting{}, connecterr.OrderClosed("order is not open for billing")
	}
	items, e := q.ListUnbilledOrderItems(ctx, order.ID)
	if e != nil {
		return sqlc.Order{}, nil, sqlc.Setting{}, connecterr.Internal(e)
	}
	settings, e := q.GetSettings(ctx)
	if e != nil {
		return sqlc.Order{}, nil, sqlc.Setting{}, connecterr.Internal(e)
	}
	return order, items, settings, nil
}

func (s *Service) subtotalOf(ctx context.Context, q *sqlc.Queries, orderID uuid.UUID, items []sqlc.OrderItem) (int64, map[uuid.UUID][]sqlc.OrderItemModifier, error) {
	mods, err := q.ListOrderItemModifiersForOrder(ctx, orderID)
	if err != nil {
		return 0, nil, connecterr.Internal(err)
	}
	byItem := map[uuid.UUID][]sqlc.OrderItemModifier{}
	for _, m := range mods {
		byItem[m.OrderItemID] = append(byItem[m.OrderItemID], m)
	}
	var subtotal int64
	for _, it := range items {
		subtotal += lineTotal(it, byItem[it.ID])
	}
	return subtotal, byItem, nil
}

// lineTotal is unit price × quantity. The order line's unit_price_paisa already
// includes its modifiers' price deltas (snapshotted that way by orders.AddItems),
// so the modifier rows are display-only here and must not be added again.
func lineTotal(it sqlc.OrderItem, _ []sqlc.OrderItemModifier) int64 {
	return it.UnitPricePaisa * int64(it.Quantity)
}

func tableIDStr(o sqlc.Order) string {
	if !o.TableID.Valid {
		return ""
	}
	return uuid.UUID(o.TableID.Bytes).String()
}

func (s *Service) compute(settings sqlc.Setting, subtotal, discount int64) money.Result {
	return money.Compute(money.Input{
		Subtotal: subtotal, Discount: discount,
		ServiceChargeEnabled: settings.ServiceChargeEnabled, ServiceChargeRateBp: settings.ServiceChargeRateBp,
		VatRegistered: settings.VatRegistered, PricesIncludeVat: settings.PricesIncludeVat, VatRateBp: settings.VatRateBp,
		RoundTotalToRupee: settings.RoundTotalToRupee,
	})
}

// placeOf is what the invoice prints as its place: the table's name for a
// dine-in order, else the counter-sale label, else "Takeaway".
func placeOf(ctx context.Context, q *sqlc.Queries, order sqlc.Order, label string) (string, error) {
	if order.TableID.Valid {
		name, err := q.GetDiningTableName(ctx, uuid.UUID(order.TableID.Bytes))
		if err == nil {
			return name, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", connecterr.Internal(err)
		}
	}
	if label = strings.TrimSpace(label); label != "" {
		return label, nil
	}
	return "Takeaway", nil
}

// issueInvoiceForItems allocates the next number and writes the immutable invoice:
// totals, lines, and the issue-time snapshot (seller details from settings,
// display number, issuer, place). label is the counter-sale label ("" otherwise).
func (s *Service) issueInvoiceForItems(ctx context.Context, q *sqlc.Queries, order sqlc.Order, items []sqlc.OrderItem, byItem map[uuid.UUID][]sqlc.OrderItemModifier, subtotal int64, settings sqlc.Setting, discount int64, discountReason string, discountApprovedBy uuid.UUID, buyerName, buyerPan string, actor *auth.Actor, label string) (sqlc.Invoice, error) {
	r := s.compute(settings, subtotal, discount)
	actorID := actor.User.ID
	place, err := placeOf(ctx, q, order, label)
	if err != nil {
		return sqlc.Invoice{}, err
	}

	bsd, err := bsdate.FromAD(time.Now())
	if err != nil {
		s.log.Error("bs date", zap.Error(err))
		return sqlc.Invoice{}, connecterr.Internal(err)
	}
	mode := settings.BillingMode
	fy := bsd.FiscalYear()
	number, err := q.AllocateInvoiceNumber(ctx, sqlc.AllocateInvoiceNumberParams{FiscalYear: fy, Mode: mode})
	if err != nil {
		return sqlc.Invoice{}, connecterr.Internal(err)
	}
	inv, err := q.CreateInvoice(ctx, sqlc.CreateInvoiceParams{
		FiscalYear: fy, Number: number, Mode: mode, BsDate: bsd.String(),
		BsYear: int32(bsd.Year), BsMonth: int32(bsd.Month), BsDay: int32(bsd.Day),
		OrderID: dbx.UUID(order.ID), BuyerName: buyerName, BuyerPan: buyerPan,
		SubtotalPaisa: r.Subtotal, DiscountPaisa: r.Discount, ServiceChargePaisa: r.ServiceCharge,
		TaxablePaisa: r.Taxable, VatPaisa: r.Vat, RoundingPaisa: r.RoundingAdjustment, TotalPaisa: r.Total,
		DiscountReason: discountReason, DiscountApprovedByUserID: dbx.UUIDOpt(discountApprovedBy), CreatedByUserID: dbx.UUID(actorID),
		DisplayNumber: displayNumber(mode, fy, number),
		SellerNameEn:  settings.NameEn, SellerNameNe: settings.NameNe, SellerAddress: settings.Address,
		SellerPanNumber: settings.PanNumber, SellerVatRateBp: settings.VatRateBp, SellerVatRegistered: settings.VatRegistered,
		SellerLogoUrl: settings.LogoUrl, IssuedByName: actor.User.Name, Place: place, OrderClientID: dbx.UUID(order.ClientID),
	})
	if err != nil {
		return sqlc.Invoice{}, connecterr.Internal(err)
	}

	itemIDs := make([]uuid.UUID, 0, len(items))
	for _, it := range items {
		mods := byItem[it.ID]
		snap := make([]lineMod, 0, len(mods))
		for _, m := range mods {
			snap = append(snap, lineMod{NameEn: m.NameEn, NameNe: m.NameNe, PriceDelta: m.PriceDeltaPaisa})
		}
		modJSON, _ := json.Marshal(snap)
		if _, e := q.CreateInvoiceLine(ctx, sqlc.CreateInvoiceLineParams{
			InvoiceID: inv.ID, OrderItemID: dbx.UUID(it.ID), NameEn: it.NameEn, NameNe: it.NameNe,
			VariantName: it.VariantNameEn, VariantNameNe: it.VariantNameNe, Quantity: it.Quantity, UnitPricePaisa: it.UnitPricePaisa,
			ModifiersJson: modJSON, LineTotalPaisa: lineTotal(it, mods), OrderItemClientID: dbx.UUID(it.ClientID),
		}); e != nil {
			return sqlc.Invoice{}, connecterr.Internal(e)
		}
		itemIDs = append(itemIDs, it.ID)
	}
	// Claim only still-unbilled items; a short count means another command billed
	// some of them, so abort the whole transaction rather than double-bill.
	claimed, err := q.SetOrderItemsInvoice(ctx, sqlc.SetOrderItemsInvoiceParams{InvoiceID: dbx.UUID(inv.ID), Ids: itemIDs})
	if err != nil {
		return sqlc.Invoice{}, connecterr.Internal(err)
	}
	if claimed != int64(len(itemIDs)) {
		return sqlc.Invoice{}, connecterr.OrderClosed("some items were already billed; refresh the order")
	}
	if err := q.CreateCbmsOutbox(ctx, sqlc.CreateCbmsOutboxParams{
		InvoiceID: inv.ID, Kind: "BILL", Payload: audit.JSON(map[string]any{"number": number, "total": r.Total, "fiscal_year": fy}), Status: "DISABLED",
	}); err != nil {
		return sqlc.Invoice{}, connecterr.Internal(err)
	}
	if err := audit.Write(ctx, q, audit.Entry{ActorID: actorID, Action: "invoice.issue", Entity: "invoice", EntityID: inv.ID.String(), After: audit.JSON(map[string]any{"number": number, "fiscal_year": fy, "total": r.Total})}); err != nil {
		return sqlc.Invoice{}, err
	}
	return inv, nil
}

func (s *Service) loadInvoice(ctx context.Context, q *sqlc.Queries, id uuid.UUID) (*restaurantv1.Invoice, error) {
	inv, err := q.GetInvoice(ctx, id)
	if err != nil {
		return nil, err
	}
	lines, err := q.GetInvoiceLines(ctx, id)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	payments, err := q.ListPaymentsForInvoice(ctx, id)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	prints, err := q.ListInvoicePrints(ctx, id)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	return toInvoice(inv, lines, payments, prints), nil
}

// computeDiscount applies the role limit and returns the discount amount in paisa.
func (s *Service) computeDiscount(ctx context.Context, in *restaurantv1.DiscountInput, subtotal int64, actor *auth.Actor) (int64, string, uuid.UUID, error) {
	if in == nil || in.GetValue() == 0 {
		return 0, "", uuid.Nil, nil
	}
	if in.GetReason() == "" {
		return 0, "", uuid.Nil, connecterr.Invalid("reason", "a discount reason is required")
	}
	amount := discountAmount(in, subtotal)
	if actor.User.Role == auth.RoleWaiter {
		return 0, "", uuid.Nil, connecterr.NotAllowed("waiters cannot apply discounts")
	}
	var effBp int64
	if subtotal > 0 {
		effBp = amount * 10000 / subtotal
	}
	if actor.User.Role == auth.RoleCashier && effBp > cashierMaxDiscountBp {
		if in.Approval == nil {
			return 0, "", uuid.Nil, connecterr.ManagerApprovalRequired("this discount exceeds your limit; a manager PIN is required")
		}
		mgr, err := s.auth.VerifyManagerApproval(ctx, auth.ManagerApproval{ManagerUserID: in.Approval.GetManagerUserId(), Pin: in.Approval.GetPin()})
		if err != nil {
			return 0, "", uuid.Nil, err
		}
		return amount, in.GetReason(), mgr.ID, nil
	}
	return amount, in.GetReason(), uuid.Nil, nil
}

func previewDiscount(in *restaurantv1.DiscountInput, subtotal int64) int64 {
	if in == nil {
		return 0
	}
	return discountAmount(in, subtotal)
}

func discountAmount(in *restaurantv1.DiscountInput, subtotal int64) int64 {
	var amount int64
	switch in.GetType() {
	case restaurantv1.DiscountType_DISCOUNT_TYPE_PERCENT:
		amount = subtotal * in.GetValue() / 10000
	default:
		amount = in.GetValue()
	}
	if amount > subtotal {
		amount = subtotal
	}
	if amount < 0 {
		amount = 0
	}
	return amount
}
