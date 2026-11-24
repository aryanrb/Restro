// Package shift owns cash shifts: open/close, one open shift per cashier, and the
// close-out reconciliation (expected vs counted cash, QR list). Payments attach to
// a shift in the billing domain.
package shift

import (
	"context"
	"errors"

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
		var e error
		closed, e = q.CloseShift(ctx, sqlc.CloseShiftParams{ID: id, CountedCashPaisa: pgtype.Int8{Int64: countedCash, Valid: true}, Note: note})
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
	return s.buildReport(ctx, closed)
}

func (s *Service) GetShiftReport(ctx context.Context, shiftID string) (*restaurantv1.ShiftReport, error) {
	if _, err := auth.RequireActor(ctx); err != nil {
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
	return s.buildReport(ctx, sh)
}

func (s *Service) buildReport(ctx context.Context, sh sqlc.Shift) (*restaurantv1.ShiftReport, error) {
	totals, err := s.q.ShiftPaymentTotals(ctx, sh.ID)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	report := &restaurantv1.ShiftReport{Shift: toShift(sh)}
	var cashTotal int64
	for _, t := range totals {
		report.Totals = append(report.Totals, &restaurantv1.MethodTotal{Method: paymentMethodEnum(t.Method), TotalPaisa: t.Total})
		if t.Method == "CASH" {
			cashTotal = t.Total
		}
	}
	report.ExpectedCashPaisa = sh.OpeningFloatPaisa + cashTotal
	if sh.Status == "CLOSED" && sh.CountedCashPaisa.Valid {
		report.DifferencePaisa = sh.CountedCashPaisa.Int64 - report.ExpectedCashPaisa
	}

	qr, err := s.q.ListShiftQrPayments(ctx, sh.ID)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	for _, p := range qr {
		report.QrPayments = append(report.QrPayments, &restaurantv1.QrPaymentRow{
			PaymentId: p.ID.String(), InvoiceNumber: p.InvoiceNumber, AmountPaisa: p.AmountPaisa,
			Reference: p.Reference, CreatedAtUnix: p.CreatedAt.Unix(),
		})
	}
	return report, nil
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
