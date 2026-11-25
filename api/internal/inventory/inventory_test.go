package inventory_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/inventory"
	"github.com/oneaccord/restro/api/internal/orders"
	"github.com/oneaccord/restro/api/internal/realtime"
)

const (
	reasonInvalid      = restaurantv1.ErrorReason_ERROR_REASON_INVALID
	reasonNotAllowed   = restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED
	reasonStale        = restaurantv1.ErrorReason_ERROR_REASON_STALE_VERSION
	reasonUnitLocked   = restaurantv1.ErrorReason_ERROR_REASON_UNIT_LOCKED
	reasonUntracked    = restaurantv1.ErrorReason_ERROR_REASON_ITEM_UNTRACKED
	reasonPosted       = restaurantv1.ErrorReason_ERROR_REASON_COUNT_ALREADY_POSTED
	reasonApprovalReq  = restaurantv1.ErrorReason_ERROR_REASON_MANAGER_APPROVAL_REQUIRED
	reasonApprovalBad  = restaurantv1.ErrorReason_ERROR_REASON_MANAGER_APPROVAL_INVALID
	typeSale           = restaurantv1.StockMovementType_STOCK_MOVEMENT_TYPE_SALE
	typeSaleReversal   = restaurantv1.StockMovementType_STOCK_MOVEMENT_TYPE_SALE_REVERSAL
	typeWaste          = restaurantv1.StockMovementType_STOCK_MOVEMENT_TYPE_WASTE
	typeCountAdjust    = restaurantv1.StockMovementType_STOCK_MOVEMENT_TYPE_COUNT_ADJUST
	typeOpening        = restaurantv1.StockMovementType_STOCK_MOVEMENT_TYPE_OPENING
	wasteSpoiled       = restaurantv1.WasteReason_WASTE_REASON_SPOILED
	wasteOther         = restaurantv1.WasteReason_WASTE_REASON_OTHER
	wasteStaffMeal     = restaurantv1.WasteReason_WASTE_REASON_STAFF_MEAL
	wasteDropped       = restaurantv1.WasteReason_WASTE_REASON_DROPPED
	statusOpen         = restaurantv1.CountStatus_COUNT_STATUS_OPEN
	statusSubmitted    = restaurantv1.CountStatus_COUNT_STATUS_SUBMITTED
	statusPosted       = restaurantv1.CountStatus_COUNT_STATUS_POSTED
	statusDiscarded    = restaurantv1.CountStatus_COUNT_STATUS_DISCARDED
	unitG              = restaurantv1.StockUnit_STOCK_UNIT_G
	unitPcs            = restaurantv1.StockUnit_STOCK_UNIT_PCS
	allDay             = int64(0)
	approvalThreshold  = 100_000
	varianceHighlightP = 50_000
)

var _ = approvalThreshold + varianceHighlightP

// --- inventory items ---

func TestItemsCostsHiddenFromStaff(t *testing.T) {
	f := newFixture(t)
	list, err := f.inv.ListInventoryItems(f.owner, false)
	f.must(err)
	if len(list) != 2 || list[0].Name.En != "Chicken" || !list[0].Tracked || list[0].OnHandMilli != 5000*g || list[0].GetUnitCostE4() != rs60 ||
		list[1].Name.En != "Flour" || list[1].Tracked || list[1].OnHandMilli != 0 || list[1].GetUnitCostE4() != 50000 {
		t.Fatalf("list = %v", list)
	}
	if !list[0].UnitLocked || list[1].UnitLocked {
		t.Fatalf("unit locked: %v %v", list[0].UnitLocked, list[1].UnitLocked)
	}
	mv := f.movements(f.chicken)
	if len(mv) != 1 || mv[0].Type != typeOpening || mv[0].QuantityMilli != 5000*g || mv[0].UnitCostE4 != rs60 || mv[0].ByUserId != f.ownerID.String() {
		t.Fatalf("movements = %v", mv)
	}
	staff, err := f.inv.ListInventoryItems(f.kitchen, false)
	f.must(err)
	for _, it := range staff {
		if it.UnitCostE4 != nil {
			t.Fatalf("kitchen sees unit cost: %v", it)
		}
	}
	_, err = f.inv.SaveInventoryItem(f.kitchen, &restaurantv1.SaveInventoryItemRequest{Input: input("Paneer", "g", 0, 0)})
	wantReason(t, err, reasonNotAllowed)
	_, err = f.inv.ListInventoryItems(f.waiter, false)
	wantReason(t, err, reasonNotAllowed)
}

