package kitchen

import (
	"context"
	"os"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/db"
)

// newKitchenTest returns a kitchen Service on a clean DB and an OWNER context.
func newKitchenTest(t *testing.T) (*Service, *pgxpool.Pool, context.Context) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run kitchen integration tests")
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
	if _, err := pool.Exec(ctx, `TRUNCATE payments, cbms_outbox, invoice_lines, order_item_modifiers, order_items,
		invoices, orders, processed_commands, audit_log, sessions, devices, users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	authSvc := auth.NewService(pool, zap.NewNop())
	if _, err := authSvc.SeedOwner(ctx, "Ram", "owner", "owner123", "1111"); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	_, token, err := authSvc.PasswordLogin(ctx, "owner", "owner123")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	req := connect.NewRequest(&restaurantv1.ListStationTicketsRequest{})
	req.Header().Set("Authorization", "Bearer "+token)
	var actorCtx context.Context
	_, _ = authSvc.Interceptor()(func(c context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		actorCtx = c
		return nil, nil
	})(ctx, req)
	if !auth.ActorFrom(actorCtx).LoggedIn() {
		t.Fatal("actor not resolved")
	}
	return NewService(pool, nil, zap.NewNop()), pool, actorCtx
}

// seedBilledOrder inserts an order of the given type whose items are SENT and
// already on an ISSUED invoice (pay-first), all at one station.
func seedBilledOrder(t *testing.T, ctx context.Context, pool *pgxpool.Pool, typ string, n int) (orderID, stationID uuid.UUID, items []string) {
	t.Helper()
	stationID = uuid.New()
	if err := pool.QueryRow(ctx, `INSERT INTO orders (client_id, type, waiter_user_id, status)
		VALUES ($1, $2, (SELECT id FROM users LIMIT 1), 'BILLED') RETURNING id`, uuid.New(), typ).Scan(&orderID); err != nil {
		t.Fatalf("insert order: %v", err)
	}
	var invID uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO invoices (fiscal_year, number, mode, bs_date, bs_year, bs_month, bs_day, order_id,
		subtotal_paisa, discount_paisa, service_charge_paisa, taxable_paisa, vat_paisa, total_paisa)
		VALUES ('2083/84', (SELECT COALESCE(MAX(number), 0) + 1 FROM invoices), 'ESTIMATE', '2083.08.10', 2083, 8, 10, $1, 1, 0, 0, 1, 0, 1)
		RETURNING id`, orderID).Scan(&invID); err != nil {
		t.Fatalf("insert invoice: %v", err)
	}
	for i := 0; i < n; i++ {
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO order_items (order_id, client_id, menu_item_id, name_en, unit_price_paisa, quantity, station_id, status, invoice_id)
			VALUES ($1, $2, $3, 'Momo', 15000, 1, $4, 'SENT', $5) RETURNING id`,
			orderID, uuid.New(), uuid.New(), stationID, invID).Scan(&id); err != nil {
			t.Fatalf("insert item: %v", err)
		}
		items = append(items, id.String())
	}
	return orderID, stationID, items
}

func statusOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orderID uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, `SELECT status FROM orders WHERE id = $1`, orderID).Scan(&s); err != nil {
		t.Fatalf("status: %v", err)
	}
	return s
}

// For a takeaway, READY is the hand-over: a billed takeaway closes when the
// kitchen marks its last item ready.
func TestMarkItemsReadyClosesBilledTakeaway(t *testing.T) {
	svc, pool, ctx := newKitchenTest(t)
	orderID, _, items := seedBilledOrder(t, ctx, pool, "TAKEAWAY", 2)

	if err := svc.MarkItemsReady(ctx, items[:1]); err != nil {
		t.Fatalf("ready 1: %v", err)
	}
	if got := statusOf(t, ctx, pool, orderID); got != "BILLED" {
		t.Fatalf("after first ready status = %s, want BILLED", got)
	}
	if err := svc.MarkItemsReady(ctx, items[1:]); err != nil {
		t.Fatalf("ready 2: %v", err)
	}
	if got := statusOf(t, ctx, pool, orderID); got != "CLOSED" {
		t.Fatalf("after last ready status = %s, want CLOSED", got)
	}
}

func TestBumpTicketClosesBilledTakeaway(t *testing.T) {
	svc, pool, ctx := newKitchenTest(t)
	orderID, stationID, _ := seedBilledOrder(t, ctx, pool, "TAKEAWAY", 2)
	if err := svc.BumpTicket(ctx, orderID.String(), stationID.String()); err != nil {
		t.Fatalf("bump: %v", err)
	}
	if got := statusOf(t, ctx, pool, orderID); got != "CLOSED" {
		t.Fatalf("status = %s, want CLOSED", got)
	}
}

// Dine-in food still has to reach the table: READY does not close it.
func TestReadyDoesNotCloseDineIn(t *testing.T) {
	svc, pool, ctx := newKitchenTest(t)
	orderID, stationID, items := seedBilledOrder(t, ctx, pool, "DINE_IN", 2)
	if err := svc.MarkItemsReady(ctx, items[:1]); err != nil {
		t.Fatalf("ready: %v", err)
	}
	if err := svc.BumpTicket(ctx, orderID.String(), stationID.String()); err != nil {
		t.Fatalf("bump: %v", err)
	}
	if got := statusOf(t, ctx, pool, orderID); got != "BILLED" {
		t.Fatalf("status = %s, want BILLED", got)
	}
}
