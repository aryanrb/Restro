package credit

import (
	"context"
	"errors"
	"sort"
	"strconv"
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
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/idem"
	"github.com/oneaccord/restro/api/internal/loyalty"
	"github.com/oneaccord/restro/api/internal/realtime"
)

// repayMethods are the methods money comes back in (not ON_ACCOUNT).
var repayMethods = map[string]bool{"CASH": true, "CARD": true, "QR_FONEPAY": true, "QR_OTHER": true}

// lockOpenShift returns the actor's open shift, share-locked so a concurrent
// close waits for this transaction (and counts what it records).
func lockOpenShift(ctx context.Context, q *sqlc.Queries, actor *auth.Actor, msg string) (sqlc.Shift, error) {
	sh, err := q.LockOpenShiftForUser(ctx, actor.User.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlc.Shift{}, connecterr.ShiftNotOpen(msg)
	}
	if err != nil {
		return sqlc.Shift{}, connecterr.Internal(err)
	}
	return sh, nil
}

// Repay takes money against a customer's balance in the caller's open shift.
// Idempotent by request_id. Inactive accounts can still repay.
func (s *Service) Repay(ctx context.Context, req *restaurantv1.RepayRequest) (*restaurantv1.Repayment, error) {
	actor, err := requireBiller(ctx)
	if err != nil {
		return nil, err
	}
	reqID, err := parseID("request_id", req.GetRequestId())
	if err != nil {
		return nil, err
	}
	out := &restaurantv1.Repayment{}
	var members []uuid.UUID
	err = idem.Run(ctx, s.pool, s.q, reqID, actor.User.ID, out, func(q *sqlc.Queries) error {
		amount := req.GetAmountPaisa()
		if amount <= 0 {
			return connecterr.Invalid("amount", "the amount must be more than zero")
		}
		method := MethodStr(req.GetMethod())
		if !repayMethods[method] {
			return connecterr.Invalid("method", "repayments are cash, QR or card")
		}
		customerID, e := parseID("customer_id", req.GetCustomerId())
		if e != nil {
			return e
		}
		shift, e := lockOpenShift(ctx, q, actor, "open a shift before taking a repayment")
		if e != nil {
			return e
		}
		if _, e := q.LockCustomer(ctx, customerID); errors.Is(e, pgx.ErrNoRows) {
			return connecterr.Invalid("customer_id", "customer not found")
		} else if e != nil {
			return connecterr.Internal(e)
		}
		balance, e := q.CustomerBalance(ctx, customerID)
		if e != nil {
			return connecterr.Internal(e)
		}
		if amount > balance {
			return repaymentTooHigh(balance)
		}
		number, e := q.AllocateRepaymentNumber(ctx)
		if e != nil {
			return connecterr.Internal(e)
		}
		bsd, e := bsdate.FromAD(time.Now())
		if e != nil {
			return connecterr.Internal(e)
		}
		id, e := q.CreateRepayment(ctx, sqlc.CreateRepaymentParams{
			Number: number, CustomerID: customerID, AmountPaisa: amount, Method: method,
			Reference: strings.TrimSpace(req.GetReference()), Note: strings.TrimSpace(req.GetNote()),
			ShiftID: shift.ID, TakenByUserID: actor.User.ID, BsDate: bsd.String(), BalanceAfterPaisa: balance - amount,
		})
		if e != nil {
			return connecterr.Internal(e)
		}
		if e := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: ActionRepay, Entity: "repayment", EntityID: id.String(),
			After: audit.JSON(map[string]any{"customer_id": customerID.String(), "method": method, "amount": amount, "balance_after": balance - amount})}); e != nil {
			return e
		}
		// Loyalty: a bill now fully repaid gets its pending stamp.
		if members, e = loyalty.SyncPending(ctx, q); e != nil {
			return e
		}
		r, e := q.GetRepaymentDetail(ctx, id)
		if e != nil {
			return connecterr.Internal(e)
		}
		proto.Merge(out, toRepayment(r))
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: EventCustomerUpdated, ID: out.CustomerId})
	s.emit(realtime.Event{Type: "shift.updated", ID: out.ShiftId})
	s.emitMembers(members)
	return out, nil
}

