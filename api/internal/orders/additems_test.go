package orders

import (
	"context"
	"testing"

	"github.com/google/uuid"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
)

// menuFixture adds a sized item with a variant, a linked optional group (max 2),
// a linked required group (min 1, max 1), and an unlinked group, on top of the
// base fixture's plain "Momo" item (25000).
type menuFixture struct {
	pizza, other               uuid.UUID // pizza: variants + groups; other: a second item with its own variant
	small, large, otherVariant uuid.UUID
	cheese, egg, olive         uuid.UUID // optional group "Extras" (0..2): +5000, +3000, +1000
	mild, hot                  uuid.UUID // required group "Spice" (1..1): +0, +500
	unlinked                   uuid.UUID // modifier in a group not linked to pizza
	plainCheese                uuid.UUID // modifier in a group linked to the plain Momo item
	station                    uuid.UUID
	category                   uuid.UUID
}

func (f *fixture) menu() menuFixture {
	f.t.Helper()
	ctx := context.Background()
	f.exec(`TRUNCATE modifier_groups RESTART IDENTITY CASCADE`)
	momo, err := f.q.GetItem(ctx, f.itemID)
	if err != nil {
		f.t.Fatalf("get item: %v", err)
	}
	var m menuFixture
	m.station, m.category = momo.StationID, momo.CategoryID
	mustItem := func(name string, price int64) uuid.UUID {
		it, err := f.q.CreateItem(ctx, sqlc.CreateItemParams{CategoryID: m.category, NameEn: name, PricePaisa: price, StationID: m.station, Active: true})
		if err != nil {
			f.t.Fatalf("item: %v", err)
		}
		return it.ID
	}
	mustVariant := func(item uuid.UUID, name string, price int64) uuid.UUID {
		v, err := f.q.CreateVariant(ctx, sqlc.CreateVariantParams{ItemID: item, NameEn: name, PricePaisa: price})
		if err != nil {
			f.t.Fatalf("variant: %v", err)
		}
		return v.ID
	}
	mustGroup := func(name string, min, max int32) uuid.UUID {
		g, err := f.q.CreateModifierGroup(ctx, sqlc.CreateModifierGroupParams{NameEn: name, MinSelect: min, MaxSelect: max})
		if err != nil {
			f.t.Fatalf("group: %v", err)
		}
		return g.ID
	}
	mustMod := func(group uuid.UUID, name string, delta int64) uuid.UUID {
		mod, err := f.q.CreateModifier(ctx, sqlc.CreateModifierParams{GroupID: group, NameEn: name, PriceDeltaPaisa: delta})
		if err != nil {
			f.t.Fatalf("modifier: %v", err)
		}
		return mod.ID
	}
	link := func(item, group uuid.UUID) {
		if err := f.q.CreateItemModifierLink(ctx, sqlc.CreateItemModifierLinkParams{ItemID: item, GroupID: group}); err != nil {
			f.t.Fatalf("link: %v", err)
		}
	}

	m.pizza = mustItem("Pizza", 50000)
	m.small = mustVariant(m.pizza, "Small", 40000)
	m.large = mustVariant(m.pizza, "Large", 60000)
	m.other = mustItem("Burger", 30000)
	m.otherVariant = mustVariant(m.other, "Double", 45000)

	extras := mustGroup("Extras", 0, 2)
	m.cheese = mustMod(extras, "Cheese", 5000)
	m.egg = mustMod(extras, "Egg", 3000)
	m.olive = mustMod(extras, "Olive", 1000)
	spice := mustGroup("Spice", 1, 1)
	m.mild = mustMod(spice, "Mild", 0)
	m.hot = mustMod(spice, "Hot", 500)
	link(m.pizza, extras)
	link(m.pizza, spice)

	loose := mustGroup("Sauces", 0, 3)
	m.unlinked = mustMod(loose, "Ketchup", 200)

	momoExtras := mustGroup("Momo extras", 0, 1)
	m.plainCheese = mustMod(momoExtras, "Cheese", 2500)
	link(f.itemID, momoExtras)
	return m
}

func (f *fixture) newOrder(ctx context.Context, typ restaurantv1.OrderType) string {
	f.t.Helper()
	ocid := uuid.NewString()
	req := &restaurantv1.CreateOrderRequest{CommandId: uuid.NewString(), OrderClientId: ocid, Type: typ, GuestCount: 1}
	if typ == restaurantv1.OrderType_ORDER_TYPE_DINE_IN {
		tid := f.tableA.String()
		req.TableId = &tid
	}
	if _, err := f.svc.CreateOrder(ctx, req); err != nil {
		f.t.Fatalf("create order: %v", err)
	}
	return ocid
}

func line(item uuid.UUID, variant *uuid.UUID, mods ...uuid.UUID) *restaurantv1.NewItem {
	ni := &restaurantv1.NewItem{ClientId: uuid.NewString(), MenuItemId: item.String(), Quantity: 1}
	if variant != nil {
		v := variant.String()
		ni.VariantId = &v
	}
	for _, m := range mods {
		ni.ModifierIds = append(ni.ModifierIds, m.String())
	}
	return ni
}