func TestSaveChecksVersionNameAndLockedUnit(t *testing.T) {
	f := newFixture(t)
	list, _ := f.inv.ListInventoryItems(f.owner, false)
	c := list[0]
	edit := func(in *restaurantv1.InventoryItemInput, id string, version int64) (*restaurantv1.InventoryItem, error) {
		in.Id = &id
		return f.inv.SaveInventoryItem(f.owner, &restaurantv1.SaveInventoryItemRequest{Input: in, BaseVersion: &version})
	}
	_, err := edit(input("Chicken", "g", 0, 0), f.chicken, c.Version-1)
	wantReason(t, err, reasonStale)
	_, err = edit(input("Chicken", "pcs", 0, 0), f.chicken, c.Version)
	if ae := wantReason(t, err, reasonUnitLocked); ae.Field != "unit" {
		t.Fatalf("field = %q", ae.Field)
	}
	_, err = f.inv.SaveInventoryItem(f.owner, &restaurantv1.SaveInventoryItemRequest{Input: input("  ", "g", 0, 0)})
	wantInvalid(t, err, "name")
	_, err = f.inv.SaveInventoryItem(f.owner, &restaurantv1.SaveInventoryItemRequest{Input: input("X", "g", -1, 0)})
	wantInvalid(t, err, "unit_cost_e4")
	saved, err := edit(input("Chicken (boneless)", "g", 651235, 3000*g), f.chicken, c.Version)
	f.must(err)
	if saved.GetUnitCostE4() != 651235 || saved.Version != c.Version+1 || saved.Name.En != "Chicken (boneless)" {
		t.Fatalf("saved = %v", saved)
	}
	fl, err := edit(input("Flour", "pcs", 0, 0), f.flour, 1)
	f.must(err)
	if fl.Unit != unitPcs {
		t.Fatalf("flour unit = %v (no movements yet, so it may change)", fl.Unit)
	}
	acts := f.auditActions()
	for _, want := range []string{"INVENTORY_ITEM_CHANGED", "TRACKING_CHANGED", "RECIPE_CHANGED"} {
		if !slices.Contains(acts, want) {
			t.Fatalf("audit %v lacks %s", acts, want)
		}
	}
}

func TestTrackingOnNeedsOpeningCount(t *testing.T) {
	f := newFixture(t)
	_, err := f.inv.SetTracked(f.owner, &restaurantv1.SetTrackedRequest{Id: f.flour, Tracked: true})
	wantInvalid(t, err, "opening_milli")
	_, err = f.inv.SetTracked(f.owner, &restaurantv1.SetTrackedRequest{Id: f.chicken, Tracked: false})
	f.must(err)
	_, err = f.inv.SetTracked(f.owner, &restaurantv1.SetTrackedRequest{Id: f.chicken, Tracked: true, OpeningMilli: ptr(4000 * g)})
	f.must(err)
	if got := f.onHand(f.chicken); got != 4000*g {
		t.Fatalf("on hand = %d", got)
	}
	var got [][2]int64
	for _, m := range f.movements(f.chicken) {
		got = append(got, [2]int64{int64(m.Type), m.QuantityMilli})
	}
	want := [][2]int64{{int64(typeOpening), 5000 * g}, {int64(typeOpening), -1000 * g}}
	if !slices.Equal(got, want) {
		t.Fatalf("movements = %v, want %v", got, want)
	}
	_, err = f.inv.SetTracked(f.cashier, &restaurantv1.SetTrackedRequest{Id: f.chicken, Tracked: false})
	wantReason(t, err, reasonNotAllowed)
}

func TestStockLedgerIsImmutable(t *testing.T) {
	f := newFixture(t)
	if _, err := f.pool.Exec(context.Background(), `UPDATE stock_movements SET quantity_milli = 0`); err == nil {
		t.Fatal("stock movements were updated")
	}
	if _, err := f.pool.Exec(context.Background(), `DELETE FROM stock_movements`); err == nil {
		t.Fatal("stock movements were deleted")
	}
}

// --- recipes ---

func (f *fixture) menuItem(id uuid.UUID) *restaurantv1.MenuItem {
	f.t.Helper()
	m, err := f.cat.GetMenu(f.owner)
	f.must(err)
	for _, it := range m.Items {
		if it.Id == id.String() {
			return it
		}
	}
	f.t.Fatalf("menu item %s missing", id)
	return nil
}

func TestItemRecipeSavedWithVersionAndKeptOnItemSave(t *testing.T) {
	f := newFixture(t)
	momo := f.menuItem(f.momo)
	if len(momo.Recipe) != 2 || momo.Version != 2 || momo.Recipe[0].QuantityMilli != 120*g {
		t.Fatalf("momo = %v", momo)
	}
	_, err := f.cat.SaveRecipe(f.owner, &restaurantv1.SaveRecipeRequest{MenuItemId: f.momo.String(), BaseVersion: 1})
	wantReason(t, err, reasonStale)
	_, err = f.cat.SaveRecipe(f.owner, &restaurantv1.SaveRecipeRequest{MenuItemId: f.momo.String(), BaseVersion: 2, Recipe: []*restaurantv1.RecipeLine{rl("nope", 1)}})
	wantInvalid(t, err, "recipe")
	_, err = f.cat.SaveRecipe(f.owner, &restaurantv1.SaveRecipeRequest{MenuItemId: f.momo.String(), BaseVersion: 2, Recipe: []*restaurantv1.RecipeLine{rl(f.chicken, 0)}})
	wantInvalid(t, err, "recipe")
	_, err = f.cat.SaveItem(f.owner, &restaurantv1.MenuItemInput{
		Id: ptr(f.momo.String()), CategoryId: momo.CategoryId, Name: &restaurantv1.LocalText{En: "Chicken Momo!", Ne: "x"},
		PricePaisa: 25000, Color: "#b91c1c", StationId: momo.StationId, Active: true, ModifierGroupIds: momo.ModifierGroupIds,
	}, ptr(int64(2)))
	f.must(err)
	if got := f.menuItem(f.momo).Recipe; len(got) != 2 {
		t.Fatalf("recipe after item save = %v", got)
	}
	n := 0
	for _, a := range f.auditActions() {
		if a == "RECIPE_CHANGED" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("RECIPE_CHANGED audits = %d", n)
	}
}

