package shift_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/shift"
)

type fixture struct {
	t       *testing.T
	svc     *shift.Service
	pool    *pgxpool.Pool
	authSvc *auth.Service
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run shift integration tests")
	}
	ctx := context.Background()
	pool, err := db.New(ctx, url)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE shift_adjustments, payments, cbms_outbox, invoice_lines, order_item_modifiers, order_items,
		invoices, invoice_counters, orders, shifts, processed_commands, audit_log, sessions, devices, users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return &fixture{t: t, svc: shift.NewService(pool, nil, zap.NewNop()), pool: pool, authSvc: auth.NewService(pool, zap.NewNop())}
}

// as creates a user with the role and name and returns a logged-in context.
func (f *fixture) as(role, name string) context.Context {
	f.t.Helper()
	ctx := context.Background()
	hash, err := auth.HashSecret("secret123")
	if err != nil {
		f.t.Fatal(err)
	}
	username := "u-" + uuid.NewString()[:8]
	if _, err := sqlc.New(f.pool).CreateUser(ctx, sqlc.CreateUserParams{
		Name: name, Role: role, Username: dbx.Text(username), PasswordHash: dbx.Text(hash), Language: "en", Active: true,
	}); err != nil {
		f.t.Fatalf("create user: %v", err)
	}
	_, token, err := f.authSvc.PasswordLogin(ctx, username, "secret123")
	if err != nil {
		f.t.Fatalf("login: %v", err)
	}
	req := connect.NewRequest(&restaurantv1.ListShiftsRequest{})
	req.Header().Set("Authorization", "Bearer "+token)
	var out context.Context
	_, _ = f.authSvc.Interceptor()(func(c context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		out = c
		return nil, nil
	})(ctx, req)
	if !auth.ActorFrom(out).LoggedIn() {
		f.t.Fatal("actor not resolved")
	}
	return out
}

func userID(ctx context.Context) uuid.UUID { return auth.ActorFrom(ctx).User.ID }

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

var invNo int32

// invoice inserts an ISSUED invoice of the given mode and total; returns its id.
func (f *fixture) invoice(mode string, total int64, by uuid.UUID) uuid.UUID {
	f.t.Helper()
	invNo++
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `INSERT INTO invoices (fiscal_year, number, mode, bs_date, bs_year, bs_month, bs_day,
		subtotal_paisa, discount_paisa, service_charge_paisa, taxable_paisa, vat_paisa, total_paisa, created_by_user_id)
		VALUES ('2083.084', $1, $2, '2083.08.10', 2083, 8, 10, $3, 0, 0, $3, 0, $3, $4) RETURNING id`,
		invNo, mode, total, by).Scan(&id); err != nil {
		f.t.Fatalf("insert invoice: %v", err)
	}
	return id
}

func (f *fixture) pay(inv, shiftID, by uuid.UUID, method string, amount int64, ref string) {
	f.t.Helper()
	f.exec(`INSERT INTO payments (invoice_id, method, amount_paisa, reference, confirmed_by_user_id, shift_id) VALUES ($1, $2, $3, $4, $5, $6)`,
		inv, method, amount, ref, by, shiftID)
}

func (f *fixture) cancel(inv, by uuid.UUID, at time.Time) {
	f.t.Helper()
	f.exec(`UPDATE invoices SET status = 'CANCELLED', cancel_reason = 'wrong table', cancelled_by_user_id = $2, cancelled_at = $3 WHERE id = $1`, inv, by, at)
}

func (f *fixture) open(ctx context.Context, float int64, note string) *restaurantv1.Shift {
	f.t.Helper()
	sh, err := f.svc.OpenShift(ctx, float, note)
	if err != nil {
		f.t.Fatalf("open shift: %v", err)
	}
	return sh
}

func (f *fixture) report(ctx context.Context, id string) *restaurantv1.ShiftReport {
	f.t.Helper()
	r, err := f.svc.GetShiftReport(ctx, id)
	if err != nil {
		f.t.Fatalf("report: %v", err)
	}
	return r
}

func total(r *restaurantv1.ShiftReport, m restaurantv1.PaymentMethod) int64 {
	var n int64
	for _, t := range r.GetTotals() {
		if t.GetMethod() == m {
			n += t.GetTotalPaisa()
		}
	}
	return n
}

const cashM = restaurantv1.PaymentMethod_PAYMENT_METHOD_CASH

func TestCloseShiftKeepsOpeningNote(t *testing.T) {
	f := newFixture(t)
	c := f.as(auth.RoleCashier, "Sita")
	sh := f.open(c, 500000, "float from safe")
	rep, err := f.svc.CloseShift(c, sh.GetId(), 500000, "all good")
	if err != nil {
		t.Fatal(err)
	}
	if rep.GetShift().GetNote() != "float from safe" || rep.GetShift().GetClosingNote() != "all good" {
		t.Fatalf("note=%q closing=%q", rep.GetShift().GetNote(), rep.GetShift().GetClosingNote())
	}
	if rep.GetShift().GetCashierName() != "Sita" {
		t.Fatalf("cashier name = %q", rep.GetShift().GetCashierName())
	}
}

