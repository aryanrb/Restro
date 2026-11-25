// Package shift owns cash shifts: open/close, one open shift per cashier, and the
// close-out reconciliation (expected vs counted cash, QR list). Payments attach to
// a shift in the billing domain.
package shift

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/audit"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/realtime"
)

type Service struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
	hub  *realtime.Hub
	log  *zap.Logger
}

func NewService(pool *pgxpool.Pool, hub *realtime.Hub, log *zap.Logger) *Service {
	return &Service{pool: pool, q: sqlc.New(pool), hub: hub, log: log}
}

func (s *Service) emit(ev realtime.Event) {
	if s.hub != nil {
		s.hub.Broadcast(ev)
	}
}

func (s *Service) OpenShift(ctx context.Context, floatPaisa int64, note string) (*restaurantv1.Shift, error) {
	actor, err := auth.RequireRole(ctx, auth.RoleOwner, auth.RoleManager, auth.RoleCashier)
	if err != nil {
		return nil, err
	}
	if _, err := s.q.GetOpenShiftForUser(ctx, actor.User.ID); err == nil {
		return nil, connecterr.Precondition("you already have an open shift")
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, connecterr.Internal(err)
	}
	var sh sqlc.Shift
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		var e error
		sh, e = q.OpenShift(ctx, sqlc.OpenShiftParams{CashierUserID: actor.User.ID, OpeningFloatPaisa: floatPaisa, Note: note})
		if e != nil {
			return connecterr.Internal(e)
		}
		return audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: "shift.open", Entity: "shift", EntityID: sh.ID.String()})
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "shift.updated", ID: sh.ID.String()})
	return toShift(sh), nil
}

func (s *Service) GetMyOpenShift(ctx context.Context) (*restaurantv1.Shift, error) {
	actor, err := auth.RequireActor(ctx)
	if err != nil {
		return nil, err
	}
	sh, err := s.q.GetOpenShiftForUser(ctx, actor.User.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // no open shift
	}
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	return toShift(sh), nil
}

func (s *Service) ListOpenShifts(ctx context.Context) ([]*restaurantv1.Shift, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	rows, err := s.q.ListOpenShifts(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := make([]*restaurantv1.Shift, 0, len(rows))
	for _, sh := range rows {
		out = append(out, toShift(sh))
	}
	return out, nil
}

func (s *Service) CloseShift(ctx context.Context, shiftID string, countedCash int64, note string) (*restaurantv1.ShiftReport, error) {
	actor, err := auth.RequireRole(ctx, auth.RoleOwner, auth.RoleManager, auth.RoleCashier)
	if err != nil {
		return nil, err
	}
	id, err := parseID("shift_id", shiftID)
	if err != nil {
		return nil, err
	}
	existing, err := s.q.GetShift(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, connecterr.NotFound("shift not found")
	}
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	// A cashier may only close their own shift; managers/owners may close any.
	if existing.CashierUserID != actor.User.ID && actor.User.Role == auth.RoleCashier {
		return nil, connecterr.NotAllowed("you can only close your own shift")
	}
	var closed sqlc.Shift
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		// Count back any bill cancelled during this shift whose payment was taken
		// in an earlier, already-closed shift, so the close-out includes it.
		if _, e := q.RecordCancelAdjustments(ctx, pgtype.UUID{}); e != nil {
			return connecterr.Internal(e)
		}
		var e error
		closed, e = q.CloseShift(ctx, sqlc.CloseShiftParams{ID: id, CountedCashPaisa: pgtype.Int8{Int64: countedCash, Valid: true}, ClosingNote: note})
		if errors.Is(e, pgx.ErrNoRows) {
			return connecterr.Invalid("shift_id", "shift is already closed")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		return audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: "shift.close", Entity: "shift", EntityID: id.String(), After: audit.JSON(map[string]int64{"counted_cash": countedCash})})
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "shift.updated", ID: id.String()})
	return s.buildReport(ctx, closed, nil)
}

