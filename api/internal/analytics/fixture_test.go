package analytics

import (
	"context"
	"errors"
	"os"
	"regexp"
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

// fixture: a clean DB with staff (owner Ram, manager Sita, cashier Hari, waiter
// Gita), a small menu (kitchen + bar), a floor (T1–T3) and an open shift.
type fixture struct {
	t                         *testing.T
	ctx                       context.Context
	pool                      *pgxpool.Pool
	svc                       *Service
	authSvc                   *auth.Service
	owner, sita, hari, gita   context.Context
	ownerID, sitaID           uuid.UUID
	hariID, gitaID            uuid.UUID
	kitchen, bar              uuid.UUID
	food, drinks              uuid.UUID
	veg, chow, thuk, lassi    uuid.UUID
	coke                      uuid.UUID
	area                      uuid.UUID
	t1, t2, t3                uuid.UUID
	shift                     uuid.UUID
	today, yesterday, weekAgo time.Time
	n                         int32
}

var calendarSeed = regexp.MustCompile(`(?s)INSERT INTO calendar_events .*?;\n`)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run analytics integration tests")
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
		repayments, customers, menu_items, menu_categories, stations, dining_tables, areas, stock_movements, inventory_items,
		analytics_days, analytics_dirty_days, calendar_events RESTART IDENTITY CASCADE`)
	// Built-in festivals, exactly as migration 014 seeds them.
	mig, err := os.ReadFile("../../db/migrations/014_analytics.sql")
	if err != nil {
		t.Fatal(err)
	}
	f.exec(calendarSeed.FindString(string(mig)))
	f.exec(`UPDATE analytics_settings SET managers_see_staff = '{}', highlight_rules = true`)

	f.owner, f.ownerID = f.as(auth.RoleOwner, "Ram Owner")
	f.sita, f.sitaID = f.as(auth.RoleManager, "Sita Gurung")
	f.hari, f.hariID = f.as(auth.RoleCashier, "Hari Thapa")
	f.gita, f.gitaID = f.as(auth.RoleWaiter, "Gita Rai")

	f.kitchen = f.id(`INSERT INTO stations (name_en, sort) VALUES ('Kitchen', 1) RETURNING id`)
	f.bar = f.id(`INSERT INTO stations (name_en, sort) VALUES ('Bar', 2) RETURNING id`)
	f.food = f.id(`INSERT INTO menu_categories (name_en, sort) VALUES ('Food', 1) RETURNING id`)
	f.drinks = f.id(`INSERT INTO menu_categories (name_en, sort) VALUES ('Drinks', 2) RETURNING id`)
	f.veg = f.menuItem("Veg Momo", f.food, f.kitchen, 18_000)
	f.chow = f.menuItem("Chowmein", f.food, f.kitchen, 15_000)
	f.thuk = f.menuItem("Thukpa", f.food, f.kitchen, 22_000)
	f.lassi = f.menuItem("Lassi", f.drinks, f.bar, 12_000)
	f.coke = f.menuItem("Coke", f.drinks, f.bar, 8_000)
	f.area = f.id(`INSERT INTO areas (name_en, sort) VALUES ('Main', 1) RETURNING id`)
	f.t1 = f.id(`INSERT INTO dining_tables (area_id, name, seats, sort) VALUES ($1, 'T1', 4, 1) RETURNING id`, f.area)
	f.t2 = f.id(`INSERT INTO dining_tables (area_id, name, seats, sort) VALUES ($1, 'T2', 4, 2) RETURNING id`, f.area)
	f.t3 = f.id(`INSERT INTO dining_tables (area_id, name, seats, sort) VALUES ($1, 'T3', 2, 3) RETURNING id`, f.area)
	f.shift = f.id(`INSERT INTO shifts (cashier_user_id, opened_at) VALUES ($1, now() - interval '3 days') RETURNING id`, f.hariID)

	f.today = ktmDay(time.Now())
	f.yesterday = addDays(f.today, -1)
	f.weekAgo = addDays(f.today, -7)
	return f
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

func (f *fixture) menuItem(name string, cat, station uuid.UUID, price int64) uuid.UUID {
	return f.id(`INSERT INTO menu_items (category_id, name_en, price_paisa, station_id) VALUES ($1, $2, $3, $4) RETURNING id`, cat, name, price, station)
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
	req := connect.NewRequest(&restaurantv1.GetAnalyticsMetaRequest{})
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

// at is a time on business day d: `frac` of the way through the part of the day
// that has passed (today) or of the whole day (past days), so "today" times are
// always in the past and on today, whatever the time of day the test runs.
func (f *fixture) at(d time.Time, frac float64) time.Time {
	start := startNPT(d)
	span := 24 * time.Hour
	if d.Equal(f.today) {
		span = time.Since(start)
	}
	return start.Add(time.Duration(float64(span) * frac))
}

type order struct {
	typ     string
	table   uuid.UUID // uuid.Nil = none
	label   string
	waiter  uuid.UUID
	guests  int32
	created time.Time
	status  string
}

func (f *fixture) order(o order) uuid.UUID {
	f.t.Helper()
	if o.status == "" {
		o.status = "OPEN"
	}
	return f.id(`INSERT INTO orders (client_id, type, table_id, waiter_user_id, guest_count, created_at, status, label)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		uuid.New(), o.typ, dbx.UUIDOpt(o.table), o.waiter, o.guests, o.created, o.status, o.label)
}

