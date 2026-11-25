package orders

import (
	"context"
	"errors"
	"os"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
)

// fixture is a clean DB with an orders.Service, its auth.Service, and a menu item
// + two tables to order against.
type fixture struct {
	t       *testing.T
	svc     *Service
	authSvc *auth.Service
	q       *sqlc.Queries
	exec    func(sql string, args ...any)
	pwHash  string
	itemID  uuid.UUID
	tableA  uuid.UUID
	tableB  uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run orders integration tests")
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
		menu_items, menu_categories, stations, dining_tables, areas RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	q := sqlc.New(pool)
	authSvc := auth.NewService(pool, zap.NewNop())
	f := &fixture{t: t, svc: NewService(pool, nil, authSvc, zap.NewNop()), authSvc: authSvc, q: q}
	f.exec = func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	f.pwHash, err = auth.HashSecret("secret123")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	st, err := q.CreateStation(ctx, sqlc.CreateStationParams{NameEn: "Kitchen"})
	if err != nil {
		t.Fatalf("station: %v", err)
	}
	cat, err := q.CreateCategory(ctx, sqlc.CreateCategoryParams{NameEn: "Food", Active: true})
	if err != nil {
		t.Fatalf("category: %v", err)
	}
	it, err := q.CreateItem(ctx, sqlc.CreateItemParams{CategoryID: cat.ID, NameEn: "Momo", PricePaisa: 25000, StationID: st.ID, Active: true})
	if err != nil {
		t.Fatalf("item: %v", err)
	}
	area, err := q.CreateArea(ctx, sqlc.CreateAreaParams{NameEn: "Hall"})
	if err != nil {
		t.Fatalf("area: %v", err)
	}
	ta, err := q.CreateTable(ctx, sqlc.CreateTableParams{AreaID: area.ID, Name: "T1", Seats: 4, Active: true})
	if err != nil {
		t.Fatalf("table: %v", err)
	}
	tb, err := q.CreateTable(ctx, sqlc.CreateTableParams{AreaID: area.ID, Name: "T2", Seats: 4, Active: true, Sort: 1})
	if err != nil {
		t.Fatalf("table: %v", err)
	}
	f.itemID, f.tableA, f.tableB = it.ID, ta.ID, tb.ID
	return f
}