// ListShifts returns recent shifts (open and closed), newest first, each with its
// report. Cashiers see their own; managers and owners see everyone's.
func (s *Service) ListShifts(ctx context.Context, limit int32) ([]*restaurantv1.ShiftReport, error) {
	actor, err := auth.RequireRole(ctx, auth.RoleOwner, auth.RoleManager, auth.RoleCashier)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	var who pgtype.UUID
	if actor.User.Role == auth.RoleCashier {
		who = dbx.UUID(actor.User.ID)
	}
	if err := s.syncAdjustments(ctx); err != nil {
		return nil, err
	}
	rows, err := s.q.ListShifts(ctx, sqlc.ListShiftsParams{CashierUserID: who, RowLimit: limit})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	names := map[uuid.UUID]string{}
	out := make([]*restaurantv1.ShiftReport, 0, len(rows))
	for _, sh := range rows {
		r, err := s.buildReport(ctx, sh, names)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// RecordCancelAdjustments counts a just-cancelled invoice back in the canceller's
// open shift for every payment that was taken in a shift that had already closed.
// Billing's CancelInvoice calls it with its transaction's queries, after the
// status->CANCELLED update, so the adjustment commits with the cancel. It returns
// SHIFT_NOT_OPEN when an adjustment is owed but the canceller has no shift open
// (the cancel is then refused). Idempotent.
//
// The shift service also records the same adjustments lazily (on every report /
// list / close) as a catch-up, from the invoices' cancel data.
func RecordCancelAdjustments(ctx context.Context, q *sqlc.Queries, invoiceID uuid.UUID) error {
	if _, err := q.RecordCancelAdjustments(ctx, dbx.UUID(invoiceID)); err != nil {
		return connecterr.Internal(err)
	}
	n, err := q.CountUnplacedCancelAdjustments(ctx, invoiceID)
	if err != nil {
		return connecterr.Internal(err)
	}
	if n > 0 {
		return connecterr.ShiftNotOpen("open a shift before cancelling a bill from a closed shift")
	}
	return nil
}

// syncAdjustments records any adjustments owed by cancelled bills (idempotent).
func (s *Service) syncAdjustments(ctx context.Context) error {
	if _, err := s.q.RecordCancelAdjustments(ctx, pgtype.UUID{}); err != nil {
		return connecterr.Internal(err)
	}
	return nil
}

func (s *Service) GetShiftReport(ctx context.Context, shiftID string) (*restaurantv1.ShiftReport, error) {
	if _, err := auth.RequireActor(ctx); err != nil {
		return nil, err
	}
	if err := s.syncAdjustments(ctx); err != nil {
		return nil, err
	}
	id, err := parseID("shift_id", shiftID)
	if err != nil {
		return nil, err
	}
	sh, err := s.q.GetShift(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, connecterr.NotFound("shift not found")
	}
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	return s.buildReport(ctx, sh, nil)
}

// buildReport assembles a shift's close-out view. names caches user names across
// calls (nil = no cache).
func (s *Service) buildReport(ctx context.Context, sh sqlc.Shift, names map[uuid.UUID]string) (*restaurantv1.ShiftReport, error) {
	totals, err := s.q.ShiftPaymentTotals(ctx, sh.ID)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	adjustments, err := s.q.ListShiftAdjustments(ctx, sh.ID)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	byMethod := map[string]int64{}
	var order []string
	add := func(method string, amount int64) {
		if _, ok := byMethod[method]; !ok {
			order = append(order, method)
		}
		byMethod[method] += amount
	}
	for _, t := range totals {
		add(t.Method, t.Total)
	}
	report := &restaurantv1.ShiftReport{Shift: toShift(sh)}
	report.Shift.CashierName, err = s.userName(ctx, sh.CashierUserID, names)
	if err != nil {
		return nil, err
	}
	for _, a := range adjustments {
		add(a.Method, -a.AmountPaisa)
		report.Adjustments = append(report.Adjustments, &restaurantv1.ShiftAdjustment{
			Id: a.ID.String(), InvoiceId: a.InvoiceID.String(), InvoiceNumber: a.InvoiceNumber,
			DisplayNumber: DisplayNumber(a.Mode, a.FiscalYear, a.InvoiceNumber), Method: paymentMethodEnum(a.Method),
			AmountPaisa: a.AmountPaisa, CreatedAtUnix: a.CreatedAt.Unix(), OriginalShiftId: a.OriginalShiftID.String(),
			PaymentId: a.PaymentID.String(),
		})
	}
	for _, m := range order {
		report.Totals = append(report.Totals, &restaurantv1.MethodTotal{Method: paymentMethodEnum(m), TotalPaisa: byMethod[m]})
	}
	report.ExpectedCashPaisa = sh.OpeningFloatPaisa + byMethod["CASH"]
	if sh.Status == "CLOSED" && sh.CountedCashPaisa.Valid {
		report.DifferencePaisa = sh.CountedCashPaisa.Int64 - report.ExpectedCashPaisa
	}

	payments, err := s.q.ListShiftPayments(ctx, sh.ID)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	for _, p := range payments {
		report.Payments = append(report.Payments, &restaurantv1.ShiftPaymentRow{
			PaymentId: p.ID.String(), InvoiceId: p.InvoiceID.String(), InvoiceNumber: p.InvoiceNumber,
			DisplayNumber: DisplayNumber(p.Mode, p.FiscalYear, p.InvoiceNumber), Method: paymentMethodEnum(p.Method),
			AmountPaisa: p.AmountPaisa, Reference: p.Reference, CreatedAtUnix: p.CreatedAt.Unix(),
			Confirmed: p.Status == "CONFIRMED", InvoiceCancelled: p.InvoiceStatus == "CANCELLED",
		})
		// Deprecated QR-only list, kept for older clients.
		if p.Method == "QR_FONEPAY" || p.Method == "QR_OTHER" {
			report.QrPayments = append(report.QrPayments, &restaurantv1.QrPaymentRow{
				PaymentId: p.ID.String(), InvoiceNumber: p.InvoiceNumber, AmountPaisa: p.AmountPaisa,
				Reference: p.Reference, CreatedAtUnix: p.CreatedAt.Unix(),
			})
		}
	}
	return report, nil
}

func (s *Service) userName(ctx context.Context, id uuid.UUID, cache map[uuid.UUID]string) (string, error) {
	if n, ok := cache[id]; ok {
		return n, nil
	}
	u, err := s.q.GetUserByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", connecterr.Internal(err)
	}
	if cache != nil {
		cache[id] = u.Name
	}
	return u.Name, nil
}

// DisplayNumber formats an invoice number for people: "2083/84-12" for a tax
// invoice, "E-2083/84-12" for an estimate, from the stored fiscal year "2083.084".
func DisplayNumber(mode, fiscalYear string, number int32) string {
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
	return prefix + fy + "-" + strconv.Itoa(int(number))
}

// --- helpers ---

func parseID(field, s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, connecterr.Invalid(field, "invalid id")
	}
	return id, nil
}

