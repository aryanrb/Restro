package purchases_test

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
	"github.com/oneaccord/restro/api/internal/alerts"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/inventory"
	"github.com/oneaccord/restro/api/internal/purchases"
	"github.com/oneaccord/restro/api/internal/shift"
)

const pcs = int64(1000) // 1 pcs in milli

type fixture struct {
	t       *testing.T
	pool    *pgxpool.Pool
	q       *sqlc.Queries
	authSvc *auth.Service
	inv     *inventory.Service
	svc     *purchases.Service
	shifts  *shift.Service

	owner, manager, cashier, waiter context.Context
	ownerID, managerID, cashierID   uuid.UUID

	dairy, market string // VAT-registered with PAN / no PAN
	coke          string // tracked pcs, crate of 24, opening 24 at 20 paisa
	seq           int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run purchases integration tests")
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
	if _, err := pool.Exec(ctx, `TRUNCATE users, sessions, devices, audit_log, processed_commands, shifts,
		inventory_items, stock_movements, suppliers, purchases, purchase_lines, purchase_returns,
		purchase_return_lines, supplier_payments, item_purchase_settings RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	q := sqlc.New(pool)
	authSvc := auth.NewService(pool, zap.NewNop())
	f := &fixture{
		t: t, pool: pool, q: q, authSvc: authSvc,
		inv:    inventory.NewService(pool, nil, authSvc, zap.NewNop()),
		svc:    purchases.NewService(pool, nil, authSvc, zap.NewNop()),
		shifts: shift.NewService(pool, nil, zap.NewNop()),
	}
	f.owner, f.ownerID = f.user("Owner", auth.RoleOwner, "1111")
	f.manager, f.managerID = f.user("Sita Gurung", auth.RoleManager, "2222")
	f.cashier, f.cashierID = f.user("Hari Thapa", auth.RoleCashier, "3333")
	f.waiter, _ = f.user("Gita", auth.RoleWaiter, "4444")

	f.dairy = f.supplier(f.owner, &restaurantv1.SupplierInput{Name: "Himalayan Dairy", Pan: "301234567", VatRegistered: true, Phone: "9841000000"}, 0).Id
	f.market = f.supplier(f.owner, &restaurantv1.SupplierInput{Name: "Kalimati vegetables"}, 0).Id
	created, err := f.inv.SaveInventoryItem(f.owner, &restaurantv1.SaveInventoryItemRequest{Input: &restaurantv1.InventoryItemInput{
		Name: &restaurantv1.LocalText{En: "Coke 250ml"}, Unit: restaurantv1.StockUnit_STOCK_UNIT_PCS, LowStockMilli: 48 * pcs, Active: true,
		UnitCostE4: 200_000, // 20 paisa per pcs
		Packs:      []*restaurantv1.StockPack{{Name: &restaurantv1.LocalText{En: "Crate"}, SizeMilli: 24 * pcs}},
	}})
	f.must(err)
	opening := 24 * pcs
	tracked, err := f.inv.SetTracked(f.owner, &restaurantv1.SetTrackedRequest{Id: created.Id, Tracked: true, OpeningMilli: &opening})
	f.must(err)
	f.coke = tracked.Id
	return f
}

func (f *fixture) must(err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) user(name, role, pin string) (context.Context, uuid.UUID) {
	f.t.Helper()
	ctx := context.Background()
	pw, ph := hashed(f.t, "secret123"), hashed(f.t, pin)
	username := "u-" + uuid.NewString()[:8]
	u, err := f.q.CreateUser(ctx, sqlc.CreateUserParams{
		Name: name, Role: role, Username: dbx.Text(username), PasswordHash: dbx.Text(pw), PinHash: dbx.Text(ph), Language: "en", Active: true,
	})
	f.must(err)
	_, token, err := f.authSvc.PasswordLogin(ctx, username, "secret123")
	f.must(err)
	req := connect.NewRequest(&restaurantv1.ListOpenOrdersRequest{})
	req.Header().Set("Authorization", "Bearer "+token)
	var out context.Context
	_, _ = f.authSvc.Interceptor()(func(c context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		out = c
		return nil, nil
	})(ctx, req)
	if !auth.ActorFrom(out).LoggedIn() {
		f.t.Fatalf("actor not resolved")
	}
	return out, u.ID
}

var hashes = map[string]string{}

func hashed(t *testing.T, secret string) string {
	t.Helper()
	if h, ok := hashes[secret]; ok {
		return h
	}
	h, err := auth.HashSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	hashes[secret] = h
	return h
}

func (f *fixture) supplier(ctx context.Context, in *restaurantv1.SupplierInput, opening int64) *restaurantv1.Supplier {
	f.t.Helper()
	s, err := f.svc.CreateSupplier(ctx, &restaurantv1.CreateSupplierRequest{Input: in, OpeningBalancePaisa: opening})
	f.must(err)
	return s
}

func (f *fixture) reqID() string { return uuid.NewString() }

func today() string { return time.Now().In(time.FixedZone("NPT", 5*3600+45*60)).Format(time.DateOnly) }
func daysAgo(n int) string {
	return time.Now().In(time.FixedZone("NPT", 5*3600+45*60)).AddDate(0, 0, -n).Format(time.DateOnly)
}

func stockLine(item string, qtyMilli int64, unitKey string, cost int64, vat bool) *restaurantv1.PurchaseLineInput {
	return &restaurantv1.PurchaseLineInput{InventoryItemId: item, QuantityMilli: qtyMilli, UnitKey: unitKey, UnitCostPaisa: cost, VatApplies: vat}
}

func nonStock(desc string, cost int64) *restaurantv1.PurchaseLineInput {
	return &restaurantv1.PurchaseLineInput{Description: desc, QuantityMilli: 1000, UnitCostPaisa: cost}
}

func (f *fixture) record(ctx context.Context, req *restaurantv1.RecordPurchaseRequest) (*restaurantv1.Purchase, error) {
	if req.RequestId == "" {
		req.RequestId = f.reqID()
	}
	if req.BillDate == "" {
		req.BillDate = today()
	}
	return f.svc.RecordPurchase(ctx, req)
}

func (f *fixture) pay(ctx context.Context, supplier string, amount int64, method restaurantv1.SupplierPaymentMethod, approval *restaurantv1.ManagerApproval) (*restaurantv1.SupplierPayment, error) {
	return f.svc.PaySupplier(ctx, &restaurantv1.PaySupplierRequest{SupplierId: supplier, AmountPaisa: amount, Method: method, Approval: approval, RequestId: f.reqID()})
}

func (f *fixture) item(id string) *restaurantv1.InventoryItem {
	f.t.Helper()
	list, err := f.inv.ListInventoryItems(f.owner, true)
	f.must(err)
	for _, it := range list {
		if it.Id == id {
			return it
		}
	}
	f.t.Fatalf("item %s not listed", id)
	return nil
}

func (f *fixture) balance(id string) int64 {
	f.t.Helper()
	s, err := f.svc.GetSupplier(f.owner, id)
	f.must(err)
	return s.BalancePaisa
}

func (f *fixture) purchase(id string) *restaurantv1.Purchase {
	f.t.Helper()
	p, err := f.svc.GetPurchase(f.owner, id)
	f.must(err)
	return p
}

func reasonOf(t *testing.T, err error) restaurantv1.ErrorReason {
	t.Helper()
	var ce *connecterr.Error
	if !errors.As(err, &ce) {
		t.Fatalf("expected a service error, got %v", err)
	}
	return ce.Reason
}

func (f *fixture) lastAudit() (action string, approver uuid.UUID) {
	f.t.Helper()
	var ap *uuid.UUID
	f.must(f.pool.QueryRow(context.Background(), `SELECT action, approver_user_id FROM audit_log ORDER BY id DESC LIMIT 1`).Scan(&action, &ap))
	if ap != nil {
		approver = *ap
	}
	return action, approver
}

func (f *fixture) movementTypes(id string) map[restaurantv1.StockMovementType]int64 {
	f.t.Helper()
	list, err := f.inv.ListMovements(f.owner, &restaurantv1.ListMovementsRequest{InventoryItemId: id})
	f.must(err)
	out := map[restaurantv1.StockMovementType]int64{}
	for _, m := range list {
		out[m.Type] += m.QuantityMilli
	}
	return out
}

// --- record purchase → stock + cost ---

func TestRecordPurchaseAddsStockAndMovesAverageCost(t *testing.T) {
	f := newFixture(t)
	// 2 crates (48 pcs) at Rs 600 a crate on a VAT bill: 120000 net + 15600 VAT, plus gas (non-stock, exempt).
	p, err := f.record(f.cashier, &restaurantv1.RecordPurchaseRequest{
		SupplierId: f.dairy, SupplierBillNo: "2451", IsVatBill: true, VatPaisa: 15600,
		Lines: []*restaurantv1.PurchaseLineInput{stockLine(f.coke, 2000, "pack:Crate", 60000, true), nonStock("Gas", 180000)},
	})
	f.must(err)
	if p.TaxablePaisa != 120000 || p.ExemptPaisa != 180000 || p.VatPaisa != 15600 || p.TotalPaisa != 315600 {
		t.Fatalf("totals: %+v", p)
	}
	if p.Status != restaurantv1.PurchaseStatus_PURCHASE_STATUS_RECORDED || p.DisplayNumber == "" || p.OwedPaisa != 315600 {
		t.Fatalf("purchase: %s %s owed %d", p.Status, p.DisplayNumber, p.OwedPaisa)
	}
	// Claimable VAT stays out of cost: 120000 / 48 = 2500 paisa per pcs.
	if l := p.Lines[0]; l.FactorMilli != 24*pcs || l.StockUnitCostE4 != 25_000_000 || l.VatPaisa != 15600 {
		t.Fatalf("line: %+v", l)
	}
	it := f.item(f.coke)
	if it.OnHandMilli != 72*pcs {
		t.Fatalf("on hand %d", it.OnHandMilli)
	}
	// (24 × 20 + 48 × 2500) / 72 = 1673.3333
	if it.GetUnitCostE4() != 16_733_333 {
		t.Fatalf("average cost %d", it.GetUnitCostE4())
	}
	if m := f.movementTypes(f.coke); m[restaurantv1.StockMovementType_STOCK_MOVEMENT_TYPE_PURCHASE] != 48*pcs {
		t.Fatalf("movements %v", m)
	}
	if a, _ := f.lastAudit(); a != "PURCHASE_RECORDED" {
		t.Fatalf("audit %s", a)
	}
	if f.balance(f.dairy) != 315600 {
		t.Fatalf("balance %d", f.balance(f.dairy))
	}

	// Idempotent on request_id; the same supplier bill can't be entered twice.
	again, err := f.svc.RecordPurchase(f.cashier, &restaurantv1.RecordPurchaseRequest{SupplierId: f.dairy, RequestId: "x"})
	if err == nil || again != nil {
		t.Fatal("expected a validation error for an empty bill")
	}
	_, err = f.record(f.cashier, &restaurantv1.RecordPurchaseRequest{SupplierId: f.dairy, SupplierBillNo: "2451",
		Lines: []*restaurantv1.PurchaseLineInput{nonStock("Gas", 100)}})
	if reasonOf(t, err) != restaurantv1.ErrorReason_ERROR_REASON_DUPLICATE_SUPPLIER_BILL {
		t.Fatalf("duplicate bill: %v", err)
	}
	// A supplier without PAN can't give a VAT bill.
	_, err = f.record(f.cashier, &restaurantv1.RecordPurchaseRequest{SupplierId: f.market, SupplierBillNo: "9", IsVatBill: true,
		Lines: []*restaurantv1.PurchaseLineInput{nonStock("Onions", 100)}})
	if reasonOf(t, err) != restaurantv1.ErrorReason_ERROR_REASON_NOT_A_VAT_BILL {
		t.Fatalf("vat bill: %v", err)
	}
	req := &restaurantv1.RecordPurchaseRequest{SupplierId: f.market, RequestId: f.reqID(), BillDate: today(),
		Lines: []*restaurantv1.PurchaseLineInput{stockLine(f.coke, 1000, "pcs", 30, false)}}
	first, err := f.svc.RecordPurchase(f.cashier, req)
	f.must(err)
	second, err := f.svc.RecordPurchase(f.cashier, req)
	f.must(err)
	if first.Id != second.Id || f.item(f.coke).OnHandMilli != 73*pcs {
		t.Fatal("record is not idempotent")
	}
}

// --- payable allocation (oldest first) ---

func TestPaymentsSettleOldestBillFirst(t *testing.T) {
	f := newFixture(t)
	old, err := f.record(f.owner, &restaurantv1.RecordPurchaseRequest{SupplierId: f.market, BillDate: daysAgo(3), Lines: []*restaurantv1.PurchaseLineInput{nonStock("Onions", 100000)}})
	f.must(err)
	newer, err := f.record(f.owner, &restaurantv1.RecordPurchaseRequest{SupplierId: f.market, Lines: []*restaurantv1.PurchaseLineInput{nonStock("Potatoes", 200000)}})
	f.must(err)
	p, err := f.pay(f.owner, f.market, 150000, restaurantv1.SupplierPaymentMethod_SUPPLIER_PAYMENT_METHOD_BANK_QR, nil)
	f.must(err)
	if p.BalanceAfterPaisa != 150000 || p.ShiftId != "" || p.DisplayNumber == "" {
		t.Fatalf("payment: %+v", p)
	}
	if o, n := f.purchase(old.Id).OwedPaisa, f.purchase(newer.Id).OwedPaisa; o != 0 || n != 150000 {
		t.Fatalf("owed: old %d newer %d", o, n)
	}
	_, err = f.pay(f.owner, f.market, 150001, restaurantv1.SupplierPaymentMethod_SUPPLIER_PAYMENT_METHOD_OWNER_CASH, nil)
	if reasonOf(t, err) != restaurantv1.ErrorReason_ERROR_REASON_PAYMENT_TOO_HIGH {
		t.Fatalf("over-payment: %v", err)
	}
	payables, err := f.svc.Payables(f.owner)
	f.must(err)
	if len(payables) != 1 || payables[0].BalancePaisa != 150000 || payables[0].OldestUnpaid != today() {
		t.Fatalf("payables: %+v", payables)
	}
	// Voiding the payment puts the debt back on the oldest bill.
	_, err = f.svc.VoidSupplierPayment(f.owner, &restaurantv1.VoidSupplierPaymentRequest{Id: p.Id, Reason: "wrong supplier"})
	f.must(err)
	if f.balance(f.market) != 300000 || f.purchase(old.Id).OwedPaisa != 100000 {
		t.Fatal("void did not restore the debt")
	}
}

// --- till paid-outs in the shift ---

func TestTillPaymentIsAPaidOutInThePayersShift(t *testing.T) {
	f := newFixture(t)
	t.Cleanup(alerts.Install(nil))
	_, err := f.pool.Exec(context.Background(), `TRUNCATE alerts CASCADE`)
	f.must(err)
	_, err = f.record(f.owner, &restaurantv1.RecordPurchaseRequest{SupplierId: f.market, Lines: []*restaurantv1.PurchaseLineInput{nonStock("Vegetables", 2_000_000)}})
	f.must(err)
	till := restaurantv1.SupplierPaymentMethod_SUPPLIER_PAYMENT_METHOD_TILL

	_, err = f.pay(f.cashier, f.market, 1000, till, nil)
	if reasonOf(t, err) != restaurantv1.ErrorReason_ERROR_REASON_SHIFT_NOT_OPEN {
		t.Fatalf("no shift: %v", err)
	}
	sh, err := f.shifts.OpenShift(f.cashier, 1_000_000, "")
	f.must(err)
	p, err := f.pay(f.cashier, f.market, 300_000, till, nil)
	f.must(err)
	if p.ShiftId != sh.Id {
		t.Fatalf("shift %s", p.ShiftId)
	}
	// Over Rs 5,000 a cashier needs a manager PIN.
	_, err = f.pay(f.cashier, f.market, 600_000, till, nil)
	if reasonOf(t, err) != restaurantv1.ErrorReason_ERROR_REASON_MANAGER_APPROVAL_REQUIRED {
		t.Fatalf("limit: %v", err)
	}
	big, err := f.pay(f.cashier, f.market, 600_000, till, &restaurantv1.ManagerApproval{ManagerUserId: f.managerID.String(), Pin: "2222"})
	f.must(err)
	if big.ApproverId != f.managerID.String() {
		t.Fatalf("approver %q", big.ApproverId)
	}
	if a, ap := f.lastAudit(); a != "SUPPLIER_PAID" || ap != f.managerID {
		t.Fatalf("audit %s %s", a, ap)
	}
	// LARGE_PAID_OUT: the Rs 6,000 payment (single) and Rs 9,000 in the shift is under the Rs 10,000 shift total.
	var single, inShift int
	f.must(f.pool.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE text_params->>'variant' = 'single'),
		count(*) FILTER (WHERE text_params->>'variant' = 'shift') FROM alerts WHERE rule = 'LARGE_PAID_OUT'`).Scan(&single, &inShift))
	if single != 1 || inShift != 0 {
		t.Fatalf("LARGE_PAID_OUT alerts: single %d shift %d", single, inShift)
	}
	rep, err := f.shifts.GetShiftReport(f.cashier, sh.Id)
	f.must(err)
	if rep.PaidOutTotalPaisa != 900_000 || len(rep.PaidOuts) != 2 || rep.ExpectedCashPaisa != 100_000 {
		t.Fatalf("report: paid-outs %d (%d) expected %d", rep.PaidOutTotalPaisa, len(rep.PaidOuts), rep.ExpectedCashPaisa)
	}
	// A cashier may only pay from the till.
	_, err = f.pay(f.cashier, f.market, 100, restaurantv1.SupplierPaymentMethod_SUPPLIER_PAYMENT_METHOD_BANK_QR, nil)
	if reasonOf(t, err) != restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED {
		t.Fatalf("cashier bank: %v", err)
	}

	// Voided after its shift closed: still counted there, and the cash goes back into the voider's open shift.
	_, err = f.shifts.CloseShift(f.cashier, sh.Id, 100_000, "")
	f.must(err)
	_, err = f.svc.VoidSupplierPayment(f.manager, &restaurantv1.VoidSupplierPaymentRequest{Id: p.Id, Reason: "returned"})
	if reasonOf(t, err) != restaurantv1.ErrorReason_ERROR_REASON_SHIFT_NOT_OPEN {
		t.Fatalf("void without shift: %v", err)
	}
	msh, err := f.shifts.OpenShift(f.manager, 0, "")
	f.must(err)
	voided, err := f.svc.VoidSupplierPayment(f.manager, &restaurantv1.VoidSupplierPaymentRequest{Id: p.Id, Reason: "returned"})
	f.must(err)
	if voided.VoidShiftId != msh.Id {
		t.Fatalf("void shift %q", voided.VoidShiftId)
	}
	closed, err := f.shifts.GetShiftReport(f.owner, sh.Id)
	f.must(err)
	mine, err := f.shifts.GetShiftReport(f.owner, msh.Id)
	f.must(err)
	if closed.PaidOutTotalPaisa != 900_000 || mine.PaidOutTotalPaisa != -300_000 || mine.ExpectedCashPaisa != 300_000 {
		t.Fatalf("after void: closed %d, voider %d / expected %d", closed.PaidOutTotalPaisa, mine.PaidOutTotalPaisa, mine.ExpectedCashPaisa)
	}
}