// VoidRepayment (owner/manager, with a reason) takes a repayment back off. One
// taken in a shift that has since closed still counts there (it was in that
// drawer) and is counted back in the voider's open shift, like a cancelled bill.
func (s *Service) VoidRepayment(ctx context.Context, req *restaurantv1.VoidRepaymentRequest) (*restaurantv1.Repayment, error) {
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
	out := &restaurantv1.Repayment{}
	var adjustedShift string
	var members []uuid.UUID
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		r, e := q.LockRepayment(ctx, id)
		if errors.Is(e, pgx.ErrNoRows) {
			return connecterr.Invalid("id", "repayment not found")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		if r.VoidedAt.Valid {
			return connecterr.Invalid("id", "this repayment is already voided")
		}
		original, e := q.GetShift(ctx, r.ShiftID)
		if e != nil {
			return connecterr.Internal(e)
		}
		if original.Status == "CLOSED" {
			current, e := lockOpenShift(ctx, q, actor, "open a shift before voiding a repayment from a closed shift")
			if e != nil {
				return e
			}
			if e := q.CreateRepaymentAdjustment(ctx, sqlc.CreateRepaymentAdjustmentParams{
				ShiftID: current.ID, OriginalShiftID: original.ID, RepaymentID: dbx.UUID(r.ID), Method: r.Method,
				AmountPaisa: r.AmountPaisa, CreatedByUserID: dbx.UUID(actor.User.ID),
			}); e != nil {
				return connecterr.Internal(e)
			}
			adjustedShift = current.ID.String()
		}
		if e := q.VoidRepayment(ctx, sqlc.VoidRepaymentParams{ID: id, VoidedByUserID: dbx.UUID(actor.User.ID), VoidReason: reason}); e != nil {
			return connecterr.Internal(e)
		}
		if e := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: ActionRepayVoid, Entity: "repayment", EntityID: id.String(), Reason: reason,
			After: audit.JSON(map[string]any{"customer_id": r.CustomerID.String(), "method": r.Method, "amount": r.AmountPaisa})}); e != nil {
			return e
		}
		// Loyalty: a bill unpaid again loses its counted stamp until repaid.
		if members, e = loyalty.SyncPending(ctx, q); e != nil {
			return e
		}
		d, e := q.GetRepaymentDetail(ctx, id)
		if e != nil {
			return connecterr.Internal(e)
		}
		proto.Merge(out, toRepayment(d))
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: EventCustomerUpdated, ID: out.CustomerId})
	s.emit(realtime.Event{Type: "shift.updated", ID: out.ShiftId})
	if adjustedShift != "" {
		s.emit(realtime.Event{Type: "shift.updated", ID: adjustedShift})
	}
	s.emitMembers(members)
	return out, nil
}

// emitMembers pokes customer.updated for members whose pending stamps changed.
func (s *Service) emitMembers(ids []uuid.UUID) {
	for _, id := range ids {
		s.emit(realtime.Event{Type: EventCustomerUpdated, ID: id.String()})
	}
}

// ListRepayments: a customer's repayments, newest first, voided included.
func (s *Service) ListRepayments(ctx context.Context, req *restaurantv1.ListRepaymentsRequest) (*restaurantv1.ListRepaymentsResponse, error) {
	if _, err := requireBiller(ctx); err != nil {
		return nil, err
	}
	id, err := parseID("customer_id", req.GetCustomerId())
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListRepaymentDetails(ctx, id)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := &restaurantv1.ListRepaymentsResponse{Repayments: make([]*restaurantv1.Repayment, 0, len(rows))}
	for _, r := range rows {
		out.Repayments = append(out.Repayments, toRepayment(sqlc.GetRepaymentDetailRow(r)))
	}
	return out, nil
}

func toRepayment(r sqlc.GetRepaymentDetailRow) *restaurantv1.Repayment {
	out := &restaurantv1.Repayment{
		Id: r.ID.String(), Number: r.Number, DisplayNumber: RepaymentNumber(r.Number), CustomerId: r.CustomerID.String(),
		CustomerName: r.CustomerName, AmountPaisa: r.AmountPaisa, Method: MethodEnum(r.Method), Reference: r.Reference, Note: r.Note,
		ShiftId: r.ShiftID.String(), TakenByUserId: r.TakenByUserID.String(), TakenByName: r.TakenByName,
		AtUnix: r.CreatedAt.Unix(), BsDate: r.BsDate, BalanceAfterPaisa: r.BalanceAfterPaisa,
		VoidedByUserId: uuidStr(r.VoidedByUserID), VoidReason: r.VoidReason,
	}
	if r.VoidedAt.Valid {
		out.VoidedAtUnix = r.VoidedAt.Time.Unix()
	}
	return out
}

