package reports_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/db"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/reports"
)

type fixture struct {
	t       *testing.T
	svc     *reports.Service
	pool    *pgxpool.Pool
	authSvc *auth.Service
	shiftID uuid.UUID
	owner   context.Context
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run reports integration tests")
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
	f := &fixture{t: t, svc: reports.NewService(pool, zap.NewNop()), pool: pool, authSvc: auth.NewService(pool, zap.NewNop())}
	f.owner = f.as(auth.RoleOwner, "Ram")
	if err := pool.QueryRow(ctx, `INSERT INTO shifts (cashier_user_id) VALUES ($1) RETURNING id`, f.id(f.owner)).Scan(&f.shiftID); err != nil {
		t.Fatalf("shift: %v", err)
	}
	return f
}

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
	req := connect.NewRequest(&restaurantv1.DashboardRequest{})
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

func (f *fixture) id(ctx context.Context) uuid.UUID { return auth.ActorFrom(ctx).User.ID }

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

type inv struct {
	mode                        string
	number                      int32
	at                          time.Time
	subtotal, discount, service int64
	total                       int64
	by                          uuid.UUID
	bsDate                      string
}

func (f *fixture) invoice(i inv) uuid.UUID {
	f.t.Helper()
	if i.bsDate == "" {
		i.bsDate = "2083.08.10"
	}
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `INSERT INTO invoices (fiscal_year, number, mode, bs_date, bs_year, bs_month, bs_day, ad_datetime,
		subtotal_paisa, discount_paisa, service_charge_paisa, taxable_paisa, vat_paisa, total_paisa, created_by_user_id)
		VALUES ('2083.084', $1, $2, $3, 2083, 8, 10, $4, $5, $6, $7, $8, 0, $8, $9) RETURNING id`,
		i.number, i.mode, i.bsDate, i.at, i.subtotal, i.discount, i.service, i.total, i.by).Scan(&id); err != nil {
		f.t.Fatalf("insert invoice: %v", err)
	}
	return id
}

func (f *fixture) pay(invID uuid.UUID, method string, amount int64) {
	f.t.Helper()
	f.exec(`INSERT INTO payments (invoice_id, method, amount_paisa, confirmed_by_user_id, shift_id) VALUES ($1, $2, $3, $4, $5)`,
		invID, method, amount, f.id(f.owner), f.shiftID)
}

func (f *fixture) cancel(invID, by, approver uuid.UUID, at time.Time) {
	f.t.Helper()
	f.exec(`UPDATE invoices SET status = 'CANCELLED', cancel_reason = 'wrong table', cancelled_by_user_id = $2,
		cancel_approved_by_user_id = $3, cancelled_at = $4 WHERE id = $1`, invID, by, approver, at)
}

