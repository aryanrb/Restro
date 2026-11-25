package kitchen

import (
	"testing"

	"github.com/oneaccord/restro/api/internal/realtime"
)

// Kitchen actions return the updated order and their order.updated events carry
// its version, so a client can ignore stale pokes.
func TestKitchenActionsReturnOrderAndVersionedEvents(t *testing.T) {
	svc, pool, ctx := newKitchenTest(t)
	hub := realtime.NewHub()
	svc.hub = hub
	events, cancel := hub.Subscribe(32)
	defer cancel()
	orderID, stationID, items := seedBilledOrder(t, ctx, pool, "DINE_IN", 3)

	check := func(name, gotOrderID string, version int64) {
		t.Helper()
		if gotOrderID != orderID.String() {
			t.Fatalf("%s: order id = %q, want %s", name, gotOrderID, orderID)
		}
		var db int64
		if err := pool.QueryRow(ctx, `SELECT version FROM orders WHERE id = $1`, orderID).Scan(&db); err != nil {
			t.Fatal(err)
		}
		if version != db {
			t.Fatalf("%s: returned version %d, db %d", name, version, db)
		}
		var found bool
		for {
			select {
			case ev := <-events:
				if ev.Type == "order.updated" && ev.ID == orderID.String() {
					if ev.Version != db {
						t.Fatalf("%s: event version %d, want %d", name, ev.Version, db)
					}
					found = true
				}
				continue
			default:
			}
			break
		}
		if !found {
			t.Fatalf("%s: no order.updated event", name)
		}
	}

	res, err := svc.StartItems(ctx, items[:1])
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(res.Orders) != 1 || res.Order == nil {
		t.Fatalf("start: order=%v orders=%d", res.Order, len(res.Orders))
	}
	check("start", res.Order.Id, res.Order.Version)
	if res.Order.Items[0].Status.String() != "ORDER_ITEM_STATUS_PREPARING" {
		t.Fatalf("start: item status %v", res.Order.Items[0].Status)
	}

	res, err = svc.MarkItemsReady(ctx, items[1:2])
	if err != nil {
		t.Fatalf("ready: %v", err)
	}
	check("ready", res.Order.Id, res.Order.Version)

	res, err = svc.BumpTicket(ctx, orderID.String(), stationID.String())
	if err != nil {
		t.Fatalf("bump: %v", err)
	}
	check("bump", res.Order.Id, res.Order.Version)
	for _, it := range res.Order.Items {
		if it.Status.String() != "ORDER_ITEM_STATUS_READY" {
			t.Fatalf("bump: item %s status %v", it.Id, it.Status)
		}
	}
}
