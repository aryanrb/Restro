package alerts_test

// Mirrors the web fake's tests (web/src/lib/api/mock/mock-alerts-api.test.ts):
// the same world (Ram the owner, Sita the manager, Hari the cashier, Gita the
// waiter, Bikash a second manager), the same rules, engine and API behaviour,
// driven through the real services so every alert comes from a real audit write.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/alerts"
	"github.com/oneaccord/restro/api/internal/audit"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/billing"
	"github.com/oneaccord/restro/api/internal/catalog"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/credit"
	"github.com/oneaccord/restro/api/internal/db"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/inventory"
	"github.com/oneaccord/restro/api/internal/orders"
	"github.com/oneaccord/restro/api/internal/realtime"
	"github.com/oneaccord/restro/api/internal/shift"
)

var ktm = time.FixedZone("NPT", 5*3600+45*60)

type fixture struct {
	t       *testing.T
	pool    *pgxpool.Pool
	q       *sqlc.Queries
	authSvc *auth.Service
	ord     *orders.Service
	bill    *billing.Service
	shifts  *shift.Service
	cat     *catalog.Service
	credit  *credit.Service
	inv     *inventory.Service
	al      *alerts.Service
	hub     *realtime.Hub

	owner, sita, hari, gita, bikash           context.Context
	ownerID, sitaID, hariID, gitaID, bikashID uuid.UUID
	pins                                      map[uuid.UUID]string
	usernames                                 map[uuid.UUID]string

	station, category uuid.UUID
	veg, chow, beer   uuid.UUID // Rs 180, Rs 200, Rs 550
	t1, t2, t3        uuid.UUID
	device            sqlc.Device
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run alerts integration tests")
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
	if _, err := pool.Exec(ctx, `TRUNCATE alert_pushes, alerts, users, sessions, devices, audit_log, processed_commands,
		orders, order_items, order_item_modifiers, invoices, invoice_lines, invoice_prints, invoice_counters, payments, cbms_outbox,
		shifts, shift_adjustments, menu_items, menu_categories, stations, dining_tables, areas, modifier_groups,
		customers, repayments, inventory_items, stock_movements, waste_entries, stock_counts, stock_count_lines
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE alert_config SET overrides = '{}', opening_hours = NULL, after_hours_grace_minutes = 30,
		quiet_from = NULL, quiet_to = NULL, daily_push_cap = 5, recipient_ids = '{}' WHERE id = 1`); err != nil {
		t.Fatalf("reset config: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE settings SET vat_registered = false, prices_include_vat = true,
		service_charge_enabled = false, round_total_to_rupee = false WHERE id = 1`); err != nil {
		t.Fatalf("settings: %v", err)
	}
	t.Cleanup(alerts.Install(zap.NewNop()))

	q := sqlc.New(pool)
	authSvc := auth.NewService(pool, zap.NewNop())
	f := &fixture{
		t: t, pool: pool, q: q, authSvc: authSvc, hub: realtime.NewHub(),
		ord:    orders.NewService(pool, nil, authSvc, zap.NewNop()),
		bill:   billing.NewService(pool, nil, authSvc, zap.NewNop()),
		shifts: shift.NewService(pool, nil, zap.NewNop()),
		cat:    catalog.NewService(pool, nil, zap.NewNop()),
		credit: credit.NewService(pool, nil, zap.NewNop()),
		inv:    inventory.NewService(pool, nil, authSvc, zap.NewNop()),
		pins:   map[uuid.UUID]string{}, usernames: map[uuid.UUID]string{},
	}
	f.al = alerts.NewService(pool, f.hub, zap.NewNop())
	f.ownerID = f.newUser("Ram Shrestha", auth.RoleOwner, "1111")
	f.sitaID = f.newUser("Sita Gurung", auth.RoleManager, "2222")
	f.hariID = f.newUser("Hari Thapa", auth.RoleCashier, "3333")
	f.gitaID = f.newUser("Gita Rai", auth.RoleWaiter, "4444")
	f.bikashID = f.newUser("Bikash Tamang", auth.RoleManager, "5555")
	f.owner, f.sita, f.hari, f.gita, f.bikash = f.login(f.ownerID), f.login(f.sitaID), f.login(f.hariID), f.login(f.gitaID), f.login(f.bikashID)

	st, err := q.CreateStation(ctx, sqlc.CreateStationParams{NameEn: "Kitchen"})
	f.must(err)
	f.station = st.ID
	cat, err := q.CreateCategory(ctx, sqlc.CreateCategoryParams{NameEn: "Food", Active: true})
	f.must(err)
	f.category = cat.ID
	mk := func(name string, price int64) uuid.UUID {
		it, err := q.CreateItem(ctx, sqlc.CreateItemParams{CategoryID: cat.ID, NameEn: name, PricePaisa: price, StationID: st.ID, Active: true})
		f.must(err)
		return it.ID
	}
	f.veg, f.chow, f.beer = mk("Veg Momo", 18_000), mk("Chowmein", 20_000), mk("Beer", 55_000)
	area, err := q.CreateArea(ctx, sqlc.CreateAreaParams{NameEn: "Hall"})
	f.must(err)
	mkTable := func(name string) uuid.UUID {
		tb, err := q.CreateTable(ctx, sqlc.CreateTableParams{AreaID: area.ID, Name: name, Seats: 4, Active: true})
		f.must(err)
		return tb.ID
	}
	f.t1, f.t2, f.t3 = mkTable("T1"), mkTable("T2"), mkTable("T3")
	f.device, err = q.CreateDevice(ctx, sqlc.CreateDeviceParams{TokenHash: "counter-" + uuid.NewString(), Label: "Counter"})
	f.must(err)
	return f
}

func (f *fixture) must(err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) newUser(name, role, pin string) uuid.UUID {
	f.t.Helper()
	hash, err := auth.HashSecret("secret123")
	f.must(err)
	pinHash, err := auth.HashSecret(pin)
	f.must(err)
	username := "u-" + uuid.NewString()[:8]
	u, err := f.q.CreateUser(context.Background(), sqlc.CreateUserParams{
		Name: name, Role: role, Username: dbx.Text(username), PasswordHash: dbx.Text(hash), PinHash: dbx.Text(pinHash), Language: "en", Active: true,
	})
	f.must(err)
	f.pins[u.ID], f.usernames[u.ID] = pin, username
	return u.ID
}

// login logs the user in again (an audited "login") and returns their request context.
func (f *fixture) login(id uuid.UUID) context.Context {
	f.t.Helper()
	_, token, err := f.authSvc.PasswordLogin(context.Background(), f.usernames[id], "secret123")
	f.must(err)
	req := connect.NewRequest(&restaurantv1.GetSessionRequest{})
	req.Header().Set("Authorization", "Bearer "+token)
	var out context.Context
	_, _ = f.authSvc.Interceptor()(func(c context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		out = c
		return nil, nil
	})(context.Background(), req)
	if !auth.ActorFrom(out).LoggedIn() {
		f.t.Fatal("actor not resolved")
	}
	return out
}

func (f *fixture) approval(id uuid.UUID) *restaurantv1.ManagerApproval {
	return &restaurantv1.ManagerApproval{ManagerUserId: id.String(), Pin: f.pins[id]}
}

// --- orders, bills, shifts ---

type order struct {
	ocid  string
	items map[string]string // line name → order item id
}

// newOrder opens a dine-in order as ctx and sends the lines (name → menu item).
func (f *fixture) newOrder(ctx context.Context, table uuid.UUID, lines ...[2]any) order {
	f.t.Helper()
	ocid, tid := uuid.NewString(), table.String()
	_, err := f.ord.CreateOrder(ctx, &restaurantv1.CreateOrderRequest{CommandId: uuid.NewString(), OrderClientId: ocid,
		Type: restaurantv1.OrderType_ORDER_TYPE_DINE_IN, TableId: &tid, GuestCount: 2})
	f.must(err)
	o := order{ocid: ocid, items: map[string]string{}}
	byClient := map[string]string{}
	var items []*restaurantv1.NewItem
	for _, l := range lines {
		cid := uuid.NewString()
		byClient[cid] = l[0].(string)
		items = append(items, &restaurantv1.NewItem{ClientId: cid, MenuItemId: l[1].(uuid.UUID).String(), Quantity: 1})
	}
	res, err := f.ord.AddItems(ctx, &restaurantv1.AddItemsRequest{CommandId: uuid.NewString(), OrderClientId: ocid, Items: items, Send: true})
	f.must(err)
	for _, it := range res.Order.Items {
		o.items[byClient[it.ClientId]] = it.Id
	}
	return o
}

func line(name string, item uuid.UUID) [2]any { return [2]any{name, item} }

func (f *fixture) setStatus(itemID, status string) {
	f.t.Helper()
	_, err := f.pool.Exec(context.Background(), `UPDATE order_items SET status = $2 WHERE id = $1`, itemID, status)
	f.must(err)
}

func (f *fixture) version(ctx context.Context, ocid string) int64 {
	f.t.Helper()
	o, err := f.ord.GetOrder(ctx, &restaurantv1.GetOrderRequest{ClientId: &ocid})
	f.must(err)
	return o.Version
}

func (f *fixture) void(ctx context.Context, o order, name string, approval *restaurantv1.ManagerApproval) error {
	_, err := f.ord.VoidItem(ctx, &restaurantv1.VoidItemRequest{CommandId: uuid.NewString(), OrderClientId: o.ocid,
		BaseVersion: f.version(ctx, o.ocid), ItemId: o.items[name], Reason: "Changed mind", Approval: approval})
	return err
}

func (f *fixture) mustVoid(ctx context.Context, o order, name string, approval *restaurantv1.ManagerApproval) {
	f.t.Helper()
	f.must(f.void(ctx, o, name, approval))
}

func pct(bp int64, reason string, approval *restaurantv1.ManagerApproval) *restaurantv1.DiscountInput {
	return &restaurantv1.DiscountInput{Type: restaurantv1.DiscountType_DISCOUNT_TYPE_PERCENT, Value: bp, Reason: reason, Approval: approval}
}

// pay bills the named lines (all unbilled when none) in cash.
func (f *fixture) pay(ctx context.Context, o order, discount *restaurantv1.DiscountInput, names ...string) *restaurantv1.Invoice {
	f.t.Helper()
	var ids []string
	for _, n := range names {
		ids = append(ids, o.items[n])
	}
	if len(ids) == 0 {
		ord, err := f.ord.GetOrder(ctx, &restaurantv1.GetOrderRequest{ClientId: &o.ocid})
		f.must(err)
		for _, it := range ord.Items {
			if it.Status != restaurantv1.OrderItemStatus_ORDER_ITEM_STATUS_VOIDED && it.InvoiceId == "" {
				ids = append(ids, it.Id)
			}
		}
	}
	// The total: subtotal of the lines less the discount (no VAT / service charge here).
	var subtotal int64
	f.must(f.pool.QueryRow(context.Background(), `SELECT COALESCE(sum(unit_price_paisa * quantity), 0) FROM order_items WHERE id::text = ANY($1)`, ids).Scan(&subtotal))
	total := subtotal
	if discount != nil {
		total -= subtotal * discount.Value / 10_000
	}
	inv, err := f.bill.IssueBill(ctx, &restaurantv1.IssueBillRequest{CommandId: uuid.NewString(), OrderClientId: o.ocid, ItemIds: ids,
		Discount: discount, Payments: []*restaurantv1.PaymentInput{{Method: restaurantv1.PaymentMethod_PAYMENT_METHOD_CASH, AmountPaisa: total}}})
	f.must(err)
	return inv
}

func (f *fixture) cancel(ctx context.Context, inv *restaurantv1.Invoice) {
	f.t.Helper()
	_, err := f.bill.CancelInvoice(ctx, &restaurantv1.CancelInvoiceRequest{CommandId: uuid.NewString(), InvoiceId: inv.Id, Reason: "Wrong items", Approval: f.approval(f.sitaID)})
	f.must(err)
}

func (f *fixture) print(ctx context.Context, inv *restaurantv1.Invoice) {
	f.t.Helper()
	_, err := f.bill.MarkPrinted(ctx, &restaurantv1.MarkPrintedRequest{InvoiceId: inv.Id})
	f.must(err)
}

func (f *fixture) openShift(ctx context.Context, float int64) string {
	f.t.Helper()
	sh, err := f.shifts.OpenShift(ctx, float, "")
	f.must(err)
	return sh.Id
}

func (f *fixture) closeShift(ctx context.Context, id string, counted int64) {
	f.t.Helper()
	_, err := f.shifts.CloseShift(ctx, id, counted, "")
	f.must(err)
}

// ago moves an invoice's issue (and cancel) time back.
func (f *fixture) ago(inv *restaurantv1.Invoice, d time.Duration) {
	f.t.Helper()
	// Issued invoices are immutable (DB trigger): bypass it for this test-only rewrite.
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	f.must(err)
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`)
	f.must(err)
	_, err = tx.Exec(ctx, `UPDATE invoices SET ad_datetime = ad_datetime - $2::interval, created_at = created_at - $2::interval,
		cancelled_at = cancelled_at - $2::interval WHERE id = $1`, inv.Id, fmt.Sprintf("%d seconds", int(d.Seconds())))
	f.must(err)
	f.must(tx.Commit(ctx))
}

// --- alerts ---

func (f *fixture) stored(rule string) []sqlc.Alert {
	f.t.Helper()
	all, err := f.q.ListAlerts(context.Background(), sqlc.ListAlertsParams{Rule: rule})
	f.must(err)
	slices.Reverse(all) // oldest first
	return all
}

func (f *fixture) pushes() []sqlc.AlertPush {
	f.t.Helper()
	p, err := f.q.ListAlertPushes(context.Background(), dbx.UUIDOpt(uuid.Nil))
	f.must(err)
	return p
}

func nums(a sqlc.Alert) map[string]int64 {
	m := map[string]int64{}
	_ = json.Unmarshal(a.NumParams, &m)
	return m
}

func texts(a sqlc.Alert) map[string]string {
	m := map[string]string{}
	_ = json.Unmarshal(a.TextParams, &m)
	return m
}

func subjectOf(a sqlc.Alert) uuid.UUID {
	if !a.SubjectUserID.Valid {
		return uuid.Nil
	}
	return a.SubjectUserID.Bytes
}

func (f *fixture) config() *restaurantv1.AlertConfig {
	f.t.Helper()
	c, err := f.al.GetAlertConfig(f.owner)
	f.must(err)
	return c
}

func (f *fixture) configure(change func(c *restaurantv1.AlertConfig)) *restaurantv1.AlertConfig {
	f.t.Helper()
	c := f.config()
	change(c)
	saved, err := f.al.UpdateAlertConfig(f.owner, &restaurantv1.UpdateAlertConfigRequest{Config: c})
	f.must(err)
	return saved
}

func setRule(c *restaurantv1.AlertConfig, rule restaurantv1.AlertRule, change func(r *restaurantv1.AlertRuleSetting)) {
	for _, r := range c.Rules {
		if r.Rule == rule {
			change(r)
		}
	}
}

// everyDay sets the same opening hours on all seven days.
func everyDay(open, close string) *restaurantv1.WeekHours {
	w := &restaurantv1.WeekHours{}
	for range 7 {
		w.Days = append(w.Days, &restaurantv1.DayHours{Open: open, Close: close})
	}
	return w
}

// hoursAround returns "HH:MM" Kathmandu time now + offset.
func hoursAround(offset time.Duration) string { return time.Now().Add(offset).In(ktm).Format("15:04") }

// closedNow: opening hours (every day) that exclude now and its 30-minute grace.
func closedNow() *restaurantv1.WeekHours {
	return everyDay(hoursAround(4*time.Hour), hoursAround(6*time.Hour))
}

func (f *fixture) auditCount(action string) int {
	f.t.Helper()
	var n int
	f.must(f.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = $1`, action).Scan(&n))
	return n
}

func reason(t *testing.T, err error) restaurantv1.ErrorReason {
	t.Helper()
	var ce *connecterr.Error
	if !errors.As(err, &ce) {
		t.Fatalf("want a service error, got %v", err)
	}
	return ce.Reason
}

const (
	notAllowed = restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED
	invalid    = restaurantv1.ErrorReason_ERROR_REASON_INVALID
)

func today() string { return time.Now().In(ktm).Format(time.DateOnly) }

// ===================================================================== rules

func TestVoidAfterKitchen(t *testing.T) {
	f := newFixture(t)
	o := f.newOrder(f.gita, f.t1, line("beer", f.beer), line("veg", f.veg), line("veg2", f.veg))
	f.setStatus(o.items["beer"], "PREPARING")
	f.setStatus(o.items["veg"], "PREPARING")
	f.mustVoid(f.gita, o, "beer", f.approval(f.sitaID))
	f.mustVoid(f.gita, o, "veg", f.approval(f.sitaID)) // Rs 180: under the threshold
	f.mustVoid(f.gita, o, "veg2", nil)                 // not started
	got := f.stored("VOID_AFTER_KITCHEN")
	if len(got) != 1 {
		t.Fatalf("alerts = %d, want 1", len(got))
	}
	a := got[0]
	if a.Severity != "NORMAL" || a.Status != "OPEN" || subjectOf(a) != f.gitaID || !slices.Equal(a.ApproverIds, []uuid.UUID{f.sitaID}) || a.Count != 1 {
		t.Fatalf("alert = %+v", a)
	}
	if nums(a)["value"] != 55_000 || len(a.AuditIds) != 1 {
		t.Fatalf("params %v, audit ids %v", nums(a), a.AuditIds)
	}
	var action string
	f.must(f.pool.QueryRow(context.Background(), `SELECT action FROM audit_log WHERE id = $1`, a.AuditIds[0]).Scan(&action))
	if action != "order.void" {
		t.Fatalf("linked audit action = %s", action)
	}
}

func TestRepeatedVoidsGroupIntoOneAlert(t *testing.T) {
	f := newFixture(t)
	o := f.newOrder(f.gita, f.t1, line("a", f.veg), line("b", f.veg), line("c", f.veg), line("d", f.veg))
	f.mustVoid(f.gita, o, "a", nil)
	f.mustVoid(f.gita, o, "b", nil)
	if n := len(f.stored("REPEATED_VOIDS")); n != 0 {
		t.Fatalf("after 2 voids: %d alerts", n)
	}
	f.mustVoid(f.gita, o, "c", nil)
	got := f.stored("REPEATED_VOIDS")
	if len(got) != 1 || nums(got[0])["count"] != 3 {
		t.Fatalf("after 3 voids: %+v", got)
	}
	f.mustVoid(f.gita, o, "d", nil)
	got = f.stored("REPEATED_VOIDS")
	if len(got) != 1 || got[0].Count != 2 || subjectOf(got[0]) != f.gitaID || nums(got[0])["count"] != 4 || len(got[0].AuditIds) != 2 {
		t.Fatalf("after 4 voids: %+v", got)
	}
}

func TestSelfApprovedVoid(t *testing.T) {
	f := newFixture(t)
	o := f.newOrder(f.sita, f.t1, line("veg", f.veg))
	f.setStatus(o.items["veg"], "READY")
	f.mustVoid(f.sita, o, "veg", f.approval(f.sitaID))
	got := f.stored("SELF_APPROVED")
	if len(got) != 1 || subjectOf(got[0]) != f.sitaID || !slices.Equal(got[0].ApproverIds, []uuid.UUID{f.sitaID}) || texts(got[0])["action"] != "order.void" {
		t.Fatalf("got %+v", got)
	}
}

func TestHighDiscount(t *testing.T) {
	f := newFixture(t)
	o := f.newOrder(f.gita, f.t1, line("veg", f.veg), line("chow", f.chow))
	f.openShift(f.hari, 0)
	f.pay(f.hari, o, pct(1000, "Regular", nil), "veg")
	if n := len(f.stored("HIGH_DISCOUNT")); n != 0 {
		t.Fatalf("10%%: %d alerts", n)
	}
	inv := f.pay(f.hari, o, pct(2500, "Friend", f.approval(f.sitaID)), "chow")
	got := f.stored("HIGH_DISCOUNT")
	if len(got) != 1 {
		t.Fatalf("alerts = %d", len(got))
	}
	a := got[0]
	if subjectOf(a) != f.hariID || !slices.Equal(a.ApproverIds, []uuid.UUID{f.sitaID}) || a.InvoiceID.Bytes != uuid.MustParse(inv.Id) || a.Subject != inv.DisplayNumber {
		t.Fatalf("alert = %+v", a)
	}
	if nums(a)["discount"] != 5000 || nums(a)["percent"] != 25 {
		t.Fatalf("params = %v", nums(a))
	}
}

func TestDiscountTotalOverTheBusinessDay(t *testing.T) {
	f := newFixture(t)
	f.configure(func(c *restaurantv1.AlertConfig) {
		setRule(c, restaurantv1.AlertRule_ALERT_RULE_DISCOUNT_TOTAL, func(r *restaurantv1.AlertRuleSetting) { r.Thresholds["amount"] = 3000 })
	})
	o := f.newOrder(f.gita, f.t1, line("veg", f.veg), line("chow", f.chow))
	f.openShift(f.hari, 0)
	f.pay(f.hari, o, pct(1000, "Regular", nil), "veg")
	if n := len(f.stored("DISCOUNT_TOTAL")); n != 0 {
		t.Fatalf("after Rs 18: %d alerts", n)
	}
	f.pay(f.hari, o, pct(1000, "Regular", nil), "chow")
	got := f.stored("DISCOUNT_TOTAL")
	if len(got) != 1 || nums(got[0])["total"] != 3800 {
		t.Fatalf("got %+v", got)
	}
}

func TestCancelledAndRebilledLowerIsHighWithAPush(t *testing.T) {
	f := newFixture(t)
	o := f.newOrder(f.gita, f.t1, line("veg", f.veg), line("chow", f.chow))
	f.openShift(f.hari, 0)
	first := f.pay(f.hari, o, nil)
	f.cancel(f.hari, first)
	f.mustVoid(f.hari, o, "chow", nil)
	second := f.pay(f.hari, o, nil)
	got := f.stored("CANCEL_REBILL_LOWER")
	if len(got) != 1 {
		t.Fatalf("alerts = %d", len(got))
	}
	a := got[0]
	if a.Severity != "HIGH" || subjectOf(a) != f.hariID || !slices.Equal(a.ApproverIds, []uuid.UUID{f.sitaID}) || a.InvoiceID.Bytes != uuid.MustParse(second.Id) {
		t.Fatalf("alert = %+v", a)
	}
	if nums(a)["from"] != first.TotalPaisa || nums(a)["to"] != second.TotalPaisa {
		t.Fatalf("params = %v", nums(a))
	}
	if a.Subject != first.DisplayNumber+" → "+second.DisplayNumber {
		t.Fatalf("subject = %q", a.Subject)
	}
	p := f.pushes()
	if len(p) != 1 || p[0].Kind != "ALERT" || p[0].Rule != "CANCEL_REBILL_LOWER" || p[0].AmountPaisa.Int64 != first.TotalPaisa-second.TotalPaisa || p[0].AlertID.Bytes != a.ID {
		t.Fatalf("pushes = %+v", p)
	}
}

func TestRebilledSameAmountOrAfterTheWindowNoAlert(t *testing.T) {
	f := newFixture(t)
	o := f.newOrder(f.gita, f.t1, line("veg", f.veg), line("chow", f.chow))
	f.openShift(f.hari, 0)
	first := f.pay(f.hari, o, nil)
	f.cancel(f.hari, first)
	again := f.pay(f.hari, o, nil)
	if n := len(f.stored("CANCEL_REBILL_LOWER")); n != 0 {
		t.Fatalf("same amount: %d alerts", n)
	}
	f.cancel(f.hari, again)
	f.ago(first, 3*time.Hour)
	f.ago(again, 3*time.Hour) // cancelled 3 h ago: outside the 2 h window
	f.mustVoid(f.hari, o, "chow", nil)
	f.pay(f.hari, o, nil)
	if n := len(f.stored("CANCEL_REBILL_LOWER")); n != 0 {
		t.Fatalf("after the window: %d alerts", n)
	}
}

func TestLateCancellation(t *testing.T) {
	f := newFixture(t)
	o1 := f.newOrder(f.gita, f.t1, line("veg", f.veg))
	o2 := f.newOrder(f.gita, f.t2, line("chow", f.chow))
	f.openShift(f.hari, 0)
	a := f.pay(f.hari, o1, nil)
	b := f.pay(f.hari, o2, nil)
	f.ago(a, 30*time.Minute)
	f.cancel(f.hari, a)
	if n := len(f.stored("LATE_CANCELLATION")); n != 0 {
		t.Fatalf("after 30 min: %d alerts", n)
	}
	f.ago(b, 90*time.Minute)
	f.cancel(f.hari, b)
	got := f.stored("LATE_CANCELLATION")
	if len(got) != 1 || got[0].Subject != b.DisplayNumber || subjectOf(got[0]) != f.hariID || !slices.Equal(got[0].ApproverIds, []uuid.UUID{f.sitaID}) {
		t.Fatalf("got %+v", got)
	}
	if m := nums(got[0])["minutes"]; m != 90 {
		t.Fatalf("minutes = %d", m)
	}
}

func TestOldReprintAndThirdCopy(t *testing.T) {
	f := newFixture(t)
	o1 := f.newOrder(f.gita, f.t1, line("veg", f.veg))
	o2 := f.newOrder(f.gita, f.t2, line("chow", f.chow))
	f.openShift(f.hari, 0)
	a := f.pay(f.hari, o1, nil)
	b := f.pay(f.hari, o2, nil)
	f.print(f.hari, a)
	f.print(f.hari, a) // copy 1, fresh
	if n := len(f.stored("OLD_REPRINT")); n != 0 {
		t.Fatalf("fresh copy: %d alerts", n)
	}
	for range 4 {
		f.print(f.hari, b) // the 4th print is copy 3
	}
	got := f.stored("OLD_REPRINT")
	if len(got) != 1 || got[0].Subject != b.DisplayNumber || nums(got[0])["copy"] != 3 {
		t.Fatalf("got %+v", got)
	}
	f.ago(a, 90*time.Minute)
	f.print(f.sita, a)
	got = f.stored("OLD_REPRINT")
	if len(got) != 2 || subjectOf(got[0]) != f.hariID || subjectOf(got[1]) != f.sitaID {
		t.Fatalf("got %+v", got)
	}
}

func TestCashShortAndCashOver(t *testing.T) {
	f := newFixture(t)
	f.closeShift(f.hari, f.openShift(f.hari, 100_000), 60_000) // Rs 400 short: under Rs 500
	if n := len(f.stored("CASH_SHORT")); n != 0 {
		t.Fatalf("Rs 400 short: %d alerts", n)
	}
	id := f.openShift(f.hari, 100_000)
	f.closeShift(f.hari, id, 40_000)
	got := f.stored("CASH_SHORT")
	if len(got) != 1 || got[0].Severity != "HIGH" || subjectOf(got[0]) != f.hariID || got[0].ShiftID.Bytes != uuid.MustParse(id) {
		t.Fatalf("got %+v", got)
	}
	if p := nums(got[0]); p["short"] != 60_000 || p["expected"] != 100_000 || p["counted"] != 40_000 {
		t.Fatalf("params = %v", p)
	}
	f.closeShift(f.hari, f.openShift(f.hari, 100_000), 250_000)
	over := f.stored("CASH_OVER")
	if len(over) != 1 || nums(over[0])["over"] != 150_000 {
		t.Fatalf("over = %+v", over)
	}
}

func TestRepeatedShortages(t *testing.T) {
	f := newFixture(t)
	for range 3 {
		f.closeShift(f.hari, f.openShift(f.hari, 100_000), 80_000) // Rs 200 short each
	}
	if n := len(f.stored("CASH_SHORT")); n != 0 {
		t.Fatalf("cash short: %d", n)
	}
	got := f.stored("REPEATED_SHORTAGES")
	if len(got) != 1 || got[0].Severity != "HIGH" || subjectOf(got[0]) != f.hariID || nums(got[0])["count"] != 3 {
		t.Fatalf("got %+v", got)
	}
}

func TestServedNotBilledWhenTheLastShiftCloses(t *testing.T) {
	f := newFixture(t)
	o1 := f.newOrder(f.gita, f.t1, line("veg", f.veg))
	o2 := f.newOrder(f.gita, f.t2, line("chow", f.chow))
	f.setStatus(o1.items["veg"], "SERVED")
	f.setStatus(o2.items["chow"], "SERVED")
	sitaShift := f.openShift(f.sita, 0)
	hariShift := f.openShift(f.hari, 0)
	f.pay(f.hari, o2, nil)
	f.closeShift(f.hari, hariShift, 20_000)
	if n := len(f.stored("SERVED_NOT_BILLED")); n != 0 {
		t.Fatalf("Sita's till still open: %d alerts", n)
	}
	f.closeShift(f.sita, sitaShift, 0)
	got := f.stored("SERVED_NOT_BILLED")
	if len(got) != 1 || got[0].Severity != "HIGH" || subjectOf(got[0]) != f.gitaID || got[0].Subject != "T1" {
		t.Fatalf("got %+v", got)
	}
	f.closeShift(f.sita, f.openShift(f.sita, 0), 0)
	if n := len(f.stored("SERVED_NOT_BILLED")); n != 1 {
		t.Fatalf("the same order reported again: %d", n)
	}
}

func TestAfterHoursLogin(t *testing.T) {
	f := newFixture(t)
	f.login(f.hariID)
	if n := len(f.stored("AFTER_HOURS_LOGIN")); n != 0 {
		t.Fatalf("no hours entered: %d alerts", n)
	}
	f.configure(func(c *restaurantv1.AlertConfig) {
		c.OpeningHours = everyDay(hoursAround(-1*time.Hour), hoursAround(time.Hour))
	})
	f.login(f.hariID)
	if n := len(f.stored("AFTER_HOURS_LOGIN")); n != 0 {
		t.Fatalf("inside hours: %d alerts", n)
	}
	f.configure(func(c *restaurantv1.AlertConfig) { c.OpeningHours = closedNow() })
	f.login(f.hariID)
	got := f.stored("AFTER_HOURS_LOGIN")
	if len(got) != 1 || got[0].Severity != "HIGH" || subjectOf(got[0]) != f.hariID {
		t.Fatalf("got %+v", got)
	}
	if tm := texts(got[0])["time"]; tm != hoursAround(0) && tm != hoursAround(-time.Minute) {
		t.Fatalf("time = %q", tm)
	}
}

func TestManagerPinLockedAfterFailedApprovals(t *testing.T) {
	f := newFixture(t)
	o := f.newOrder(f.gita, f.t1, line("veg", f.veg))
	f.setStatus(o.items["veg"], "PREPARING")
	bad := &restaurantv1.ManagerApproval{ManagerUserId: f.sitaID.String(), Pin: "0000"}
	for range 4 {
		if r := reason(t, f.void(f.gita, o, "veg", bad)); r != restaurantv1.ErrorReason_ERROR_REASON_MANAGER_APPROVAL_INVALID {
			t.Fatalf("reason = %v", r)
		}
	}
	if r := reason(t, f.void(f.gita, o, "veg", bad)); r != restaurantv1.ErrorReason_ERROR_REASON_PIN_LOCKED {
		t.Fatalf("5th: %v", r)
	}
	_ = f.void(f.gita, o, "veg", bad)
	if n := f.auditCount("manager_pin.locked"); n != 1 {
		t.Fatalf("lock audit rows = %d", n)
	}
	got := f.stored("MANAGER_PIN_LOCKED")
	if len(got) != 1 || got[0].Severity != "HIGH" || subjectOf(got[0]) != f.gitaID || texts(got[0])["manager"] != "Sita Gurung" {
		t.Fatalf("got %+v", got)
	}
}

func TestManagerPinLoginLockoutAlertsCashierDoesNot(t *testing.T) {
	f := newFixture(t)
	for range 5 {
		_, _, _ = f.authSvc.PinLogin(context.Background(), f.hariID, "0000", &f.device)
	}
	if n := len(f.stored("MANAGER_PIN_LOCKED")); n != 0 {
		t.Fatalf("cashier lockout: %d alerts", n)
	}
	for range 5 {
		_, _, _ = f.authSvc.PinLogin(context.Background(), f.sitaID, "0000", &f.device)
	}
	got := f.stored("MANAGER_PIN_LOCKED")
	if len(got) != 1 || subjectOf(got[0]) != uuid.Nil || !slices.Contains(got[0].InvolvedIds, f.sitaID) {
		t.Fatalf("got %+v", got)
	}
}

func TestPriceDip(t *testing.T) {
	f := newFixture(t)
	saved, err := f.cat.SaveItem(f.owner, &restaurantv1.MenuItemInput{CategoryId: f.category.String(), Name: &restaurantv1.LocalText{En: "Paneer Momo"},
		PricePaisa: 18_000, Color: "#b91c1c", StationId: f.station.String(), Active: true}, nil)
	f.must(err)
	price := func(p int64) {
		t.Helper()
		v := saved.Version
		saved, err = f.cat.SaveItem(f.owner, &restaurantv1.MenuItemInput{Id: &saved.Id, CategoryId: f.category.String(), Name: saved.Name,
			PricePaisa: p, Color: "#b91c1c", StationId: f.station.String(), Active: true}, &v)
		f.must(err)
	}
	price(10_000)
	price(18_000)
	if n := len(f.stored("PRICE_DIP")); n != 0 {
		t.Fatalf("nothing sold at the low price: %d alerts", n)
	}
	price(10_000)
	item := uuid.MustParse(saved.Id)
	f.newOrder(f.gita, f.t1, line("a", item), line("b", item))
	time.Sleep(10 * time.Millisecond)
	price(18_000)
	got := f.stored("PRICE_DIP")
	if len(got) != 1 || got[0].Subject != "Paneer Momo" || subjectOf(got[0]) != f.ownerID {
		t.Fatalf("got %+v", got)
	}
	if p := nums(got[0]); p["from"] != 18_000 || p["low"] != 10_000 || p["sold"] != 2 {
		t.Fatalf("params = %v", p)
	}
}

func TestCreditOverLimitAndNewAccount(t *testing.T) {
	f := newFixture(t)
	f.openShift(f.hari, 0)
	cust, err := f.credit.CreateCustomer(f.hari, &restaurantv1.CreateCustomerRequest{Customer: &restaurantv1.CustomerInput{Name: "Ram Bahadur", Phone: "9841234567"}})
	f.must(err)
	o := f.newOrder(f.gita, f.t1, line("veg", f.veg))
	// New account (limit 0): over the limit, so Sita approves.
	inv, err := f.bill.IssueBill(f.hari, &restaurantv1.IssueBillRequest{CommandId: uuid.NewString(), OrderClientId: o.ocid,
		Payments:       []*restaurantv1.PaymentInput{{Method: restaurantv1.PaymentMethod_PAYMENT_METHOD_ON_ACCOUNT, AmountPaisa: 18_000, CustomerId: cust.Id}},
		CreditApproval: f.approval(f.sitaID)})
	f.must(err)
	over := f.stored("CREDIT_OVER_LIMIT")
	if len(over) != 1 || subjectOf(over[0]) != f.hariID || !slices.Equal(over[0].ApproverIds, []uuid.UUID{f.sitaID}) ||
		nums(over[0])["value"] != 18_000 || texts(over[0])["bill"] != inv.DisplayNumber || over[0].InvoiceID.Bytes != uuid.MustParse(inv.Id) {
		t.Fatalf("over limit = %+v", over)
	}
	fresh := f.stored("CREDIT_NEW_ACCOUNT")
	if len(fresh) != 1 || subjectOf(fresh[0]) != f.hariID || fresh[0].Subject != "Ram Bahadur" {
		t.Fatalf("new account = %+v", fresh)
	}
}

func TestHighWasteAndCountLoss(t *testing.T) {
	f := newFixture(t)
	f.configure(func(c *restaurantv1.AlertConfig) {
		setRule(c, restaurantv1.AlertRule_ALERT_RULE_HIGH_WASTE, func(r *restaurantv1.AlertRuleSetting) { r.Thresholds["dayWaste"] = 50_000 })
	})
	item, err := f.inv.SaveInventoryItem(f.owner, &restaurantv1.SaveInventoryItemRequest{Input: &restaurantv1.InventoryItemInput{
		Name: &restaurantv1.LocalText{En: "Chicken"}, Unit: restaurantv1.StockUnit_STOCK_UNIT_G, Active: true, UnitCostE4: 600_000}})
	f.must(err)
	opening := int64(100_000_000)
	_, err = f.inv.SetTracked(f.owner, &restaurantv1.SetTrackedRequest{Id: item.Id, Tracked: true, OpeningMilli: &opening})
	f.must(err)
	waste := func(grams int64) {
		t.Helper()
		_, err := f.inv.LogWaste(f.sita, &restaurantv1.LogWasteRequest{
			Target:        &restaurantv1.LogWasteRequest_Inventory{Inventory: &restaurantv1.InventoryWasteTarget{InventoryItemId: item.Id}},
			QuantityMilli: grams * 1000, Reason: restaurantv1.WasteReason_WASTE_REASON_SPOILED, RequestId: uuid.NewString()})
		f.must(err)
	}
	waste(500) // Rs 300
	if n := len(f.stored("HIGH_WASTE")); n != 0 {
		t.Fatalf("Rs 300: %d alerts", n)
	}
	waste(500) // Rs 600 in the day
	waste(100)
	got := f.stored("HIGH_WASTE")
	if len(got) != 1 || got[0].Count != 2 || nums(got[0])["waste"] != 66_000 || texts(got[0])["variant"] != "day" || subjectOf(got[0]) != uuid.Nil {
		t.Fatalf("waste = %+v", got)
	}

	// A posted count with Rs 3,500 missing (and Rs 1,000 found over, which doesn't offset it).
	ctx := context.Background()
	var countID uuid.UUID
	var number int64
	f.must(f.pool.QueryRow(ctx, `INSERT INTO stock_counts (status, started_by_user_id) VALUES ('POSTED', $1) RETURNING id, number`, f.sitaID).Scan(&countID, &number))
	other, err := f.inv.SaveInventoryItem(f.owner, &restaurantv1.SaveInventoryItemRequest{Input: &restaurantv1.InventoryItemInput{
		Name: &restaurantv1.LocalText{En: "Rice"}, Unit: restaurantv1.StockUnit_STOCK_UNIT_G, Active: true}})
	f.must(err)
	_, err = f.pool.Exec(ctx, `INSERT INTO stock_count_lines (count_id, inventory_item_id, value_paisa) VALUES ($1, $2, -350000), ($1, $3, 100000)`, countID, item.Id, other.Id)
	f.must(err)
	f.must(dbx.WithTx(ctx, f.pool, f.q, func(q *sqlc.Queries) error {
		return audit.Write(ctx, q, audit.Entry{ActorID: f.sitaID, Action: "STOCK_COUNT_POSTED", Entity: "stock_count", EntityID: countID.String(),
			After: audit.JSON(map[string]any{"total": -250000, "number": number})})
	}))
	loss := f.stored("HIGH_WASTE")
	if len(loss) != 2 || nums(loss[1])["loss"] != 350_000 || loss[1].Subject != fmt.Sprintf("Count #%d", number) || !slices.Contains(loss[1].InvolvedIds, f.sitaID) {
		t.Fatalf("count loss = %+v", loss)
	}
}

// ==================================================================== engine

func TestRuleSwitchedOffDoesNotFireAndTheChangeIsAudited(t *testing.T) {
	f := newFixture(t)
	f.configure(func(c *restaurantv1.AlertConfig) {
		setRule(c, restaurantv1.AlertRule_ALERT_RULE_CASH_SHORT, func(r *restaurantv1.AlertRuleSetting) { r.Enabled = false })
	})
	if n := f.auditCount("alert.rules_change"); n != 1 {
		t.Fatalf("rules_change audits = %d", n)
	}
	f.closeShift(f.hari, f.openShift(f.hari, 100_000), 0)
	if n := len(f.stored("CASH_SHORT")); n != 0 {
		t.Fatalf("disabled rule fired: %d", n)
	}
	// Only differences from the defaults are stored.
	var overrides string
	f.must(f.pool.QueryRow(context.Background(), `SELECT overrides::text FROM alert_config`).Scan(&overrides))
	if overrides != `{"CASH_SHORT": {"enabled": false}}` {
		t.Fatalf("overrides = %s", overrides)
	}
}

func TestOwnerCanRaiseARuleToHighOnlyHighPushes(t *testing.T) {
	f := newFixture(t)
	f.closeShift(f.hari, f.openShift(f.hari, 0), 150_000) // cash over: Normal
	alertPushes := func() int {
		n := 0
		for _, p := range f.pushes() {
			if p.Kind != "SUMMARY" {
				n++
			}
		}
		return n
	}
	if n := alertPushes(); n != 0 {
		t.Fatalf("normal alert pushed: %d", n)
	}
	f.configure(func(c *restaurantv1.AlertConfig) {
		setRule(c, restaurantv1.AlertRule_ALERT_RULE_CASH_OVER, func(r *restaurantv1.AlertRuleSetting) {
			r.Severity = restaurantv1.AlertSeverity_ALERT_SEVERITY_HIGH
		})
	})
	f.closeShift(f.hari, f.openShift(f.hari, 0), 150_000)
	got := f.stored("CASH_OVER")
	if len(got) != 2 || got[0].Severity != "NORMAL" || got[1].Severity != "HIGH" {
		t.Fatalf("got %+v", got)
	}
	if n := alertPushes(); n != 1 {
		t.Fatalf("pushes = %d", n)
	}
}

func TestPushCapThenOneMoreWaitingAndNoNames(t *testing.T) {
	f := newFixture(t)
	f.configure(func(c *restaurantv1.AlertConfig) {
		c.OpeningHours = closedNow()
		c.DailyPushCap = 2
	})
	for _, id := range []uuid.UUID{f.hariID, f.gitaID, f.bikashID, f.sitaID} {
		f.login(id)
	}
	if n := len(f.stored("AFTER_HOURS_LOGIN")); n != 4 {
		t.Fatalf("alerts = %d", n)
	}
	var kinds []string
	for _, p := range f.pushes() {
		kinds = append(kinds, p.Kind)
	}
	if !slices.Equal(kinds, []string{"ALERT", "ALERT", "MORE"}) {
		t.Fatalf("kinds = %v", kinds)
	}
	res, err := f.al.ListNotifications(f.owner)
	f.must(err)
	text, _ := json.Marshal(res)
	for _, name := range []string{"Hari", "Gita", "Bikash", "Sita"} {
		if strings.Contains(string(text), name) {
			t.Fatalf("push mentions %s: %s", name, text)
		}
	}
}

func TestQuietHoursAlertCollectsNoPush(t *testing.T) {
	f := newFixture(t)
	f.configure(func(c *restaurantv1.AlertConfig) {
		c.OpeningHours = closedNow()
		c.QuietHours = &restaurantv1.QuietHours{From: hoursAround(-time.Hour), To: hoursAround(time.Hour)}
	})
	if n := f.auditCount("alert.notifications_change"); n != 1 {
		t.Fatalf("notifications_change audits = %d", n)
	}
	f.login(f.hariID)
	if n := len(f.stored("AFTER_HOURS_LOGIN")); n != 1 {
		t.Fatalf("alerts = %d", n)
	}
	if p := f.pushes(); len(p) != 0 {
		t.Fatalf("pushes = %+v", p)
	}
}

func TestFailingRuleNeverFailsTheChange(t *testing.T) {
	f := newFixture(t)
	defer alerts.AddFailingRule("shift.open")()
	id := f.openShift(f.hari, 0) // the rule's SQL error is rolled back to its savepoint
	if id == "" {
		t.Fatal("no shift")
	}
	var n int
	f.must(f.pool.QueryRow(context.Background(), `SELECT count(*) FROM shifts`).Scan(&n))
	if n != 1 || f.auditCount("shift.open") != 1 {
		t.Fatalf("shifts = %d, audit = %d", n, f.auditCount("shift.open"))
	}
}

func TestAlertsAreNeverDeleted(t *testing.T) {
	f := newFixture(t)
	f.closeShift(f.hari, f.openShift(f.hari, 100_000), 0)
	if _, err := f.pool.Exec(context.Background(), `DELETE FROM alerts`); err == nil {
		t.Fatal("delete allowed")
	}
}

func TestRealtimeEventsAfterTheRequest(t *testing.T) {
	f := newFixture(t)
	ch, cancel := f.hub.Subscribe(16)
	defer cancel()
	ic := alerts.EventsInterceptor(f.hub)
	run := func(ctx context.Context, fn func(context.Context)) {
		_, _ = ic(func(c context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
			fn(c)
			return nil, nil
		})(ctx, connect.NewRequest(&restaurantv1.GetSessionRequest{}))
	}
	id := f.openShift(f.hari, 100_000)
	run(f.hari, func(c context.Context) { f.closeShift(c, id, 0) })
	select {
	case ev := <-ch:
		if ev.Type != alerts.EventAlertCreated {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no alert event")
	}
}

// ======================================================================= API

// someAlerts: Gita voids 3 (Repeated voids, about Gita; the 3rd also Void after
// kitchen, approved by Sita) and Hari closes a short shift (Cash short, High).
func (f *fixture) someAlerts() {
	f.t.Helper()
	o := f.newOrder(f.gita, f.t1, line("a", f.veg), line("b", f.veg), line("beer", f.beer))
	f.setStatus(o.items["beer"], "PREPARING")
	f.mustVoid(f.gita, o, "a", nil)
	f.mustVoid(f.gita, o, "b", nil)
	f.mustVoid(f.gita, o, "beer", f.approval(f.sitaID))
	time.Sleep(10 * time.Millisecond)
	f.closeShift(f.hari, f.openShift(f.hari, 100_000), 0)
}

func rulesOf(as []*restaurantv1.Alert) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.Rule.String()[len("ALERT_RULE_"):])
	}
	return out
}

func TestOwnerListsFiltersAndGetsLinkedAuditEntries(t *testing.T) {
	f := newFixture(t)
	f.someAlerts()
	list := func(req *restaurantv1.ListAlertsRequest) []*restaurantv1.Alert {
		t.Helper()
		res, err := f.al.ListAlerts(f.owner, req)
		f.must(err)
		return res.Alerts
	}
	all := list(&restaurantv1.ListAlertsRequest{})
	if got := rulesOf(all); !slices.Equal(got, []string{"CASH_SHORT", "VOID_AFTER_KITCHEN", "REPEATED_VOIDS"}) {
		t.Fatalf("all = %v", got)
	}
	if all[2].SubjectName != "Gita Rai" || all[1].ApproverNames[0] != "Sita Gurung" {
		t.Fatalf("names: %+v / %+v", all[2], all[1])
	}
	if got := rulesOf(list(&restaurantv1.ListAlertsRequest{Rule: restaurantv1.AlertRule_ALERT_RULE_CASH_SHORT})); !slices.Equal(got, []string{"CASH_SHORT"}) {
		t.Fatalf("by rule = %v", got)
	}
	if got := rulesOf(list(&restaurantv1.ListAlertsRequest{UserId: f.gitaID.String()})); !slices.Equal(got, []string{"VOID_AFTER_KITCHEN", "REPEATED_VOIDS"}) {
		t.Fatalf("by user = %v", got)
	}
	tomorrow := time.Now().In(ktm).AddDate(0, 0, 1).Format(time.DateOnly)
	if got := list(&restaurantv1.ListAlertsRequest{FromDate: tomorrow, ToDate: tomorrow}); len(got) != 0 {
		t.Fatalf("tomorrow = %v", rulesOf(got))
	}
	counts, err := f.al.GetAlertCounts(f.owner)
	f.must(err)
	if counts.High != 1 || counts.Open != 3 {
		t.Fatalf("counts = %+v", counts)
	}
	detail, err := f.al.GetAlert(f.owner, &restaurantv1.GetAlertRequest{Id: all[0].Id})
	f.must(err)
	if len(detail.AuditEntries) != 1 || detail.AuditEntries[0].Action != "shift.close" || detail.AuditEntries[0].Subject != "Hari Thapa" {
		t.Fatalf("entries = %+v", detail.AuditEntries)
	}
}

func TestResolveOKNeedsNoNoteProblemDoesAuditedNeverDeleted(t *testing.T) {
	f := newFixture(t)
	f.someAlerts()
	res, err := f.al.ListAlerts(f.owner, &restaurantv1.ListAlertsRequest{})
	f.must(err)
	cash, voids := res.Alerts[0], res.Alerts[1]
	_, err = f.al.ResolveAlerts(f.owner, &restaurantv1.ResolveAlertsRequest{Ids: []string{cash.Id}, Status: restaurantv1.AlertStatus_ALERT_STATUS_PROBLEM, Note: " "})
	if reason(t, err) != invalid {
		t.Fatalf("blank problem note: %v", err)
	}
	done, err := f.al.ResolveAlerts(f.owner, &restaurantv1.ResolveAlertsRequest{Ids: []string{cash.Id, voids.Id}, Status: restaurantv1.AlertStatus_ALERT_STATUS_OK})
	f.must(err)
	for _, a := range done.Alerts {
		if a.Status != restaurantv1.AlertStatus_ALERT_STATUS_OK || a.ResolvedByName != "Ram Shrestha" || a.ResolvedAtUnix == 0 {
			t.Fatalf("resolved = %+v", a)
		}
	}
	_, err = f.al.ResolveAlerts(f.owner, &restaurantv1.ResolveAlertsRequest{Ids: []string{cash.Id}, Status: restaurantv1.AlertStatus_ALERT_STATUS_PROBLEM, Note: "Rs 1,000 missing, talked to Hari"})
	f.must(err)
	problems, err := f.al.ListAlerts(f.owner, &restaurantv1.ListAlertsRequest{Status: restaurantv1.AlertStatus_ALERT_STATUS_PROBLEM})
	f.must(err)
	if len(problems.Alerts) != 1 || problems.Alerts[0].Id != cash.Id || problems.Alerts[0].Note != "Rs 1,000 missing, talked to Hari" {
		t.Fatalf("problems = %+v", problems.Alerts)
	}
	all, err := f.al.ListAlerts(f.owner, &restaurantv1.ListAlertsRequest{})
	f.must(err)
	open, err := f.al.ListAlerts(f.owner, &restaurantv1.ListAlertsRequest{Status: restaurantv1.AlertStatus_ALERT_STATUS_OPEN})
	f.must(err)
	if len(all.Alerts) != 3 || len(open.Alerts) != 1 || f.auditCount("alert.resolve") != 3 {
		t.Fatalf("all %d, open %d, audits %d", len(all.Alerts), len(open.Alerts), f.auditCount("alert.resolve"))
	}
	counts, err := f.al.GetAlertCounts(f.owner)
	f.must(err)
	if counts.High != 0 || counts.Open != 1 {
		t.Fatalf("counts = %+v", counts)
	}
	_, err = f.al.ResolveAlerts(f.owner, &restaurantv1.ResolveAlertsRequest{Ids: []string{uuid.NewString()}, Status: restaurantv1.AlertStatus_ALERT_STATUS_OK})
	if reason(t, err) != restaurantv1.ErrorReason_ERROR_REASON_NOT_FOUND {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestStaffCannotReadAlertsManagersOnlyWhenAdded(t *testing.T) {
	f := newFixture(t)
	f.someAlerts()
	for _, ctx := range []context.Context{f.hari, f.gita, f.sita} {
		if _, err := f.al.ListAlerts(ctx, &restaurantv1.ListAlertsRequest{}); reason(t, err) != notAllowed {
			t.Fatalf("list: %v", err)
		}
		if _, err := f.al.GetAlertCounts(ctx); reason(t, err) != notAllowed {
			t.Fatalf("counts: %v", err)
		}
	}
	cfg, err := f.al.GetAlertConfig(f.sita)
	f.must(err)
	if _, err := f.al.UpdateAlertConfig(f.sita, &restaurantv1.UpdateAlertConfigRequest{Config: cfg}); reason(t, err) != notAllowed {
		t.Fatalf("manager update: %v", err)
	}
	if _, err := f.al.GetAlertConfig(f.hari); reason(t, err) != notAllowed {
		t.Fatalf("cashier config: %v", err)
	}
}

func TestManagerRecipientNeverSeesOrResolvesAlertsAboutThemselves(t *testing.T) {
	f := newFixture(t)
	f.someAlerts()
	f.configure(func(c *restaurantv1.AlertConfig) { c.RecipientIds = []string{f.sitaID.String()} })
	seen, err := f.al.ListAlerts(f.sita, &restaurantv1.ListAlertsRequest{})
	f.must(err)
	// Void after kitchen was approved by Sita: hidden from her.
	if got := rulesOf(seen.Alerts); !slices.Equal(got, []string{"CASH_SHORT", "REPEATED_VOIDS"}) {
		t.Fatalf("seen = %v", got)
	}
	hidden := f.stored("VOID_AFTER_KITCHEN")[0].ID.String()
	if _, err := f.al.GetAlert(f.sita, &restaurantv1.GetAlertRequest{Id: hidden}); reason(t, err) != notAllowed {
		t.Fatalf("get hidden: %v", err)
	}
	if _, err := f.al.ResolveAlerts(f.sita, &restaurantv1.ResolveAlertsRequest{Ids: []string{hidden}, Status: restaurantv1.AlertStatus_ALERT_STATUS_OK}); reason(t, err) != notAllowed {
		t.Fatalf("resolve hidden: %v", err)
	}
	_, err = f.al.ResolveAlerts(f.sita, &restaurantv1.ResolveAlertsRequest{Ids: []string{seen.Alerts[0].Id}, Status: restaurantv1.AlertStatus_ALERT_STATUS_OK})
	f.must(err)
	// Only the owner changes a resolution, rules or recipients.
	_, err = f.al.ResolveAlerts(f.sita, &restaurantv1.ResolveAlertsRequest{Ids: []string{seen.Alerts[0].Id}, Status: restaurantv1.AlertStatus_ALERT_STATUS_PROBLEM, Note: "x"})
	if reason(t, err) != notAllowed {
		t.Fatalf("re-resolve: %v", err)
	}
	counts, err := f.al.GetAlertCounts(f.sita)
	f.must(err)
	if counts.High != 0 || counts.Open != 1 {
		t.Fatalf("counts = %+v", counts)
	}
	// Recipients must be active managers.
	c := f.config()
	c.RecipientIds = []string{f.hariID.String()}
	if _, err := f.al.UpdateAlertConfig(f.owner, &restaurantv1.UpdateAlertConfigRequest{Config: c}); reason(t, err) != invalid {
		t.Fatalf("cashier recipient: %v", err)
	}
}

func TestNotificationsGoToOwnerAndRecipientsUnlessAboutThem(t *testing.T) {
	f := newFixture(t)
	f.configure(func(c *restaurantv1.AlertConfig) {
		c.RecipientIds = []string{f.sitaID.String()}
		c.OpeningHours = closedNow()
	})
	f.login(f.sitaID) // after-hours login by Sita
	f.login(f.hariID)
	f.login(f.ownerID)
	forOwner, err := f.al.ListNotifications(f.owner)
	f.must(err)
	if len(forOwner.Pushes) != 3 { // incl. the owner's own login
		t.Fatalf("owner pushes = %d", len(forOwner.Pushes))
	}
	forSita, err := f.al.ListNotifications(f.sita)
	f.must(err)
	sitaAlert := ""
	for _, a := range f.stored("AFTER_HOURS_LOGIN") {
		if subjectOf(a) == f.sitaID {
			sitaAlert = a.ID.String()
		}
	}
	if len(forSita.Pushes) != 2 {
		t.Fatalf("sita pushes = %d", len(forSita.Pushes))
	}
	for _, p := range forSita.Pushes {
		if p.AlertId == sitaAlert {
			t.Fatal("Sita got the push about herself")
		}
	}
}

func TestDaySummary(t *testing.T) {
	f := newFixture(t)
	f.someAlerts()
	o := f.newOrder(f.gita, f.t3, line("veg", f.veg))
	f.openShift(f.hari, 0)
	inv := f.pay(f.hari, o, pct(1000, "Regular", nil))
	sum, err := f.al.GetDaySummary(f.owner, &restaurantv1.GetDaySummaryRequest{Date: today()})
	f.must(err)
	if sum.SalesPaisa != inv.TotalPaisa || sum.Bills != 1 || sum.DiscountsPaisa != 1800 || sum.VoidCount != 3 || sum.VoidValuePaisa != 18_000*2+55_000 ||
		sum.CancelledCount != 0 || sum.OpenAlerts != 3 || sum.HighAlerts != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	if len(sum.ByMethod) != 1 || sum.ByMethod[0].Method != restaurantv1.PaymentMethod_PAYMENT_METHOD_CASH || sum.ByMethod[0].TotalPaisa != inv.TotalPaisa {
		t.Fatalf("by method = %+v", sum.ByMethod)
	}
	if len(sum.Shifts) != 2 || sum.Shifts[0].UserName != "Hari Thapa" || sum.Shifts[0].GetDifferencePaisa() != -100_000 || sum.Shifts[1].DifferencePaisa != nil {
		t.Fatalf("shifts = %+v", sum.Shifts)
	}
	if sum.Alerts[0].Rule != restaurantv1.AlertRule_ALERT_RULE_CASH_SHORT || sum.Alerts[0].Severity != restaurantv1.AlertSeverity_ALERT_SEVERITY_HIGH || sum.Alerts[0].Total != 1 {
		t.Fatalf("alerts = %+v", sum.Alerts)
	}
	yesterday := time.Now().In(ktm).AddDate(0, 0, -1).Format(time.DateOnly)
	prev, err := f.al.GetDaySummary(f.owner, &restaurantv1.GetDaySummaryRequest{Date: yesterday})
	f.must(err)
	if prev.Bills != 0 {
		t.Fatalf("yesterday bills = %d", prev.Bills)
	}
}

func TestClosingTheLastOpenShiftSendsTheSummary(t *testing.T) {
	f := newFixture(t)
	f.closeShift(f.hari, f.openShift(f.hari, 0), 0)
	p := f.pushes()
	if len(p) != 1 || p[0].Kind != "SUMMARY" || p[0].Rule != "" {
		t.Fatalf("pushes = %+v", p)
	}
}

func TestSummaryTimerAfterClosingPlus30(t *testing.T) {
	f := newFixture(t)
	// Closed an hour ago (opened 13 h ago): the summary is due.
	f.configure(func(c *restaurantv1.AlertConfig) {
		c.OpeningHours = everyDay(hoursAround(-13*time.Hour), hoursAround(-time.Hour))
	})
	summaries := func() (n int, todays bool) {
		for _, p := range f.pushes() {
			if p.Kind == "SUMMARY" {
				n++
				todays = todays || p.BusinessDate.Time.Format(time.DateOnly) == time.Now().Add(-13*time.Hour).In(ktm).Format(time.DateOnly)
			}
		}
		return n, todays
	}
	f.must(f.al.SendDueSummaries(context.Background(), time.Now()))
	n, todays := summaries()
	if n == 0 || !todays {
		t.Fatalf("summaries = %d (today's: %v)", n, todays)
	}
	f.must(f.al.SendDueSummaries(context.Background(), time.Now())) // once per business day
	if again, _ := summaries(); again != n {
		t.Fatalf("summaries %d → %d", n, again)
	}
	// An open shift holds it back.
	f.must(f.pool.QueryRow(context.Background(), `DELETE FROM alert_pushes RETURNING 1`).Scan(new(int)))
	f.openShift(f.hari, 0)
	f.must(f.al.SendDueSummaries(context.Background(), time.Now()))
	if again, _ := summaries(); again != 0 {
		t.Fatalf("summary with a shift open: %d", again)
	}
}

func TestNonRecipientManagerGetsNoNotificationsRatherThanAnError(t *testing.T) {
	f := newFixture(t)
	res, err := f.al.ListNotifications(f.sita)
	f.must(err)
	if len(res.Pushes) != 0 {
		t.Fatalf("pushes = %d", len(res.Pushes))
	}
	if _, err := f.al.ListNotifications(f.hari); reason(t, err) != notAllowed {
		t.Fatalf("cashier: %v", err)
	}
}
