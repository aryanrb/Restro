package kitchen

import (
	"testing"
)

// StartItems moves a READY line back to PREPARING ("undo ready"), and item RPCs
// accept a line's client_id as well as its server id.
func TestStartItemsUndoesReadyByClientID(t *testing.T) {
	svc, pool, ctx := newKitchenTest(t)
	_, _, items := seedBilledOrder(t, ctx, pool, "DINE_IN", 1)
	var clientID, status string
	if err := pool.QueryRow(ctx, `SELECT client_id::text FROM order_items WHERE id = $1`, items[0]).Scan(&clientID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.MarkItemsReady(ctx, []string{clientID}); err != nil {
		t.Fatalf("ready: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM order_items WHERE id = $1`, items[0]).Scan(&status); err != nil || status != "READY" {
		t.Fatalf("after ready: status = %s (%v), want READY", status, err)
	}
	if _, err := svc.StartItems(ctx, []string{clientID}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM order_items WHERE id = $1`, items[0]).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "PREPARING" {
		t.Fatalf("status = %s, want PREPARING", status)
	}
}
