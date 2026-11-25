package credit_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/billing"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/credit"
	"github.com/oneaccord/restro/api/internal/db"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/reports"
	"github.com/oneaccord/restro/api/internal/shift"
)

const (
	cashM    = restaurantv1.PaymentMethod_PAYMENT_METHOD_CASH
	qrM      = restaurantv1.PaymentMethod_PAYMENT_METHOD_QR_FONEPAY
	accountM = restaurantv1.PaymentMethod_PAYMENT_METHOD_ON_ACCOUNT
)

var nepal = time.FixedZone("NPT", 5*3600+45*60)

type fixture struct {
	t       *testing.T
	pool    *pgxpool.Pool
	authSvc *auth.Service
	credit  *credit.Service
	billing *billing.Service
	shifts  *shift.Service
	reports *reports.Service

	owner, manager, cashier, waiter         context.Context
	ownerID, managerID, cashierID, waiterID uuid.UUID
	item                                    uuid.UUID // Rs 180
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run credit integration tests")
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
	if _, err := pool.Exec(ctx, `TRUNCATE repayments, repayment_counters, customers, shift_adjustments, payments, cbms_outbox,
		invoice_prints, invoice_lines, order_item_modifiers, order_items, invoices, invoice_counters, orders, shifts,
		processed_commands, audit_log, sessions, devices, modifier_groups, menu_items, menu_categories, stations, users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE settings SET vat_registered = true, prices_include_vat = true, vat_rate_bp = 1300,
		service_charge_enabled = false, round_total_to_rupee = false WHERE id = 1`); err != nil {
		t.Fatalf("settings: %v", err)
	}
	f := &fixture{t: t, pool: pool, authSvc: auth.NewService(pool, zap.NewNop())}
	f.credit = credit.NewService(pool, nil, zap.NewNop())
	f.billing = billing.NewService(pool, nil, f.authSvc, zap.NewNop())
	f.shifts = shift.NewService(pool, nil, zap.NewNop())
	f.reports = reports.NewService(pool, zap.NewNop())
	f.owner, f.ownerID = f.user("Ram Owner", auth.RoleOwner, "1111")
	f.manager, f.managerID = f.user("Sita Gurung", auth.RoleManager, "2222")
	f.cashier, f.cashierID = f.user("Hari Thapa", auth.RoleCashier, "3333")
	f.waiter, f.waiterID = f.user("Gita", auth.RoleWaiter, "4444")

	q := sqlc.New(pool)
	st, err := q.CreateStation(ctx, sqlc.CreateStationParams{NameEn: "Kitchen"})
	if err != nil {
		t.Fatalf("station: %v", err)
	}
	cat, err := q.CreateCategory(ctx, sqlc.CreateCategoryParams{NameEn: "Food", Active: true})
	if err != nil {
		t.Fatalf("category: %v", err)
	}
	it, err := q.CreateItem(ctx, sqlc.CreateItemParams{CategoryID: cat.ID, NameEn: "Veg Momo", PricePaisa: 18000, StationID: st.ID, Active: true})
	if err != nil {
		t.Fatalf("item: %v", err)
	}
	f.item = it.ID
	f.openShift(f.cashier)
	return f
}

// user creates a logged-in user with a PIN and returns their context.
func (f *fixture) user(name, role, pin string) (context.Context, uuid.UUID) {
	f.t.Helper()
	ctx := context.Background()
	hash, err := auth.HashSecret("secret123")
	if err != nil {
		f.t.Fatal(err)
	}
	pinHash, err := auth.HashSecret(pin)
	if err != nil {
		f.t.Fatal(err)
	}
	username := "u-" + uuid.NewString()[:8]
	u, err := sqlc.New(f.pool).CreateUser(ctx, sqlc.CreateUserParams{
		Name: name, Role: role, Username: dbx.Text(username), PasswordHash: dbx.Text(hash), PinHash: dbx.Text(pinHash), Language: "en", Active: true,
	})
	if err != nil {
		f.t.Fatalf("create user: %v", err)
	}
	_, token, err := f.authSvc.PasswordLogin(ctx, username, "secret123")
	if err != nil {
		f.t.Fatalf("login: %v", err)
	}
	req := connect.NewRequest(&restaurantv1.GetSessionRequest{})
	req.Header().Set("Authorization", "Bearer "+token)
	var out context.Context
	_, _ = f.authSvc.Interceptor()(func(c context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		out = c
		return nil, nil
	})(ctx, req)
	if !auth.ActorFrom(out).LoggedIn() {
		f.t.Fatal("actor not resolved")
	}
	return out, u.ID
}

func (f *fixture) openShift(ctx context.Context) string {
	f.t.Helper()
	sh, err := f.shifts.OpenShift(ctx, 0, "")
	if err != nil {
		f.t.Fatalf("open shift: %v", err)
	}
	return sh.Id
}

func (f *fixture) myShift(ctx context.Context) *restaurantv1.ShiftReport {
	f.t.Helper()
	sh, err := f.shifts.GetMyOpenShift(ctx)
	if err != nil || sh == nil {
		f.t.Fatalf("my shift: %v (%v)", err, sh)
	}
	r, err := f.shifts.GetShiftReport(ctx, sh.Id)
	if err != nil {
		f.t.Fatalf("report: %v", err)
	}
	return r
}

