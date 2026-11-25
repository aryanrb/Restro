package billing_test

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
	"github.com/oneaccord/restro/api/internal/billing"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db"
)

// newTestBilling returns a billing.Service on a clean TEST_DATABASE_URL plus a
// context carrying a logged-in OWNER actor. Skips when no DB is configured.
func newTestBilling(t *testing.T) (*billing.Service, *pgxpool.Pool, context.Context) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run billing integration tests")
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
		invoices, invoice_counters, orders, processed_commands, audit_log, sessions, devices, users RESTART IDENTITY CASCADE`); err != nil {
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
	// Resolve the actor exactly as a real request would: through the interceptor.
	req := connect.NewRequest(&restaurantv1.IssueFullBillRequest{})
	req.Header().Set("Authorization", "Bearer "+token)
	var actorCtx context.Context
	_, _ = authSvc.Interceptor()(func(c context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		actorCtx = c
		return nil, nil
	})(ctx, req)
	if !auth.ActorFrom(actorCtx).LoggedIn() {
		t.Fatal("actor not resolved")
	}
	return billing.NewService(pool, nil, authSvc, zap.NewNop()), pool, actorCtx
}

// seedOrder inserts an OPEN order with n unbilled items; returns its client id
// and the item ids.
func seedOrder(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int) (uuid.UUID, []uuid.UUID) {
	t.Helper()
	ocid := uuid.New()
	var orderID uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO orders (client_id, type, waiter_user_id)
		VALUES ($1, 'TAKEAWAY', (SELECT id FROM users LIMIT 1)) RETURNING id`, ocid).Scan(&orderID); err != nil {
		t.Fatalf("insert order: %v", err)
	}
	ids := make([]uuid.UUID, 0, n)
	for i := 0; i < n; i++ {
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO order_items (order_id, client_id, menu_item_id, name_en, unit_price_paisa, quantity, station_id, status)
			VALUES ($1, $2, $3, 'Momo', 15000, 1, $4, 'SERVED') RETURNING id`,
			orderID, uuid.New(), uuid.New(), uuid.New()).Scan(&id); err != nil {
			t.Fatalf("insert item: %v", err)
		}
		ids = append(ids, id)
	}
	return ocid, ids
}

// assertBilledOnce checks every item has exactly one invoice line on an ISSUED
// invoice and that exactly wantInvoices invoices exist for the order.
func assertBilledOnce(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ocid uuid.UUID, items []uuid.UUID, wantInvoices int) {
	t.Helper()
	var invoices int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM invoices i JOIN orders o ON o.id = i.order_id WHERE o.client_id = $1`, ocid).Scan(&invoices); err != nil {
		t.Fatalf("count invoices: %v", err)
	}
	if invoices != wantInvoices {
		t.Errorf("invoices for order = %d, want %d", invoices, wantInvoices)
	}
	for _, id := range items {
		var lines int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM invoice_lines WHERE order_item_id = $1`, id).Scan(&lines); err != nil {
			t.Fatalf("count lines: %v", err)
		}
		if lines != 1 {
			t.Errorf("item %s has %d invoice lines, want 1", id, lines)
		}
	}
}

// race runs a and b at the same instant and returns their errors.
func race(a, b func() error) (error, error) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	var ea, eb error
	wg.Add(2)
	go func() { defer wg.Done(); <-start; ea = a() }()
	go func() { defer wg.Done(); <-start; eb = b() }()
	close(start)
	wg.Wait()
	return ea, eb
}

func exactlyOne(t *testing.T, ea, eb error) {
	t.Helper()
	if (ea == nil) == (eb == nil) {
		t.Fatalf("want exactly one success, got errA=%v errB=%v", ea, eb)
	}
	loser := ea
	if loser == nil {
		loser = eb
	}
	// The loser must be a clean client-facing rejection, not a deadlock/internal.
	var ae *connecterr.Error
	if !errors.As(loser, &ae) || ae.Code == connect.CodeInternal {
		t.Fatalf("loser error = %v, want a non-internal *connecterr.Error", loser)
	}
}

const raceRounds = 15

// Two different command ids billing the same order concurrently must not both
// bill the same items.
func TestConcurrentIssueFullBillBillsOnce(t *testing.T) {
	svc, pool, ctx := newTestBilling(t)
	for round := 0; round < raceRounds; round++ {
		ocid, items := seedOrder(t, ctx, pool, 3)
		bill := func() error {
			_, err := svc.IssueFullBill(ctx, &restaurantv1.IssueFullBillRequest{CommandId: uuid.NewString(), OrderClientId: ocid.String()})
			return err
		}
		ea, eb := race(bill, bill)
		exactlyOne(t, ea, eb)
		assertBilledOnce(t, ctx, pool, ocid, items, 1)
		if t.Failed() {
			t.Fatalf("failed in round %d", round)
		}
	}
}

func TestConcurrentSplitByItemBillsOnce(t *testing.T) {
	svc, pool, ctx := newTestBilling(t)
	for round := 0; round < raceRounds; round++ {
		ocid, items := seedOrder(t, ctx, pool, 3)
		ids := []string{items[0].String(), items[1].String()}
		split := func() error {
			_, err := svc.SplitByItem(ctx, &restaurantv1.SplitByItemRequest{
				CommandId: uuid.NewString(), OrderClientId: ocid.String(),
				Groups: []*restaurantv1.LineGroup{{OrderItemIds: ids}},
			})
			return err
		}
		ea, eb := race(split, split)
		exactlyOne(t, ea, eb)
		assertBilledOnce(t, ctx, pool, ocid, items[:2], 1)
		if t.Failed() {
			t.Fatalf("failed in round %d", round)
		}
	}
}

// A full bill racing a split of the same order: one wins, nothing is billed twice.
func TestConcurrentFullBillVsSplitBillsOnce(t *testing.T) {
	svc, pool, ctx := newTestBilling(t)
	for round := 0; round < raceRounds; round++ {
		ocid, items := seedOrder(t, ctx, pool, 2)
		full := func() error {
			_, err := svc.IssueFullBill(ctx, &restaurantv1.IssueFullBillRequest{CommandId: uuid.NewString(), OrderClientId: ocid.String()})
			return err
		}
		split := func() error {
			_, err := svc.SplitByItem(ctx, &restaurantv1.SplitByItemRequest{
				CommandId: uuid.NewString(), OrderClientId: ocid.String(),
				Groups: []*restaurantv1.LineGroup{{OrderItemIds: []string{items[0].String(), items[1].String()}}},
			})
			return err
		}
		ea, eb := race(full, split)
		exactlyOne(t, ea, eb)
		assertBilledOnce(t, ctx, pool, ocid, items, 1)
		if t.Failed() {
			t.Fatalf("failed in round %d", round)
		}
	}
}