// voidedItem inserts a voided order item worth qty x price (the snapshot price
// already includes modifiers; the modifier row is display-only) and its audit row.
func (f *fixture) voidedItem(name string, price, modifier int64, qty int32, actor uuid.UUID) uuid.UUID {
	f.t.Helper()
	ctx := context.Background()
	var orderID, itemID uuid.UUID
	if err := f.pool.QueryRow(ctx, `INSERT INTO orders (client_id, type, waiter_user_id) VALUES ($1, 'TAKEAWAY', $2) RETURNING id`, uuid.New(), actor).Scan(&orderID); err != nil {
		f.t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO order_items (order_id, client_id, menu_item_id, name_en, unit_price_paisa, quantity, station_id, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'VOIDED') RETURNING id`, orderID, uuid.New(), uuid.New(), name, price, qty, uuid.New()).Scan(&itemID); err != nil {
		f.t.Fatal(err)
	}
	if modifier != 0 {
		f.exec(`INSERT INTO order_item_modifiers (order_item_id, name_en, price_delta_paisa) VALUES ($1, 'Extra', $2)`, itemID, modifier)
	}
	f.exec(`INSERT INTO audit_log (actor_user_id, action, entity, entity_id, reason) VALUES ($1, 'order.void', 'order_item', $2, 'guest left')`, actor, itemID.String())
	return itemID
}

func methodTotal(ts []*restaurantv1.MethodTotal, m restaurantv1.PaymentMethod) int64 {
	var n int64
	for _, t := range ts {
		if t.GetMethod() == m {
			n += t.GetTotalPaisa()
		}
	}
	return n
}

const (
	cashM = restaurantv1.PaymentMethod_PAYMENT_METHOD_CASH
	cardM = restaurantv1.PaymentMethod_PAYMENT_METHOD_CARD
)

func TestDashboard(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	me := f.id(f.owner)
	a := f.invoice(inv{mode: "TAX_INVOICE", number: 1, at: now, subtotal: 10000, discount: 500, total: 9500, by: me})
	b := f.invoice(inv{mode: "TAX_INVOICE", number: 2, at: now, subtotal: 4000, total: 4000, by: me})
	c := f.invoice(inv{mode: "TAX_INVOICE", number: 3, at: now, subtotal: 7000, discount: 700, total: 6300, by: me})
	f.pay(a, "CASH", 9500)
	f.pay(b, "CARD", 4000)
	f.pay(c, "CASH", 6300)
	f.cancel(c, me, me, now)
	f.voidedItem("Momo", 15000, 2000, 2, me) // 30000: unit price already includes modifiers
	f.voidedItem("Tea", 5000, 0, 1, me)      // 5000

	d, err := f.svc.Dashboard(f.owner)
	if err != nil {
		t.Fatal(err)
	}
	if d.GetTodaySalesPaisa() != 13500 || d.GetTodayInvoiceCount() != 2 {
		t.Fatalf("sales = %d / %d", d.GetTodaySalesPaisa(), d.GetTodayInvoiceCount())
	}
	if methodTotal(d.GetByMethod(), cashM) != 9500 || methodTotal(d.GetByMethod(), cardM) != 4000 {
		t.Fatalf("by method = %v", d.GetByMethod())
	}
	if d.GetDiscountPaisa() != 500 || d.GetDiscountCount() != 1 {
		t.Fatalf("discount = %d (count %d), want 500", d.GetDiscountPaisa(), d.GetDiscountCount())
	}
	if d.GetVoidCount() != 2 || d.GetVoidValuePaisa() != 35000 {
		t.Fatalf("voids = %d / %d, want 2 / 35000", d.GetVoidCount(), d.GetVoidValuePaisa())
	}
	if d.GetCancelledCount() != 1 || d.GetCancelledValuePaisa() != 6300 {
		t.Fatalf("cancelled = %d / %d", d.GetCancelledCount(), d.GetCancelledValuePaisa())
	}
}

func TestDailySales(t *testing.T) {
	f := newFixture(t)
	me := f.id(f.owner)
	day := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC) // 17:45 in Kathmandu
	a := f.invoice(inv{mode: "TAX_INVOICE", number: 1, at: day, subtotal: 10000, discount: 1000, service: 900, total: 9900, by: me, bsDate: "2083.08.10"})
	b := f.invoice(inv{mode: "TAX_INVOICE", number: 2, at: day, subtotal: 5000, service: 500, total: 5500, by: me, bsDate: "2083.08.10"})
	c := f.invoice(inv{mode: "TAX_INVOICE", number: 3, at: day, subtotal: 2000, total: 2000, by: me, bsDate: "2083.08.10"})
	f.pay(a, "CASH", 9900)
	f.pay(b, "QR_FONEPAY", 5500)
	f.pay(c, "CASH", 2000)
	f.cancel(c, me, me, day.Add(time.Hour))

	r, err := f.svc.DailySales(f.owner, day.Add(-24*time.Hour).Unix(), day.Add(24*time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.GetRows()) != 1 {
		t.Fatalf("rows = %v", r.GetRows())
	}
	row := r.GetRows()[0]
	if row.GetAdDate() != "2026-11-25" || row.GetBsDate() != "2083.08.10" {
		t.Fatalf("dates = %q / %q", row.GetAdDate(), row.GetBsDate())
	}
	if row.GetInvoiceCount() != 2 || row.GetTotalPaisa() != 15400 || row.GetDiscountPaisa() != 1000 || row.GetServiceChargePaisa() != 1400 {
		t.Fatalf("row = %v", row)
	}
	if methodTotal(row.GetByMethod(), cashM) != 9900 || methodTotal(row.GetByMethod(), restaurantv1.PaymentMethod_PAYMENT_METHOD_QR_FONEPAY) != 5500 {
		t.Fatalf("by method = %v", row.GetByMethod())
	}
	if row.GetCancelledCount() != 1 {
		t.Fatalf("cancelled = %d, want 1", row.GetCancelledCount())
	}
}

func TestSalesBookEstimateFilterAndDisplayNumber(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	issuer := f.as(auth.RoleCashier, "Sita")
	f.invoice(inv{mode: "ESTIMATE", number: 7, at: now, subtotal: 100, total: 100, by: f.id(issuer)})
	f.invoice(inv{mode: "TAX_INVOICE", number: 12, at: now, subtotal: 200, total: 200, by: f.id(issuer)})
	from, to := now.Add(-time.Hour).Unix(), now.Add(time.Hour).Unix()

	taxOnly, err := f.svc.SalesBook(f.owner, &restaurantv1.RangeRequest{FromUnix: from, ToUnix: to})
	if err != nil {
		t.Fatal(err)
	}
	if len(taxOnly.GetRows()) != 1 || taxOnly.GetRows()[0].GetDisplayNumber() != "2083/84-12" {
		t.Fatalf("tax-only rows = %v", taxOnly.GetRows())
	}
	if row := taxOnly.GetRows()[0]; row.GetEnteredByName() != "Sita" || row.GetInvoiceId() == "" {
		t.Fatalf("row = %v", row)
	}
	all, err := f.svc.SalesBook(f.owner, &restaurantv1.RangeRequest{FromUnix: from, ToUnix: to, IncludeEstimates: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.GetRows()) != 2 {
		t.Fatalf("with estimates = %d rows, want 2", len(all.GetRows()))
	}
	var est bool
	for _, r := range all.GetRows() {
		est = est || r.GetDisplayNumber() == "E-2083/84-7"
	}
	if !est {
		t.Fatalf("estimate display number missing: %v", all.GetRows())
	}
	csv, err := f.svc.ExportSalesBookCsv(f.owner, &restaurantv1.RangeRequest{FromUnix: from, ToUnix: to})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(csv.GetData(), []byte("E-2083/84-7")) || !bytes.Contains(csv.GetData(), []byte("2083/84-12")) {
		t.Fatalf("csv = %s", csv.GetData())
	}
}

func TestCancelledInvoicesNames(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	issuer := f.as(auth.RoleCashier, "Sita")
	canceller := f.as(auth.RoleCashier, "Gita")
	mgr := f.as(auth.RoleManager, "Hari")
	id := f.invoice(inv{mode: "ESTIMATE", number: 3, at: now, subtotal: 900, total: 900, by: f.id(issuer)})
	f.cancel(id, f.id(canceller), f.id(mgr), now)

	r, err := f.svc.CancelledInvoices(f.owner, now.Add(-time.Hour).Unix(), now.Add(time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.GetRows()) != 1 {
		t.Fatalf("rows = %v", r.GetRows())
	}
	row := r.GetRows()[0]
	if row.GetDisplayNumber() != "E-2083/84-3" || row.GetIssuedByName() != "Sita" || row.GetCancelledByName() != "Gita" ||
		row.GetApprovedByName() != "Hari" || row.GetApprovedByUserId() != f.id(mgr).String() || row.GetInvoiceId() != id.String() ||
		row.GetIssuedByUserId() != f.id(issuer).String() {
		t.Fatalf("row = %v", row)
	}
}

func TestAuditLog(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	sita := f.as(auth.RoleCashier, "Sita")
	mgr := f.as(auth.RoleManager, "Hari")
	invID := f.invoice(inv{mode: "TAX_INVOICE", number: 5, at: now, subtotal: 100, total: 100, by: f.id(sita)})
	f.exec(`INSERT INTO audit_log (actor_user_id, action, entity, entity_id, reason, approver_user_id, device_label)
		VALUES ($1, 'invoice.cancel', 'invoice', $2, 'wrong table', $3, 'Counter iPad')`, f.id(sita), invID.String(), f.id(mgr))
	f.exec(`INSERT INTO audit_log (actor_user_id, action, entity, entity_id, after)
		VALUES ($1, 'payment.record', 'invoice', $2, '{"method":"CASH","amount":15050}')`, f.id(sita), invID.String())
	f.voidedItem("Chicken Momo", 100, 0, 1, f.id(sita))
	f.exec(`INSERT INTO audit_log (actor_user_id, action, entity, entity_id) VALUES ($1, 'staff.reset_pin', 'user', $2)`, f.id(mgr), f.id(sita).String())
	f.exec(`INSERT INTO audit_log (actor_user_id, action, entity, entity_id) VALUES ($1, 'login', 'user', $2)`, f.id(mgr), f.id(mgr).String())

	r, err := f.svc.AuditLog(f.owner, &restaurantv1.AuditLogRequest{FromUnix: now.Add(-time.Hour).Unix(), ToUnix: now.Add(time.Hour).Unix(), ActorId: f.id(sita).String()})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.GetEntries()) != 4 { // incl. her login from the fixture
		t.Fatalf("entries for Sita = %d, want 4: %v", len(r.GetEntries()), r.GetEntries())
	}
	by := map[string]*restaurantv1.AuditEntry{}
	for _, e := range r.GetEntries() {
		if e.GetActorName() != "Sita" {
			t.Fatalf("actor name = %q", e.GetActorName())
		}
		by[e.GetAction()] = e
	}
	c := by["invoice.cancel"]
	if c.GetSubject() != "2083/84-5" || c.GetApproverName() != "Hari" || c.GetDeviceLabel() != "Counter iPad" || !strings.Contains(c.GetDetail(), "wrong table") {
		t.Fatalf("cancel entry = %v", c)
	}
	if p := by["payment.record"]; !strings.Contains(p.GetDetail(), "CASH") || !strings.Contains(p.GetDetail(), "150.50") {
		t.Fatalf("payment detail = %q", p.GetDetail())
	}
	if v := by["order.void"]; v.GetSubject() != "Chicken Momo" || !strings.Contains(v.GetDetail(), "guest left") {
		t.Fatalf("void entry = %v", v)
	}

	all, err := f.svc.AuditLog(f.owner, &restaurantv1.AuditLogRequest{FromUnix: now.Add(-time.Hour).Unix(), ToUnix: now.Add(time.Hour).Unix(), Action: "staff.reset_pin"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.GetEntries()) != 1 || all.GetEntries()[0].GetSubject() != "Sita" || all.GetEntries()[0].GetActorName() != "Hari" {
		t.Fatalf("reset pin entries = %v", all.GetEntries())
	}
	if _, err := f.svc.AuditLog(f.owner, &restaurantv1.AuditLogRequest{ActorId: "nope"}); err == nil {
		t.Fatal("bad actor id accepted")
	}
}

// The sales book carries each bill's last print: when and by whom.
func TestSalesBookLastPrint(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	sita := f.as(auth.RoleCashier, "Sita")
	gita := f.as(auth.RoleCashier, "Gita")
	printed := f.invoice(inv{mode: "TAX_INVOICE", number: 1, at: now, subtotal: 100, total: 100, by: f.id(sita)})
	f.invoice(inv{mode: "TAX_INVOICE", number: 2, at: now, subtotal: 100, total: 100, by: f.id(sita)})
	last := now.Add(-time.Minute).Truncate(time.Second)
	f.exec(`INSERT INTO invoice_prints (invoice_id, printed_by_user_id, printed_at) VALUES ($1, $2, $3), ($1, $4, $5)`,
		printed, f.id(sita), last.Add(-10*time.Minute), f.id(gita), last)
	from, to := now.Add(-time.Hour).Unix(), now.Add(time.Hour).Unix()

	r, err := f.svc.SalesBook(f.owner, &restaurantv1.RangeRequest{FromUnix: from, ToUnix: to})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.GetRows()) != 2 {
		t.Fatalf("rows = %v", r.GetRows())
	}
	for _, row := range r.GetRows() {
		if row.GetInvoiceId() == printed.String() {
			if row.GetLastPrintedByName() != "Gita" || row.GetLastPrintedAtUnix() != last.Unix() {
				t.Fatalf("printed row = %v", row)
			}
		} else if row.GetLastPrintedByName() != "" || row.GetLastPrintedAtUnix() != 0 {
			t.Fatalf("unprinted row = %v", row)
		}
	}
	csv, err := f.svc.ExportSalesBookCsv(f.owner, &restaurantv1.RangeRequest{FromUnix: from, ToUnix: to})
	if err != nil {
		t.Fatal(err)
	}
	head, _, _ := strings.Cut(string(csv.GetData()), "\n")
	if !strings.Contains(head, "Last Printed") || !strings.Contains(head, "Printed By") || !strings.Contains(string(csv.GetData()), ",Gita") {
		t.Fatalf("csv = %s", csv.GetData())
	}
}