func TestVariantOwnRecipeAndModifierRecipeKeptAcrossGroupSave(t *testing.T) {
	f := newFixture(t)
	dal := f.menuItem(f.dal)
	saved, err := f.cat.SaveRecipe(f.owner, &restaurantv1.SaveRecipeRequest{
		MenuItemId: f.dal.String(), BaseVersion: dal.Version,
		Recipe: []*restaurantv1.RecipeLine{rl(f.flour, 200*g)},
		VariantRecipes: []*restaurantv1.VariantRecipe{
			{VariantId: f.chkVariant.String(), Lines: []*restaurantv1.RecipeLine{rl(f.chicken, 150*g)}},
			{VariantId: f.vegVariant.String(), UseItemRecipe: true},
		},
	})
	f.must(err)
	if saved.Variants[0].HasOwnRecipe || !saved.Variants[1].HasOwnRecipe || len(saved.Variants[1].Recipe) != 1 || saved.Variants[1].Recipe[0].QuantityMilli != 150*g {
		t.Fatalf("variants = %v", saved.Variants)
	}
	_, err = f.cat.SaveRecipe(f.owner, &restaurantv1.SaveRecipeRequest{
		MenuItemId: f.dal.String(), BaseVersion: saved.Version,
		VariantRecipes: []*restaurantv1.VariantRecipe{{VariantId: uuid.NewString()}},
	})
	wantInvalid(t, err, "variant_recipes")

	f.must(f.cat.SaveModifierRecipe(f.owner, &restaurantv1.SaveModifierRecipeRequest{ModifierId: f.cheese.String(), Recipe: []*restaurantv1.RecipeLine{rl(f.chicken, 10*g)}}))
	m, err := f.cat.GetMenu(f.owner)
	f.must(err)
	grp := m.ModifierGroups[0]
	var mods []*restaurantv1.ModifierInput
	for _, md := range grp.Modifiers {
		mods = append(mods, &restaurantv1.ModifierInput{Id: ptr(md.Id), Name: md.Name, PriceDeltaPaisa: md.PriceDeltaPaisa})
	}
	_, err = f.cat.SaveModifierGroup(f.owner, &restaurantv1.ModifierGroupInput{Id: ptr(grp.Id), Name: grp.Name, Min: 0, Max: 1, Modifiers: mods}, ptr(grp.Version))
	f.must(err)
	m, _ = f.cat.GetMenu(f.owner)
	if r := m.ModifierGroups[0].Modifiers[0].Recipe; len(r) != 1 || r[0].InventoryItemId != f.chicken || r[0].QuantityMilli != 10*g {
		t.Fatalf("modifier recipe = %v", r)
	}
	err = f.cat.SaveModifierRecipe(f.cashier, &restaurantv1.SaveModifierRecipeRequest{ModifierId: f.cheese.String()})
	wantReason(t, err, reasonNotAllowed)
}

func TestItemsWithoutRecipe(t *testing.T) {
	f := newFixture(t)
	_, err := f.cat.SaveRecipe(f.owner, &restaurantv1.SaveRecipeRequest{
		MenuItemId: f.dal.String(), BaseVersion: 1,
		VariantRecipes: []*restaurantv1.VariantRecipe{{VariantId: f.chkVariant.String(), Lines: []*restaurantv1.RecipeLine{rl(f.chicken, 150*g)}}},
	})
	f.must(err)
	rows, err := f.inv.ItemsWithoutRecipe(f.owner)
	f.must(err)
	var got []string
	for _, r := range rows {
		got = append(got, r.Name.En+"/"+r.GetVariantName().GetEn())
	}
	if !slices.Contains(got, "Dal Bhat Set/Veg") || !slices.Contains(got, "Veg Momo/") || slices.Contains(got, "Dal Bhat Set/Chicken") || slices.Contains(got, "Chicken Momo/") {
		t.Fatalf("without recipe = %v", got)
	}
	_, err = f.inv.ItemsWithoutRecipe(f.kitchen)
	wantReason(t, err, reasonNotAllowed)
}

// --- stock flow from orders ---

func (f *fixture) cheeseRecipe() {
	f.t.Helper()
	f.must(f.cat.SaveModifierRecipe(f.owner, &restaurantv1.SaveModifierRecipeRequest{ModifierId: f.cheese.String(), Recipe: []*restaurantv1.RecipeLine{rl(f.chicken, 10*g)}}))
}

func TestSendDeductsOnceAtSentAt(t *testing.T) {
	f := newFixture(t)
	f.cheeseRecipe()
	ocid := f.newOrder(f.waiter)
	req := &restaurantv1.AddItemsRequest{CommandId: uuid.NewString(), OrderClientId: ocid, Items: []*restaurantv1.NewItem{line(f.momo, 2, nil, f.cheese)}, Send: true}
	res, err := f.ord.AddItems(f.waiter, req)
	f.must(err)
	_, err = f.ord.AddItems(f.waiter, req) // retried command
	f.must(err)
	it := res.Order.Items[0]
	sale := f.movementsOfType(f.chicken, typeSale)
	if len(sale) != 1 || sale[0].QuantityMilli != -260*g || sale[0].RefId != it.Id || sale[0].AtUnix != it.SentAtUnix ||
		sale[0].UnitCostE4 != rs60 || sale[0].ByUserId != f.waiterID.String() {
		t.Fatalf("sale = %v (item %v)", sale, it)
	}
	if mv := f.movements(f.flour); len(mv) != 0 {
		t.Fatalf("untracked flour moved: %v", mv)
	}
	if got := f.onHand(f.chicken); got != 4740*g {
		t.Fatalf("on hand = %d", got)
	}
}