func total(r *restaurantv1.ShiftReport, m restaurantv1.PaymentMethod) int64 {
	for _, t := range r.Totals {
		if t.Method == m {
			return t.TotalPaisa
		}
	}
	return 0
}

func (f *fixture) create(ctx context.Context, in *restaurantv1.CustomerInput) *restaurantv1.Customer {
	f.t.Helper()
	c, err := f.credit.CreateCustomer(ctx, &restaurantv1.CreateCustomerRequest{Customer: in})
	if err != nil {
		f.t.Fatalf("create customer: %v", err)
	}
	return c
}

func (f *fixture) balance(id string) int64 {
	f.t.Helper()
	c, err := f.credit.GetCustomer(f.cashier, &restaurantv1.GetCustomerRequest{Id: id})
	if err != nil {
		f.t.Fatalf("get customer: %v", err)
	}
	return c.BalancePaisa
}

// ramWithLimit is Ram's account (created by the cashier) with a limit set by the manager.
func (f *fixture) ramWithLimit(limit int64) *restaurantv1.Customer {
	f.t.Helper()
	ram := f.create(f.cashier, &restaurantv1.CustomerInput{Name: "Ram Shrestha", Phone: "9841234567"})
	if limit > 0 {
		var err error
		ram, err = f.credit.UpdateCustomer(f.manager, &restaurantv1.UpdateCustomerRequest{Id: ram.Id, Customer: &restaurantv1.CustomerInput{Name: ram.Name, Phone: ram.Phone}, CreditLimitPaisa: limit})
		if err != nil {
			f.t.Fatalf("set limit: %v", err)
		}
	}
	return ram
}

// seedOrder is a takeaway order of two SERVED lines (Rs 190 each, Rs 380 total).
func (f *fixture) seedOrder() string {
	f.t.Helper()
	ctx := context.Background()
	ocid := uuid.New()
	var orderID uuid.UUID
	if err := f.pool.QueryRow(ctx, `INSERT INTO orders (client_id, type, waiter_user_id) VALUES ($1, 'TAKEAWAY', $2) RETURNING id`, ocid, f.waiterID).Scan(&orderID); err != nil {
		f.t.Fatalf("insert order: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := f.pool.Exec(ctx, `INSERT INTO order_items (order_id, client_id, menu_item_id, name_en, unit_price_paisa, quantity, station_id, status)
			VALUES ($1, $2, $3, 'Thali', 19000, 1, $4, 'SERVED')`, orderID, uuid.New(), uuid.New(), uuid.New()); err != nil {
			f.t.Fatalf("insert item: %v", err)
		}
	}
	return ocid.String()
}

func onAccount(customerID string, amount int64) *restaurantv1.PaymentInput {
	return &restaurantv1.PaymentInput{Method: accountM, AmountPaisa: amount, CustomerId: customerID}
}

func cashOf(amount int64) *restaurantv1.PaymentInput {
	return &restaurantv1.PaymentInput{Method: cashM, AmountPaisa: amount}
}

func (f *fixture) issue(ctx context.Context, approval *restaurantv1.ManagerApproval, pays ...*restaurantv1.PaymentInput) (*restaurantv1.Invoice, error) {
	return f.billing.IssueBill(ctx, &restaurantv1.IssueBillRequest{CommandId: uuid.NewString(), OrderClientId: f.seedOrder(), Payments: pays, CreditApproval: approval})
}

// counter is a counter sale of n × Rs 180 on account.
func (f *fixture) counter(ctx context.Context, customerID string, n int32, approval *restaurantv1.ManagerApproval) (*restaurantv1.Invoice, error) {
	return f.billing.CounterSale(ctx, &restaurantv1.CounterSaleRequest{
		CommandId: uuid.NewString(), Label: "Ram",
		Lines:          []*restaurantv1.NewItem{{ClientId: uuid.NewString(), MenuItemId: f.item.String(), Quantity: n}},
		Payments:       []*restaurantv1.PaymentInput{onAccount(customerID, 18000*int64(n))},
		CreditApproval: approval,
	})
}

func (f *fixture) mustCounter(ctx context.Context, customerID string, n int32) *restaurantv1.Invoice {
	f.t.Helper()
	inv, err := f.counter(ctx, customerID, n, nil)
	if err != nil {
		f.t.Fatalf("counter sale: %v", err)
	}
	return inv
}

func (f *fixture) repay(ctx context.Context, customerID string, amount int64, m restaurantv1.PaymentMethod) (*restaurantv1.Repayment, error) {
	return f.credit.Repay(ctx, &restaurantv1.RepayRequest{RequestId: uuid.NewString(), CustomerId: customerID, AmountPaisa: amount, Method: m})
}

func (f *fixture) mustRepay(ctx context.Context, customerID string, amount int64) *restaurantv1.Repayment {
	f.t.Helper()
	r, err := f.repay(ctx, customerID, amount, cashM)
	if err != nil {
		f.t.Fatalf("repay: %v", err)
	}
	return r
}

type auditRow struct {
	action, entityID, reason string
	approver                 *uuid.UUID
	after                    string
}

func (f *fixture) audits(action string) []auditRow {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT action, COALESCE(entity_id, ''), COALESCE(reason, ''), approver_user_id, COALESCE(after::text, '')
		FROM audit_log WHERE action = $1 ORDER BY id`, action)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.action, &r.entityID, &r.reason, &r.approver, &r.after); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func wantErr(t *testing.T, err error, reason restaurantv1.ErrorReason, field string) {
	t.Helper()
	var ae *connecterr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("want %v (field %q), got %v", reason, field, err)
	}
	if ae.Reason != reason || (field != "" && ae.Field != field) {
		t.Fatalf("got reason=%v field=%q (%q), want %v field %q", ae.Reason, ae.Field, ae.Message, reason, field)
	}
}

