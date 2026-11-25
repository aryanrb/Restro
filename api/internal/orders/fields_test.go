package orders

import (
	"context"
	"testing"

	"github.com/google/uuid"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
)

func actorID(ctx context.Context) string { return auth.ActorFrom(ctx).User.ID.String() }

func (f *fixture) get(ctx context.Context, ocid string) *restaurantv1.Order {
	f.t.Helper()
	o, err := f.svc.GetOrder(ctx, &restaurantv1.GetOrderRequest{ClientId: &ocid})
	if err != nil {
		f.t.Fatalf("get order: %v", err)
	}
	return o
}

// Orders carry label / waiter name / created time; lines carry who added them,
// when they were sent, and whether they are billed.
func TestOrderAndLineFields(t *testing.T) {
	f := newFixture(t)
	ctx := f.as(auth.RoleWaiter)

	ocid := uuid.NewString()
	res, err := f.svc.CreateOrder(ctx, &restaurantv1.CreateOrderRequest{
		CommandId: uuid.NewString(), OrderClientId: ocid, Type: restaurantv1.OrderType_ORDER_TYPE_TAKEAWAY, Label: "Ram", GuestCount: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	o := res.Order
	if o.Label != "Ram" {
		t.Errorf("label = %q, want Ram", o.Label)
	}
	if o.WaiterName != auth.RoleWaiter { // fixture users are named after their role
		t.Errorf("waiter_name = %q, want %q", o.WaiterName, auth.RoleWaiter)
	}
	if o.CreatedAtUnix == 0 {
		t.Error("created_at_unix not set")
	}

	draft, sent := uuid.NewString(), uuid.NewString()
	if _, err := f.svc.AddItems(ctx, &restaurantv1.AddItemsRequest{CommandId: uuid.NewString(), OrderClientId: ocid,
		Items: []*restaurantv1.NewItem{{ClientId: draft, MenuItemId: f.itemID.String(), Quantity: 1}}}); err != nil {
		t.Fatalf("add draft: %v", err)
	}
	if _, err := f.svc.AddItems(ctx, &restaurantv1.AddItemsRequest{CommandId: uuid.NewString(), OrderClientId: ocid, Send: true,
		Items: []*restaurantv1.NewItem{{ClientId: sent, MenuItemId: f.itemID.String(), Quantity: 1}}}); err != nil {
		t.Fatalf("add+send: %v", err)
	}
	byCID := func(o *restaurantv1.Order) map[string]*restaurantv1.OrderItem {
		m := map[string]*restaurantv1.OrderItem{}
		for _, it := range o.Items {
			m[it.ClientId] = it
		}
		return m
	}
	lines := byCID(f.get(ctx, ocid))
	if lines[draft].SentAtUnix != 0 {
		t.Errorf("draft line sent_at_unix = %d, want 0", lines[draft].SentAtUnix)
	}
	if lines[sent].SentAtUnix == 0 {
		t.Error("add+send line has no sent_at_unix")
	}
	for cid, it := range lines {
		if it.AddedByUserId != actorID(ctx) {
			t.Errorf("line %s added_by_user_id = %q, want %q", cid, it.AddedByUserId, actorID(ctx))
		}
		if it.Billed || it.InvoiceId != "" {
			t.Errorf("line %s billed before any invoice", cid)
		}
	}

	if _, err := f.svc.SendItems(ctx, &restaurantv1.SendItemsRequest{CommandId: uuid.NewString(), OrderClientId: ocid}); err != nil {
		t.Fatalf("send: %v", err)
	}
	f.bill("ISSUED", lines[sent].Id)
	lines = byCID(f.get(ctx, ocid))
	if lines[draft].SentAtUnix == 0 {
		t.Error("sent line has no sent_at_unix")
	}
	if !lines[sent].Billed || lines[sent].InvoiceId == "" {
		t.Errorf("billed line: billed=%v invoice_id=%q", lines[sent].Billed, lines[sent].InvoiceId)
	}
	if lines[draft].Billed {
		t.Error("unbilled line reported billed")
	}
}

func TestSetGuestCount(t *testing.T) {
	f := newFixture(t)
	ctx := f.as(auth.RoleWaiter)
	ocid, _, ver := f.openOrder(ctx, f.tableA)

	cmd := uuid.NewString()
	res, err := f.svc.SetGuestCount(ctx, &restaurantv1.SetGuestCountRequest{CommandId: cmd, OrderClientId: ocid, BaseVersion: ver, GuestCount: 5})
	if err != nil {
		t.Fatalf("set guest count: %v", err)
	}
	if res.Order.GuestCount != 5 || res.Order.Version != ver+1 {
		t.Fatalf("guest_count=%d version=%d, want 5 / %d", res.Order.GuestCount, res.Order.Version, ver+1)
	}
	// Idempotent replay returns the original result.
	again, err := f.svc.SetGuestCount(ctx, &restaurantv1.SetGuestCountRequest{CommandId: cmd, OrderClientId: ocid, BaseVersion: ver, GuestCount: 5})
	if err != nil || again.Order.Version != res.Order.Version {
		t.Fatalf("replay: %v (version %d)", err, again.GetOrder().GetVersion())
	}
	// Stale base version.
	_, err = f.svc.SetGuestCount(ctx, &restaurantv1.SetGuestCountRequest{CommandId: uuid.NewString(), OrderClientId: ocid, BaseVersion: ver, GuestCount: 3})
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_STALE_VERSION)
	if got := f.get(ctx, ocid).GuestCount; got != 5 {
		t.Fatalf("guest count after stale = %d, want 5", got)
	}
	// Not on a closed order.
	f.exec(`UPDATE orders SET status = 'CLOSED' WHERE client_id = $1`, ocid)
	_, err = f.svc.SetGuestCount(ctx, &restaurantv1.SetGuestCountRequest{CommandId: uuid.NewString(), OrderClientId: ocid, BaseVersion: res.Order.Version, GuestCount: 3})
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_ORDER_CLOSED)
}

