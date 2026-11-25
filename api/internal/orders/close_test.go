package orders

import (
	"context"
	"testing"

	"github.com/google/uuid"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
)

// bill puts the given items on a new invoice with the given status (as billing
// would), without going through the billing service (which imports this package).
func (f *fixture) bill(status string, itemIDs ...string) {
	f.t.Helper()
	f.invoiceNo++
	inv, err := f.q.CreateInvoice(context.Background(), sqlc.CreateInvoiceParams{
		FiscalYear: "2083/84", Number: f.invoiceNo, Mode: "ESTIMATE", BsDate: "2083.08.10", BsYear: 2083, BsMonth: 8, BsDay: 10,
		SubtotalPaisa: 1, TaxablePaisa: 1, TotalPaisa: 1,
	})
	if err != nil {
		f.t.Fatalf("invoice: %v", err)
	}
	for _, id := range itemIDs {
		f.exec(`UPDATE order_items SET invoice_id = $2 WHERE id = $1`, id, inv.ID)
	}
	if status != "ISSUED" {
		f.exec(`UPDATE invoices SET status = $2 WHERE id = $1`, inv.ID, status)
	}
}

func (f *fixture) orderStatus(ocid string) string {
	f.t.Helper()
	o, err := f.q.GetOrderByClientID(context.Background(), uuid.MustParse(ocid))
	if err != nil {
		f.t.Fatalf("get order: %v", err)
	}
	return o.Status
}

func (f *fixture) closeIfDone(ocid string) bool {
	f.t.Helper()
	o, err := f.q.GetOrderByClientID(context.Background(), uuid.MustParse(ocid))
	if err != nil {
		f.t.Fatalf("get order: %v", err)
	}
	closed, err := CloseIfDone(context.Background(), f.q, o.ID)
	if err != nil {
		f.t.Fatalf("CloseIfDone: %v", err)
	}
	return closed
}

// addSent adds n SENT lines and returns their server ids.
func (f *fixture) addSent(ctx context.Context, ocid string, n int) []string {
	f.t.Helper()
	var lines []*restaurantv1.NewItem
	for i := 0; i < n; i++ {
		lines = append(lines, line(f.itemID, nil))
	}
	res, err := f.svc.AddItems(ctx, &restaurantv1.AddItemsRequest{CommandId: uuid.NewString(), OrderClientId: ocid, Send: true, Items: lines})
	if err != nil {
		f.t.Fatalf("add: %v", err)
	}
	var ids []string
	for _, l := range lines {
		ids = append(ids, itemByClientID(res.Order, l.ClientId).Id)
	}
	return ids
}

func TestCloseRuleBilledThenServed(t *testing.T) {
	f := newFixture(t)
	ctx := f.as(auth.RoleWaiter)
	ocid := f.newOrder(ctx, restaurantv1.OrderType_ORDER_TYPE_DINE_IN)
	ids := f.addSent(ctx, ocid, 2)

	// Pay-first: everything billed, order BILLED, nothing handed over yet.
	f.bill("ISSUED", ids...)
	f.exec(`UPDATE orders SET status = 'BILLED' WHERE client_id = $1`, ocid)

	res, err := f.svc.MarkServed(ctx, &restaurantv1.MarkServedRequest{CommandId: uuid.NewString(), OrderClientId: ocid, ItemIds: ids[:1]})
	if err != nil {
		t.Fatalf("serve 1: %v", err)
	}
	if res.Order.Status != restaurantv1.OrderStatus_ORDER_STATUS_BILLED {
		t.Fatalf("after first hand-over status = %v, want BILLED", res.Order.Status)
	}
	res, err = f.svc.MarkServed(ctx, &restaurantv1.MarkServedRequest{CommandId: uuid.NewString(), OrderClientId: ocid, ItemIds: ids[1:]})
	if err != nil {
		t.Fatalf("serve 2: %v", err)
	}
	if res.Order.Status != restaurantv1.OrderStatus_ORDER_STATUS_CLOSED {
		t.Fatalf("after last hand-over status = %v, want CLOSED", res.Order.Status)
	}
}

func TestCloseRuleServedButNotBilledStaysOpen(t *testing.T) {
	f := newFixture(t)
	ctx := f.as(auth.RoleWaiter)
	ocid := f.newOrder(ctx, restaurantv1.OrderType_ORDER_TYPE_DINE_IN)
	ids := f.addSent(ctx, ocid, 2)
	f.bill("ISSUED", ids[0]) // split: only one line billed

	res, err := f.svc.MarkServed(ctx, &restaurantv1.MarkServedRequest{CommandId: uuid.NewString(), OrderClientId: ocid, ItemIds: ids})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if res.Order.Status != restaurantv1.OrderStatus_ORDER_STATUS_OPEN {
		t.Fatalf("status = %v, want OPEN (one line unbilled)", res.Order.Status)
	}

	// Billing the rest closes it (billing calls the helper).
	f.bill("ISSUED", ids[1])
	if !f.closeIfDone(ocid) || f.orderStatus(ocid) != "CLOSED" {
		t.Fatalf("status = %s, want CLOSED", f.orderStatus(ocid))
	}
}

