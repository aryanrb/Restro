package billing_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/billing"
	"github.com/oneaccord/restro/api/internal/bsdate"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/orders"
)

// billFixture is newTestBilling plus helpers for roles, shifts and a menu item.
type billFixture struct {
	t       *testing.T
	svc     *billing.Service
	pool    *pgxpool.Pool
	owner   context.Context // OWNER actor (PIN 1111)
	ownerID uuid.UUID
	authSvc *auth.Service
	momo    uuid.UUID // menu item, 25000
}

func newBillFixture(t *testing.T) *billFixture {
	t.Helper()
	svc, pool, ctx := newTestBilling(t)
	f := &billFixture{t: t, svc: svc, pool: pool, owner: ctx, ownerID: auth.ActorFrom(ctx).User.ID, authSvc: auth.NewService(pool, zap.NewNop())}
	// Known money rules: VAT-inclusive prices, no service charge, no rounding, so
	// an invoice total is simply subtotal - discount.
	f.exec(`UPDATE settings SET vat_registered = true, prices_include_vat = true, vat_rate_bp = 1300,
		service_charge_enabled = false, round_total_to_rupee = false WHERE id = 1`)
	f.exec(`TRUNCATE shifts, modifier_groups, menu_items, menu_categories, stations RESTART IDENTITY CASCADE`)
	q := sqlc.New(pool)
	st, err := q.CreateStation(context.Background(), sqlc.CreateStationParams{NameEn: "Kitchen"})
	if err != nil {
		t.Fatalf("station: %v", err)
	}
	cat, err := q.CreateCategory(context.Background(), sqlc.CreateCategoryParams{NameEn: "Food", Active: true})
	if err != nil {
		t.Fatalf("category: %v", err)
	}
	it, err := q.CreateItem(context.Background(), sqlc.CreateItemParams{CategoryID: cat.ID, NameEn: "Momo", PricePaisa: 25000, StationID: st.ID, Active: true})
	if err != nil {
		t.Fatalf("item: %v", err)
	}
	f.momo = it.ID
	return f
}