func TestSendItemsDeductsDraftLines(t *testing.T) {
	f := newFixture(t)
	ocid := f.newOrder(f.waiter)
	res, err := f.ord.AddItems(f.waiter, &restaurantv1.AddItemsRequest{CommandId: uuid.NewString(), OrderClientId: ocid, Items: []*restaurantv1.NewItem{line(f.momo, 1, nil)}})
	f.must(err)
	if n := len(f.movementsOfType(f.chicken, typeSale)); n != 0 {
		t.Fatalf("a draft deducted: %d", n)
	}
	sendReq := &restaurantv1.SendItemsRequest{CommandId: uuid.NewString(), OrderClientId: ocid}
	_, err = f.ord.SendItems(f.waiter, sendReq)
	f.must(err)
	_, err = f.ord.SendItems(f.waiter, &restaurantv1.SendItemsRequest{CommandId: uuid.NewString(), OrderClientId: ocid}) // nothing left to send
	f.must(err)
	sale := f.movementsOfType(f.chicken, typeSale)
	if len(sale) != 1 || sale[0].QuantityMilli != -120*g || sale[0].RefId != res.Order.Items[0].Id {
		t.Fatalf("sale = %v", sale)
	}
}

func TestVoidBeforeCookingReversesAfterCookingKeeps(t *testing.T) {
	f := newFixture(t)
	ocid := f.newOrder(f.waiter)
	o := f.send(f.waiter, ocid, line(f.momo, 1, nil), line(f.momo, 1, nil))
	a, b := o.Items[0], o.Items[1]
	_, err := f.ord.VoidItem(f.waiter, &restaurantv1.VoidItemRequest{CommandId: uuid.NewString(), OrderClientId: ocid, BaseVersion: o.Version, ItemId: a.Id, Reason: "Changed mind"})
	f.must(err)
	f.setLineStatus(b.Id, "PREPARING")
	_, err = f.ord.VoidItem(f.waiter, &restaurantv1.VoidItemRequest{
		CommandId: uuid.NewString(), OrderClientId: ocid, BaseVersion: f.version(f.waiter, ocid), ItemId: b.Id, Reason: "Dropped",
		Approval: &restaurantv1.ManagerApproval{ManagerUserId: f.managerID.String(), Pin: "2222"},
	})
	f.must(err)
	type mv struct {
		typ restaurantv1.StockMovementType
		qty int64
		ref string
	}
	var got []mv
	for _, m := range f.movements(f.chicken) {
		got = append(got, mv{m.Type, m.QuantityMilli, m.RefId})
	}
	want := []mv{{typeOpening, 5000 * g, f.chicken}, {typeSale, -120 * g, a.Id}, {typeSale, -120 * g, b.Id}, {typeSaleReversal, 120 * g, a.Id}}
	if !slices.Equal(got, want) {
		t.Fatalf("movements = %v\nwant %v", got, want)
	}
}

// CounterSale (billing) creates its lines SENT through orders.InsertLines, so the
// deduction happens there.
func TestInsertLinesSentDeducts(t *testing.T) {
	f := newFixture(t)
	ocid := f.newOrder(f.cashier)
	o, err := f.ord.GetOrder(f.cashier, &restaurantv1.GetOrderRequest{ClientId: &ocid})
	f.must(err)
	orderID := uuid.MustParse(o.Id)
	err = dbx.WithTx(f.cashier, f.pool, f.q, func(q *sqlc.Queries) error {
		_, rej, e := orders.InsertLines(f.cashier, q, orderID, []*restaurantv1.NewItem{line(f.momo, 3, nil)}, "SENT")
		if len(rej) > 0 {
			t.Fatalf("rejected %v", rej)
		}
		return e
	})
	f.must(err)
	sale := f.movementsOfType(f.chicken, typeSale)
	if len(sale) != 1 || sale[0].QuantityMilli != -360*g || sale[0].ByUserId != f.cashierID.String() {
		t.Fatalf("sale = %v", sale)
	}
}

func TestVariantRecipeReplacesItemRecipe(t *testing.T) {
	f := newFixture(t)
	_, err := f.cat.SaveRecipe(f.owner, &restaurantv1.SaveRecipeRequest{
		MenuItemId: f.dal.String(), BaseVersion: 1, Recipe: []*restaurantv1.RecipeLine{rl(f.chicken, 1*g)},
		VariantRecipes: []*restaurantv1.VariantRecipe{{VariantId: f.chkVariant.String(), Lines: []*restaurantv1.RecipeLine{rl(f.chicken, 150*g)}}},
	})
	f.must(err)
	ocid := f.newOrder(f.waiter)
	f.send(f.waiter, ocid, line(f.dal, 1, &f.chkVariant), line(f.dal, 1, &f.vegVariant))
	var got []int64
	for _, m := range f.movementsOfType(f.chicken, typeSale) {
		got = append(got, m.QuantityMilli)
	}
	if !slices.Equal(got, []int64{-150 * g, -1 * g}) {
		t.Fatalf("sales = %v", got)
	}
}

