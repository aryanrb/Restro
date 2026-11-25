package purchases

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/proto"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/audit"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/bsdate"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/idem"
	"github.com/oneaccord/restro/api/internal/realtime"
)

const itemUntrackedCode = connect.CodeFailedPrecondition

func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

var methodStr = map[restaurantv1.SupplierPaymentMethod]string{
	restaurantv1.SupplierPaymentMethod_SUPPLIER_PAYMENT_METHOD_TILL:       "TILL",
	restaurantv1.SupplierPaymentMethod_SUPPLIER_PAYMENT_METHOD_OWNER_CASH: "OWNER_CASH",
	restaurantv1.SupplierPaymentMethod_SUPPLIER_PAYMENT_METHOD_BANK_QR:    "BANK_QR",
	restaurantv1.SupplierPaymentMethod_SUPPLIER_PAYMENT_METHOD_CHEQUE:     "CHEQUE",
}

func methodEnum(s string) restaurantv1.SupplierPaymentMethod {
	for k, v := range methodStr {
		if v == s {
			return k
		}
	}
	return restaurantv1.SupplierPaymentMethod_SUPPLIER_PAYMENT_METHOD_UNSPECIFIED
}

type paymentCheck struct {
	method   string
	shiftID  string // TILL: the payer's open shift
	shift    uuid.UUID
	approver uuid.UUID
}

// checkPayment checks a payment before anything is written. A cashier pays from
// the till only, up to the limit without a manager PIN; a manager or owner paying
// over it is their own approver.
func (s *Service) checkPayment(ctx context.Context, q *sqlc.Queries, actor *auth.Actor, amount int64, m restaurantv1.SupplierPaymentMethod,
	approval *restaurantv1.ManagerApproval, owedNow int64) (*paymentCheck, error) {
	if amount <= 0 || amount > maxMoneyBill {
		return nil, connecterr.Invalid("amount", "the amount must be more than zero")
	}
	method, ok := methodStr[m]
	if !ok {
		return nil, connecterr.Invalid("method", "choose how it was paid")
	}
	if method != "TILL" && !isManager(actor) {
		return nil, withField(connecterr.NotAllowed("a cashier pays suppliers from the till only"), "method")
	}
	if amount > owedNow {
		return nil, paymentTooHigh()
	}
	c := &paymentCheck{method: method}
	if method != "TILL" {
		return c, nil
	}
	sh, err := lockOpenShift(ctx, q, actor.User.ID, "open a shift before paying from the till")
	if err != nil {
		return nil, err
	}
	c.shift, c.shiftID = sh.ID, sh.ID.String()
	if amount > TillPayoutLimitPaisa {
		switch {
		case isManager(actor):
			c.approver = actor.User.ID
		case approval.GetManagerUserId() == "":
			return nil, withField(connecterr.ManagerApprovalRequired("a till payment over Rs 5,000 needs a manager PIN"), "amount") // web: PAID_OUT_LIMIT
		default:
			mgr, err := s.auth.VerifyManagerApproval(ctx, auth.ManagerApproval{ManagerUserID: approval.GetManagerUserId(), Pin: approval.GetPin()})
			if err != nil {
				return nil, err
			}
			c.approver = mgr.ID
		}
	}
	return c, nil
}

// createPayment writes the payment and its audit entry (inside the caller's tx,
// supplier locked) and returns the slip.
func (s *Service) createPayment(ctx context.Context, q *sqlc.Queries, actor *auth.Actor, sup sqlc.Supplier, amount int64,
	_ restaurantv1.SupplierPaymentMethod, reference, note string, c *paymentCheck) (*restaurantv1.SupplierPayment, error) {
	balance, err := q.SupplierBalance(ctx, sup.ID)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	number, err := q.AllocatePurchaseCounter(ctx, "PAYMENT")
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	bsd, err := bsdate.FromAD(now())
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	params := sqlc.InsertSupplierPaymentParams{
		Number: number, SupplierID: sup.ID, SupplierName: sup.Name, AmountPaisa: amount, Method: c.method,
		Reference: strings.TrimSpace(reference), Note: strings.TrimSpace(note), PaidByUserID: actor.User.ID,
		ApproverUserID: dbx.UUIDOpt(c.approver), BsDate: bsd.String(), BalanceAfterPaisa: balance - amount,
	}
	if c.shift != uuid.Nil {
		params.ShiftID = dbx.UUID(c.shift)
	}
	p, err := q.InsertSupplierPayment(ctx, params)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	after := map[string]any{"supplier_id": sup.ID.String(), "amount": amount, "method": c.method, "number": number}
	if c.shiftID != "" {
		after["shift_id"] = c.shiftID
	}
	if err := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: ActionPaid, Entity: "supplier_payment", EntityID: p.ID.String(),
		Reason: sup.Name + " · " + itoa(amount) + " paisa · " + c.method, ApproverID: c.approver, After: audit.JSON(after)}); err != nil {
		return nil, err
	}
	return loadPayment(ctx, q, p.ID)
}

func paymentNumber(n int32) string { return "SP-" + itoa(int64(n)) }

func toPayment(r sqlc.GetSupplierPaymentRowRow) *restaurantv1.SupplierPayment {
	return &restaurantv1.SupplierPayment{
		Id: r.ID.String(), Number: r.Number, DisplayNumber: paymentNumber(r.Number), SupplierId: r.SupplierID.String(),
		SupplierName: r.SupplierName, AmountPaisa: r.AmountPaisa, Method: methodEnum(r.Method), Reference: r.Reference, Note: r.Note,
		ShiftId: idStr(r.ShiftID), PaidById: r.PaidByUserID.String(), PaidByName: r.PaidByName, ApproverId: idStr(r.ApproverUserID),
		ApproverName: r.ApproverName, AtUnix: r.At.Unix(), BsDate: r.BsDate, BalanceAfterPaisa: r.BalanceAfterPaisa,
		VoidedAtUnix: unixOf(r.VoidedAt), VoidedById: idStr(r.VoidedByUserID), VoidReason: r.VoidReason, VoidShiftId: idStr(r.VoidShiftID),
	}
}