// Transferring to no table takes the order off the floor (TAKEAWAY); moving it
// back onto a table makes it DINE_IN again.
func TestTransferTableToNoTableMakesTakeaway(t *testing.T) {
	f := newFixture(t)
	ctx := f.as(auth.RoleWaiter)
	ocid, _, ver := f.openOrder(ctx, f.tableA)

	res, err := f.svc.TransferTable(ctx, &restaurantv1.TransferTableRequest{CommandId: uuid.NewString(), OrderClientId: ocid, BaseVersion: ver})
	if err != nil {
		t.Fatalf("transfer to none: %v", err)
	}
	if res.Order.Type != restaurantv1.OrderType_ORDER_TYPE_TAKEAWAY || res.Order.TableId != nil {
		t.Fatalf("type=%v table=%v, want TAKEAWAY/no table", res.Order.Type, res.Order.TableId)
	}
	res, err = f.svc.TransferTable(ctx, &restaurantv1.TransferTableRequest{CommandId: uuid.NewString(), OrderClientId: ocid, BaseVersion: res.Order.Version, NewTableId: f.tableB.String()})
	if err != nil {
		t.Fatalf("transfer to table: %v", err)
	}
	if res.Order.Type != restaurantv1.OrderType_ORDER_TYPE_DINE_IN || res.Order.GetTableId() != f.tableB.String() {
		t.Fatalf("type=%v table=%v, want DINE_IN on tableB", res.Order.Type, res.Order.GetTableId())
	}
}