var mgrPIN = func(f *fixture) *restaurantv1.ManagerApproval {
	return &restaurantv1.ManagerApproval{ManagerUserId: f.managerID.String(), Pin: "2222"}
}

// --- customer accounts ---

func TestCashierCreatesAccount(t *testing.T) {
	f := newFixture(t)
	c := f.create(f.cashier, &restaurantv1.CustomerInput{Name: " Ram Shrestha ", Phone: "+977 984-1234567", Notes: "Regular"})
	if c.Name != "Ram Shrestha" || c.Phone != "9841234567" || c.CreditLimitPaisa != 0 || !c.Active || c.CreatedByUserId != f.cashierID.String() || c.BalancePaisa != 0 || c.Notes != "Regular" {
		t.Fatalf("created = %+v", c)
	}
	got, err := f.credit.GetCustomer(f.cashier, &restaurantv1.GetCustomerRequest{Id: c.Id})
	if err != nil || got.Name != c.Name || got.CreatedAtUnix != c.CreatedAtUnix {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if a := f.audits("customer.change"); len(a) != 1 || a[0].entityID != c.Id {
		t.Fatalf("audit = %+v", a)
	}
}

func TestPhoneUniqueAndValid(t *testing.T) {
	f := newFixture(t)
	f.create(f.cashier, &restaurantv1.CustomerInput{Name: "Ram", Phone: "9841234567"})
	_, err := f.credit.CreateCustomer(f.cashier, &restaurantv1.CreateCustomerRequest{Customer: &restaurantv1.CustomerInput{Name: "Ram 2", Phone: "984-123-4567"}})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_DUPLICATE, "phone")
	_, err = f.credit.CreateCustomer(f.cashier, &restaurantv1.CreateCustomerRequest{Customer: &restaurantv1.CustomerInput{Name: "Hari", Phone: "12345"}})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID, "phone")
	_, err = f.credit.CreateCustomer(f.cashier, &restaurantv1.CreateCustomerRequest{Customer: &restaurantv1.CustomerInput{Name: " "}})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID, "name")
	_, err = f.credit.CreateCustomer(f.cashier, &restaurantv1.CreateCustomerRequest{Customer: &restaurantv1.CustomerInput{Name: "Office", Pan: "12AB"}})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID, "pan")
	// Without a phone is fine, more than once.
	f.create(f.cashier, &restaurantv1.CustomerInput{Name: "Office A"})
	f.create(f.cashier, &restaurantv1.CustomerInput{Name: "Office B"})
	// Editing another account onto a taken phone is a duplicate too.
	b := f.create(f.cashier, &restaurantv1.CustomerInput{Name: "Shyam", Phone: "9800000001"})
	_, err = f.credit.UpdateCustomer(f.cashier, &restaurantv1.UpdateCustomerRequest{Id: b.Id, Customer: &restaurantv1.CustomerInput{Name: "Shyam", Phone: "9841234567"}})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_DUPLICATE, "phone")
}

func TestOnlyOwnerOrManagerChangesLimit(t *testing.T) {
	f := newFixture(t)
	c := f.create(f.cashier, &restaurantv1.CustomerInput{Name: "Ram", Phone: "9841234567"})
	_, err := f.credit.UpdateCustomer(f.cashier, &restaurantv1.UpdateCustomerRequest{Id: c.Id, Customer: &restaurantv1.CustomerInput{Name: "Ram", Phone: c.Phone}, CreditLimitPaisa: 500000})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED, "credit_limit")
	// A cashier may still edit the other fields.
	u, err := f.credit.UpdateCustomer(f.cashier, &restaurantv1.UpdateCustomerRequest{Id: c.Id, Customer: &restaurantv1.CustomerInput{Name: "Ram S", Phone: c.Phone, Notes: "Tea"}})
	if err != nil || u.Notes != "Tea" || u.Name != "Ram S" {
		t.Fatalf("cashier edit = %+v, %v", u, err)
	}
	u, err = f.credit.UpdateCustomer(f.manager, &restaurantv1.UpdateCustomerRequest{Id: c.Id, Customer: &restaurantv1.CustomerInput{Name: "Ram S", Phone: c.Phone, Notes: "Tea"}, CreditLimitPaisa: 500000})
	if err != nil || u.CreditLimitPaisa != 500000 {
		t.Fatalf("manager limit = %+v, %v", u, err)
	}
	_, err = f.credit.UpdateCustomer(f.manager, &restaurantv1.UpdateCustomerRequest{Id: c.Id, Customer: &restaurantv1.CustomerInput{Name: "Ram S"}, CreditLimitPaisa: -1})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID, "credit_limit")
	a := f.audits("customer.credit_limit")
	if len(a) != 1 || a[0].entityID != c.Id {
		t.Fatalf("limit audit = %+v", a)
	}
	// created + the cashier's edit; the manager's limit-only change is not a field change.
	if n := len(f.audits("customer.change")); n != 2 {
		t.Fatalf("customer.change audits = %d, want 2", n)
	}
}

