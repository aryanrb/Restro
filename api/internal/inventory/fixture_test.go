package inventory_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/catalog"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/inventory"
	"github.com/oneaccord/restro/api/internal/orders"
)

// Quantities are thousandths of the base unit; unit cost is paisa/base unit ×10,000.
const (
	g    = int64(1000)   // 1 g (or 1 ml / 1 pcs) in milli
	rs60 = int64(600000) // 60.0000 paisa per g (Rs 600/kg)
)

// fixture mirrors the web fake's test world: an owner, a manager (Sita, PIN 2222),
// a cashier (Hari), a waiter (Gita) and kitchen staff (Bikash); a "Chicken Momo"
// with an optional extras group (Cheese), a "Dal Bhat Set" with Veg/Chicken
// variants and a "Veg Momo"; Chicken (tracked, 5000 g, Rs 60/g... paisa) and Flour
// (untracked) stock items, and the momo recipe: chicken 120 g + flour 80 g.
type fixture struct {
	t       *testing.T
	pool    *pgxpool.Pool
	q       *sqlc.Queries
	authSvc *auth.Service
	inv     *inventory.Service
	cat     *catalog.Service
	ord     *orders.Service

	owner, manager, cashier, waiter, kitchen context.Context
	ownerID, managerID, cashierID, waiterID  uuid.UUID
	kitchenID                                uuid.UUID

	table                  uuid.UUID
	momo, dal, vegMomo     uuid.UUID
	vegVariant, chkVariant uuid.UUID
	cheese, extras         uuid.UUID

	chicken, flour string
	momoVersion    int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run inventory integration tests")
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
	if _, err := pool.Exec(ctx, `TRUNCATE users, sessions, devices, audit_log, processed_commands,
		orders, order_items, order_item_modifiers, invoices, invoice_lines, payments, cbms_outbox, shifts,
		menu_items, menu_categories, stations, dining_tables, areas, modifier_groups,
		inventory_items, stock_movements, waste_entries, count_lists, stock_counts, stock_count_lines
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	q := sqlc.New(pool)
	authSvc := auth.NewService(pool, zap.NewNop())
	f := &fixture{
		t: t, pool: pool, q: q, authSvc: authSvc,
		inv: inventory.NewService(pool, nil, authSvc, zap.NewNop()),
		cat: catalog.NewService(pool, nil, zap.NewNop()),
		ord: orders.NewService(pool, nil, authSvc, zap.NewNop()),
	}
	f.owner, f.ownerID = f.user("Owner", auth.RoleOwner, "1111")
	f.manager, f.managerID = f.user("Sita Gurung", auth.RoleManager, "2222")
	f.cashier, f.cashierID = f.user("Hari Thapa", auth.RoleCashier, "3333")
	f.waiter, f.waiterID = f.user("Gita", auth.RoleWaiter, "4444")
	f.kitchen, f.kitchenID = f.user("Bikash Tamang", auth.RoleKitchen, "5555")

	st, err := q.CreateStation(ctx, sqlc.CreateStationParams{NameEn: "Kitchen"})
	f.must(err)
	cat, err := q.CreateCategory(ctx, sqlc.CreateCategoryParams{NameEn: "Food", Active: true})
	f.must(err)
	mkItem := func(name string, sort int32) uuid.UUID {
		it, err := q.CreateItem(ctx, sqlc.CreateItemParams{CategoryID: cat.ID, NameEn: name, NameNe: name + " (ne)", PricePaisa: 25000, StationID: st.ID, Active: true, Sort: sort})
		f.must(err)
		return it.ID
	}
	f.momo = mkItem("Chicken Momo", 1)
	f.vegMomo = mkItem("Veg Momo", 2)
	f.dal = mkItem("Dal Bhat Set", 3)
	v1, err := q.CreateVariant(ctx, sqlc.CreateVariantParams{ItemID: f.dal, NameEn: "Veg", PricePaisa: 30000, Sort: 0})
	f.must(err)
	v2, err := q.CreateVariant(ctx, sqlc.CreateVariantParams{ItemID: f.dal, NameEn: "Chicken", PricePaisa: 40000, Sort: 1})
	f.must(err)
	f.vegVariant, f.chkVariant = v1.ID, v2.ID
	grp, err := q.CreateModifierGroup(ctx, sqlc.CreateModifierGroupParams{NameEn: "Extras", MinSelect: 0, MaxSelect: 2})
	f.must(err)
	f.extras = grp.ID
	ch, err := q.CreateModifier(ctx, sqlc.CreateModifierParams{GroupID: grp.ID, NameEn: "Cheese", PriceDeltaPaisa: 5000})
	f.must(err)
	f.cheese = ch.ID
	f.must(q.CreateItemModifierLink(ctx, sqlc.CreateItemModifierLinkParams{ItemID: f.momo, GroupID: grp.ID}))
	area, err := q.CreateArea(ctx, sqlc.CreateAreaParams{NameEn: "Hall"})
	f.must(err)
	tb, err := q.CreateTable(ctx, sqlc.CreateTableParams{AreaID: area.ID, Name: "T1", Seats: 4, Active: true})
	f.must(err)
	f.table = tb.ID

	f.chicken = f.item("Chicken", "g", rs60, 3000*g, ptr(5000*g)).Id
	f.flour = f.item("Flour", "g", 50000, 0, nil).Id // 5 paisa/g, untracked
	saved, err := f.cat.SaveRecipe(f.owner, &restaurantv1.SaveRecipeRequest{
		MenuItemId: f.momo.String(), BaseVersion: 1,
		Recipe: []*restaurantv1.RecipeLine{rl(f.chicken, 120*g), rl(f.flour, 80*g)},
	})
	f.must(err)
	f.momoVersion = saved.Version
	return f
}

func (f *fixture) must(err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
}

func ptr[T any](v T) *T { return &v }

func rl(id string, qty int64) *restaurantv1.RecipeLine {
	return &restaurantv1.RecipeLine{InventoryItemId: id, QuantityMilli: qty}
}

// user creates a staff member with a PIN and returns a context carrying their
// session, resolved through the auth interceptor exactly as a request would be.
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

// hashes caches argon2 hashes across tests (hashing is deliberately slow).
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

// item creates a stock item as the owner, tracked with an opening count when given.
func (f *fixture) item(name, unit string, costE4, low int64, opening *int64) *restaurantv1.InventoryItem {
	f.t.Helper()
	created, err := f.inv.SaveInventoryItem(f.owner, &restaurantv1.SaveInventoryItemRequest{Input: input(name, unit, costE4, low)})
	f.must(err)
	if opening == nil {
		return created
	}
	tracked, err := f.inv.SetTracked(f.owner, &restaurantv1.SetTrackedRequest{Id: created.Id, Tracked: true, OpeningMilli: opening})
	f.must(err)
	return tracked
}

func input(name, unit string, costE4, low int64) *restaurantv1.InventoryItemInput {
	u := map[string]restaurantv1.StockUnit{"g": restaurantv1.StockUnit_STOCK_UNIT_G, "ml": restaurantv1.StockUnit_STOCK_UNIT_ML, "pcs": restaurantv1.StockUnit_STOCK_UNIT_PCS}[unit]
	return &restaurantv1.InventoryItemInput{Name: &restaurantv1.LocalText{En: name, Ne: name + " (ne)"}, Unit: u, LowStockMilli: low, Active: true, UnitCostE4: costE4}
}

func (f *fixture) onHand(id string) int64 {
	f.t.Helper()
	list, err := f.inv.ListInventoryItems(f.owner, true)
	f.must(err)
	for _, it := range list {
		if it.Id == id {
			return it.OnHandMilli
		}
	}
	f.t.Fatalf("item %s not listed", id)
	return 0
}

// movements of one stock item, oldest first.
func (f *fixture) movements(id string) []*restaurantv1.StockMovement {
	f.t.Helper()
	list, err := f.inv.ListMovements(f.owner, &restaurantv1.ListMovementsRequest{InventoryItemId: id})
	f.must(err)
	out := make([]*restaurantv1.StockMovement, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- {
		out = append(out, list[i])
	}
	return out
}

func (f *fixture) movementsOfType(id string, typ restaurantv1.StockMovementType) []*restaurantv1.StockMovement {
	var out []*restaurantv1.StockMovement
	for _, m := range f.movements(id) {
		if m.Type == typ {
			out = append(out, m)
		}
	}
	return out
}

func (f *fixture) auditActions() []string {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT action FROM audit_log ORDER BY id`)
	f.must(err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		f.must(rows.Scan(&a))
		out = append(out, a)
	}
	return out
}

