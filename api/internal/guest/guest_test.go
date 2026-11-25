package guest_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

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
	"github.com/oneaccord/restro/api/internal/guest"
)

type fixture struct {
	t       *testing.T
	pool    *pgxpool.Pool
	q       *sqlc.Queries
	authSvc *auth.Service
	svc     *guest.Service

	owner, waiter, cashier context.Context
	tableID                uuid.UUID
	code                   string
	momo, pizza, large     uuid.UUID
	cheese                 uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run guest integration tests")
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
	if _, err := pool.Exec(ctx, `TRUNCATE guest_requests, users, sessions, devices, audit_log, processed_commands,
		orders, order_items, order_item_modifiers, modifier_groups, modifiers, menu_items, menu_categories, stations,
		dining_tables, areas RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE settings SET guest_ordering = true, service_charge_enabled = false,
		vat_registered = false, round_total_to_rupee = false`); err != nil {
		t.Fatalf("settings: %v", err)
	}
	q := sqlc.New(pool)
	f := &fixture{t: t, pool: pool, q: q, authSvc: auth.NewService(pool, zap.NewNop())}
	f.svc = guest.NewService(pool, nil, zap.NewNop())
	f.owner = f.user(auth.RoleOwner)
	f.waiter = f.user(auth.RoleWaiter)
	f.cashier = f.user(auth.RoleCashier)

	st, err := q.CreateStation(ctx, sqlc.CreateStationParams{NameEn: "Kitchen"})
	must(t, err)
	cat, err := q.CreateCategory(ctx, sqlc.CreateCategoryParams{NameEn: "Food", Active: true})
	must(t, err)
	momo, err := q.CreateItem(ctx, sqlc.CreateItemParams{CategoryID: cat.ID, NameEn: "Momo", PricePaisa: 25000, StationID: st.ID, Active: true})
	must(t, err)
	pizza, err := q.CreateItem(ctx, sqlc.CreateItemParams{CategoryID: cat.ID, NameEn: "Pizza", PricePaisa: 1, StationID: st.ID, Active: true})
	must(t, err)
	large, err := q.CreateVariant(ctx, sqlc.CreateVariantParams{ItemID: pizza.ID, NameEn: "Large", PricePaisa: 60000})
	must(t, err)
	g, err := q.CreateModifierGroup(ctx, sqlc.CreateModifierGroupParams{NameEn: "Extras", MinSelect: 0, MaxSelect: 2})
	must(t, err)
	cheese, err := q.CreateModifier(ctx, sqlc.CreateModifierParams{GroupID: g.ID, NameEn: "Cheese", PriceDeltaPaisa: 5000})
	must(t, err)
	must(t, q.CreateItemModifierLink(ctx, sqlc.CreateItemModifierLinkParams{ItemID: pizza.ID, GroupID: g.ID}))
	area, err := q.CreateArea(ctx, sqlc.CreateAreaParams{NameEn: "Hall"})
	must(t, err)
	tb, err := q.CreateTable(ctx, sqlc.CreateTableParams{AreaID: area.ID, Name: "T4", Seats: 4, Active: true})
	must(t, err)
	f.tableID, f.momo, f.pizza, f.large, f.cheese = tb.ID, momo.ID, pizza.ID, large.ID, cheese.ID
	f.code = f.codeOf(tb.ID)
	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) codeOf(tableID uuid.UUID) string {
	f.t.Helper()
	res, err := f.svc.ListTables(f.owner)
	must(f.t, err)
	for _, c := range res.Tables {
		if c.TableId == tableID.String() {
			return c.Code
		}
	}
	f.t.Fatal("table not listed")
	return ""
}

func (f *fixture) user(role string) context.Context {
	f.t.Helper()
	ctx := context.Background()
	hash, err := auth.HashSecret("secret123")
	must(f.t, err)
	username := "u-" + uuid.NewString()[:8]
	_, err = f.q.CreateUser(ctx, sqlc.CreateUserParams{
		Name: role + " user", Role: role, Username: dbx.Text(username), PasswordHash: dbx.Text(hash), Language: "en", Active: true,
	})
	must(f.t, err)
	_, token, err := f.authSvc.PasswordLogin(ctx, username, "secret123")
	must(f.t, err)
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
	return out
}