// --- statements ---

// GetStatement is a customer's account for an AD date range (Kathmandu days,
// inclusive): the opening balance, each on-account bill and repayment in the
// range with the running balance, and the closing balance. Bills show how much
// of them repayments have paid off (oldest first, as of now).
func (s *Service) GetStatement(ctx context.Context, req *restaurantv1.StatementRequest) (*restaurantv1.Statement, error) {
	if _, err := requireBiller(ctx); err != nil {
		return nil, err
	}
	customerID, err := parseID("customer_id", req.GetCustomerId())
	if err != nil {
		return nil, err
	}
	from, err := time.ParseInLocation("2006-01-02", req.GetFrom(), nepal)
	if err != nil {
		return nil, connecterr.Invalid("from", "expected a date as YYYY-MM-DD")
	}
	to, err := time.ParseInLocation("2006-01-02", req.GetTo(), nepal)
	if err != nil {
		return nil, connecterr.Invalid("to", "expected a date as YYYY-MM-DD")
	}
	if to.Before(from) {
		return nil, connecterr.Invalid("to", "the end date is before the start date")
	}
	end := to.AddDate(0, 0, 1) // exclusive
	row, err := s.q.GetCustomerWithBalance(ctx, customerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound("customer_id", "customer not found")
	}
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	customer := toCustomer(row)
	charges, err := s.q.ListCreditCharges(ctx, dbx.UUID(customerID))
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	repayments, err := s.q.ListLiveRepayments(ctx, dbx.UUID(customerID))
	if err != nil {
		return nil, connecterr.Internal(err)
	}

	type event struct {
		at    time.Time
		delta int64
		entry *restaurantv1.StatementEntry
	}
	events := make([]event, 0, len(charges)+len(repayments))
	for _, c := range allocate(charges, sumRepaid(repayments)) {
		ref := c.DisplayNumber
		if ref == "" {
			ref = invoiceNumber(c.Mode, c.FiscalYear, c.Number)
		}
		events = append(events, event{at: c.AdDatetime, delta: c.AmountPaisa, entry: &restaurantv1.StatementEntry{
			Kind: restaurantv1.StatementEntryKind_STATEMENT_ENTRY_KIND_BILL, InvoiceId: c.InvoiceID.String(), Ref: ref,
			AtUnix: c.AdDatetime.Unix(), BsDate: c.BsDate, AmountPaisa: c.AmountPaisa, SettledPaisa: c.settled,
		}})
	}
	for _, r := range repayments {
		events = append(events, event{at: r.CreatedAt, delta: -r.AmountPaisa, entry: &restaurantv1.StatementEntry{
			Kind: restaurantv1.StatementEntryKind_STATEMENT_ENTRY_KIND_REPAYMENT, RepaymentId: r.ID.String(), Ref: RepaymentNumber(r.Number),
			AtUnix: r.CreatedAt.Unix(), BsDate: r.BsDate, AmountPaisa: r.AmountPaisa, Method: MethodEnum(r.Method),
		}})
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })

	out := &restaurantv1.Statement{Customer: customer, From: req.GetFrom(), To: req.GetTo(), Entries: []*restaurantv1.StatementEntry{}}
	var balance int64
	for _, e := range events {
		if !e.at.Before(end) {
			break
		}
		balance += e.delta
		if e.at.Before(from) {
			out.OpeningPaisa = balance
			continue
		}
		e.entry.BalancePaisa = balance
		out.Entries = append(out.Entries, e.entry)
	}
	out.ClosingPaisa = balance
	return out, nil
}

// invoiceNumber formats an invoice number like billing does ("2083/84-12",
// "E-2083/84-12"), for invoices issued before display numbers were stored.
func invoiceNumber(mode, fiscalYear string, n int32) string {
	fy := fiscalYear
	if y, rest, ok := strings.Cut(fiscalYear, "."); ok {
		if len(rest) > 2 {
			rest = rest[len(rest)-2:]
		}
		fy = y + "/" + rest
	}
	prefix := ""
	if mode == "ESTIMATE" {
		prefix = "E-"
	}
	return prefix + fy + "-" + strconv.Itoa(int(n))
}