// --- returns ---

func TestReturnReducesStockAndOwedAndBlocksVoid(t *testing.T) {
	f := newFixture(t)
	p, err := f.record(f.owner, &restaurantv1.RecordPurchaseRequest{SupplierId: f.market,
		Lines: []*restaurantv1.PurchaseLineInput{stockLine(f.coke, 2000, "pack:Crate", 60000, false)}})
	f.must(err)
	costBefore := f.item(f.coke).GetUnitCostE4()
	line := p.Lines[0].Id
	ret, err := f.svc.ReturnPurchase(f.manager, &restaurantv1.ReturnPurchaseRequest{PurchaseId: p.Id, NoteNo: "CN-1", Reason: "broken",
		Lines: []*restaurantv1.PurchaseReturnLine{{PurchaseLineId: line, QuantityMilli: 1000}}, RequestId: f.reqID()})
	f.must(err)
	if ret.TotalPaisa != 60000 || ret.ExemptPaisa != 60000 {
		t.Fatalf("return: %+v", ret)
	}
	it := f.item(f.coke)
	if it.OnHandMilli != 48*pcs || it.GetUnitCostE4() != costBefore {
		t.Fatalf("after return: on hand %d cost %d (was %d)", it.OnHandMilli, it.GetUnitCostE4(), costBefore)
	}
	if m := f.movementTypes(f.coke); m[restaurantv1.StockMovementType_STOCK_MOVEMENT_TYPE_PURCHASE_RETURN] != -24*pcs {
		t.Fatalf("movements %v", m)
	}
	got := f.purchase(p.Id)
	if got.OwedPaisa != 60000 || got.ReturnedTotalPaisa != 60000 || got.Lines[0].ReturnedMilli != 1000 || f.balance(f.market) != 60000 {
		t.Fatalf("after return: owed %d returned %d", got.OwedPaisa, got.ReturnedTotalPaisa)
	}
	_, err = f.svc.ReturnPurchase(f.manager, &restaurantv1.ReturnPurchaseRequest{PurchaseId: p.Id, Reason: "more",
		Lines: []*restaurantv1.PurchaseReturnLine{{PurchaseLineId: line, QuantityMilli: 1001}}, RequestId: f.reqID()})
	if reasonOf(t, err) != restaurantv1.ErrorReason_ERROR_REASON_RETURN_TOO_HIGH {
		t.Fatalf("over-return: %v", err)
	}
	_, err = f.svc.VoidPurchase(f.manager, &restaurantv1.VoidPurchaseRequest{Id: p.Id, Reason: "mistake"})
	if reasonOf(t, err) != restaurantv1.ErrorReason_ERROR_REASON_IN_USE {
		t.Fatalf("void with returns: %v", err)
	}

	// A bill without returns voids: stock goes back out and the debt is dropped.
	q, err := f.record(f.owner, &restaurantv1.RecordPurchaseRequest{SupplierId: f.market,
		Lines: []*restaurantv1.PurchaseLineInput{stockLine(f.coke, 5000, "pcs", 30, false)}})
	f.must(err)
	_, err = f.svc.VoidPurchase(f.manager, &restaurantv1.VoidPurchaseRequest{Id: q.Id, Reason: "entered twice"})
	f.must(err)
	if f.item(f.coke).OnHandMilli != 48*pcs || f.balance(f.market) != 60000 {
		t.Fatal("void did not reverse stock and debt")
	}
}