func TestSearchInactiveAndDeactivate(t *testing.T) {
	f := newFixture(t)
	ram := f.create(f.cashier, &restaurantv1.CustomerInput{Name: "Ram Shrestha", Phone: "9841234567"})
	f.create(f.cashier, &restaurantv1.CustomerInput{Name: "Sita Office", Phone: "014412345", Pan: "123456789"})
	names := func(ctx context.Context, q string, inactive bool) []string {
		t.Helper()
		res, err := f.credit.ListCustomers(ctx, &restaurantv1.ListCustomersRequest{Query: q, IncludeInactive: inactive})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		out := []string{}
		for _, c := range res.Customers {
			out = append(out, c.Name)
		}
		return out
	}
	for _, q := range []string{"ram", "98412", "+977 9841"} {
		if got := names(f.cashier, q, false); len(got) != 1 || got[0] != "Ram Shrestha" {
			t.Fatalf("search %q = %v", q, got)
		}
	}
	if got := names(f.cashier, "", false); len(got) != 2 || got[0] != "Ram Shrestha" {
		t.Fatalf("all = %v", got)
	}
	_, err := f.credit.SetCustomerActive(f.cashier, &restaurantv1.SetCustomerActiveRequest{Id: ram.Id, Active: false})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED, "")
	c, err := f.credit.SetCustomerActive(f.manager, &restaurantv1.SetCustomerActiveRequest{Id: ram.Id, Active: false})
	if err != nil || c.Active {
		t.Fatalf("deactivate = %+v, %v", c, err)
	}
	if a := f.audits("customer.change"); len(a) != 3 || a[2].entityID != ram.Id {
		t.Fatalf("audit = %+v", a)
	}
	if got := names(f.cashier, "ram", false); len(got) != 0 {
		t.Fatalf("inactive listed: %v", got)
	}
	if got := names(f.cashier, "ram", true); len(got) != 1 {
		t.Fatalf("include inactive = %v", got)
	}
	_, err = f.credit.GetCustomer(f.cashier, &restaurantv1.GetCustomerRequest{Id: uuid.NewString()})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_NOT_FOUND, "id")
}

func TestWaitersCannotSeeOrCreate(t *testing.T) {
	f := newFixture(t)
	_, err := f.credit.ListCustomers(f.waiter, &restaurantv1.ListCustomersRequest{})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED, "")
	_, err = f.credit.CreateCustomer(f.waiter, &restaurantv1.CreateCustomerRequest{Customer: &restaurantv1.CustomerInput{Name: "X"}})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED, "")
}

// --- on account at billing ---

func TestOnAccountWithinLimit(t *testing.T) {
	f := newFixture(t)
	ram := f.ramWithLimit(500000)
	inv, err := f.issue(f.cashier, nil, onAccount(ram.Id, 38000))
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	p := inv.Payments[0]
	if p.Method != accountM || p.CustomerId != ram.Id || p.CustomerName != "Ram Shrestha" || p.BalanceAfterPaisa != 38000 {
		t.Fatalf("payment = %+v", p)
	}
	if b := f.balance(ram.Id); b != 38000 {
		t.Fatalf("balance = %d", b)
	}
	r := f.myShift(f.cashier)
	if total(r, cashM) != 0 || total(r, accountM) != 0 || r.CreditGivenPaisa != 38000 || len(r.Payments) != 0 || r.ExpectedCashPaisa != 0 {
		t.Fatalf("shift = %+v", r)
	}
	a := f.audits("credit.give")
	if len(a) != 1 || a[0].entityID != ram.Id || a[0].approver != nil {
		t.Fatalf("audit = %+v", a)
	}
}

func TestOverLimitNeedsManager(t *testing.T) {
	f := newFixture(t)
	ram := f.ramWithLimit(20000)
	_, err := f.issue(f.cashier, nil, onAccount(ram.Id, 38000))
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_CREDIT_LIMIT_EXCEEDED, "customer_id")
	_, err = f.issue(f.cashier, &restaurantv1.ManagerApproval{ManagerUserId: f.managerID.String(), Pin: "0000"}, onAccount(ram.Id, 38000))
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_MANAGER_APPROVAL_INVALID, "credit_approval")
	inv, err := f.issue(f.cashier, mgrPIN(f), onAccount(ram.Id, 38000))
	if err != nil {
		t.Fatalf("approved: %v", err)
	}
	if inv.Number != 1 {
		t.Fatalf("number = %d, want 1 (refused attempts use no number)", inv.Number)
	}
	if a := f.audits("credit.give"); len(a) != 1 || a[0].approver == nil || *a[0].approver != f.managerID {
		t.Fatalf("audit = %+v", a)
	}
}

func TestNewAccountNeedsManagerForAnyCredit(t *testing.T) {
	f := newFixture(t)
	ram := f.create(f.cashier, &restaurantv1.CustomerInput{Name: "Ram"})
	_, err := f.counter(f.cashier, ram.Id, 1, nil)
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_CREDIT_LIMIT_EXCEEDED, "")
	if _, err := f.counter(f.cashier, ram.Id, 1, mgrPIN(f)); err != nil {
		t.Fatalf("approved counter sale: %v", err)
	}
	if b := f.balance(ram.Id); b != 18000 {
		t.Fatalf("balance = %d", b)
	}
}