func reasonOf(err error) restaurantv1.ErrorReason {
	var ae *connecterr.Error
	if errors.As(err, &ae) {
		return ae.Reason
	}
	return restaurantv1.ErrorReason_ERROR_REASON_UNSPECIFIED
}

func wantReason(t *testing.T, err error, want restaurantv1.ErrorReason) {
	t.Helper()
	if got := reasonOf(err); got != want {
		t.Fatalf("err = %v (reason %v), want %v", err, got, want)
	}
}

func (f *fixture) submit(lines ...*restaurantv1.GuestLineInput) (*restaurantv1.GuestRequestView, error) {
	return f.svc.SubmitOrder(context.Background(), &restaurantv1.SubmitGuestOrderRequest{
		TableCode: f.code, RequestId: uuid.NewString(), Lines: lines,
	}, "")
}

func momoLine(f *fixture, qty int32) *restaurantv1.GuestLineInput {
	return &restaurantv1.GuestLineInput{MenuItemId: f.momo.String(), Quantity: qty}
}

// Guest submit: prices come from the server, retries are idempotent, 86'd items and
// a switched-off table are refused, and at most 3 orders wait per table.
func TestSubmitPricesOnServer(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	large := f.large.String()
	req := &restaurantv1.SubmitGuestOrderRequest{TableCode: f.code, RequestId: uuid.NewString(), Lines: []*restaurantv1.GuestLineInput{
		{MenuItemId: f.pizza.String(), VariantId: &large, ModifierIds: []string{f.cheese.String()}, Quantity: 2, Note: "no onion"},
		momoLine(f, 1),
	}}
	v, err := f.svc.SubmitOrder(ctx, req, "")
	must(t, err)
	if v.Status != restaurantv1.GuestRequestStatus_GUEST_REQUEST_STATUS_PENDING || len(v.Lines) != 2 {
		t.Fatalf("view = %v", v)
	}
	if p := v.Lines[0].UnitPricePaisa; p != 65000 {
		t.Fatalf("pizza unit price = %d, want 65000 (variant + modifier)", p)
	}
	if v.Lines[0].VariantName.GetEn() != "Large" || len(v.Lines[0].Modifiers) != 1 || v.Lines[1].UnitPricePaisa != 25000 {
		t.Fatalf("lines = %v", v.Lines)
	}
	again, err := f.svc.SubmitOrder(ctx, req, "")
	must(t, err)
	if again.Id != v.Id {
		t.Fatalf("retry created a second request")
	}

	_, err = f.submit(momoLine(f, 21))
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID)

	_, err = f.pool.Exec(ctx, `UPDATE menu_items SET is_86d = true WHERE id = $1`, f.momo)
	must(t, err)
	_, err = f.submit(momoLine(f, 1))
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_ITEM_86D)
	_, err = f.pool.Exec(ctx, `UPDATE menu_items SET is_86d = false WHERE id = $1`, f.momo)
	must(t, err)

	_, err = f.submit(momoLine(f, 1))
	must(t, err)
	_, err = f.submit(momoLine(f, 1))
	must(t, err)
	_, err = f.submit(momoLine(f, 1))
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_TOO_MANY_PENDING)

	_, err = f.svc.SetTableOrdering(f.owner, &restaurantv1.SetTableGuestOrderingRequest{TableId: f.tableID.String(), Ordering: false})
	must(t, err)
	_, err = f.submit(momoLine(f, 1))
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_GUEST_ORDERING_OFF)
	menu, err := f.svc.Menu(ctx, f.code)
	must(t, err)
	if menu.Ordering || len(menu.Menu.GetItems()) != 2 {
		t.Fatalf("menu while off = ordering %v, %d items", menu.Ordering, len(menu.Menu.GetItems()))
	}
}