func itemByClientID(o *restaurantv1.Order, cid string) *restaurantv1.OrderItem {
	for _, it := range o.Items {
		if it.ClientId == cid {
			return it
		}
	}
	return nil
}

func TestAddItemsUnitPriceIncludesModifierDeltas(t *testing.T) {
	f := newFixture(t)
	m := f.menu()
	ctx := f.as(auth.RoleWaiter)
	ocid := f.newOrder(ctx, restaurantv1.OrderType_ORDER_TYPE_DINE_IN)

	sized := line(m.pizza, &m.large, m.cheese, m.egg, m.hot) // 60000 + 5000 + 3000 + 500
	plain := line(f.itemID, nil, m.plainCheese)              // 25000 + 2500
	bare := line(f.itemID, nil)                              // 25000
	res, err := f.svc.AddItems(ctx, &restaurantv1.AddItemsRequest{
		CommandId: uuid.NewString(), OrderClientId: ocid, Items: []*restaurantv1.NewItem{sized, plain, bare},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if len(res.Rejected) != 0 {
		t.Fatalf("rejected: %v", res.Rejected)
	}
	for _, c := range []struct {
		cid  string
		want int64
		mods int
	}{{sized.ClientId, 68500, 3}, {plain.ClientId, 27500, 1}, {bare.ClientId, 25000, 0}} {
		it := itemByClientID(res.Order, c.cid)
		if it == nil {
			t.Fatalf("line %s missing", c.cid)
		}
		if it.UnitPricePaisa != c.want {
			t.Errorf("%s unit price = %d, want %d", it.Name.En, it.UnitPricePaisa, c.want)
		}
		if len(it.Modifiers) != c.mods {
			t.Errorf("%s modifiers = %d, want %d", it.Name.En, len(it.Modifiers), c.mods)
		}
	}
	if got := itemByClientID(res.Order, sized.ClientId).VariantName.GetEn(); got != "Large" {
		t.Errorf("variant snapshot = %q, want Large", got)
	}
}

func TestAddItemsRejectsInvalidLines(t *testing.T) {
	f := newFixture(t)
	m := f.menu()
	ctx := f.as(auth.RoleWaiter)
	ocid := f.newOrder(ctx, restaurantv1.OrderType_ORDER_TYPE_DINE_IN)

	inactive, err := f.q.CreateItem(context.Background(), sqlc.CreateItemParams{CategoryID: m.category, NameEn: "Old", PricePaisa: 100, StationID: m.station, Active: false})
	if err != nil {
		t.Fatalf("item: %v", err)
	}
	sold := line(f.itemID, nil)
	f.exec(`UPDATE menu_items SET is_86d = true WHERE id = $1`, m.other)

	invalid := restaurantv1.ErrorReason_ERROR_REASON_INVALID
	cases := map[string]struct {
		line   *restaurantv1.NewItem
		reason restaurantv1.ErrorReason
	}{
		"unknown item":               {line(uuid.New(), nil), invalid},
		"inactive item":              {line(inactive.ID, nil), invalid},
		"86d item":                   {line(m.other, &m.otherVariant), restaurantv1.ErrorReason_ERROR_REASON_ITEM_86D},
		"variant of another item":    {line(m.pizza, &m.otherVariant, m.mild), invalid},
		"unknown variant":            {line(m.pizza, ptr(uuid.New()), m.mild), invalid},
		"variant on plain item":      {line(f.itemID, &m.small), invalid},
		"missing required variant":   {line(m.pizza, nil, m.mild), invalid},
		"unknown modifier":           {line(m.pizza, &m.small, m.mild, uuid.New()), invalid},
		"modifier of unlinked group": {line(m.pizza, &m.small, m.mild, m.unlinked), invalid},
		"duplicate modifier":         {line(m.pizza, &m.small, m.mild, m.cheese, m.cheese), invalid},
		"group min violated":         {line(m.pizza, &m.small, m.cheese), invalid},
		"group max violated":         {line(m.pizza, &m.small, m.mild, m.cheese, m.egg, m.olive), invalid},
		"required group over max":    {line(m.pizza, &m.small, m.mild, m.hot), invalid},
	}
	items := []*restaurantv1.NewItem{sold}
	byCID := map[string]string{}
	for name, c := range cases {
		items = append(items, c.line)
		byCID[c.line.ClientId] = name
	}
	res, err := f.svc.AddItems(ctx, &restaurantv1.AddItemsRequest{CommandId: uuid.NewString(), OrderClientId: ocid, Items: items})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	got := map[string]*restaurantv1.RejectedLine{}
	for _, r := range res.Rejected {
		got[r.ClientId] = r
	}
	for name, c := range cases {
		r := got[c.line.ClientId]
		if r == nil {
			t.Errorf("%s: not rejected", name)
			continue
		}
		if r.Reason != c.reason {
			t.Errorf("%s: reason %v (%q), want %v", name, r.Reason, r.Message, c.reason)
		}
		if r.Message == "" {
			t.Errorf("%s: empty message", name)
		}
	}
	if len(res.Order.Items) != 1 || res.Order.Items[0].ClientId != sold.ClientId {
		t.Fatalf("applied lines = %d, want only the valid one", len(res.Order.Items))
	}
}

func ptr[T any](v T) *T { return &v }

func TestAddItemsSendCreatesSentLines(t *testing.T) {
	f := newFixture(t)
	ctx := f.as(auth.RoleWaiter)
	ocid := f.newOrder(ctx, restaurantv1.OrderType_ORDER_TYPE_DINE_IN)

	draft, err := f.svc.AddItems(ctx, &restaurantv1.AddItemsRequest{
		CommandId: uuid.NewString(), OrderClientId: ocid, Items: []*restaurantv1.NewItem{line(f.itemID, nil)},
	})
	if err != nil {
		t.Fatalf("add draft: %v", err)
	}
	if st := draft.Order.Items[0].Status; st != restaurantv1.OrderItemStatus_ORDER_ITEM_STATUS_DRAFT {
		t.Fatalf("default status = %v, want DRAFT", st)
	}

	sent := line(f.itemID, nil)
	res, err := f.svc.AddItems(ctx, &restaurantv1.AddItemsRequest{
		CommandId: uuid.NewString(), OrderClientId: ocid, Send: true,
		Items: []*restaurantv1.NewItem{sent, line(uuid.New(), nil)},
	})
	if err != nil {
		t.Fatalf("add+send: %v", err)
	}
	if len(res.Rejected) != 1 {
		t.Fatalf("rejected = %d, want 1", len(res.Rejected))
	}
	if st := itemByClientID(res.Order, sent.ClientId).Status; st != restaurantv1.OrderItemStatus_ORDER_ITEM_STATUS_SENT {
		t.Fatalf("send=true status = %v, want SENT", st)
	}
	// The earlier DRAFT line is untouched: send only applies to this command's lines.
	if st := itemByClientID(res.Order, draft.Order.Items[0].ClientId).Status; st != restaurantv1.OrderItemStatus_ORDER_ITEM_STATUS_DRAFT {
		t.Fatalf("earlier draft status = %v, want DRAFT", st)
	}
	// It shows on the station's board.
	rows, err := f.q.ListStationOpenItems(context.Background(), uuid.MustParse(itemByClientID(res.Order, sent.ClientId).StationId))
	if err != nil {
		t.Fatalf("station items: %v", err)
	}
	if len(rows) != 1 || rows[0].ClientID.String() != sent.ClientId {
		t.Fatalf("station board = %d rows, want the sent line", len(rows))
	}
}

func TestAddItemsLineIdempotency(t *testing.T) {
	f := newFixture(t)
	ctx := f.as(auth.RoleWaiter)
	ocid := f.newOrder(ctx, restaurantv1.OrderType_ORDER_TYPE_DINE_IN)

	a := line(f.itemID, nil)
	res, err := f.svc.AddItems(ctx, &restaurantv1.AddItemsRequest{CommandId: uuid.NewString(), OrderClientId: ocid, Items: []*restaurantv1.NewItem{a}})
	if err != nil {
		t.Fatalf("first add: %v", err)
	}
	ver := res.Order.Version

	// Same line re-sent under a NEW command id (e.g. the client rebuilt its queue),
	// alongside a fresh line: the duplicate is skipped, the new one applied.
	b := line(f.itemID, nil)
	res, err = f.svc.AddItems(ctx, &restaurantv1.AddItemsRequest{CommandId: uuid.NewString(), OrderClientId: ocid, Items: []*restaurantv1.NewItem{a, b}})
	if err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if len(res.Rejected) != 0 {
		t.Fatalf("rejected = %v, want none", res.Rejected)
	}
	if len(res.Order.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(res.Order.Items))
	}

	// Only a duplicate: no-op, version unchanged.
	ver = res.Order.Version
	res, err = f.svc.AddItems(ctx, &restaurantv1.AddItemsRequest{CommandId: uuid.NewString(), OrderClientId: ocid, Items: []*restaurantv1.NewItem{a}})
	if err != nil {
		t.Fatalf("dup only: %v", err)
	}
	if len(res.Order.Items) != 2 || res.Order.Version != ver {
		t.Fatalf("dup-only add changed the order: items=%d version=%d (was %d)", len(res.Order.Items), res.Order.Version, ver)
	}

	// The same line id on a different order is a client bug: rejected, not silently dropped.
	other := f.newOrder(ctx, restaurantv1.OrderType_ORDER_TYPE_TAKEAWAY)
	res, err = f.svc.AddItems(ctx, &restaurantv1.AddItemsRequest{CommandId: uuid.NewString(), OrderClientId: other, Items: []*restaurantv1.NewItem{a}})
	if err != nil {
		t.Fatalf("cross-order add: %v", err)
	}
	if len(res.Rejected) != 1 || res.Rejected[0].Reason != restaurantv1.ErrorReason_ERROR_REASON_INVALID {
		t.Fatalf("cross-order dup rejected = %v, want one INVALID", res.Rejected)
	}
	if len(res.Order.Items) != 0 {
		t.Fatalf("cross-order dup was applied")
	}
}