type item struct {
	order      uuid.UUID
	menu       uuid.UUID
	name       string
	price      int64
	qty        int32
	station    uuid.UUID
	status     string
	sent       time.Time
	addedBy    uuid.UUID
	voidedBy   uuid.UUID
	approvedBy uuid.UUID
	reason     string
}

func (f *fixture) item(i item) uuid.UUID {
	f.t.Helper()
	if i.qty == 0 {
		i.qty = 1
	}
	if i.station == uuid.Nil {
		i.station = f.kitchen
	}
	return f.id(`INSERT INTO order_items (order_id, client_id, menu_item_id, name_en, unit_price_paisa, quantity, station_id, status,
		sent_at, added_by_user_id, voided_by_user_id, void_approved_by_user_id, void_reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING id`,
		i.order, uuid.New(), i.menu, i.name, i.price, i.qty, i.station, i.status, i.sent, dbx.UUIDOpt(i.addedBy),
		dbx.UUIDOpt(i.voidedBy), dbx.UUIDOpt(i.approvedBy), i.reason)
}

type bill struct {
	order    uuid.UUID
	at       time.Time
	items    []uuid.UUID
	discount int64
	reason   string
	by       uuid.UUID
	method   string
	customer uuid.UUID // ON_ACCOUNT payment when set
}

// bill issues an invoice for the given items (total = their value − discount),
// paid in full by one payment, and attaches the items to it.
func (f *fixture) bill(b bill) (uuid.UUID, int64) {
	f.t.Helper()
	if b.method == "" {
		b.method = "CASH"
	}
	var subtotal int64
	for _, it := range b.items {
		var v int64
		if err := f.pool.QueryRow(f.ctx, `SELECT unit_price_paisa * quantity FROM order_items WHERE id = $1`, it).Scan(&v); err != nil {
			f.t.Fatal(err)
		}
		subtotal += v
	}
	total := subtotal - b.discount
	f.n++
	inv := f.id(`INSERT INTO invoices (fiscal_year, number, mode, bs_date, bs_year, bs_month, bs_day, ad_datetime, order_id,
		subtotal_paisa, discount_paisa, service_charge_paisa, taxable_paisa, vat_paisa, total_paisa, discount_reason, created_by_user_id)
		VALUES ('2083.084', $1, 'ESTIMATE', '2083.08.10', 2083, 8, 10, $2, $3, $4, $5, 0, $6, 0, $6, $7, $8) RETURNING id`,
		f.n, b.at, b.order, subtotal, b.discount, total, b.reason, b.by)
	for _, it := range b.items {
		f.exec(`INSERT INTO invoice_lines (invoice_id, order_item_id, name_en, quantity, unit_price_paisa, line_total_paisa)
			SELECT $1, id, name_en, quantity, unit_price_paisa, unit_price_paisa * quantity FROM order_items WHERE id = $2`, inv, it)
		f.exec(`UPDATE order_items SET invoice_id = $1 WHERE id = $2`, inv, it)
	}
	if b.customer != uuid.Nil {
		f.exec(`INSERT INTO payments (invoice_id, method, amount_paisa, confirmed_by_user_id, shift_id, customer_id, customer_name, created_at)
			VALUES ($1, 'ON_ACCOUNT', $2, $3, $4, $5, 'x', $6)`, inv, total, b.by, f.shift, b.customer, b.at)
	} else {
		f.exec(`INSERT INTO payments (invoice_id, method, amount_paisa, confirmed_by_user_id, shift_id, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)`, inv, b.method, total, b.by, f.shift, b.at)
	}
	return inv, total
}