func (f *billFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (f *billFixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		f.t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// as returns a context for a fresh logged-in user with the given role.
func (f *billFixture) as(role string) context.Context {
	f.t.Helper()
	ctx := context.Background()
	hash, err := auth.HashSecret("secret123")
	if err != nil {
		f.t.Fatal(err)
	}
	username := "u-" + uuid.NewString()[:8]
	if _, err := sqlc.New(f.pool).CreateUser(ctx, sqlc.CreateUserParams{
		Name: role, Role: role, Username: dbx.Text(username), PasswordHash: dbx.Text(hash), Language: "en", Active: true, OrganisationID: dbx.BootstrapOrgID(ctx, sqlc.New(f.pool)),
	}); err != nil {
		f.t.Fatalf("create user: %v", err)
	}
	_, token, err := f.authSvc.PasswordLogin(ctx, username, "secret123")
	if err != nil {
		f.t.Fatalf("login: %v", err)
	}
	req := connect.NewRequest(&restaurantv1.ListInvoicesRequest{})
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

// openShift opens a shift for the actor in ctx and returns its id.
func (f *billFixture) openShift(ctx context.Context) uuid.UUID {
	f.t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `INSERT INTO shifts (cashier_user_id) VALUES ($1) RETURNING id`, auth.ActorFrom(ctx).User.ID).Scan(&id); err != nil {
		f.t.Fatalf("open shift: %v", err)
	}
	return id
}

func cash(amount int64) *restaurantv1.PaymentInput {
	return &restaurantv1.PaymentInput{Method: restaurantv1.PaymentMethod_PAYMENT_METHOD_CASH, AmountPaisa: amount}
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

func ids(us []uuid.UUID) []string {
	out := make([]string, len(us))
	for i, u := range us {
		out[i] = u.String()
	}
	return out
}

// --- IssueBill ---

func TestIssueBillFullOrderPaysAndCloses(t *testing.T) {
	f := newBillFixture(t)
	shift := f.openShift(f.owner)
	ocid, _ := seedOrder(t, f.owner, f.pool, 2) // 2 × 15000, SERVED

	inv, err := f.svc.IssueBill(f.owner, &restaurantv1.IssueBillRequest{
		CommandId: uuid.NewString(), OrderClientId: ocid.String(),
		Payments: []*restaurantv1.PaymentInput{cash(20000), {Method: restaurantv1.PaymentMethod_PAYMENT_METHOD_CARD, AmountPaisa: 10000, Reference: "visa"}},
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if inv.TotalPaisa != 30000 || len(inv.Lines) != 2 || len(inv.Payments) != 2 {
		t.Fatalf("invoice total=%d lines=%d payments=%d", inv.TotalPaisa, len(inv.Lines), len(inv.Payments))
	}
	if n := f.count(`SELECT count(*) FROM payments WHERE invoice_id = $1 AND shift_id = $2`, inv.Id, shift); n != 2 {
		t.Fatalf("payments on shift = %d, want 2", n)
	}
	if n := f.count(`SELECT count(*) FROM audit_log WHERE entity_id = $1 AND action IN ('invoice.issue', 'payment.record')`, inv.Id); n != 3 {
		t.Fatalf("audit rows = %d, want 3 (issue + 2 payments)", n)
	}
	if got := orderStatus(t, f.owner, f.pool, ocid); got != "CLOSED" {
		t.Fatalf("status = %s, want CLOSED (billed + served)", got)
	}
}

func TestIssueBillSubsetWithDiscountAndBuyer(t *testing.T) {
	f := newBillFixture(t)
	f.openShift(f.owner)
	ocid, items := seedOrder(t, f.owner, f.pool, 3)

	inv, err := f.svc.IssueBill(f.owner, &restaurantv1.IssueBillRequest{
		CommandId: uuid.NewString(), OrderClientId: ocid.String(), ItemIds: ids(items[:2]),
		Discount:  &restaurantv1.DiscountInput{Type: restaurantv1.DiscountType_DISCOUNT_TYPE_AMOUNT, Value: 3000, Reason: "regular"},
		BuyerName: "Hari Traders", BuyerPan: "123456789",
		Payments: []*restaurantv1.PaymentInput{cash(27000)},
	})
	if err != nil {
		t.Fatalf("issue subset: %v", err)
	}
	if inv.SubtotalPaisa != 30000 || inv.DiscountPaisa != 3000 || inv.TotalPaisa != 27000 {
		t.Fatalf("subtotal=%d discount=%d total=%d", inv.SubtotalPaisa, inv.DiscountPaisa, inv.TotalPaisa)
	}
	if inv.BuyerName != "Hari Traders" || inv.BuyerPan != "123456789" || len(inv.Lines) != 2 {
		t.Fatalf("buyer=%q pan=%q lines=%d", inv.BuyerName, inv.BuyerPan, len(inv.Lines))
	}
	if got := orderStatus(t, f.owner, f.pool, ocid); got != "OPEN" {
		t.Fatalf("after subset status = %s, want OPEN", got)
	}
	// An item not on the order (or already billed) is rejected.
	_, err = f.svc.IssueBill(f.owner, &restaurantv1.IssueBillRequest{
		CommandId: uuid.NewString(), OrderClientId: ocid.String(), ItemIds: ids(items[:1]), Payments: []*restaurantv1.PaymentInput{cash(15000)},
	})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID, "item_ids")

	// The rest (empty item_ids = all unbilled) completes and closes the served order.
	if _, err := f.svc.IssueBill(f.owner, &restaurantv1.IssueBillRequest{
		CommandId: uuid.NewString(), OrderClientId: ocid.String(), Payments: []*restaurantv1.PaymentInput{cash(15000)},
	}); err != nil {
		t.Fatalf("issue rest: %v", err)
	}
	if got := orderStatus(t, f.owner, f.pool, ocid); got != "CLOSED" {
		t.Fatalf("after last bill status = %s, want CLOSED", got)
	}
}

func TestIssueBillRejectsBadPaymentsWritingNothing(t *testing.T) {
	f := newBillFixture(t)
	f.openShift(f.owner)
	ocid, _ := seedOrder(t, f.owner, f.pool, 2) // total 30000

	cases := map[string][]*restaurantv1.PaymentInput{
		"none":        nil,
		"short":       {cash(29999)},
		"over":        {cash(20000), cash(10001)},
		"zero amount": {cash(30000), cash(0)},
		"negative":    {cash(30001), cash(-1)},
		"no method":   {{AmountPaisa: 30000}},
	}
	for name, pays := range cases {
		_, err := f.svc.IssueBill(f.owner, &restaurantv1.IssueBillRequest{CommandId: uuid.NewString(), OrderClientId: ocid.String(), Payments: pays})
		if err == nil {
			t.Fatalf("%s: issued, want INVALID payments", name)
		}
		wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID, "payments")
	}
	for _, tbl := range []string{"invoices", "invoice_lines", "invoice_counters", "payments", "processed_commands", "cbms_outbox"} {
		if n := f.count(`SELECT count(*) FROM ` + tbl); n != 0 {
			t.Fatalf("%s has %d rows after rejected bills, want 0", tbl, n)
		}
	}
	inv, err := f.svc.IssueBill(f.owner, &restaurantv1.IssueBillRequest{CommandId: uuid.NewString(), OrderClientId: ocid.String(), Payments: []*restaurantv1.PaymentInput{cash(30000)}})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if inv.Number != 1 {
		t.Fatalf("invoice number = %d, want 1 (rejections consume no number)", inv.Number)
	}
}

func TestIssueBillNeedsOpenShift(t *testing.T) {
	f := newBillFixture(t)
	ocid, _ := seedOrder(t, f.owner, f.pool, 1)
	_, err := f.svc.IssueBill(f.owner, &restaurantv1.IssueBillRequest{CommandId: uuid.NewString(), OrderClientId: ocid.String(), Payments: []*restaurantv1.PaymentInput{cash(15000)}})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_SHIFT_NOT_OPEN, "")
	if n := f.count(`SELECT count(*) FROM invoices`); n != 0 {
		t.Fatalf("invoices = %d, want 0", n)
	}
}

func TestIssueBillReplayReturnsSameInvoice(t *testing.T) {
	f := newBillFixture(t)
	f.openShift(f.owner)
	ocid, _ := seedOrder(t, f.owner, f.pool, 1)
	req := &restaurantv1.IssueBillRequest{CommandId: uuid.NewString(), OrderClientId: ocid.String(), Payments: []*restaurantv1.PaymentInput{cash(15000)}}
	a, err := f.svc.IssueBill(f.owner, req)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	b, err := f.svc.IssueBill(f.owner, req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if a.Id != b.Id || a.Number != b.Number || len(b.Payments) != 1 {
		t.Fatalf("replay returned a different invoice: %s/%d vs %s/%d", a.Id, a.Number, b.Id, b.Number)
	}
	if n := f.count(`SELECT count(*) FROM payments`); n != 1 {
		t.Fatalf("payments = %d, want 1", n)
	}
}

func TestIssueBillCashierDiscountOverLimitNeedsApproval(t *testing.T) {
	f := newBillFixture(t)
	cashier := f.as(auth.RoleCashier)
	f.openShift(cashier)
	ocid, _ := seedOrder(t, f.owner, f.pool, 2)                                                                               // 30000
	disc := &restaurantv1.DiscountInput{Type: restaurantv1.DiscountType_DISCOUNT_TYPE_PERCENT, Value: 2000, Reason: "friend"} // 20% > 10%

	_, err := f.svc.IssueBill(cashier, &restaurantv1.IssueBillRequest{CommandId: uuid.NewString(), OrderClientId: ocid.String(), Discount: disc, Payments: []*restaurantv1.PaymentInput{cash(24000)}})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_MANAGER_APPROVAL_REQUIRED, "")
	if n := f.count(`SELECT count(*) FROM invoices`); n != 0 {
		t.Fatalf("invoices = %d, want 0", n)
	}

	disc.Approval = &restaurantv1.ManagerApproval{ManagerUserId: f.ownerID.String(), Pin: "1111"}
	inv, err := f.svc.IssueBill(cashier, &restaurantv1.IssueBillRequest{CommandId: uuid.NewString(), OrderClientId: ocid.String(), Discount: disc, Payments: []*restaurantv1.PaymentInput{cash(24000)}})
	if err != nil {
		t.Fatalf("issue with approval: %v", err)
	}
	if inv.DiscountPaisa != 6000 || inv.TotalPaisa != 24000 {
		t.Fatalf("discount=%d total=%d, want 6000/24000", inv.DiscountPaisa, inv.TotalPaisa)
	}
	if n := f.count(`SELECT count(*) FROM invoices WHERE id = $1 AND discount_approved_by_user_id = $2`, inv.Id, f.ownerID); n != 1 {
		t.Fatal("approver not recorded on the invoice")
	}
}

// Pay-first: billed and paid before the food is handed over stays BILLED; the
// last hand-over closes it.
func TestIssueBillPayFirstStaysBilledUntilServed(t *testing.T) {
	f := newBillFixture(t)
	f.openShift(f.owner)
	ocid, items := seedOrder(t, f.owner, f.pool, 2)
	f.exec(`UPDATE order_items SET status = 'SENT' WHERE id = ANY($1)`, items)

	if _, err := f.svc.IssueBill(f.owner, &restaurantv1.IssueBillRequest{CommandId: uuid.NewString(), OrderClientId: ocid.String(), Payments: []*restaurantv1.PaymentInput{cash(30000)}}); err != nil {
		t.Fatalf("issue: %v", err)
	}
	if got := orderStatus(t, f.owner, f.pool, ocid); got != "BILLED" {
		t.Fatalf("status = %s, want BILLED", got)
	}
	ord := orders.NewService(f.pool, nil, f.authSvc, zap.NewNop())
	res, err := ord.MarkServed(f.owner, &restaurantv1.MarkServedRequest{CommandId: uuid.NewString(), OrderClientId: ocid.String(), ItemIds: ids(items)})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if res.Order.Status != restaurantv1.OrderStatus_ORDER_STATUS_CLOSED {
		t.Fatalf("after hand-over status = %v, want CLOSED", res.Order.Status)
	}
}

func TestConcurrentIssueBillBillsOnce(t *testing.T) {
	f := newBillFixture(t)
	f.openShift(f.owner)
	for round := 0; round < raceRounds; round++ {
		ocid, items := seedOrder(t, f.owner, f.pool, 3)
		bill := func() error {
			_, err := f.svc.IssueBill(f.owner, &restaurantv1.IssueBillRequest{CommandId: uuid.NewString(), OrderClientId: ocid.String(), Payments: []*restaurantv1.PaymentInput{cash(45000)}})
			return err
		}
		ea, eb := race(bill, bill)
		exactlyOne(t, ea, eb)
		assertBilledOnce(t, f.owner, f.pool, ocid, items, 1)
		if t.Failed() {
			t.Fatalf("failed in round %d", round)
		}
	}
	if n := f.count(`SELECT count(*) FROM payments`); n != raceRounds {
		t.Fatalf("payments = %d, want %d", n, raceRounds)
	}
}

// --- CounterSale ---

func (f *billFixture) line(qty int32) *restaurantv1.NewItem {
	return &restaurantv1.NewItem{ClientId: uuid.NewString(), MenuItemId: f.momo.String(), Quantity: qty}
}

func TestCounterSaleCreatesPaidTakeaway(t *testing.T) {
	f := newBillFixture(t)
	cashier := f.as(auth.RoleCashier)
	shift := f.openShift(cashier)
	ocid := uuid.NewString()

	inv, err := f.svc.CounterSale(cashier, &restaurantv1.CounterSaleRequest{
		CommandId: uuid.NewString(), OrderClientId: ocid, Label: "Sita",
		Lines:     []*restaurantv1.NewItem{f.line(1), f.line(2)},
		Discount:  &restaurantv1.DiscountInput{Type: restaurantv1.DiscountType_DISCOUNT_TYPE_AMOUNT, Value: 5000, Reason: "loyal"},
		BuyerName: "Sita", Payments: []*restaurantv1.PaymentInput{cash(70000)},
	})
	if err != nil {
		t.Fatalf("counter sale: %v", err)
	}
	if inv.SubtotalPaisa != 75000 || inv.TotalPaisa != 70000 || len(inv.Lines) != 2 || len(inv.Payments) != 1 || inv.BuyerName != "Sita" {
		t.Fatalf("invoice subtotal=%d total=%d lines=%d payments=%d buyer=%q", inv.SubtotalPaisa, inv.TotalPaisa, len(inv.Lines), len(inv.Payments), inv.BuyerName)
	}
	var typ, status string
	if err := f.pool.QueryRow(context.Background(), `SELECT type, status FROM orders WHERE client_id = $1 AND id = $2`, ocid, inv.OrderId).Scan(&typ, &status); err != nil {
		t.Fatalf("order: %v", err)
	}
	if typ != "TAKEAWAY" || status != "BILLED" {
		t.Fatalf("order type=%s status=%s, want TAKEAWAY/BILLED (food not handed over)", typ, status)
	}
	if n := f.count(`SELECT count(*) FROM order_items WHERE order_id = $1 AND status = 'SENT' AND invoice_id = $2`, inv.OrderId, inv.Id); n != 2 {
		t.Fatalf("sent+billed lines = %d, want 2", n)
	}
	if n := f.count(`SELECT count(*) FROM payments WHERE shift_id = $1`, shift); n != 1 {
		t.Fatalf("payments on shift = %d, want 1", n)
	}
	// Kitchen marks it READY: a takeaway is handed over, so it closes.
	f.exec(`UPDATE order_items SET status = 'READY' WHERE order_id = $1`, inv.OrderId)
	var closed bool
	if err := dbxTx(f.pool, func(q *sqlc.Queries) error {
		var e error
		closed, e = orders.CloseIfDone(context.Background(), q, uuid.MustParse(inv.OrderId))
		return e
	}); err != nil || !closed {
		t.Fatalf("close after READY: closed=%v err=%v", closed, err)
	}

	// Generated order client id when blank.
	inv2, err := f.svc.CounterSale(cashier, &restaurantv1.CounterSaleRequest{CommandId: uuid.NewString(), Lines: []*restaurantv1.NewItem{f.line(1)}, Payments: []*restaurantv1.PaymentInput{cash(25000)}})
	if err != nil || inv2.OrderId == "" || inv2.OrderId == inv.OrderId {
		t.Fatalf("counter sale without client id: %v", err)
	}
}

func dbxTx(pool *pgxpool.Pool, fn func(q *sqlc.Queries) error) error {
	return dbx.WithTx(context.Background(), pool, sqlc.New(pool), fn)
}

func TestCounterSaleBadLineRejectsWholeSale(t *testing.T) {
	f := newBillFixture(t)
	f.openShift(f.owner)
	f.exec(`UPDATE menu_items SET is_86d = true WHERE id = $1`, f.momo)
	bad := &restaurantv1.NewItem{ClientId: uuid.NewString(), MenuItemId: uuid.NewString(), Quantity: 1}
	for name, lines := range map[string][]*restaurantv1.NewItem{
		"unknown item": {bad},
		"86d item":     {f.line(1)},
		"none":         nil,
	} {
		_, err := f.svc.CounterSale(f.owner, &restaurantv1.CounterSaleRequest{CommandId: uuid.NewString(), Lines: lines, Payments: []*restaurantv1.PaymentInput{cash(25000)}})
		if err == nil {
			t.Fatalf("%s: sale accepted", name)
		}
		wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID, "lines")
	}
	f.exec(`UPDATE menu_items SET is_86d = false WHERE id = $1`, f.momo)
	_, err := f.svc.CounterSale(f.owner, &restaurantv1.CounterSaleRequest{CommandId: uuid.NewString(), Lines: []*restaurantv1.NewItem{f.line(1), bad}, Payments: []*restaurantv1.PaymentInput{cash(25000)}})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID, "lines")
	for _, tbl := range []string{"orders", "order_items", "invoices", "payments", "invoice_counters"} {
		if n := f.count(`SELECT count(*) FROM ` + tbl); n != 0 {
			t.Fatalf("%s has %d rows after rejected sales, want 0", tbl, n)
		}
	}
}