// as returns a context carrying a real session for a fresh user with the given
// role, resolved through the auth interceptor exactly as a request would be.
func (f *fixture) as(role string) context.Context {
	f.t.Helper()
	ctx := context.Background()
	username := "u-" + uuid.NewString()[:8]
	if _, err := f.q.CreateUser(ctx, sqlc.CreateUserParams{
		Name: role, Role: role, Username: dbx.Text(username), PasswordHash: dbx.Text(f.pwHash), Language: "en", Active: true,
	}); err != nil {
		f.t.Fatalf("create user: %v", err)
	}
	_, token, err := f.authSvc.PasswordLogin(ctx, username, "secret123")
	if err != nil {
		f.t.Fatalf("login: %v", err)
	}
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
	return out
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

// openOrder creates a dine-in order on tableA with one DRAFT item.
func (f *fixture) openOrder(ctx context.Context, table uuid.UUID) (orderClientID string, itemID string, version int64) {
	f.t.Helper()
	ocid := uuid.NewString()
	tid := table.String()
	if _, err := f.svc.CreateOrder(ctx, &restaurantv1.CreateOrderRequest{
		CommandId: uuid.NewString(), OrderClientId: ocid, Type: restaurantv1.OrderType_ORDER_TYPE_DINE_IN, TableId: &tid, GuestCount: 2,
	}); err != nil {
		f.t.Fatalf("create order: %v", err)
	}
	res, err := f.svc.AddItems(ctx, &restaurantv1.AddItemsRequest{
		CommandId: uuid.NewString(), OrderClientId: ocid,
		Items: []*restaurantv1.NewItem{{ClientId: uuid.NewString(), MenuItemId: f.itemID.String(), Quantity: 1}},
	})
	if err != nil || len(res.Order.Items) != 1 {
		f.t.Fatalf("add items: %v", err)
	}
	return ocid, res.Order.Items[0].Id, res.Order.Version
}

func TestOrderRPCsRequireOrderRole(t *testing.T) {
	f := newFixture(t)
	type call func(ctx context.Context) error
	calls := map[string]call{
		"ListOpenOrders": func(ctx context.Context) error { _, err := f.svc.ListOpenOrders(ctx); return err },
		"GetOrder": func(ctx context.Context) error {
			_, err := f.svc.GetOrder(ctx, &restaurantv1.GetOrderRequest{})
			return err
		},
		"CreateOrder": func(ctx context.Context) error {
			_, err := f.svc.CreateOrder(ctx, &restaurantv1.CreateOrderRequest{})
			return err
		},
		"AddItems": func(ctx context.Context) error {
			_, err := f.svc.AddItems(ctx, &restaurantv1.AddItemsRequest{})
			return err
		},
		"SendItems": func(ctx context.Context) error {
			_, err := f.svc.SendItems(ctx, &restaurantv1.SendItemsRequest{})
			return err
		},
		"VoidItem": func(ctx context.Context) error {
			_, err := f.svc.VoidItem(ctx, &restaurantv1.VoidItemRequest{})
			return err
		},
		"MarkServed": func(ctx context.Context) error {
			_, err := f.svc.MarkServed(ctx, &restaurantv1.MarkServedRequest{})
			return err
		},
		"TransferTable": func(ctx context.Context) error {
			_, err := f.svc.TransferTable(ctx, &restaurantv1.TransferTableRequest{})
			return err
		},
		"MergeTables": func(ctx context.Context) error {
			_, err := f.svc.MergeTables(ctx, &restaurantv1.MergeTablesRequest{})
			return err
		},
	}
	allowed := map[string]bool{auth.RoleOwner: true, auth.RoleManager: true, auth.RoleWaiter: true, auth.RoleCashier: true}
	for _, role := range []string{auth.RoleOwner, auth.RoleManager, auth.RoleWaiter, auth.RoleCashier, auth.RoleKitchen} {
		ctx := f.as(role)
		for name, c := range calls {
			err := c(ctx)
			got := reasonOf(err)
			if allowed[role] && got == restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED {
				t.Errorf("%s as %s: got NOT_ALLOWED, want allowed", name, role)
			}
			if !allowed[role] {
				var ae *connecterr.Error
				if !errors.As(err, &ae) || ae.Reason != restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED || ae.Code != connect.CodePermissionDenied {
					t.Errorf("%s as %s: got %v, want PermissionDenied/NOT_ALLOWED", name, role, err)
				}
			}
		}
	}

	// Logged out stays Unauthenticated (no reason), not NOT_ALLOWED.
	_, err := f.svc.ListOpenOrders(context.Background())
	var ae *connecterr.Error
	if !errors.As(err, &ae) || ae.Code != connect.CodeUnauthenticated {
		t.Fatalf("logged out: got %v, want Unauthenticated", err)
	}
}

func TestCommandsRejectNonOpenOrder(t *testing.T) {
	f := newFixture(t)
	ctx := f.as(auth.RoleManager)

	for _, status := range []string{"BILLED", "CLOSED", "CANCELLED"} {
		t.Run(status, func(t *testing.T) {
			ocid, itemID, ver := f.openOrder(ctx, f.tableA)
			otherCID, _, otherVer := f.openOrder(ctx, f.tableB)
			// Send so MarkServed/Void would otherwise have something to act on.
			f.exec(`UPDATE order_items SET status = 'SENT' WHERE id = $1`, itemID)
			f.exec(`UPDATE orders SET status = $2 WHERE client_id = $1`, ocid, status)

			_, err := f.svc.SendItems(ctx, &restaurantv1.SendItemsRequest{CommandId: uuid.NewString(), OrderClientId: ocid})
			wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_ORDER_CLOSED)

			_, err = f.svc.VoidItem(ctx, &restaurantv1.VoidItemRequest{
				CommandId: uuid.NewString(), OrderClientId: ocid, BaseVersion: ver, ItemId: itemID, Reason: "wrong",
			})
			wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_ORDER_CLOSED)

			_, err = f.svc.MarkServed(ctx, &restaurantv1.MarkServedRequest{CommandId: uuid.NewString(), OrderClientId: ocid, ItemIds: []string{itemID}})
			if status == "BILLED" {
				// Paid but not yet handed over (pay-first counter, bill before the last dish): serving must still work.
				if err != nil {
					t.Fatalf("MarkServed on a BILLED order: %v", err)
				}
			} else {
				wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_ORDER_CLOSED)
			}

			_, err = f.svc.TransferTable(ctx, &restaurantv1.TransferTableRequest{
				CommandId: uuid.NewString(), OrderClientId: ocid, BaseVersion: ver, NewTableId: f.tableB.String(),
			})
			wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_ORDER_CLOSED)

			// Closed source into open target.
			_, err = f.svc.MergeTables(ctx, &restaurantv1.MergeTablesRequest{
				CommandId: uuid.NewString(), SourceOrderClientId: ocid, SourceBaseVersion: ver,
				TargetOrderClientId: otherCID, TargetBaseVersion: otherVer,
			})
			wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_ORDER_CLOSED)

			// Open source into closed target.
			_, err = f.svc.MergeTables(ctx, &restaurantv1.MergeTablesRequest{
				CommandId: uuid.NewString(), SourceOrderClientId: otherCID, SourceBaseVersion: otherVer,
				TargetOrderClientId: ocid, TargetBaseVersion: ver,
			})
			wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_ORDER_CLOSED)

			// Nothing else changed on the order.
			it, gerr := f.q.GetOrderItem(context.Background(), uuid.MustParse(itemID))
			if gerr != nil {
				t.Fatalf("get item: %v", gerr)
			}
			want := "SENT"
			if status == "BILLED" {
				want = "SERVED" // the only change allowed on a billed order
			}
			if it.Status != want || it.OrderID.String() == "" {
				t.Fatalf("item status %s on a %s order, want %s", it.Status, status, want)
			}
		})
	}
}

