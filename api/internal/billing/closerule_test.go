package billing_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/orders"
)

func orderStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ocid uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, `SELECT status FROM orders WHERE client_id = $1`, ocid).Scan(&s); err != nil {
		t.Fatalf("order status: %v", err)
	}
	return s
}

// Issuing the bill for an order whose food is all handed over closes it at once:
// the close rule is billed + handed over, not paid.
func TestIssueFullBillClosesHandedOverOrder(t *testing.T) {
	svc, pool, ctx := newTestBilling(t)
	ocid, _ := seedOrder(t, ctx, pool, 2) // items are SERVED
	if _, err := svc.IssueFullBill(ctx, &restaurantv1.IssueFullBillRequest{CommandId: uuid.NewString(), OrderClientId: ocid.String()}); err != nil {
		t.Fatalf("bill: %v", err)
	}
	if got := orderStatus(t, ctx, pool, ocid); got != "CLOSED" {
		t.Fatalf("status = %s, want CLOSED", got)
	}
}

func TestSplitBillClosesWhenLastGroupBilled(t *testing.T) {
	svc, pool, ctx := newTestBilling(t)
	ocid, items := seedOrder(t, ctx, pool, 2)
	split := func(id uuid.UUID) {
		t.Helper()
		if _, err := svc.SplitByItem(ctx, &restaurantv1.SplitByItemRequest{
			CommandId: uuid.NewString(), OrderClientId: ocid.String(),
			Groups: []*restaurantv1.LineGroup{{OrderItemIds: []string{id.String()}}},
		}); err != nil {
			t.Fatalf("split: %v", err)
		}
	}
	split(items[0])
	if got := orderStatus(t, ctx, pool, ocid); got != "OPEN" {
		t.Fatalf("after first split status = %s, want OPEN", got)
	}
	split(items[1])
	if got := orderStatus(t, ctx, pool, ocid); got != "CLOSED" {
		t.Fatalf("after last split status = %s, want CLOSED", got)
	}
}

// Pay-first: billed (and paid) before the food is handed over. The order stays
// BILLED (still on the floor / kitchen) until the last hand-over closes it.
func TestPayFirstOrderClosesOnLastHandOver(t *testing.T) {
	svc, pool, ctx := newTestBilling(t)
	ocid, items := seedOrder(t, ctx, pool, 2)
	if _, err := pool.Exec(ctx, `UPDATE order_items SET status = 'SENT' WHERE id = ANY($1)`, items); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO shifts (cashier_user_id) SELECT id FROM users LIMIT 1`); err != nil {
		t.Fatalf("shift: %v", err)
	}

	inv, err := svc.IssueFullBill(ctx, &restaurantv1.IssueFullBillRequest{CommandId: uuid.NewString(), OrderClientId: ocid.String()})
	if err != nil {
		t.Fatalf("bill: %v", err)
	}
	if got := orderStatus(t, ctx, pool, ocid); got != "BILLED" {
		t.Fatalf("after bill status = %s, want BILLED", got)
	}
	if _, err := svc.RecordPayment(ctx, &restaurantv1.RecordPaymentRequest{
		CommandId: uuid.NewString(), InvoiceId: inv.Id, Method: restaurantv1.PaymentMethod_PAYMENT_METHOD_CASH, AmountPaisa: inv.TotalPaisa,
	}); err != nil {
		t.Fatalf("pay: %v", err)
	}
	if got := orderStatus(t, ctx, pool, ocid); got != "BILLED" {
		t.Fatalf("after full payment, nothing handed over: status = %s, want BILLED", got)
	}

	ord := orders.NewService(pool, nil, auth.NewService(pool, zap.NewNop()), zap.NewNop())
	serve := func(id uuid.UUID) *restaurantv1.Order {
		t.Helper()
		res, err := ord.MarkServed(ctx, &restaurantv1.MarkServedRequest{CommandId: uuid.NewString(), OrderClientId: ocid.String(), ItemIds: []string{id.String()}})
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
		return res.Order
	}
	if o := serve(items[0]); o.Status != restaurantv1.OrderStatus_ORDER_STATUS_BILLED {
		t.Fatalf("after first hand-over status = %v, want BILLED", o.Status)
	}
	if o := serve(items[1]); o.Status != restaurantv1.OrderStatus_ORDER_STATUS_CLOSED {
		t.Fatalf("after last hand-over status = %v, want CLOSED", o.Status)
	}
}

// unit_price_paisa already includes the modifier deltas (orders snapshots it that
// way), so the invoice must not add them a second time.
func TestBillDoesNotDoubleCountModifierDeltas(t *testing.T) {
	svc, pool, ctx := newTestBilling(t)
	ocid, items := seedOrder(t, ctx, pool, 1)
	// 15000 base + 5000 cheese = 20000 per unit, quantity 2.
	if _, err := pool.Exec(ctx, `UPDATE order_items SET unit_price_paisa = 20000, quantity = 2 WHERE id = $1`, items[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO order_item_modifiers (order_item_id, name_en, price_delta_paisa) VALUES ($1, 'Cheese', 5000)`, items[0]); err != nil {
		t.Fatal(err)
	}
	preview, err := svc.PreviewBill(ctx, &restaurantv1.PreviewBillRequest{OrderClientId: ocid.String()})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if preview.SubtotalPaisa != 40000 {
		t.Fatalf("preview subtotal = %d, want 40000", preview.SubtotalPaisa)
	}
	inv, err := svc.IssueFullBill(ctx, &restaurantv1.IssueFullBillRequest{CommandId: uuid.NewString(), OrderClientId: ocid.String()})
	if err != nil {
		t.Fatalf("bill: %v", err)
	}
	if inv.SubtotalPaisa != 40000 {
		t.Fatalf("invoice subtotal = %d, want 40000", inv.SubtotalPaisa)
	}
	if l := inv.Lines[0]; l.UnitPricePaisa != 20000 || l.LineTotalPaisa != 40000 {
		t.Fatalf("line unit=%d total=%d, want 20000/40000", l.UnitPricePaisa, l.LineTotalPaisa)
	}
}