func TestManagerMayGoOverLimitThemselves(t *testing.T) {
	f := newFixture(t)
	ram := f.ramWithLimit(0)
	f.openShift(f.manager)
	f.mustCounter(f.manager, ram.Id, 1)
	if a := f.audits("credit.give"); len(a) != 1 || a[0].approver == nil || *a[0].approver != f.managerID {
		t.Fatalf("audit = %+v", a)
	}
}

func TestMixedCashAndOnAccount(t *testing.T) {
	f := newFixture(t)
	ram := f.ramWithLimit(500000)
	if _, err := f.issue(f.cashier, nil, cashOf(10000), onAccount(ram.Id, 28000)); err != nil {
		t.Fatalf("issue: %v", err)
	}
	if b := f.balance(ram.Id); b != 28000 {
		t.Fatalf("balance = %d", b)
	}
	r := f.myShift(f.cashier)
	if total(r, cashM) != 10000 || r.CreditGivenPaisa != 28000 || len(r.Payments) != 1 {
		t.Fatalf("shift = %+v", r)
	}
}

func TestInactiveOrMissingCustomerRefused(t *testing.T) {
	f := newFixture(t)
	ram := f.ramWithLimit(500000)
	if _, err := f.credit.SetCustomerActive(f.manager, &restaurantv1.SetCustomerActiveRequest{Id: ram.Id}); err != nil {
		t.Fatal(err)
	}
	_, err := f.issue(f.cashier, nil, onAccount(ram.Id, 38000))
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_CUSTOMER_INACTIVE, "customer_id")
	_, err = f.issue(f.cashier, nil, onAccount("", 38000))
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID, "customer_id")
	_, err = f.issue(f.cashier, nil, onAccount(uuid.NewString(), 38000))
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID, "customer_id")
	// ON_ACCOUNT is not a RecordPayment method.
	if n := countRows(t, f.pool, `SELECT count(*) FROM invoices`); n != 0 {
		t.Fatalf("invoices = %d", n)
	}
}