func TestMergeCancelsSourceAndSumsGuests(t *testing.T) {
	f := newFixture(t)
	ctx := f.as(auth.RoleWaiter)
	src, _, srcVer := f.openOrder(ctx, f.tableA) // guest_count 2
	tgt, _, tgtVer := f.openOrder(ctx, f.tableB) // guest_count 2

	res, err := f.svc.MergeTables(ctx, &restaurantv1.MergeTablesRequest{
		CommandId: uuid.NewString(), SourceOrderClientId: src, SourceBaseVersion: srcVer, TargetOrderClientId: tgt, TargetBaseVersion: tgtVer,
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if res.Order.GuestCount != 4 || len(res.Order.Items) != 2 {
		t.Fatalf("target guest_count=%d items=%d, want 4 / 2", res.Order.GuestCount, len(res.Order.Items))
	}
	if got := f.orderStatus(src); got != "CANCELLED" {
		t.Fatalf("source status = %s, want CANCELLED", got)
	}
}

// A command queued by one user must not run under another user's session — also
// when it is a replay of an already processed command.
func TestCommandsRejectWrongUser(t *testing.T) {
	f := newFixture(t)
	ctxA := f.as(auth.RoleWaiter)
	ctxB := f.as(auth.RoleWaiter)
	a, b := actorID(ctxA), actorID(ctxB)

	wantWrongUser := func(name string, err error) {
		t.Helper()
		ae := wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED)
		if ae.Field != "user_id" {
			t.Fatalf("%s: field = %q, want user_id", name, ae.Field)
		}
	}

	ocid, itemID, ver := f.openOrder(ctxA, f.tableA)
	other, _, otherVer := f.openOrder(ctxA, f.tableB)
	tid := f.tableA.String()
	_, err := f.svc.CreateOrder(ctxB, &restaurantv1.CreateOrderRequest{CommandId: uuid.NewString(), OrderClientId: uuid.NewString(),
		Type: restaurantv1.OrderType_ORDER_TYPE_DINE_IN, TableId: &tid, UserId: &a})
	wantWrongUser("CreateOrder", err)
	_, err = f.svc.AddItems(ctxB, &restaurantv1.AddItemsRequest{CommandId: uuid.NewString(), OrderClientId: ocid, UserId: &a,
		Items: []*restaurantv1.NewItem{{ClientId: uuid.NewString(), MenuItemId: f.itemID.String(), Quantity: 1}}})
	wantWrongUser("AddItems", err)
	_, err = f.svc.SendItems(ctxB, &restaurantv1.SendItemsRequest{CommandId: uuid.NewString(), OrderClientId: ocid, UserId: &a})
	wantWrongUser("SendItems", err)
	_, err = f.svc.VoidItem(ctxB, &restaurantv1.VoidItemRequest{CommandId: uuid.NewString(), OrderClientId: ocid, BaseVersion: ver, ItemId: itemID, UserId: &a})
	wantWrongUser("VoidItem", err)
	_, err = f.svc.MarkServed(ctxB, &restaurantv1.MarkServedRequest{CommandId: uuid.NewString(), OrderClientId: ocid, ItemIds: []string{itemID}, UserId: &a})
	wantWrongUser("MarkServed", err)
	_, err = f.svc.TransferTable(ctxB, &restaurantv1.TransferTableRequest{CommandId: uuid.NewString(), OrderClientId: ocid, BaseVersion: ver, UserId: &a})
	wantWrongUser("TransferTable", err)
	_, err = f.svc.MergeTables(ctxB, &restaurantv1.MergeTablesRequest{CommandId: uuid.NewString(), SourceOrderClientId: other, SourceBaseVersion: otherVer,
		TargetOrderClientId: ocid, TargetBaseVersion: ver, UserId: &a})
	wantWrongUser("MergeTables", err)
	_, err = f.svc.SetGuestCount(ctxB, &restaurantv1.SetGuestCountRequest{CommandId: uuid.NewString(), OrderClientId: ocid, BaseVersion: ver, GuestCount: 3, UserId: &a})
	wantWrongUser("SetGuestCount", err)

	// Matching user runs; a replay of that command under another session is refused.
	cmd := uuid.NewString()
	if _, err := f.svc.SetGuestCount(ctxA, &restaurantv1.SetGuestCountRequest{CommandId: cmd, OrderClientId: ocid, BaseVersion: ver, GuestCount: 3, UserId: &a}); err != nil {
		t.Fatalf("matching user: %v", err)
	}
	_, err = f.svc.SetGuestCount(ctxB, &restaurantv1.SetGuestCountRequest{CommandId: cmd, OrderClientId: ocid, BaseVersion: ver, GuestCount: 3, UserId: &a})
	wantWrongUser("replay", err)
	// The same replay from the right user returns the stored result.
	if _, err := f.svc.SetGuestCount(ctxA, &restaurantv1.SetGuestCountRequest{CommandId: cmd, OrderClientId: ocid, BaseVersion: ver, GuestCount: 3, UserId: &a}); err != nil {
		t.Fatalf("replay as the right user: %v", err)
	}
	// user_id is optional: omitted means "whoever is signed in".
	if _, err := f.svc.SendItems(ctxB, &restaurantv1.SendItemsRequest{CommandId: uuid.NewString(), OrderClientId: ocid, UserId: &b}); err != nil {
		t.Fatalf("own user_id: %v", err)
	}
	if _, err := f.svc.SendItems(ctxB, &restaurantv1.SendItemsRequest{CommandId: uuid.NewString(), OrderClientId: ocid}); err != nil {
		t.Fatalf("no user_id: %v", err)
	}
}