type auditRow struct {
	action, reason string
	approver       uuid.UUID
	actor          uuid.UUID
}

func (f *fixture) lastAudit() auditRow {
	f.t.Helper()
	var r auditRow
	var reason *string
	var approver, actor *uuid.UUID
	f.must(f.pool.QueryRow(context.Background(), `SELECT action, reason, approver_user_id, actor_user_id FROM audit_log ORDER BY id DESC LIMIT 1`).Scan(&r.action, &reason, &approver, &actor))
	if reason != nil {
		r.reason = *reason
	}
	if approver != nil {
		r.approver = *approver
	}
	if actor != nil {
		r.actor = *actor
	}
	return r
}

// newOrder opens a dine-in order (as the waiter) and returns its client id.
func (f *fixture) newOrder(ctx context.Context) string {
	f.t.Helper()
	ocid := uuid.NewString()
	tid := f.table.String()
	_, err := f.ord.CreateOrder(ctx, &restaurantv1.CreateOrderRequest{
		CommandId: uuid.NewString(), OrderClientId: ocid, Type: restaurantv1.OrderType_ORDER_TYPE_DINE_IN, TableId: &tid, GuestCount: 1,
	})
	f.must(err)
	return ocid
}

func line(item uuid.UUID, qty int32, variant *uuid.UUID, mods ...uuid.UUID) *restaurantv1.NewItem {
	ni := &restaurantv1.NewItem{ClientId: uuid.NewString(), MenuItemId: item.String(), Quantity: qty}
	if variant != nil {
		v := variant.String()
		ni.VariantId = &v
	}
	for _, m := range mods {
		ni.ModifierIds = append(ni.ModifierIds, m.String())
	}
	return ni
}