func toShift(sh sqlc.Shift) *restaurantv1.Shift {
	out := &restaurantv1.Shift{
		Id: sh.ID.String(), CashierUserId: sh.CashierUserID.String(), OpenedAtUnix: sh.OpenedAt.Unix(),
		OpeningFloatPaisa: sh.OpeningFloatPaisa, Note: sh.Note, Status: shiftStatusEnum(sh.Status),
		ClosingNote: sh.ClosingNote,
	}
	if sh.ClosedAt.Valid {
		out.ClosedAtUnix = sh.ClosedAt.Time.Unix()
	}
	if sh.CountedCashPaisa.Valid {
		out.CountedCashPaisa = sh.CountedCashPaisa.Int64
	}
	return out
}

func shiftStatusEnum(s string) restaurantv1.ShiftStatus {
	if s == "CLOSED" {
		return restaurantv1.ShiftStatus_SHIFT_STATUS_CLOSED
	}
	return restaurantv1.ShiftStatus_SHIFT_STATUS_OPEN
}

func paymentMethodEnum(s string) restaurantv1.PaymentMethod {
	switch s {
	case "CASH":
		return restaurantv1.PaymentMethod_PAYMENT_METHOD_CASH
	case "CARD":
		return restaurantv1.PaymentMethod_PAYMENT_METHOD_CARD
	case "QR_FONEPAY":
		return restaurantv1.PaymentMethod_PAYMENT_METHOD_QR_FONEPAY
	case "QR_OTHER":
		return restaurantv1.PaymentMethod_PAYMENT_METHOD_QR_OTHER
	default:
		return restaurantv1.PaymentMethod_PAYMENT_METHOD_UNSPECIFIED
	}
}