// --- roles ---

func TestRoles(t *testing.T) {
	f := newFixture(t)
	notAllowed := func(name string, err error) {
		t.Helper()
		if reasonOf(t, err) != restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED {
			t.Fatalf("%s: %v", name, err)
		}
	}
	_, err := f.svc.ListSuppliers(f.waiter, &restaurantv1.ListSuppliersRequest{})
	notAllowed("waiter lists suppliers", err)
	_, err = f.record(f.waiter, &restaurantv1.RecordPurchaseRequest{SupplierId: f.market, Lines: []*restaurantv1.PurchaseLineInput{nonStock("x", 1)}})
	notAllowed("waiter records", err)
	_, err = f.svc.CreateSupplier(f.cashier, &restaurantv1.CreateSupplierRequest{Input: &restaurantv1.SupplierInput{Name: "New"}, OpeningBalancePaisa: 100})
	notAllowed("cashier opening balance", err)
	_, err = f.svc.UpdateSupplier(f.cashier, &restaurantv1.UpdateSupplierRequest{Id: f.market, Input: &restaurantv1.SupplierInput{Name: "Renamed"}})
	notAllowed("cashier edits supplier", err)
	p, err := f.record(f.cashier, &restaurantv1.RecordPurchaseRequest{SupplierId: f.market, Lines: []*restaurantv1.PurchaseLineInput{nonStock("x", 100)}})
	f.must(err)
	_, err = f.svc.VoidPurchase(f.cashier, &restaurantv1.VoidPurchaseRequest{Id: p.Id, Reason: "no"})
	notAllowed("cashier voids", err)
	_, err = f.svc.Payables(f.cashier)
	notAllowed("cashier payables", err)
}