// Accept: edited lines join the table's order (created when the table is empty),
// sent to the kitchen; only one of two concurrent accepts wins; base_version guards.
func TestAcceptAddsToOrderFirstWins(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	v, err := f.submit(momoLine(f, 3), &restaurantv1.GuestLineInput{MenuItemId: f.momo.String(), Quantity: 1, Note: "spicy"})
	must(t, err)

	var wg sync.WaitGroup
	results := make([]error, 2)
	orders := make([]*restaurantv1.Order, 2)
	for i, c := range []context.Context{f.waiter, f.cashier} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			orders[i], results[i] = f.svc.Accept(c, &restaurantv1.AcceptGuestOrderRequest{
				RequestId: v.Id, Lines: []*restaurantv1.AcceptedGuestLine{{LineId: v.Lines[0].LineId, Quantity: 2}},
			})
		}()
	}
	wg.Wait()
	var order *restaurantv1.Order
	wins := 0
	for i, e := range results {
		if e == nil {
			wins++
			order = orders[i]
		} else {
			wantReason(t, e, restaurantv1.ErrorReason_ERROR_REASON_ALREADY_HANDLED)
		}
	}
	if wins != 1 {
		t.Fatalf("%d accepts won, want 1", wins)
	}
	if order.GetTableId() != f.tableID.String() || len(order.Items) != 1 {
		t.Fatalf("order = %v", order)
	}
	it := order.Items[0]
	if it.Quantity != 2 || it.Status != restaurantv1.OrderItemStatus_ORDER_ITEM_STATUS_SENT || it.Source != "GUEST" || it.UnitPricePaisa != 25000 {
		t.Fatalf("item = %v", it)
	}

	// A second round joins the same order; a stale or missing base version is refused.
	v2, err := f.submit(momoLine(f, 1))
	must(t, err)
	_, err = f.svc.Accept(f.waiter, &restaurantv1.AcceptGuestOrderRequest{RequestId: v2.Id, Lines: []*restaurantv1.AcceptedGuestLine{{LineId: v2.Lines[0].LineId, Quantity: 1}}})
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_STALE_VERSION)
	stale := order.Version - 1
	_, err = f.svc.Accept(f.waiter, &restaurantv1.AcceptGuestOrderRequest{RequestId: v2.Id, BaseVersion: &stale, Lines: []*restaurantv1.AcceptedGuestLine{{LineId: v2.Lines[0].LineId, Quantity: 1}}})
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_STALE_VERSION)
	cur := order.Version
	o2, err := f.svc.Accept(f.waiter, &restaurantv1.AcceptGuestOrderRequest{RequestId: v2.Id, BaseVersion: &cur, Lines: []*restaurantv1.AcceptedGuestLine{{LineId: v2.Lines[0].LineId, Quantity: 1}}})
	must(t, err)
	if o2.Id != order.Id || len(o2.Items) != 2 {
		t.Fatalf("second round order = %v", o2)
	}

	st, err := f.svc.Status(ctx, f.code)
	must(t, err)
	if len(st.Items) != 2 || st.TotalPaisa != 75000 {
		t.Fatalf("status = %d items, total %d; want 2, 75000", len(st.Items), st.TotalPaisa)
	}
	var audits int
	must(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = $1`, guest.ActionAccept).Scan(&audits))
	if audits != 2 {
		t.Fatalf("accept audits = %d, want 2", audits)
	}
}

func TestReject(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	v, err := f.submit(momoLine(f, 1))
	must(t, err)
	_, err = f.svc.Reject(f.waiter, &restaurantv1.RejectGuestOrderRequest{RequestId: v.Id, Reason: "  "})
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID)
	_, err = f.svc.Reject(f.waiter, &restaurantv1.RejectGuestOrderRequest{RequestId: v.Id, Reason: "ITEM_FINISHED"})
	must(t, err)
	_, err = f.svc.Accept(f.cashier, &restaurantv1.AcceptGuestOrderRequest{RequestId: v.Id, Lines: []*restaurantv1.AcceptedGuestLine{{LineId: v.Lines[0].LineId, Quantity: 1}}})
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_ALREADY_HANDLED)

	st, err := f.svc.Status(ctx, f.code)
	must(t, err)
	if len(st.Requests) != 1 || st.Requests[0].Status != restaurantv1.GuestRequestStatus_GUEST_REQUEST_STATUS_REJECTED || st.Requests[0].RejectReason != "ITEM_FINISHED" {
		t.Fatalf("guest sees %v", st.Requests)
	}
	list, err := f.svc.List(f.waiter)
	must(t, err)
	if len(list.Requests) != 1 || list.Requests[0].HandledByName != "WAITER user" {
		t.Fatalf("desk list = %v", list.Requests)
	}
}

// Call waiter / Ask for bill: a second tap returns the open alert; Ask for bill
// flags the open order until Done.
func TestCallDedupeAndBillRequest(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	call := func(kind restaurantv1.GuestRequestKind) *restaurantv1.GuestRequestView {
		t.Helper()
		v, err := f.svc.Call(ctx, &restaurantv1.CallStaffRequest{TableCode: f.code, Kind: kind}, "")
		must(t, err)
		return v
	}
	a := call(restaurantv1.GuestRequestKind_GUEST_REQUEST_KIND_CALL_WAITER)
	b := call(restaurantv1.GuestRequestKind_GUEST_REQUEST_KIND_CALL_WAITER)
	if a.Id != b.Id {
		t.Fatal("second Call waiter created a new alert")
	}
	_, err := f.svc.Resolve(f.waiter, &restaurantv1.ResolveGuestAlertRequest{RequestId: a.Id})
	must(t, err)
	_, err = f.svc.Resolve(f.waiter, &restaurantv1.ResolveGuestAlertRequest{RequestId: a.Id})
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_ALREADY_HANDLED)
	if c := call(restaurantv1.GuestRequestKind_GUEST_REQUEST_KIND_CALL_WAITER); c.Id == a.Id {
		t.Fatal("a call after Done reused the handled alert")
	}

	v, err := f.submit(momoLine(f, 1))
	must(t, err)
	order, err := f.svc.Accept(f.waiter, &restaurantv1.AcceptGuestOrderRequest{RequestId: v.Id, Lines: []*restaurantv1.AcceptedGuestLine{{LineId: v.Lines[0].LineId, Quantity: 1}}})
	must(t, err)
	bill := call(restaurantv1.GuestRequestKind_GUEST_REQUEST_KIND_BILL)
	var requested bool
	must(t, f.pool.QueryRow(ctx, `SELECT bill_requested_at IS NOT NULL FROM orders WHERE id = $1`, order.Id).Scan(&requested))
	if !requested {
		t.Fatal("Ask for bill did not flag the order")
	}
	_, err = f.svc.Resolve(f.cashier, &restaurantv1.ResolveGuestAlertRequest{RequestId: bill.Id})
	must(t, err)
	must(t, f.pool.QueryRow(ctx, `SELECT bill_requested_at IS NOT NULL FROM orders WHERE id = $1`, order.Id).Scan(&requested))
	if requested {
		t.Fatal("Done did not clear bill_requested_at")
	}
}

func TestNewCodeInvalidatesOld(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, err := f.svc.Menu(ctx, f.code)
	must(t, err)
	_, err = f.svc.NewCode(f.waiter, &restaurantv1.NewGuestCodeRequest{TableId: f.tableID.String()})
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED)
	cfg, err := f.svc.NewCode(f.owner, &restaurantv1.NewGuestCodeRequest{TableId: f.tableID.String()})
	must(t, err)
	if cfg.Code == f.code || len(cfg.Code) < 8 {
		t.Fatalf("new code %q", cfg.Code)
	}
	_, err = f.svc.Menu(ctx, f.code)
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_TABLE_CODE_INVALID)
	_, err = f.svc.Status(ctx, f.code)
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_TABLE_CODE_INVALID)
	m, err := f.svc.Menu(ctx, cfg.Code)
	must(t, err)
	if m.TableName != "T4" || !m.Ordering {
		t.Fatalf("menu = %v", m)
	}
}