func TestCommandsStillWorkOnOpenOrder(t *testing.T) {
	f := newFixture(t)
	ctx := f.as(auth.RoleWaiter)
	ocid, itemID, _ := f.openOrder(ctx, f.tableA)
	otherCID, _, _ := f.openOrder(ctx, f.tableB)

	res, err := f.svc.SendItems(ctx, &restaurantv1.SendItemsRequest{CommandId: uuid.NewString(), OrderClientId: ocid})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	res, err = f.svc.MarkServed(ctx, &restaurantv1.MarkServedRequest{CommandId: uuid.NewString(), OrderClientId: ocid, ItemIds: []string{itemID}})
	if err != nil {
		t.Fatalf("served: %v", err)
	}
	res, err = f.svc.TransferTable(ctx, &restaurantv1.TransferTableRequest{
		CommandId: uuid.NewString(), OrderClientId: ocid, BaseVersion: res.Order.Version, NewTableId: f.tableB.String(),
	})
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	other, err := f.svc.GetOrder(ctx, &restaurantv1.GetOrderRequest{ClientId: &otherCID})
	if err != nil {
		t.Fatalf("get other: %v", err)
	}
	if _, err := f.svc.MergeTables(ctx, &restaurantv1.MergeTablesRequest{
		CommandId: uuid.NewString(), SourceOrderClientId: otherCID, SourceBaseVersion: other.Version,
		TargetOrderClientId: ocid, TargetBaseVersion: res.Order.Version,
	}); err != nil {
		t.Fatalf("merge: %v", err)
	}
}

func TestVoidItemRefusesInvoicedItem(t *testing.T) {
	f := newFixture(t)
	ctx := f.as(auth.RoleManager)
	ocid, itemID, ver := f.openOrder(ctx, f.tableA)

	// Split bill: the line is on an issued invoice while the order stays OPEN.
	inv, err := f.q.CreateInvoice(context.Background(), sqlc.CreateInvoiceParams{
		FiscalYear: "2083/84", Number: 1, Mode: "ESTIMATE", BsDate: "2083.08.10", BsYear: 2083, BsMonth: 8, BsDay: 10,
		SubtotalPaisa: 25000, TaxablePaisa: 25000, TotalPaisa: 25000,
	})
	if err != nil {
		t.Fatalf("invoice: %v", err)
	}
	f.exec(`UPDATE order_items SET invoice_id = $2 WHERE id = $1`, itemID, inv.ID)

	_, err = f.svc.VoidItem(ctx, &restaurantv1.VoidItemRequest{
		CommandId: uuid.NewString(), OrderClientId: ocid, BaseVersion: ver, ItemId: itemID, Reason: "wrong",
	})
	ae := wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID)
	if ae.Field != "item_id" {
		t.Fatalf("field = %q, want item_id", ae.Field)
	}
	it, err := f.q.GetOrderItem(context.Background(), uuid.MustParse(itemID))
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if it.Status == "VOIDED" {
		t.Fatal("invoiced item was voided")
	}

	// Once the invoice is cancelled (invoice_id cleared) the line can be voided.
	f.exec(`UPDATE order_items SET invoice_id = NULL WHERE id = $1`, itemID)
	if _, err := f.svc.VoidItem(ctx, &restaurantv1.VoidItemRequest{
		CommandId: uuid.NewString(), OrderClientId: ocid, BaseVersion: ver, ItemId: itemID, Reason: "wrong",
	}); err != nil {
		t.Fatalf("void after invoice cleared: %v", err)
	}
}