func TestCounterSaleNeedsShiftAndExactPayment(t *testing.T) {
	f := newBillFixture(t)
	_, err := f.svc.CounterSale(f.owner, &restaurantv1.CounterSaleRequest{CommandId: uuid.NewString(), Lines: []*restaurantv1.NewItem{f.line(1)}, Payments: []*restaurantv1.PaymentInput{cash(25000)}})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_SHIFT_NOT_OPEN, "")
	f.openShift(f.owner)
	_, err = f.svc.CounterSale(f.owner, &restaurantv1.CounterSaleRequest{CommandId: uuid.NewString(), Lines: []*restaurantv1.NewItem{f.line(1)}, Payments: []*restaurantv1.PaymentInput{cash(20000)}})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID, "payments")
	if n := f.count(`SELECT count(*) FROM orders`); n != 0 {
		t.Fatalf("orders = %d, want 0", n)
	}
}

func TestCounterSaleReplayReturnsSameInvoice(t *testing.T) {
	f := newBillFixture(t)
	f.openShift(f.owner)
	req := &restaurantv1.CounterSaleRequest{CommandId: uuid.NewString(), Lines: []*restaurantv1.NewItem{f.line(1)}, Payments: []*restaurantv1.PaymentInput{cash(25000)}}
	a, err := f.svc.CounterSale(f.owner, req)
	if err != nil {
		t.Fatalf("sale: %v", err)
	}
	b, err := f.svc.CounterSale(f.owner, req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if a.Id != b.Id {
		t.Fatalf("replay returned %s, want %s", b.Id, a.Id)
	}
	if n := f.count(`SELECT count(*) FROM orders`); n != 1 {
		t.Fatalf("orders = %d, want 1", n)
	}
	if n := f.count(`SELECT count(*) FROM payments`); n != 1 {
		t.Fatalf("payments = %d, want 1", n)
	}
}

// --- ListInvoices ---

func TestListInvoicesByOrderAndDay(t *testing.T) {
	f := newBillFixture(t)
	f.openShift(f.owner)
	ocid, items := seedOrder(t, f.owner, f.pool, 2)
	first, err := f.svc.IssueBill(f.owner, &restaurantv1.IssueBillRequest{CommandId: uuid.NewString(), OrderClientId: ocid.String(), ItemIds: ids(items[:1]), Payments: []*restaurantv1.PaymentInput{cash(15000)}})
	if err != nil {
		t.Fatalf("bill 1: %v", err)
	}
	second, err := f.svc.IssueBill(f.owner, &restaurantv1.IssueBillRequest{CommandId: uuid.NewString(), OrderClientId: ocid.String(), ItemIds: ids(items[1:]), Payments: []*restaurantv1.PaymentInput{cash(15000)}})
	if err != nil {
		t.Fatalf("bill 2: %v", err)
	}
	if _, err := f.svc.CancelInvoice(f.owner, &restaurantv1.CancelInvoiceRequest{
		CommandId: uuid.NewString(), InvoiceId: first.Id, Reason: "wrong table",
		Approval: &restaurantv1.ManagerApproval{ManagerUserId: f.ownerID.String(), Pin: "1111"},
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	otherOcid, _ := seedOrder(t, f.owner, f.pool, 1)
	other, err := f.svc.IssueBill(f.owner, &restaurantv1.IssueBillRequest{CommandId: uuid.NewString(), OrderClientId: otherOcid.String(), Payments: []*restaurantv1.PaymentInput{cash(15000)}})
	if err != nil {
		t.Fatalf("bill other: %v", err)
	}

	byOrder, err := f.svc.ListInvoices(f.owner, &restaurantv1.ListInvoicesRequest{Filter: &restaurantv1.ListInvoicesRequest_OrderClientId{OrderClientId: ocid.String()}})
	if err != nil {
		t.Fatalf("list by order: %v", err)
	}
	if len(byOrder.Invoices) != 2 || byOrder.Invoices[0].Id != second.Id || byOrder.Invoices[1].Id != first.Id {
		t.Fatalf("by order = %v, want [second, first]", invIDs(byOrder.Invoices))
	}
	if byOrder.Invoices[1].Status != restaurantv1.InvoiceStatus_INVOICE_STATUS_CANCELLED || len(byOrder.Invoices[0].Lines) != 1 || len(byOrder.Invoices[0].Payments) != 1 {
		t.Fatalf("cancelled status / lines / payments not carried: %+v", byOrder.Invoices)
	}

	today, err := bsdate.FromAD(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	byBs, err := f.svc.ListInvoices(f.owner, &restaurantv1.ListInvoicesRequest{Filter: &restaurantv1.ListInvoicesRequest_BsDate{BsDate: today.String()}})
	if err != nil {
		t.Fatalf("list by bs date: %v", err)
	}
	if got := invIDs(byBs.Invoices); len(got) != 3 || got[0] != other.Id || got[2] != first.Id {
		t.Fatalf("by bs date = %v, want newest first [other, second, first]", got)
	}
	now := time.Now()
	byAd, err := f.svc.ListInvoices(f.owner, &restaurantv1.ListInvoicesRequest{Filter: &restaurantv1.ListInvoicesRequest_AdRange{AdRange: &restaurantv1.InvoiceDayRange{FromUnix: now.Add(-time.Hour).Unix(), ToUnix: now.Add(time.Hour).Unix()}}})
	if err != nil {
		t.Fatalf("list by ad range: %v", err)
	}
	if len(byAd.Invoices) != 3 {
		t.Fatalf("by ad range = %d invoices, want 3", len(byAd.Invoices))
	}
	empty, err := f.svc.ListInvoices(f.owner, &restaurantv1.ListInvoicesRequest{Filter: &restaurantv1.ListInvoicesRequest_AdRange{AdRange: &restaurantv1.InvoiceDayRange{FromUnix: now.Add(-48 * time.Hour).Unix(), ToUnix: now.Add(-24 * time.Hour).Unix()}}})
	if err != nil || len(empty.Invoices) != 0 {
		t.Fatalf("yesterday: %v %d", err, len(empty.Invoices))
	}
	_, err = f.svc.ListInvoices(f.owner, &restaurantv1.ListInvoicesRequest{Filter: &restaurantv1.ListInvoicesRequest_BsDate{BsDate: "2081-13-40"}})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID, "bs_date")
}

func invIDs(invs []*restaurantv1.Invoice) []string {
	out := make([]string, len(invs))
	for i, inv := range invs {
		out[i] = inv.Id
	}
	return out
}

// --- roles ---

func TestBillingPayRPCsRequireBillingRole(t *testing.T) {
	f := newBillFixture(t)
	ocid, _ := seedOrder(t, f.owner, f.pool, 1)
	for _, role := range []string{auth.RoleWaiter, auth.RoleKitchen} {
		ctx := f.as(role)
		f.openShift(ctx)
		_, err := f.svc.IssueBill(ctx, &restaurantv1.IssueBillRequest{CommandId: uuid.NewString(), OrderClientId: ocid.String(), Payments: []*restaurantv1.PaymentInput{cash(15000)}})
		wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED, "")
		_, err = f.svc.CounterSale(ctx, &restaurantv1.CounterSaleRequest{CommandId: uuid.NewString(), Lines: []*restaurantv1.NewItem{f.line(1)}, Payments: []*restaurantv1.PaymentInput{cash(25000)}})
		wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED, "")
		_, err = f.svc.ListInvoices(ctx, &restaurantv1.ListInvoicesRequest{Filter: &restaurantv1.ListInvoicesRequest_OrderClientId{OrderClientId: ocid.String()}})
		wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED, "")
	}
	for _, role := range []string{auth.RoleManager, auth.RoleCashier} {
		ctx := f.as(role)
		if _, err := f.svc.ListInvoices(ctx, &restaurantv1.ListInvoicesRequest{Filter: &restaurantv1.ListInvoicesRequest_OrderClientId{OrderClientId: ocid.String()}}); err != nil {
			t.Fatalf("%s list: %v", role, err)
		}
	}
	if n := f.count(`SELECT count(*) FROM invoices`); n != 0 {
		t.Fatalf("invoices = %d, want 0", n)
	}
}