func (f *fixture) cancel(inv, by, approver uuid.UUID, at time.Time) {
	f.exec(`UPDATE invoices SET status = 'CANCELLED', cancel_reason = 'Wrong table', cancelled_by_user_id = $2,
		cancel_approved_by_user_id = $3, cancelled_at = $4 WHERE id = $1`, inv, by, approver, at)
	f.exec(`UPDATE order_items SET invoice_id = NULL WHERE invoice_id = $1`, inv)
}

func reasonOf(err error) restaurantv1.ErrorReason {
	var ce *connecterr.Error
	if errors.As(err, &ce) {
		return ce.Reason
	}
	return restaurantv1.ErrorReason_ERROR_REASON_UNSPECIFIED
}

func (f *fixture) rng(from, to time.Time) *restaurantv1.AnalyticsRange {
	return &restaurantv1.AnalyticsRange{From: dayStr(from), To: dayStr(to)}
}

// scenario mirrors the web fake's analytics test data:
//   - a: T1 (Gita, 2 guests, opened 45 min before its bill… or as much of today
//     as has passed): veg momo + chowmein, cash, today, issued by Hari
//   - b: takeaway #1, lassi ×2, 10% "Regular" discount, Fonepay, today
//   - c: T2 (3 guests) yesterday: veg momo, and a thukpa voided before cooking; cash
type scenario struct {
	a, b, c                   uuid.UUID
	aTotal, bTotal, cTotal    int64
	o1, o2, o3                uuid.UUID
	o1Created, aAt, bAt       time.Time
	cAt                       time.Time
	bDiscount, dwellA, dwellC int64
}

func (f *fixture) scenario() *scenario { return f.scenarioWith(f.today) }

// scenarioWith puts the takeaway order b on bDay instead of today.
func (f *fixture) scenarioWith(bDay time.Time) *scenario {
	f.t.Helper()
	s := &scenario{}
	s.o1Created, s.aAt = f.at(f.today, 0.2), f.at(f.today, 0.6)
	s.o1 = f.order(order{typ: "DINE_IN", table: f.t1, waiter: f.gitaID, guests: 2, created: s.o1Created, status: "CLOSED"})
	veg := f.item(item{order: s.o1, menu: f.veg, name: "Veg Momo", price: 18_000, status: "SERVED", sent: s.o1Created.Add(time.Second), addedBy: f.gitaID})
	chow := f.item(item{order: s.o1, menu: f.chow, name: "Chowmein", price: 15_000, status: "SERVED", sent: s.o1Created.Add(time.Second), addedBy: f.gitaID})
	s.a, s.aTotal = f.bill(bill{order: s.o1, at: s.aAt, items: []uuid.UUID{veg, chow}, by: f.hariID})

	bCreated := f.at(bDay, 0.3)
	s.bAt = f.at(bDay, 0.7)
	if !bDay.Equal(f.today) { // the same time of day as today's bill a
		shift := bDay.Sub(f.today)
		bCreated, s.bAt = s.o1Created.Add(shift), s.aAt.Add(shift)
	}
	s.o2 = f.order(order{typ: "TAKEAWAY", label: "#1", waiter: f.gitaID, created: bCreated, status: "CLOSED"})
	lassi := f.item(item{order: s.o2, menu: f.lassi, name: "Lassi", price: 12_000, qty: 2, station: f.bar, status: "SERVED", sent: bCreated, addedBy: f.gitaID})
	s.bDiscount = 2_400
	s.b, s.bTotal = f.bill(bill{order: s.o2, at: s.bAt, items: []uuid.UUID{lassi}, discount: s.bDiscount, reason: "Regular", by: f.hariID, method: "QR_FONEPAY"})

	cCreated := f.at(f.yesterday, 0.5)
	s.cAt = cCreated.Add(50 * time.Minute)
	s.o3 = f.order(order{typ: "DINE_IN", table: f.t2, waiter: f.gitaID, guests: 3, created: cCreated, status: "CLOSED"})
	veg3 := f.item(item{order: s.o3, menu: f.veg, name: "Veg Momo", price: 18_000, status: "SERVED", sent: cCreated, addedBy: f.gitaID})
	f.item(item{order: s.o3, menu: f.thuk, name: "Thukpa", price: 22_000, status: "VOIDED", sent: cCreated, addedBy: f.gitaID, voidedBy: f.gitaID, reason: "Changed mind"})
	s.c, s.cTotal = f.bill(bill{order: s.o3, at: s.cAt, items: []uuid.UUID{veg3}, by: f.hariID})
	s.dwellA = int64(s.aAt.Sub(s.o1Created).Round(time.Minute) / time.Minute)
	s.dwellC = 50
	// Inserting yesterday's rows marked yesterday dirty; analytics rebuilds it on read.
	return s
}