func TestBusinessAccountFillsBuyer(t *testing.T) {
	f := newFixture(t)
	office := f.create(f.cashier, &restaurantv1.CustomerInput{Name: "Everest Traders", Pan: "301234567", Address: "Thamel"})
	if _, err := f.credit.UpdateCustomer(f.manager, &restaurantv1.UpdateCustomerRequest{Id: office.Id, Customer: &restaurantv1.CustomerInput{Name: office.Name, Pan: office.Pan, Address: office.Address}, CreditLimitPaisa: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	inv, err := f.issue(f.cashier, nil, onAccount(office.Id, 38000))
	if err != nil {
		t.Fatal(err)
	}
	if inv.BuyerName != "Everest Traders" || inv.BuyerPan != "301234567" {
		t.Fatalf("buyer = %q %q", inv.BuyerName, inv.BuyerPan)
	}
}

func TestCancelReversesAndCreditInFavour(t *testing.T) {
	f := newFixture(t)
	ram := f.ramWithLimit(500000)
	inv, err := f.issue(f.cashier, nil, onAccount(ram.Id, 38000))
	if err != nil {
		t.Fatal(err)
	}
	f.mustRepay(f.cashier, ram.Id, 38000)
	if b := f.balance(ram.Id); b != 0 {
		t.Fatalf("balance after repay = %d", b)
	}
	if _, err := f.billing.CancelInvoice(f.cashier, &restaurantv1.CancelInvoiceRequest{CommandId: uuid.NewString(), InvoiceId: inv.Id, Reason: "wrong table", Approval: mgrPIN(f)}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if b := f.balance(ram.Id); b != -38000 {
		t.Fatalf("balance after cancel = %d, want -38000", b)
	}
	// The credit in their favour pays for the next bill without any limit.
	if _, err := f.credit.UpdateCustomer(f.manager, &restaurantv1.UpdateCustomerRequest{Id: ram.Id, Customer: &restaurantv1.CustomerInput{Name: ram.Name, Phone: ram.Phone}}); err != nil {
		t.Fatal(err)
	}
	f.mustCounter(f.cashier, ram.Id, 1)
	if b := f.balance(ram.Id); b != -20000 {
		t.Fatalf("balance = %d, want -20000", b)
	}
	// Cancelled in the open shift: no credit given counted, and no adjustment owed.
	if r := f.myShift(f.cashier); r.CreditGivenPaisa != 18000 || len(r.Adjustments) != 0 {
		t.Fatalf("shift = %+v", r)
	}
}

func TestCancelOnAccountBillFromClosedShiftNeedsNoAdjustment(t *testing.T) {
	f := newFixture(t)
	ram := f.ramWithLimit(500000)
	inv, err := f.issue(f.cashier, nil, onAccount(ram.Id, 38000))
	if err != nil {
		t.Fatal(err)
	}
	sh, _ := f.shifts.GetMyOpenShift(f.cashier)
	if _, err := f.shifts.CloseShift(f.cashier, sh.Id, 0, ""); err != nil {
		t.Fatal(err)
	}
	// No money moved, so the canceller needs no open shift and nothing is counted back.
	if _, err := f.billing.CancelInvoice(f.manager, &restaurantv1.CancelInvoiceRequest{CommandId: uuid.NewString(), InvoiceId: inv.Id, Reason: "wrong", Approval: mgrPIN(f)}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if n := countRows(t, f.pool, `SELECT count(*) FROM shift_adjustments`); n != 0 {
		t.Fatalf("adjustments = %d", n)
	}
	if b := f.balance(ram.Id); b != 0 {
		t.Fatalf("balance = %d", b)
	}
}

func TestConcurrentBillsCannotBothUseTheLastOfALimit(t *testing.T) {
	f := newFixture(t)
	ram := f.ramWithLimit(30000)
	other, _ := f.user("Kiran", auth.RoleCashier, "5555")
	f.openShift(other)
	errs := make(chan error, 2)
	for _, ctx := range []context.Context{f.cashier, other} {
		go func(ctx context.Context) {
			_, err := f.counter(ctx, ram.Id, 1, nil)
			errs <- err
		}(ctx)
	}
	var ok, refused int
	for i := 0; i < 2; i++ {
		err := <-errs
		if err == nil {
			ok++
			continue
		}
		wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_CREDIT_LIMIT_EXCEEDED, "")
		refused++
	}
	if ok != 1 || refused != 1 || f.balance(ram.Id) != 18000 {
		t.Fatalf("ok=%d refused=%d balance=%d", ok, refused, f.balance(ram.Id))
	}
}

// --- repayments ---

func TestRepaymentsInShiftWithOwnNumbers(t *testing.T) {
	f := newFixture(t)
	ram := f.ramWithLimit(500000)
	if _, err := f.issue(f.cashier, nil, onAccount(ram.Id, 38000)); err != nil {
		t.Fatal(err)
	}
	reqID := uuid.NewString()
	r, err := f.credit.Repay(f.cashier, &restaurantv1.RepayRequest{RequestId: reqID, CustomerId: ram.Id, AmountPaisa: 10000, Method: cashM, Note: " part "})
	if err != nil {
		t.Fatal(err)
	}
	if r.Number != 1 || r.DisplayNumber != "R-1" || r.AmountPaisa != 10000 || r.BalanceAfterPaisa != 28000 || r.CustomerName != "Ram Shrestha" ||
		r.TakenByName != "Hari Thapa" || r.VoidedAtUnix != 0 || r.Note != "part" || len(r.BsDate) != 10 || r.Method != cashM {
		t.Fatalf("repayment = %+v", r)
	}
	qr, err := f.credit.Repay(f.cashier, &restaurantv1.RepayRequest{RequestId: uuid.NewString(), CustomerId: ram.Id, AmountPaisa: 5000, Method: qrM, Reference: "FP-9"})
	if err != nil || qr.DisplayNumber != "R-2" {
		t.Fatalf("qr = %+v, %v", qr, err)
	}
	// Retrying the same request returns the same repayment.
	again, err := f.credit.Repay(f.cashier, &restaurantv1.RepayRequest{RequestId: reqID, CustomerId: ram.Id, AmountPaisa: 10000, Method: cashM, Note: "part"})
	if err != nil || again.Id != r.Id {
		t.Fatalf("retry = %+v, %v", again, err)
	}
	sh := f.myShift(f.cashier)
	if total(sh, cashM) != 10000 || total(sh, qrM) != 5000 || sh.RepaymentsPaisa != 15000 || sh.ExpectedCashPaisa != 10000 {
		t.Fatalf("shift = %+v", sh)
	}
	if len(sh.Payments) != 2 || !sh.Payments[0].Repayment || sh.Payments[0].DisplayNumber != "R-1" || sh.Payments[0].Method != cashM ||
		sh.Payments[1].DisplayNumber != "R-2" || sh.Payments[1].Method != qrM || sh.Payments[1].RepaymentId != qr.Id {
		t.Fatalf("shift payments = %+v", sh.Payments)
	}
	if b := f.balance(ram.Id); b != 23000 {
		t.Fatalf("balance = %d", b)
	}
	if n := len(f.audits("credit.repay")); n != 2 {
		t.Fatalf("repay audits = %d", n)
	}
}

func TestRepaymentLimitsAndShift(t *testing.T) {
	f := newFixture(t)
	ram := f.ramWithLimit(500000)
	f.mustCounter(f.cashier, ram.Id, 1)
	_, err := f.repay(f.cashier, ram.Id, 18001, cashM)
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_REPAYMENT_TOO_HIGH, "amount")
	_, err = f.repay(f.cashier, ram.Id, 0, cashM)
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID, "amount")
	_, err = f.repay(f.cashier, ram.Id, 100, accountM)
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID, "method")
	_, err = f.credit.Repay(f.cashier, &restaurantv1.RepayRequest{RequestId: "p1", CustomerId: ram.Id, AmountPaisa: 100, Method: cashM})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID, "request_id")
	_, err = f.repay(f.manager, ram.Id, 100, cashM)
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_SHIFT_NOT_OPEN, "")
	_, err = f.repay(f.waiter, ram.Id, 100, cashM)
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED, "")
	if _, err := f.credit.SetCustomerActive(f.manager, &restaurantv1.SetCustomerActiveRequest{Id: ram.Id}); err != nil {
		t.Fatal(err)
	}
	f.mustRepay(f.cashier, ram.Id, 18000) // inactive accounts can still repay
	if b := f.balance(ram.Id); b != 0 {
		t.Fatalf("balance = %d", b)
	}
}