func eventsOf(evs []realtime.Event, typ string) []realtime.Event {
	var out []realtime.Event
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func TestLowStockOnceAnd86Suggestion(t *testing.T) {
	f := newFixture(t)
	ocid := f.newOrder(f.waiter)
	sendWithEvents := func(qty int32) []realtime.Event {
		ctx, take := inventory.WithEvents(f.waiter)
		f.send(ctx, ocid, line(f.momo, qty, nil))
		return take()
	}
	if evs := sendWithEvents(10); len(eventsOf(evs, "stock.low")) != 0 || len(eventsOf(evs, "stock.updated")) != 1 { // 5000 -> 3800
		t.Fatalf("events = %v", evs)
	}
	if evs := eventsOf(sendWithEvents(10), "stock.low"); len(evs) != 1 || evs[0].ID != f.chicken { // -> 2600: crossed
		t.Fatalf("stock.low = %v", evs)
	}
	if evs := eventsOf(sendWithEvents(1), "stock.low"); len(evs) != 0 { // still low: no second toast
		t.Fatalf("stock.low again = %v", evs)
	}
	if evs := eventsOf(sendWithEvents(20), "stock.86suggest"); len(evs) != 1 || evs[0].ID != f.momo.String() { // -> 80 g left
		t.Fatalf("stock.86suggest = %v", evs)
	}
	sug, err := f.inv.Suggestions86(f.kitchen)
	f.must(err)
	if len(sug) != 1 || sug[0].MenuItemId != f.momo.String() || sug[0].InventoryItemId != f.chicken || sug[0].LeftMilli != 80*g || sug[0].Unit != unitG {
		t.Fatalf("suggestions = %v", sug)
	}
	f.must(f.cat.Set86(f.kitchen, []string{f.momo.String()}, true)) // one tap uses the existing 86
	if sug, _ = f.inv.Suggestions86(f.kitchen); len(sug) != 0 {
		t.Fatalf("suggestions after 86 = %v", sug)
	}
	low, err := f.inv.LowStock(f.owner)
	f.must(err)
	if len(low) != 1 || low[0].InventoryItemId != f.chicken || low[0].OnHandMilli != 80*g || low[0].LowStockMilli != 3000*g {
		t.Fatalf("low stock = %v", low)
	}
	_, err = f.inv.Suggestions86(f.waiter)
	wantReason(t, err, reasonNotAllowed)
}

// --- waste ---

func invTarget(id string) *restaurantv1.LogWasteRequest_Inventory {
	return &restaurantv1.LogWasteRequest_Inventory{Inventory: &restaurantv1.InventoryWasteTarget{InventoryItemId: id}}
}

func TestRawWaste(t *testing.T) {
	f := newFixture(t)
	req := func(item string, qty int64, reason restaurantv1.WasteReason, requestID string) *restaurantv1.LogWasteRequest {
		return &restaurantv1.LogWasteRequest{Target: invTarget(item), QuantityMilli: qty, Reason: reason, RequestId: requestID}
	}
	_, err := f.inv.LogWaste(f.kitchen, req(f.chicken, 400*g, wasteOther, uuid.NewString()))
	wantInvalid(t, err, "note")
	_, err = f.inv.LogWaste(f.kitchen, req(f.flour, 400*g, wasteSpoiled, uuid.NewString()))
	wantReason(t, err, reasonUntracked)
	_, err = f.inv.LogWaste(f.kitchen, req(f.chicken, 0, wasteSpoiled, uuid.NewString()))
	wantInvalid(t, err, "quantity_milli")
	rid := uuid.NewString()
	entry, err := f.inv.LogWaste(f.kitchen, req(f.chicken, 400*g, wasteSpoiled, rid))
	f.must(err)
	again, err := f.inv.LogWaste(f.kitchen, req(f.chicken, 400*g, wasteSpoiled, rid))
	f.must(err)
	if again.Id != entry.Id {
		t.Fatalf("retry made a second entry")
	}
	if entry.ByName != "Bikash Tamang" || entry.Reason != wasteSpoiled || entry.ValuePaisa != nil || entry.Unit != unitG {
		t.Fatalf("entry = %v", entry)
	}
	if w := f.movementsOfType(f.chicken, typeWaste); len(w) != 1 || w[0].QuantityMilli != -400*g || w[0].RefId != entry.Id {
		t.Fatalf("waste movements = %v", w)
	}
	list, err := f.inv.ListWaste(f.kitchen, allDay, allDay)
	f.must(err)
	if len(list) != 1 || list[0].ValuePaisa != nil {
		t.Fatalf("kitchen list = %v", list)
	}
	list, _ = f.inv.ListWaste(f.owner, allDay, allDay)
	if list[0].GetValuePaisa() != 24000 {
		t.Fatalf("value = %d", list[0].GetValuePaisa())
	}
	if a := f.lastAudit(); a.action != "WASTE_LOGGED" || a.actor != f.kitchenID {
		t.Fatalf("audit = %v", a)
	}
	_, err = f.inv.LogWaste(f.waiter, req(f.chicken, 400*g, wasteSpoiled, uuid.NewString()))
	wantReason(t, err, reasonNotAllowed)
}

func TestMenuWasteAndManagerPinOverThreshold(t *testing.T) {
	f := newFixture(t)
	dropped, err := f.inv.LogWaste(f.cashier, &restaurantv1.LogWasteRequest{
		Target:        &restaurantv1.LogWasteRequest_Menu{Menu: &restaurantv1.MenuWasteTarget{MenuItemId: f.momo.String()}},
		QuantityMilli: 2 * g, Reason: wasteDropped, RequestId: uuid.NewString(),
	})
	f.must(err)
	if dropped.Name.En != "Chicken Momo" || dropped.Unit != restaurantv1.StockUnit_STOCK_UNIT_UNSPECIFIED {
		t.Fatalf("entry = %v", dropped)
	}
	if w := f.movementsOfType(f.chicken, typeWaste); len(w) != 1 || w[0].QuantityMilli != -240*g {
		t.Fatalf("waste = %v", w)
	}
	_, err = f.inv.LogWaste(f.cashier, &restaurantv1.LogWasteRequest{
		Target:        &restaurantv1.LogWasteRequest_Menu{Menu: &restaurantv1.MenuWasteTarget{MenuItemId: f.momo.String()}},
		QuantityMilli: 1500, Reason: wasteDropped, RequestId: uuid.NewString(),
	})
	wantInvalid(t, err, "quantity_milli")
	big := func(a *restaurantv1.ManagerApproval) (*restaurantv1.WasteEntry, error) { // Rs 1,200
		return f.inv.LogWaste(f.cashier, &restaurantv1.LogWasteRequest{Target: invTarget(f.chicken), QuantityMilli: 2000 * g, Reason: wasteStaffMeal, RequestId: uuid.NewString(), Approval: a})
	}
	_, err = big(nil)
	wantReason(t, err, reasonApprovalReq)
	_, err = big(&restaurantv1.ManagerApproval{ManagerUserId: f.managerID.String(), Pin: "9999"})
	wantReason(t, err, reasonApprovalBad)
	ok, err := big(&restaurantv1.ManagerApproval{ManagerUserId: f.managerID.String(), Pin: "2222"})
	f.must(err)
	if ok.ApprovedByUserId != f.managerID.String() || ok.ApprovedByName != "Sita Gurung" {
		t.Fatalf("entry = %v", ok)
	}
	if a := f.lastAudit(); a.action != "WASTE_LOGGED" || a.approver != f.managerID {
		t.Fatalf("audit = %v", a)
	}
}

func TestWastageReport(t *testing.T) {
	f := newFixture(t)
	_, err := f.inv.LogWaste(f.kitchen, &restaurantv1.LogWasteRequest{Target: invTarget(f.chicken), QuantityMilli: 100 * g, Reason: wasteStaffMeal, RequestId: uuid.NewString()})
	f.must(err)
	_, err = f.inv.LogWaste(f.kitchen, &restaurantv1.LogWasteRequest{Target: invTarget(f.chicken), QuantityMilli: 50 * g, Reason: wasteSpoiled, RequestId: uuid.NewString()})
	f.must(err)
	ocid := f.newOrder(f.waiter)
	o := f.send(f.waiter, ocid, line(f.momo, 1, nil))
	f.setLineStatus(o.Items[0].Id, "PREPARING")
	_, err = f.ord.VoidItem(f.waiter, &restaurantv1.VoidItemRequest{
		CommandId: uuid.NewString(), OrderClientId: ocid, BaseVersion: f.version(f.waiter, ocid), ItemId: o.Items[0].Id, Reason: "Burnt",
		Approval: &restaurantv1.ManagerApproval{ManagerUserId: f.managerID.String(), Pin: "2222"},
	})
	f.must(err)
	_, err = f.inv.Wastage(f.kitchen, allDay, allDay)
	wantReason(t, err, reasonNotAllowed)
	rep, err := f.inv.Wastage(f.manager, allDay, allDay)
	f.must(err)
	if rep.TotalPaisa != 9000 {
		t.Fatalf("total = %d", rep.TotalPaisa)
	}
	if len(rep.ByReason) != 2 || rep.ByReason[0].Reason != wasteStaffMeal || rep.ByReason[0].ValuePaisa != 6000 || rep.ByReason[1].Reason != wasteSpoiled || rep.ByReason[1].ValuePaisa != 3000 {
		t.Fatalf("by reason = %v", rep.ByReason)
	}
	if len(rep.ByPerson) != 1 || rep.ByPerson[0].Name != "Bikash Tamang" || rep.ByPerson[0].Count != 2 || rep.ByPerson[0].ValuePaisa != 9000 {
		t.Fatalf("by person = %v", rep.ByPerson)
	}
	if len(rep.ByItem) != 1 || rep.ByItem[0].QuantityMilli != 150*g || rep.ByItem[0].ValuePaisa != 9000 || rep.ByItem[0].Unit != unitG {
		t.Fatalf("by item = %v", rep.ByItem)
	}
	v := rep.VoidedAfterCooking
	if len(v) != 1 || v[0].Quantity != 1 || v[0].Reason != "Burnt" || v[0].ApproverName != "Sita Gurung" || v[0].ValuePaisa != 7200 {
		t.Fatalf("voided after cooking = %v", v)
	}
}

// --- stock counts ---

func (f *fixture) countLines(ctx context.Context, id string, lines map[string]int64) *restaurantv1.StockCount {
	f.t.Helper()
	var in []*restaurantv1.CountLineInput
	for item, qty := range lines {
		in = append(in, &restaurantv1.CountLineInput{InventoryItemId: item, CountedMilli: ptr(qty)})
	}
	c, err := f.inv.SaveCountLines(ctx, &restaurantv1.SaveCountLinesRequest{CountId: id, Lines: in})
	f.must(err)
	return c
}

func TestBlindCountReviewAndPost(t *testing.T) {
	f := newFixture(t)
	beer := f.item("Beer", "pcs", 410_000_000, 0, ptr(48*g)).Id // Rs 410 per bottle
	count, err := f.inv.StartCount(f.cashier, &restaurantv1.StartCountRequest{})
	f.must(err)
	if count.Number != 1 || count.Status != statusOpen || count.StartedByName != "Hari Thapa" || len(count.Lines) != 2 ||
		count.Lines[0].InventoryItemId != f.chicken || count.Lines[1].InventoryItemId != beer {
		t.Fatalf("count = %v", count)
	}
	c := f.countLines(f.cashier, count.Id, map[string]int64{f.chicken: 4500 * g, beer: 44 * g})
	if l := c.Lines[0]; l.GetCountedMilli() != 4500*g || l.CountedByUserId != f.cashierID.String() || l.ExpectedMilli != nil || l.CountedAtUnix == 0 {
		t.Fatalf("line = %v", l)
	}
	time.Sleep(2 * time.Millisecond)
	// a sale after the line was counted must not create variance
	ocid := f.newOrder(f.waiter)
	f.send(f.waiter, ocid, line(f.momo, 1, nil))
	_, err = f.inv.PostCount(f.cashier, count.Id)
	wantReason(t, err, reasonNotAllowed)
	c, err = f.inv.SubmitCount(f.cashier, count.Id)
	f.must(err)
	if c.Status != statusSubmitted {
		t.Fatalf("status = %v", c.Status)
	}
	_, err = f.inv.SaveCountLines(f.cashier, &restaurantv1.SaveCountLinesRequest{CountId: count.Id, Lines: []*restaurantv1.CountLineInput{{InventoryItemId: beer, CountedMilli: ptr(g)}}})
	wantReason(t, err, reasonInvalid)
	_, err = f.inv.Variance(f.cashier, count.Id)
	wantReason(t, err, reasonNotAllowed)
	rep, err := f.inv.Variance(f.manager, count.Id)
	f.must(err)
	if len(rep.Lines) != 2 {
		t.Fatalf("lines = %v", rep.Lines)
	}
	b, ch := rep.Lines[0], rep.Lines[1]
	if b.Name.En != "Beer" || b.ExpectedMilli != 48*g || b.CountedMilli != 44*g || b.VarianceMilli != -4*g || b.ValuePaisa != -164000 || !b.Highlight ||
		ch.Name.En != "Chicken" || ch.ExpectedMilli != 5000*g || ch.VarianceMilli != -500*g || ch.ValuePaisa != -30000 || !ch.Highlight {
		t.Fatalf("variance = %v", rep.Lines)
	}
	if rep.TotalPaisa != -194000 {
		t.Fatalf("total = %d", rep.TotalPaisa)
	}
	posted, err := f.inv.PostCount(f.manager, count.Id)
	f.must(err)
	if posted.Status != statusPosted || posted.PostedByUserId != f.managerID.String() || posted.GetTotalValuePaisa() != -194000 {
		t.Fatalf("posted = %v", posted)
	}
	if l := posted.Lines[0]; l.GetExpectedMilli() != 5000*g || l.GetVarianceMilli() != -500*g || l.GetValuePaisa() != -30000 {
		t.Fatalf("posted line = %v", l)
	}
	if got := f.onHand(f.chicken); got != 4380*g { // 4500 counted − 120 sold afterwards
		t.Fatalf("chicken on hand = %d", got)
	}
	if got := f.onHand(beer); got != 44*g {
		t.Fatalf("beer on hand = %d", got)
	}
	adj := f.movementsOfType(beer, typeCountAdjust)
	if len(adj) != 1 || adj[0].QuantityMilli != -4*g || adj[0].RefId != count.Id || adj[0].UnitCostE4 != 410_000_000 {
		t.Fatalf("adjust = %v", adj)
	}
	_, err = f.inv.PostCount(f.manager, count.Id)
	wantReason(t, err, reasonPosted)
	if a := f.lastAudit(); a.action != "STOCK_COUNT_POSTED" || !strings.Contains(a.reason, "-194000") {
		t.Fatalf("audit = %v", a)
	}
	blind, err := f.inv.GetCount(f.cashier, count.Id)
	f.must(err)
	if blind.Lines[0].ExpectedMilli != nil || blind.TotalValuePaisa != nil {
		t.Fatalf("staff sees expected: %v", blind)
	}
}

func TestSubmitRecountDiscard(t *testing.T) {
	f := newFixture(t)
	count, err := f.inv.StartCount(f.kitchen, &restaurantv1.StartCountRequest{})
	f.must(err)
	_, err = f.inv.SubmitCount(f.kitchen, count.Id)
	wantInvalid(t, err, "lines")
	f.countLines(f.kitchen, count.Id, map[string]int64{f.chicken: g})
	_, err = f.inv.SubmitCount(f.kitchen, count.Id)
	f.must(err)
	re, err := f.inv.RecountLines(f.owner, &restaurantv1.RecountLinesRequest{CountId: count.Id, InventoryItemIds: []string{f.chicken}})
	f.must(err)
	if re.Status != statusOpen || re.Lines[0].CountedMilli != nil || !re.Lines[0].Recount {
		t.Fatalf("recount = %v", re)
	}
	_, err = f.inv.DiscardCount(f.kitchen, count.Id, "wrong")
	wantReason(t, err, reasonNotAllowed)
	_, err = f.inv.DiscardCount(f.owner, count.Id, " ")
	wantInvalid(t, err, "reason")
	d, err := f.inv.DiscardCount(f.owner, count.Id, "Started by mistake")
	f.must(err)
	if d.Status != statusDiscarded || d.DiscardedReason != "Started by mistake" {
		t.Fatalf("discarded = %v", d)
	}
	if a := f.lastAudit(); a.action != "STOCK_COUNT_DISCARDED" {
		t.Fatalf("audit = %v", a)
	}
	list, err := f.inv.ListCounts(f.owner)
	f.must(err)
	if len(list) != 1 || list[0].Status != statusDiscarded {
		t.Fatalf("counts = %v", list)
	}
	_, err = f.inv.StartCount(f.kitchen, &restaurantv1.StartCountRequest{InventoryItemIds: []string{f.flour}})
	wantReason(t, err, reasonUntracked)
}

func TestSingleItemCountsListsAndVariancePeriod(t *testing.T) {
	f := newFixture(t)
	beer := f.item("Beer", "pcs", 1_000_000, 0, ptr(10*g)).Id
	list, err := f.inv.SaveCountList(f.owner, &restaurantv1.SaveCountListRequest{Name: "Bar daily", InventoryItemIds: []string{beer}})
	f.must(err)
	lists, err := f.inv.ListCountLists(f.cashier)
	f.must(err)
	if len(lists) != 1 || lists[0].Id != list.Id || lists[0].Name != "Bar daily" {
		t.Fatalf("lists = %v", lists)
	}
	_, err = f.inv.SaveCountList(f.cashier, &restaurantv1.SaveCountListRequest{Name: "x", InventoryItemIds: []string{beer}})
	wantReason(t, err, reasonNotAllowed)

	first, err := f.inv.StartCount(f.manager, &restaurantv1.StartCountRequest{InventoryItemIds: []string{f.chicken}})
	f.must(err)
	if len(first.Lines) != 1 {
		t.Fatalf("lines = %v", first.Lines)
	}
	f.countLines(f.manager, first.Id, map[string]int64{f.chicken: 4900 * g})
	_, err = f.inv.SubmitCount(f.manager, first.Id)
	f.must(err)
	_, err = f.inv.PostCount(f.manager, first.Id)
	f.must(err)
	time.Sleep(2 * time.Millisecond)
	// period after the first count: a sale and some waste
	ocid := f.newOrder(f.waiter)
	f.send(f.waiter, ocid, line(f.momo, 1, nil))
	_, err = f.inv.LogWaste(f.manager, &restaurantv1.LogWasteRequest{Target: invTarget(f.chicken), QuantityMilli: 30 * g, Reason: wasteSpoiled, RequestId: uuid.NewString()})
	f.must(err)
	second, err := f.inv.StartCount(f.manager, &restaurantv1.StartCountRequest{InventoryItemIds: []string{f.chicken}})
	f.must(err)
	f.countLines(f.manager, second.Id, map[string]int64{f.chicken: 4700 * g})
	_, err = f.inv.SubmitCount(f.manager, second.Id)
	f.must(err)
	rep, err := f.inv.Variance(f.manager, second.Id)
	f.must(err)
	if rep.SinceUnix == 0 {
		t.Fatalf("since not set")
	}
	l := rep.Lines[0]
	if l.OpeningMilli != 4900*g || l.PurchasesMilli != 0 || l.SalesMilli != 120*g || l.WasteMilli != 30*g || l.ExpectedMilli != 4750*g ||
		l.CountedMilli != 4700*g || l.VarianceMilli != -50*g || l.ValuePaisa != -3000 || l.Highlight {
		t.Fatalf("variance line = %v", l)
	}
	byList, err := f.inv.StartCount(f.manager, &restaurantv1.StartCountRequest{ListId: &list.Id})
	f.must(err)
	if byList.Label != "Bar daily" || len(byList.Lines) != 1 || byList.Lines[0].InventoryItemId != beer {
		t.Fatalf("by list = %v", byList)
	}
}

func TestVarianceNotesVoidedAfterCooking(t *testing.T) {
	f := newFixture(t)
	ocid := f.newOrder(f.waiter)
	o := f.send(f.waiter, ocid, line(f.momo, 1, nil))
	f.setLineStatus(o.Items[0].Id, "READY")
	_, err := f.ord.VoidItem(f.waiter, &restaurantv1.VoidItemRequest{
		CommandId: uuid.NewString(), OrderClientId: ocid, BaseVersion: f.version(f.waiter, ocid), ItemId: o.Items[0].Id, Reason: "Burnt",
		Approval: &restaurantv1.ManagerApproval{ManagerUserId: f.managerID.String(), Pin: "2222"},
	})
	f.must(err)
	count, err := f.inv.StartCount(f.manager, &restaurantv1.StartCountRequest{})
	f.must(err)
	f.countLines(f.manager, count.Id, map[string]int64{f.chicken: 4880 * g})
	_, err = f.inv.SubmitCount(f.manager, count.Id)
	f.must(err)
	rep, err := f.inv.Variance(f.manager, count.Id)
	f.must(err)
	if l := rep.Lines[0]; l.SalesMilli != 120*g || l.VarianceMilli != 0 {
		t.Fatalf("line = %v", l)
	}
	if v := rep.VoidedAfterCooking; v.Count != 1 || v.ValuePaisa != 7200 {
		t.Fatalf("voided after cooking = %v", v)
	}
}