func loadPayment(ctx context.Context, q *sqlc.Queries, id uuid.UUID) (*restaurantv1.SupplierPayment, error) {
	r, err := q.GetSupplierPaymentRow(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, connecterr.NotFound("payment not found")
	}
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	return toPayment(r), nil
}

// PaySupplier: any amount up to what is owed, applied to the oldest debt first.
// Idempotent on request_id.
func (s *Service) PaySupplier(ctx context.Context, req *restaurantv1.PaySupplierRequest) (*restaurantv1.SupplierPayment, error) {
	actor, err := requireBuyer(ctx)
	if err != nil {
		return nil, err
	}
	reqID, err := parseID("request_id", req.GetRequestId())
	if err != nil {
		return nil, err
	}
	supplierID, err := parseID("supplier_id", req.GetSupplierId())
	if err != nil {
		return nil, err
	}
	out := &restaurantv1.SupplierPayment{}
	err = idem.Run(ctx, s.pool, s.q, reqID, actor.User.ID, out, func(q *sqlc.Queries) error {
		sup, e := q.LockSupplier(ctx, supplierID)
		if errors.Is(e, pgx.ErrNoRows) {
			return connecterr.Invalid("supplier_id", "supplier not found")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		balance, e := q.SupplierBalance(ctx, sup.ID)
		if e != nil {
			return connecterr.Internal(e)
		}
		c, e := s.checkPayment(ctx, q, actor, req.GetAmountPaisa(), req.GetMethod(), req.GetApproval(), balance)
		if e != nil {
			return e
		}
		p, e := s.createPayment(ctx, q, actor, sup, req.GetAmountPaisa(), req.GetMethod(), req.GetReference(), req.GetNote(), c)
		if e != nil {
			return e
		}
		proto.Merge(out, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: EventSupplierUpdated, ID: out.SupplierId})
	if out.ShiftId != "" {
		s.emit(realtime.Event{Type: "shift.updated", ID: out.ShiftId})
	}
	return out, nil
}

// VoidSupplierPayment: owner/manager, with a reason. Like a bill cancellation, a
// till payment whose shift has closed puts the cash back in the voider's open
// shift (void_shift_id); one voided while its shift is open simply stops counting.
func (s *Service) VoidSupplierPayment(ctx context.Context, req *restaurantv1.VoidSupplierPaymentRequest) (*restaurantv1.SupplierPayment, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID("id", req.GetId())
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(req.GetReason())
	if reason == "" {
		return nil, connecterr.Invalid("reason", "a reason is required")
	}
	var out *restaurantv1.SupplierPayment
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		p, e := q.LockSupplierPayment(ctx, id)
		if errors.Is(e, pgx.ErrNoRows) {
			return connecterr.Invalid("id", "payment not found")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		if p.VoidedAt.Valid {
			return connecterr.Invalid("id", "this payment is already voided")
		}
		if _, e := q.LockSupplier(ctx, p.SupplierID); e != nil {
			return connecterr.Internal(e)
		}
		var voidShift = dbx.UUIDOpt(uuid.Nil)
		if p.Method == "TILL" && p.ShiftID.Valid {
			original, e := q.GetShift(ctx, p.ShiftID.Bytes)
			if e != nil {
				return connecterr.Internal(e)
			}
			if original.Status == "CLOSED" {
				current, e := lockOpenShift(ctx, q, actor.User.ID, "open a shift before voiding a till payment from a closed shift")
				if e != nil {
					return e
				}
				voidShift = dbx.UUID(current.ID)
			}
		}
		if e := q.VoidSupplierPayment(ctx, sqlc.VoidSupplierPaymentParams{ID: id, VoidedByUserID: dbx.UUID(actor.User.ID), VoidReason: reason, VoidShiftID: voidShift}); e != nil {
			return connecterr.Internal(e)
		}
		if e := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: ActionPaymentVoided, Entity: "supplier_payment", EntityID: id.String(),
			Reason: p.SupplierName + " · " + itoa(p.AmountPaisa) + " paisa · " + reason,
			After:  audit.JSON(map[string]any{"supplier_id": p.SupplierID.String(), "amount": p.AmountPaisa, "method": p.Method})}); e != nil {
			return e
		}
		out, e = loadPayment(ctx, q, id)
		return e
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: EventSupplierUpdated, ID: out.SupplierId})
	for _, sh := range []string{out.ShiftId, out.VoidShiftId} {
		if sh != "" {
			s.emit(realtime.Event{Type: "shift.updated", ID: sh})
		}
	}
	return out, nil
}

// ListSupplierPayments: a supplier's payments, newest first (voided included).
func (s *Service) ListSupplierPayments(ctx context.Context, supplierID string) ([]*restaurantv1.SupplierPayment, error) {
	if _, err := requireBuyer(ctx); err != nil {
		return nil, err
	}
	id, err := parseID("supplier_id", supplierID)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListSupplierPaymentRows(ctx, id)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := make([]*restaurantv1.SupplierPayment, 0, len(rows))
	for _, r := range rows {
		out = append(out, toPayment(sqlc.GetSupplierPaymentRowRow(r)))
	}
	return out, nil
}