func TestVoidRepaymentInOpenShift(t *testing.T) {
	f := newFixture(t)
	ram := f.ramWithLimit(500000)
	f.mustCounter(f.cashier, ram.Id, 1)
	r := f.mustRepay(f.cashier, ram.Id, 10000)
	_, err := f.credit.VoidRepayment(f.cashier, &restaurantv1.VoidRepaymentRequest{Id: r.Id, Reason: "typo"})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED, "")
	_, err = f.credit.VoidRepayment(f.manager, &restaurantv1.VoidRepaymentRequest{Id: r.Id, Reason: " "})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID, "reason")
	v, err := f.credit.VoidRepayment(f.manager, &restaurantv1.VoidRepaymentRequest{Id: r.Id, Reason: "typo"})
	if err != nil || v.VoidReason != "typo" || v.VoidedByUserId != f.managerID.String() || v.VoidedAtUnix == 0 {
		t.Fatalf("void = %+v, %v", v, err)
	}
	if a := f.audits("credit.repay_void"); len(a) != 1 || a[0].reason != "typo" || a[0].entityID != r.Id {
		t.Fatalf("audit = %+v", a)
	}
	_, err = f.credit.VoidRepayment(f.manager, &restaurantv1.VoidRepaymentRequest{Id: r.Id, Reason: "again"})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID, "id")
	if b := f.balance(ram.Id); b != 18000 {
		t.Fatalf("balance = %d", b)
	}
	sh := f.myShift(f.cashier)
	if total(sh, cashM) != 0 || sh.RepaymentsPaisa != 0 || len(sh.Payments) != 1 || !sh.Payments[0].RepaymentVoided {
		t.Fatalf("shift = %+v", sh)
	}
	list, err := f.credit.ListRepayments(f.cashier, &restaurantv1.ListRepaymentsRequest{CustomerId: ram.Id})
	if err != nil || len(list.Repayments) != 1 || list.Repayments[0].VoidReason != "typo" {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

func TestVoidAfterShiftClosedAdjustsVoidersShift(t *testing.T) {
	f := newFixture(t)
	ram := f.ramWithLimit(500000)
	f.mustCounter(f.cashier, ram.Id, 1)
	r := f.mustRepay(f.cashier, ram.Id, 10000)
	hari, _ := f.shifts.GetMyOpenShift(f.cashier)
	if _, err := f.shifts.CloseShift(f.cashier, hari.Id, 10000, ""); err != nil {
		t.Fatal(err)
	}
	_, err := f.credit.VoidRepayment(f.manager, &restaurantv1.VoidRepaymentRequest{Id: r.Id, Reason: "typo"})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_SHIFT_NOT_OPEN, "")
	f.openShift(f.manager)
	if _, err := f.credit.VoidRepayment(f.manager, &restaurantv1.VoidRepaymentRequest{Id: r.Id, Reason: "typo"}); err != nil {
		t.Fatal(err)
	}
	mine := f.myShift(f.manager)
	if len(mine.Adjustments) != 1 || mine.Adjustments[0].DisplayNumber != "R-1" || mine.Adjustments[0].Method != cashM ||
		mine.Adjustments[0].AmountPaisa != 10000 || mine.Adjustments[0].RepaymentId != r.Id || mine.Adjustments[0].OriginalShiftId != hari.Id {
		t.Fatalf("adjustments = %+v", mine.Adjustments)
	}
	if total(mine, cashM) != -10000 {
		t.Fatalf("my cash = %d", total(mine, cashM))
	}
	closed, err := f.shifts.GetShiftReport(f.manager, hari.Id)
	if err != nil || total(closed, cashM) != 10000 || closed.RepaymentsPaisa != 10000 || closed.DifferencePaisa != 0 {
		t.Fatalf("closed shift = %+v, %v", closed, err)
	}
	// The other close-out path (lazy catch-up of cancel adjustments) leaves it alone.
	if _, err := f.shifts.ListShifts(f.manager, 10); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, f.pool, `SELECT count(*) FROM shift_adjustments`); n != 1 {
		t.Fatalf("adjustments = %d", n)
	}
}

// --- statements and reports ---