// send adds lines already SENT (one-step send) on the order.
func (f *fixture) send(ctx context.Context, ocid string, lines ...*restaurantv1.NewItem) *restaurantv1.Order {
	f.t.Helper()
	res, err := f.ord.AddItems(ctx, &restaurantv1.AddItemsRequest{CommandId: uuid.NewString(), OrderClientId: ocid, Items: lines, Send: true})
	f.must(err)
	if len(res.Rejected) > 0 {
		f.t.Fatalf("rejected: %v", res.Rejected)
	}
	return res.Order
}

func (f *fixture) setLineStatus(itemID, status string) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), `UPDATE order_items SET status = $2 WHERE id = $1`, itemID, status); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) version(ctx context.Context, ocid string) int64 {
	f.t.Helper()
	o, err := f.ord.GetOrder(ctx, &restaurantv1.GetOrderRequest{ClientId: &ocid})
	f.must(err)
	return o.Version
}

func reasonOf(err error) restaurantv1.ErrorReason {
	var ae *connecterr.Error
	if errors.As(err, &ae) {
		return ae.Reason
	}
	return restaurantv1.ErrorReason_ERROR_REASON_UNSPECIFIED
}

func wantReason(t *testing.T, err error, reason restaurantv1.ErrorReason) *connecterr.Error {
	t.Helper()
	var ae *connecterr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("want %v, got %v", reason, err)
	}
	if ae.Reason != reason {
		t.Fatalf("reason = %v (%q), want %v", ae.Reason, ae.Message, reason)
	}
	return ae
}

func wantInvalid(t *testing.T, err error, field string) {
	t.Helper()
	ae := wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID)
	if ae.Field != field {
		t.Fatalf("field = %q (%q), want %q", ae.Field, ae.Message, field)
	}
}