func TestListShifts(t *testing.T) {
	f := newFixture(t)
	a := f.as(auth.RoleCashier, "Sita")
	b := f.as(auth.RoleCashier, "Gita")
	m := f.as(auth.RoleManager, "Hari")
	first := f.open(a, 0, "")
	if _, err := f.svc.CloseShift(a, first.GetId(), 0, ""); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE shifts SET opened_at = now() - interval '1 hour' WHERE id = $1`, first.GetId())
	second := f.open(a, 0, "")
	f.open(b, 0, "")

	mine, err := f.svc.ListShifts(a, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 2 || mine[0].GetShift().GetId() != second.GetId() || mine[1].GetShift().GetId() != first.GetId() {
		t.Fatalf("cashier list = %v", mine)
	}
	if mine[1].GetShift().GetStatus() != restaurantv1.ShiftStatus_SHIFT_STATUS_CLOSED || mine[0].GetShift().GetCashierName() != "Sita" {
		t.Fatalf("unexpected shift %v", mine[1].GetShift())
	}
	all, err := f.svc.ListShifts(m, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("manager sees %d shifts, want 3", len(all))
	}
	one, err := f.svc.ListShifts(m, 1)
	if err != nil || len(one) != 1 {
		t.Fatalf("limit 1: %d %v", len(one), err)
	}
	// A waiter has no till.
	if _, err := f.svc.ListShifts(f.as(auth.RoleWaiter, "Ram"), 0); err == nil {
		t.Fatal("waiter listed shifts")
	}
}

func TestShiftReportListsAllPayments(t *testing.T) {
	f := newFixture(t)
	c := f.as(auth.RoleCashier, "Sita")
	sh := f.open(c, 1000, "")
	sid := uuid.MustParse(sh.GetId())
	est := f.invoice("ESTIMATE", 3000, userID(c))
	tax := f.invoice("TAX_INVOICE", 5000, userID(c))
	f.pay(est, sid, userID(c), "CASH", 1000, "")
	f.pay(est, sid, userID(c), "CARD", 2000, "slip 9")
	f.pay(tax, sid, userID(c), "QR_FONEPAY", 5000, "FP123")

	r := f.report(c, sh.GetId())
	if len(r.GetPayments()) != 3 {
		t.Fatalf("payments = %d, want 3", len(r.GetPayments()))
	}
	p := r.GetPayments()[1]
	if p.GetMethod() != restaurantv1.PaymentMethod_PAYMENT_METHOD_CARD || p.GetAmountPaisa() != 2000 || p.GetReference() != "slip 9" ||
		p.GetInvoiceId() != est.String() || p.GetDisplayNumber() != "E-2083/84-"+itoa(p.GetInvoiceNumber()) || p.GetCreatedAtUnix() == 0 || !p.GetConfirmed() {
		t.Fatalf("card payment row = %v", p)
	}
	q := r.GetPayments()[2]
	if q.GetDisplayNumber() != "2083/84-"+itoa(q.GetInvoiceNumber()) || q.GetInvoiceId() != tax.String() {
		t.Fatalf("qr payment row = %v", q)
	}
	if r.GetExpectedCashPaisa() != 2000 {
		t.Fatalf("expected cash = %d, want 2000", r.GetExpectedCashPaisa())
	}
}

func TestBillCancelledWhileShiftOpenIsNotCounted(t *testing.T) {
	f := newFixture(t)
	c := f.as(auth.RoleCashier, "Sita")
	sh := f.open(c, 1000, "")
	sid := uuid.MustParse(sh.GetId())
	kept := f.invoice("TAX_INVOICE", 4000, userID(c))
	gone := f.invoice("TAX_INVOICE", 2500, userID(c))
	f.pay(kept, sid, userID(c), "CASH", 4000, "")
	f.pay(gone, sid, userID(c), "CASH", 2500, "")
	f.cancel(gone, userID(c), time.Now())

	r := f.report(c, sh.GetId())
	if got := total(r, cashM); got != 4000 {
		t.Fatalf("cash takings = %d, want 4000", got)
	}
	if r.GetExpectedCashPaisa() != 5000 {
		t.Fatalf("expected cash = %d, want 5000", r.GetExpectedCashPaisa())
	}
	if len(r.GetAdjustments()) != 0 {
		t.Fatalf("adjustments = %v, want none", r.GetAdjustments())
	}
	var cancelledListed bool
	for _, p := range r.GetPayments() {
		if p.GetInvoiceId() == gone.String() {
			cancelledListed = p.GetInvoiceCancelled()
		}
	}
	if !cancelledListed {
		t.Fatal("cancelled bill's payment should still be listed, flagged cancelled")
	}
	rep, err := f.svc.CloseShift(c, sh.GetId(), 5000, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.GetDifferencePaisa() != 0 {
		t.Fatalf("difference = %d, want 0", rep.GetDifferencePaisa())
	}
}

func TestBillCancelledAfterShiftClosedAdjustsCancellersShift(t *testing.T) {
	f := newFixture(t)
	a := f.as(auth.RoleCashier, "Sita")
	b := f.as(auth.RoleCashier, "Gita")
	shA := f.open(a, 0, "")
	inv := f.invoice("TAX_INVOICE", 3000, userID(a))
	f.pay(inv, uuid.MustParse(shA.GetId()), userID(a), "CASH", 1000, "")
	f.pay(inv, uuid.MustParse(shA.GetId()), userID(a), "CARD", 2000, "")
	if _, err := f.svc.CloseShift(a, shA.GetId(), 1000, ""); err != nil {
		t.Fatal(err)
	}
	shB := f.open(b, 5000, "")
	f.cancel(inv, userID(b), time.Now().Add(time.Second))

	// The closed shift keeps its takings: the cash was in that drawer.
	ra := f.report(a, shA.GetId())
	if total(ra, cashM) != 1000 || ra.GetDifferencePaisa() != 0 || len(ra.GetAdjustments()) != 0 {
		t.Fatalf("closed shift changed: %v", ra)
	}
	// The canceller's shift counts it back.
	rb := f.report(b, shB.GetId())
	if len(rb.GetAdjustments()) != 2 {
		t.Fatalf("adjustments = %v, want 2", rb.GetAdjustments())
	}
	adj := rb.GetAdjustments()[0]
	if adj.GetInvoiceId() != inv.String() || adj.GetOriginalShiftId() != shA.GetId() || adj.GetDisplayNumber() == "" || adj.GetCreatedAtUnix() == 0 {
		t.Fatalf("adjustment = %v", adj)
	}
	if total(rb, cashM) != -1000 || total(rb, restaurantv1.PaymentMethod_PAYMENT_METHOD_CARD) != -2000 {
		t.Fatalf("totals = %v", rb.GetTotals())
	}
	if rb.GetExpectedCashPaisa() != 4000 {
		t.Fatalf("expected cash = %d, want 4000", rb.GetExpectedCashPaisa())
	}
	// Idempotent: reading again doesn't double the adjustment.
	if again := f.report(b, shB.GetId()); len(again.GetAdjustments()) != 2 {
		t.Fatalf("adjustments after re-read = %d", len(again.GetAdjustments()))
	}
	closed, err := f.svc.CloseShift(b, shB.GetId(), 4000, "")
	if err != nil {
		t.Fatal(err)
	}
	if closed.GetDifferencePaisa() != 0 || len(closed.GetAdjustments()) != 2 {
		t.Fatalf("close-out = %v", closed)
	}
}

// RecordCancelAdjustments is the hook billing's cancel transaction can call.
func TestRecordCancelAdjustments(t *testing.T) {
	f := newFixture(t)
	a := f.as(auth.RoleCashier, "Sita")
	m := f.as(auth.RoleManager, "Hari")
	shA := f.open(a, 0, "")
	inv := f.invoice("TAX_INVOICE", 1000, userID(a))
	f.pay(inv, uuid.MustParse(shA.GetId()), userID(a), "CASH", 1000, "")
	if _, err := f.svc.CloseShift(a, shA.GetId(), 1000, ""); err != nil {
		t.Fatal(err)
	}
	q := sqlc.New(f.pool)

	// The manager has no open shift: the cancel can't be counted back anywhere.
	f.cancel(inv, userID(m), time.Now().Add(time.Second))
	err := shift.RecordCancelAdjustments(context.Background(), q, inv)
	var ce *connecterr.Error
	if !errors.As(err, &ce) || ce.Reason != restaurantv1.ErrorReason_ERROR_REASON_SHIFT_NOT_OPEN {
		t.Fatalf("err = %v, want SHIFT_NOT_OPEN", err)
	}

	// With a shift open at the time of the cancel it lands there, once.
	shM := f.open(m, 0, "")
	f.exec(`UPDATE invoices SET cancelled_at = now() + interval '2 seconds' WHERE id = $1`, inv)
	for i := 0; i < 2; i++ {
		if err := shift.RecordCancelAdjustments(context.Background(), q, inv); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM shift_adjustments WHERE shift_id = $1`, shM.GetId()).Scan(&n); err != nil || n != 1 {
		t.Fatalf("adjustments in manager shift = %d (%v), want 1", n, err)
	}
}

func itoa(n int32) string { return strconv.Itoa(int(n)) }