func TestStatementOldestFirstAllocation(t *testing.T) {
	f := newFixture(t)
	ram := f.ramWithLimit(500000)
	a := f.mustCounter(f.cashier, ram.Id, 1) // 180
	b := f.mustCounter(f.cashier, ram.Id, 2) // 360
	f.mustRepay(f.cashier, ram.Id, 25000)
	today := time.Now().In(nepal).Format("2006-01-02")
	st, err := f.credit.GetStatement(f.cashier, &restaurantv1.StatementRequest{CustomerId: ram.Id, From: today, To: today})
	if err != nil {
		t.Fatal(err)
	}
	if st.OpeningPaisa != 0 || st.ClosingPaisa != 29000 || st.Customer.GetId() != ram.Id || len(st.Entries) != 3 {
		t.Fatalf("statement = %+v", st)
	}
	type row struct {
		kind    restaurantv1.StatementEntryKind
		ref     string
		amount  int64
		balance int64
	}
	want := []row{
		{restaurantv1.StatementEntryKind_STATEMENT_ENTRY_KIND_BILL, a.DisplayNumber, 18000, 18000},
		{restaurantv1.StatementEntryKind_STATEMENT_ENTRY_KIND_BILL, b.DisplayNumber, 36000, 54000},
		{restaurantv1.StatementEntryKind_STATEMENT_ENTRY_KIND_REPAYMENT, "R-1", 25000, 29000},
	}
	for i, w := range want {
		e := st.Entries[i]
		if (row{e.Kind, e.Ref, e.AmountPaisa, e.BalancePaisa}) != w {
			t.Fatalf("entry %d = %+v, want %+v", i, e, w)
		}
	}
	if st.Entries[0].SettledPaisa != 18000 || st.Entries[1].SettledPaisa != 7000 || st.Entries[0].InvoiceId != a.Id || st.Entries[2].Method != cashM {
		t.Fatalf("settled = %d, %d", st.Entries[0].SettledPaisa, st.Entries[1].SettledPaisa)
	}
	// A range after today: everything is in the opening balance.
	later, err := f.credit.GetStatement(f.cashier, &restaurantv1.StatementRequest{CustomerId: ram.Id, From: "2099-01-01", To: "2099-01-31"})
	if err != nil || later.OpeningPaisa != 29000 || len(later.Entries) != 0 || later.ClosingPaisa != 29000 {
		t.Fatalf("later = %+v, %v", later, err)
	}
	// A range before anything: nothing.
	earlier, err := f.credit.GetStatement(f.cashier, &restaurantv1.StatementRequest{CustomerId: ram.Id, From: "2020-01-01", To: "2020-01-31"})
	if err != nil || earlier.OpeningPaisa != 0 || len(earlier.Entries) != 0 || earlier.ClosingPaisa != 0 {
		t.Fatalf("earlier = %+v, %v", earlier, err)
	}
	_, err = f.credit.GetStatement(f.cashier, &restaurantv1.StatementRequest{CustomerId: ram.Id, From: "2026-02-01", To: "2026-01-01"})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID, "")
}

func TestCreditReportAndDashboard(t *testing.T) {
	f := newFixture(t)
	ram := f.ramWithLimit(500000)
	a := f.mustCounter(f.cashier, ram.Id, 1)
	f.create(f.cashier, &restaurantv1.CustomerInput{Name: "Nobody owes"})
	f.mustRepay(f.cashier, ram.Id, 5000)
	_, err := f.reports.CreditReport(f.cashier)
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED, "")
	rep, err := f.reports.CreditReport(f.manager)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rows) != 1 {
		t.Fatalf("rows = %+v", rep.Rows)
	}
	r := rep.Rows[0]
	if r.Name != "Ram Shrestha" || r.BalancePaisa != 13000 || r.CreditLimitPaisa != 500000 || r.OldestUnpaidAtUnix != a.AdDatetimeUnix || r.LastRepaymentAtUnix == 0 || !r.Active {
		t.Fatalf("row = %+v", r)
	}
	dash, err := f.reports.Dashboard(f.manager)
	if err != nil {
		t.Fatal(err)
	}
	if dash.CreditGivenTodayPaisa != 18000 || dash.RepaymentsTodayPaisa != 5000 || dash.CreditOwedPaisa != 13000 {
		t.Fatalf("dashboard credit = %d %d %d", dash.CreditGivenTodayPaisa, dash.RepaymentsTodayPaisa, dash.CreditOwedPaisa)
	}
	var onAcc int64
	for _, m := range dash.ByMethod {
		if m.Method == accountM {
			onAcc = m.TotalPaisa
		}
	}
	if onAcc != 18000 {
		t.Fatalf("dashboard by method = %+v", dash.ByMethod)
	}
	now := time.Now()
	daily, err := f.reports.DailySales(f.manager, now.Add(-24*time.Hour).Unix(), now.Add(time.Hour).Unix())
	if err != nil || len(daily.Rows) != 1 {
		t.Fatalf("daily = %+v, %v", daily, err)
	}
	onAcc = 0
	for _, m := range daily.Rows[0].ByMethod {
		if m.Method == accountM {
			onAcc = m.TotalPaisa
		}
	}
	if onAcc != 18000 {
		t.Fatalf("daily by method = %+v", daily.Rows[0].ByMethod)
	}
	// Audit browser shows the customer as the subject.
	log, err := f.reports.AuditLog(f.manager, &restaurantv1.AuditLogRequest{FromUnix: now.Add(-time.Hour).Unix(), ToUnix: now.Add(time.Hour).Unix(), Action: "credit.give"})
	if err != nil || len(log.Entries) != 1 || log.Entries[0].Subject != "Ram Shrestha" {
		t.Fatalf("audit log = %+v, %v", log, err)
	}
	log, err = f.reports.AuditLog(f.manager, &restaurantv1.AuditLogRequest{FromUnix: now.Add(-time.Hour).Unix(), ToUnix: now.Add(time.Hour).Unix(), Action: "credit.repay"})
	if err != nil || len(log.Entries) != 1 || log.Entries[0].Subject != "R-1" {
		t.Fatalf("audit log = %+v, %v", log, err)
	}
}

func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}