func TestCloseRuleEdgeCases(t *testing.T) {
	f := newFixture(t)
	ctx := f.as(auth.RoleManager)

	t.Run("no items", func(t *testing.T) {
		ocid := f.newOrder(ctx, restaurantv1.OrderType_ORDER_TYPE_DINE_IN)
		if f.closeIfDone(ocid) || f.orderStatus(ocid) != "OPEN" {
			t.Fatal("an empty order closed")
		}
	})

	t.Run("cancelled invoice does not count", func(t *testing.T) {
		ocid := f.newOrder(ctx, restaurantv1.OrderType_ORDER_TYPE_DINE_IN)
		ids := f.addSent(ctx, ocid, 1)
		f.exec(`UPDATE order_items SET status = 'SERVED' WHERE id = $1`, ids[0])
		f.bill("CANCELLED", ids...)
		if f.closeIfDone(ocid) {
			t.Fatal("closed on a cancelled invoice")
		}
	})

	t.Run("voided lines are ignored", func(t *testing.T) {
		ocid := f.newOrder(ctx, restaurantv1.OrderType_ORDER_TYPE_DINE_IN)
		ids := f.addSent(ctx, ocid, 2)
		f.exec(`UPDATE order_items SET status = 'SERVED' WHERE id = $1`, ids[0])
		f.bill("ISSUED", ids[0])
		if f.closeIfDone(ocid) {
			t.Fatal("closed with a live unbilled line")
		}
		o, err := f.svc.GetOrder(ctx, &restaurantv1.GetOrderRequest{ClientId: &ocid})
		if err != nil {
			t.Fatal(err)
		}
		// Voiding the last open line completes the order.
		res, err := f.svc.VoidItem(ctx, &restaurantv1.VoidItemRequest{
			CommandId: uuid.NewString(), OrderClientId: ocid, BaseVersion: o.Version, ItemId: ids[1], Reason: "guest left",
		})
		if err != nil {
			t.Fatalf("void: %v", err)
		}
		if res.Order.Status != restaurantv1.OrderStatus_ORDER_STATUS_CLOSED {
			t.Fatalf("status after void = %v, want CLOSED", res.Order.Status)
		}
	})

	t.Run("only voided lines", func(t *testing.T) {
		ocid := f.newOrder(ctx, restaurantv1.OrderType_ORDER_TYPE_DINE_IN)
		ids := f.addSent(ctx, ocid, 1)
		f.exec(`UPDATE order_items SET status = 'VOIDED' WHERE id = $1`, ids[0])
		if f.closeIfDone(ocid) {
			t.Fatal("an order with only voided lines closed")
		}
	})

	t.Run("dine-in READY is not handed over", func(t *testing.T) {
		ocid := f.newOrder(ctx, restaurantv1.OrderType_ORDER_TYPE_DINE_IN)
		ids := f.addSent(ctx, ocid, 1)
		f.exec(`UPDATE order_items SET status = 'READY' WHERE id = $1`, ids[0])
		f.bill("ISSUED", ids...)
		if f.closeIfDone(ocid) {
			t.Fatal("dine-in closed on READY")
		}
	})

	t.Run("takeaway READY is handed over", func(t *testing.T) {
		ocid := f.newOrder(ctx, restaurantv1.OrderType_ORDER_TYPE_TAKEAWAY)
		ids := f.addSent(ctx, ocid, 1)
		f.exec(`UPDATE order_items SET status = 'READY' WHERE id = $1`, ids[0])
		f.bill("ISSUED", ids...)
		if !f.closeIfDone(ocid) || f.orderStatus(ocid) != "CLOSED" {
			t.Fatal("takeaway did not close on READY + billed")
		}
	})

	t.Run("cancelled order stays cancelled", func(t *testing.T) {
		ocid := f.newOrder(ctx, restaurantv1.OrderType_ORDER_TYPE_TAKEAWAY)
		ids := f.addSent(ctx, ocid, 1)
		f.exec(`UPDATE order_items SET status = 'SERVED' WHERE id = $1`, ids[0])
		f.bill("ISSUED", ids...)
		f.exec(`UPDATE orders SET status = 'CANCELLED' WHERE client_id = $1`, ocid)
		if f.closeIfDone(ocid) || f.orderStatus(ocid) != "CANCELLED" {
			t.Fatal("a cancelled order was closed")
		}
	})
}
