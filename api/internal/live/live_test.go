package live

import (
	"context"
	"errors"
	"os"
	"slices"
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
)

// Integration tests mirroring web/src/lib/api/mock/mock-live-api.test.ts.

type fixture struct {
	t                         *testing.T
	ctx                       context.Context
	pool                      *pgxpool.Pool
	svc                       *Service
	authSvc                   *auth.Service
	sita, hari, gita, bikash  context.Context
	sitaID, hariID, gitaID    uuid.UUID
	kitchen, bar              uuid.UUID
	veg                       uuid.UUID
	t1, t2, t3                uuid.UUID
	o1, o2, o3, o4            uuid.UUID
	vegLine, coke, chow, thuk uuid.UUID
	bill                      uuid.UUID
	billTotal                 int64
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (f *fixture) id(sql string, args ...any) uuid.UUID {
	f.t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(&id); err != nil {
		f.t.Fatalf("query %q: %v", sql, err)
	}
	return id
}

func (f *fixture) as(role, name string) (context.Context, uuid.UUID) {
	f.t.Helper()
	hash, err := auth.HashSecret("secret123")
	if err != nil {
		f.t.Fatal(err)
	}
	username := "u-" + uuid.NewString()[:8]
	u, err := sqlc.New(f.pool).CreateUser(f.ctx, sqlc.CreateUserParams{
		Name: name, Role: role, Username: dbx.Text(username), PasswordHash: dbx.Text(hash), Language: "en", Active: true,
	})
	if err != nil {
		f.t.Fatalf("create user: %v", err)
	}
	_, token, err := f.authSvc.PasswordLogin(f.ctx, username, "secret123")
	if err != nil {
		f.t.Fatalf("login: %v", err)
	}
	req := connect.NewRequest(&restaurantv1.GetLiveSnapshotRequest{})
	req.Header().Set("Authorization", "Bearer "+token)
	var out context.Context
	_, _ = f.authSvc.Interceptor()(func(c context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		out = c
		return nil, nil
	})(f.ctx, req)
	if !auth.ActorFrom(out).LoggedIn() {
		f.t.Fatal("actor not resolved")
	}
	return out, u.ID
}

func (f *fixture) item(order, menu uuid.UUID, name string, price int64, qty int32, station uuid.UUID, status string, sent time.Time) uuid.UUID {
	return f.id(`INSERT INTO order_items (order_id, client_id, menu_item_id, name_en, unit_price_paisa, quantity, station_id, status, sent_at, added_by_user_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`, order, uuid.New(), menu, name, price, qty, station, status, sent, f.gitaID)
}

func (f *fixture) audit(action, entity, entityID string, actor, approver uuid.UUID, reason string) {
	f.exec(`INSERT INTO audit_log (actor_user_id, action, entity, entity_id, reason, approver_user_id) VALUES ($1, $2, $3, $4, $5, $6)`,
		actor, action, entity, entityID, dbx.TextOpt(reason), dbx.UUIDOpt(approver))
}

func minutesAgo(m int) time.Time { return time.Now().Add(-time.Duration(m) * time.Minute) }

// T1: veg momo (kitchen, sent 12 min ago: past yellow) + 2 coke (bar, READY, on
// an issued bill), 3 guests. T2: chowmein (sent 25 min ago: past red) + a thukpa
// voided after cooking started (approved by Sita), 2 guests. T3: seated, nothing
// ordered. Takeaway #7: lassi. Hari's shift opened with a Rs 5,000 float.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run live integration tests")
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
	f := &fixture{t: t, ctx: ctx, pool: pool, svc: NewService(pool, zap.NewNop()), authSvc: auth.NewService(pool, zap.NewNop())}
	f.exec(`TRUNCATE users, sessions, devices, audit_log, processed_commands, orders, order_items, order_item_modifiers,
		invoices, invoice_lines, invoice_prints, invoice_counters, payments, cbms_outbox, shifts, shift_adjustments,
		repayments, customers, menu_items, menu_categories, stations, dining_tables, areas RESTART IDENTITY CASCADE`)
	f.exec(`UPDATE settings SET kds_yellow_minutes = 10, kds_red_minutes = 20`)
	f.sita, f.sitaID = f.as(auth.RoleManager, "Sita Gurung")
	f.hari, f.hariID = f.as(auth.RoleCashier, "Hari Thapa")
	f.gita, f.gitaID = f.as(auth.RoleWaiter, "Gita Rai")
	f.bikash, _ = f.as(auth.RoleKitchen, "Bikash")

	f.kitchen = f.id(`INSERT INTO stations (name_en, name_ne, sort) VALUES ('Kitchen', 'भान्सा', 1) RETURNING id`)
	f.bar = f.id(`INSERT INTO stations (name_en, sort) VALUES ('Bar', 2) RETURNING id`)
	cat := f.id(`INSERT INTO menu_categories (name_en) VALUES ('Food') RETURNING id`)
	menu := func(name string, station uuid.UUID) uuid.UUID {
		return f.id(`INSERT INTO menu_items (category_id, name_en, station_id) VALUES ($1, $2, $3) RETURNING id`, cat, name, station)
	}
	f.veg = menu("Veg Momo", f.kitchen)
	cokeItem, chowItem, thukItem, lassiItem := menu("Coke", f.bar), menu("Chowmein", f.kitchen), menu("Thukpa", f.kitchen), menu("Lassi", f.bar)
	area := f.id(`INSERT INTO areas (name_en, sort) VALUES ('Main', 1) RETURNING id`)
	table := func(name string, sort int, active bool) uuid.UUID {
		return f.id(`INSERT INTO dining_tables (area_id, name, seats, sort, active) VALUES ($1, $2, 4, $3, $4) RETURNING id`, area, name, sort, active)
	}
	f.t1, f.t2, f.t3 = table("T1", 1, true), table("T2", 2, true), table("T3", 3, true)
	table("T4", 4, true)
	table("Old", 5, false) // inactive: not shown

	order := func(typ string, tbl uuid.UUID, label string, guests int) uuid.UUID {
		return f.id(`INSERT INTO orders (client_id, type, table_id, waiter_user_id, guest_count, label, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
			uuid.New(), typ, dbx.UUIDOpt(tbl), f.gitaID, guests, label, minutesAgo(30))
	}
	f.o1 = order("DINE_IN", f.t1, "", 3)
	f.vegLine = f.item(f.o1, f.veg, "Veg Momo", 18_000, 1, f.kitchen, "SENT", minutesAgo(12))
	f.coke = f.item(f.o1, cokeItem, "Coke", 8_000, 2, f.bar, "READY", minutesAgo(5))
	f.o2 = order("DINE_IN", f.t2, "", 2)
	f.chow = f.item(f.o2, chowItem, "Chowmein", 20_000, 1, f.kitchen, "SENT", minutesAgo(25))
	f.thuk = f.item(f.o2, thukItem, "Thukpa", 22_000, 1, f.kitchen, "VOIDED", minutesAgo(3))
	f.exec(`UPDATE order_items SET void_reason = 'Guest left', voided_by_user_id = $2, void_approved_by_user_id = $3 WHERE id = $1`, f.thuk, f.gitaID, f.sitaID)
	f.audit("order.void", "order_item", f.thuk.String(), f.gitaID, f.sitaID, "Guest left")
	f.o3 = order("DINE_IN", f.t3, "", 1)
	f.o4 = order("TAKEAWAY", uuid.Nil, "#7", 0)
	f.item(f.o4, lassiItem, "Lassi", 12_000, 1, f.bar, "SENT", minutesAgo(1))

	shiftID := f.id(`INSERT INTO shifts (cashier_user_id, opening_float_paisa) VALUES ($1, 500000) RETURNING id`, f.hariID)
	f.billTotal = 16_000
	f.bill = f.id(`INSERT INTO invoices (fiscal_year, number, mode, bs_date, bs_year, bs_month, bs_day, order_id,
		subtotal_paisa, discount_paisa, service_charge_paisa, taxable_paisa, vat_paisa, total_paisa, created_by_user_id)
		VALUES ('2083.084', 1, 'ESTIMATE', '2083.08.10', 2083, 8, 10, $1, 16000, 0, 0, 16000, 0, 16000, $2) RETURNING id`, f.o1, f.hariID)
	f.exec(`UPDATE order_items SET invoice_id = $1 WHERE id = $2`, f.bill, f.coke)
	f.exec(`INSERT INTO payments (invoice_id, method, amount_paisa, confirmed_by_user_id, shift_id) VALUES ($1, 'CASH', 16000, $2, $3)`, f.bill, f.hariID, shiftID)
	f.audit("invoice.issue", "invoice", f.bill.String(), f.hariID, uuid.Nil, "") // a plain bill: not an exception
	f.audit("login", "user", f.hariID.String(), f.hariID, uuid.Nil, "")          // not an exception
	return f
}

func reasonOf(err error) restaurantv1.ErrorReason {
	var ce *connecterr.Error
	if errors.As(err, &ce) {
		return ce.Reason
	}
	return restaurantv1.ErrorReason_ERROR_REASON_UNSPECIFIED
}

func TestLiveOwnerAndManagerOnly(t *testing.T) {
	f := newFixture(t)
	for _, ctx := range []context.Context{f.hari, f.gita, f.bikash} {
		if _, err := f.svc.Snapshot(ctx); reasonOf(err) != restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED {
			t.Fatalf("snapshot: %v", err)
		}
		if _, err := f.svc.Order(ctx, f.o1.String()); reasonOf(err) != restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED {
			t.Fatalf("order: %v", err)
		}
	}
}

func TestLiveFloorEveryTableWithItsStateTakeawayApart(t *testing.T) {
	f := newFixture(t)
	snap, err := f.svc.Snapshot(f.sita)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]*restaurantv1.LiveTable{}
	for _, tb := range snap.Tables {
		by[tb.Name] = tb
	}
	if len(snap.Tables) != 4 {
		t.Fatalf("tables %d (inactive tables are not shown)", len(snap.Tables))
	}
	if by["T1"].State != restaurantv1.LiveState_LIVE_STATE_READY || by["T2"].State != restaurantv1.LiveState_LIVE_STATE_ORDERED ||
		by["T3"].State != restaurantv1.LiveState_LIVE_STATE_SEATED || by["T4"].State != restaurantv1.LiveState_LIVE_STATE_FREE {
		t.Fatalf("states T1 %v T2 %v T3 %v T4 %v", by["T1"].State, by["T2"].State, by["T3"].State, by["T4"].State)
	}
	t2 := by["T2"].Orders[0]
	if t2.WaiterName != "Gita Rai" || t2.GuestCount != 2 || t2.RunningTotalPaisa != 20_000 || !t2.Late || t2.GetOldestWaitingMinutes() != 25 || t2.Place != "T2" {
		t.Fatalf("T2 order = %+v", t2)
	}
	t1 := by["T1"].Orders[0]
	if t1.RunningTotalPaisa != 18_000+2*8_000 || t1.BilledTotalPaisa != 16_000 || t1.ItemCounts.Sent != 1 || t1.ItemCounts.Ready != 1 || t1.Late {
		t.Fatalf("T1 order = %+v", t1)
	}
	if len(snap.Takeaway) != 1 || snap.Takeaway[0].Place != "#7" || snap.Takeaway[0].State != restaurantv1.LiveState_LIVE_STATE_ORDERED {
		t.Fatalf("takeaway = %+v", snap.Takeaway)
	}
	if len(snap.Areas) != 1 || snap.Areas[0].Name.En != "Main" {
		t.Fatalf("areas = %+v", snap.Areas)
	}
}

func TestLiveKitchenPerStationWithKDSThresholds(t *testing.T) {
	f := newFixture(t)
	snap, err := f.svc.Snapshot(f.sita)
	if err != nil {
		t.Fatal(err)
	}
	if snap.YellowMinutes != 10 || snap.RedMinutes != 20 {
		t.Fatalf("thresholds %d/%d", snap.YellowMinutes, snap.RedMinutes)
	}
	var kitchen, bar *restaurantv1.LiveStation
	for _, st := range snap.Stations {
		switch st.StationId {
		case f.kitchen.String():
			kitchen = st
		case f.bar.String():
			bar = st
		}
	}
	if kitchen.Tickets != 2 || kitchen.ItemsSent != 2 || kitchen.ItemsPreparing != 0 || kitchen.ItemsReady != 0 || kitchen.GetOldestMinutes() != 25 || kitchen.Slow != 1 || kitchen.Late != 1 {
		t.Fatalf("kitchen = %+v", kitchen)
	}
	if len(kitchen.LateItems) != 1 || kitchen.LateItems[0].OrderId != f.o2.String() || kitchen.LateItems[0].Place != "T2" ||
		kitchen.LateItems[0].Name.En != "Chowmein" || kitchen.LateItems[0].Quantity != 1 || kitchen.LateItems[0].Minutes != 25 {
		t.Fatalf("late items = %+v", kitchen.LateItems)
	}
	if bar.Tickets != 1 || bar.ItemsSent != 1 || bar.ItemsReady != 1 || bar.Late != 0 {
		t.Fatalf("bar = %+v", bar)
	}
}

func TestLiveTillTodayMoneyOnTablesAnd86(t *testing.T) {
	f := newFixture(t)
	snap, err := f.svc.Snapshot(f.sita)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Tills) != 1 {
		t.Fatalf("tills = %+v", snap.Tills)
	}
	till := snap.Tills[0]
	if till.UserName != "Hari Thapa" || till.OpeningFloatPaisa != 500_000 || till.ExpectedCashPaisa != 500_000+f.billTotal || len(till.Takings) != 4 ||
		till.Takings[0].Method != restaurantv1.PaymentMethod_PAYMENT_METHOD_CASH || till.Takings[0].TotalPaisa != f.billTotal {
		t.Fatalf("till = %+v", till)
	}
	if snap.Today.SalesPaisa != f.billTotal || snap.Today.Bills != 1 || snap.Today.Covers != 6 || snap.Today.AlertsOpen != nil {
		t.Fatalf("today = %+v", snap.Today)
	}
	if snap.MoneyOnTablesPaisa != 18_000+20_000+12_000 { // veg (T1) + chowmein (T2) + lassi (#7)
		t.Fatalf("money on tables %d", snap.MoneyOnTablesPaisa)
	}
	if snap.CreditTodayPaisa == nil || *snap.CreditTodayPaisa != 0 {
		t.Fatalf("credit today %v", snap.CreditTodayPaisa)
	}
	f.exec(`UPDATE menu_items SET is_86d = true WHERE id = $1`, f.veg)
	snap, _ = f.svc.Snapshot(f.sita)
	if len(snap.Items_86) != 1 || snap.Items_86[0].En != "Veg Momo" {
		t.Fatalf("86 = %+v", snap.Items_86)
	}
}

func TestLiveExceptionsNewestFirstNotLoginsOrPlainBills(t *testing.T) {
	f := newFixture(t)
	time.Sleep(5 * time.Millisecond)
	f.exec(`UPDATE invoices SET status = 'CANCELLED', cancel_reason = 'Wrong table', cancelled_by_user_id = $2, cancel_approved_by_user_id = $3, cancelled_at = now() WHERE id = $1`, f.bill, f.hariID, f.sitaID)
	f.audit("invoice.cancel", "invoice", f.bill.String(), f.hariID, f.sitaID, "Wrong table")
	snap, err := f.svc.Snapshot(f.sita)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range snap.Exceptions {
		actions = append(actions, e.Entry.Action)
	}
	if !slices.Equal(actions, []string{"invoice.cancel", "order.void"}) {
		t.Fatalf("exceptions = %v", actions)
	}
	void := snap.Exceptions[1].Entry
	if void.Detail != "Guest left" || void.ApproverName != "Sita Gurung" || void.Subject != "Thukpa" || snap.Exceptions[1].AlertId != "" {
		t.Fatalf("void = %+v", void)
	}
	if snap.Exceptions[0].Entry.Subject != "E-2083/84-1" {
		t.Fatalf("cancel subject %q", snap.Exceptions[0].Entry.Subject)
	}
}

func TestLiveDiscountedBillIsAnException(t *testing.T) {
	f := newFixture(t)
	inv := f.id(`INSERT INTO invoices (fiscal_year, number, mode, bs_date, bs_year, bs_month, bs_day, order_id,
		subtotal_paisa, discount_paisa, service_charge_paisa, taxable_paisa, vat_paisa, total_paisa, discount_reason, created_by_user_id)
		VALUES ('2083.084', 2, 'ESTIMATE', '2083.08.10', 2083, 8, 10, $1, 12000, 1200, 0, 10800, 0, 10800, 'Regular', $2) RETURNING id`, f.o4, f.hariID)
	f.audit("invoice.issue", "invoice", inv.String(), f.hariID, uuid.Nil, "")
	snap, err := f.svc.Snapshot(f.sita)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Exceptions) != 2 || snap.Exceptions[0].Entry.Action != "invoice.issue" || snap.Exceptions[0].Entry.Detail != "discount Rs 12.00 · Regular" {
		t.Fatalf("exceptions = %+v", snap.Exceptions)
	}
}

func TestLiveOrderCard(t *testing.T) {
	f := newFixture(t)
	card, err := f.svc.Order(f.sita, f.o2.String())
	if err != nil {
		t.Fatal(err)
	}
	if card.Order.Place != "T2" {
		t.Fatalf("place %q", card.Order.Place)
	}
	var thuk *restaurantv1.LiveOrderLine
	for _, l := range card.Lines {
		if l.Id == f.thuk.String() {
			thuk = l
		}
	}
	if thuk == nil || thuk.Status != restaurantv1.OrderItemStatus_ORDER_ITEM_STATUS_VOIDED || thuk.VoidReason != "Guest left" ||
		thuk.VoidedByName != "Gita Rai" || thuk.VoidApprovedByName != "Sita Gurung" || thuk.BillNumber != "" {
		t.Fatalf("thukpa = %+v", thuk)
	}
	c1, err := f.svc.Order(f.sita, f.o1.String())
	if err != nil {
		t.Fatal(err)
	}
	var coke *restaurantv1.LiveOrderLine
	for _, l := range c1.Lines {
		if l.Id == f.coke.String() {
			coke = l
		}
	}
	if coke == nil || coke.Quantity != 2 || coke.UnitPricePaisa != 8_000 || coke.BillNumber != "E-2083/84-1" {
		t.Fatalf("coke = %+v", coke)
	}
	if _, err := f.svc.Order(f.sita, uuid.NewString()); reasonOf(err) != restaurantv1.ErrorReason_ERROR_REASON_NOT_FOUND {
		t.Fatalf("unknown order: %v", err)
	}
	if _, err := f.svc.Order(f.sita, "nope"); reasonOf(err) != restaurantv1.ErrorReason_ERROR_REASON_INVALID {
		t.Fatalf("bad id: %v", err)
	}
}
