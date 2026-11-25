package inventory

import (
	"context"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/realtime"
)

// Movement types (stock_movements.type).
const (
	TypeSale           = "SALE"
	TypeSaleReversal   = "SALE_REVERSAL"
	TypeWaste          = "WASTE"
	TypeCountAdjust    = "COUNT_ADJUST"
	TypePurchase       = "PURCHASE"
	TypePurchaseReturn = "PURCHASE_RETURN"
	TypeOpening        = "OPENING"
)

// --- realtime events found while writing, sent once the request succeeded ---

type eventsKey struct{}

type collector struct {
	mu  sync.Mutex
	evs []realtime.Event
}

// WithEvents returns a context that collects the stock events (stock.updated,
// stock.low, stock.86suggest) written under it, and a func returning them. The
// server's EventsInterceptor does this for every request and broadcasts them only
// when the handler succeeded, i.e. after its transaction committed.
func WithEvents(ctx context.Context) (context.Context, func() []realtime.Event) {
	c := &collector{}
	return context.WithValue(ctx, eventsKey{}, c), func() []realtime.Event {
		c.mu.Lock()
		defer c.mu.Unlock()
		out := c.evs
		c.evs = nil
		return out
	}
}

// queue adds an event once per type+id. Without a collector it is dropped.
func queue(ctx context.Context, ev realtime.Event) {
	c, _ := ctx.Value(eventsKey{}).(*collector)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.evs {
		if e.Type == ev.Type && e.ID == ev.ID {
			return
		}
	}
	c.evs = append(c.evs, ev)
}

// EventsInterceptor gives every request an event collector and broadcasts the
// collected stock events after a successful response (the write has committed).
// Order commands and billing's CounterSale deduct stock without knowing the hub.
func EventsInterceptor(hub *realtime.Hub) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			ctx, take := WithEvents(ctx)
			resp, err := next(ctx, req)
			if err == nil && hub != nil {
				for _, ev := range take() {
					hub.Broadcast(ev)
				}
			}
			return resp, err
		}
	}
}

// --- the ledger ---

// move is one stock movement to write. costE4 < 0 means "the item's unit cost now".
type move struct {
	itemID   uuid.UUID
	qtyMilli int64
	typ      string
	ref      uuid.UUID
	reason   string
	by       uuid.UUID
	at       time.Time
	costE4   int64
}

func live(it sqlc.InventoryItem) bool { return it.Tracked && it.Active }

// record is the one way stock changes: it writes the movements and the on-hand
// cache inside the caller's transaction (items locked in id order, so concurrent
// sends can't deadlock) and queues stock.low for a tracked item crossing its
// low-stock level, stock.86suggest for a dish that can no longer be made (once per
// crossing), and stock.updated.
func record(ctx context.Context, q *sqlc.Queries, moves []move) error {
	if len(moves) == 0 {
		return nil
	}
	ordered := slices.Clone(moves)
	slices.SortStableFunc(ordered, func(a, b move) int { return slices.Compare(a.itemID[:], b.itemID[:]) })
	delta := map[uuid.UUID]int64{}
	for _, m := range ordered {
		after, err := q.AddOnHand(ctx, sqlc.AddOnHandParams{ID: m.itemID, Delta: m.qtyMilli})
		if err != nil {
			return connecterr.Internal(err)
		}
		cost := m.costE4
		if cost < 0 {
			cost = after.UnitCostE4
		}
		if _, err := q.InsertStockMovement(ctx, sqlc.InsertStockMovementParams{
			InventoryItemID: m.itemID, QuantityMilli: m.qtyMilli, Type: m.typ, RefID: m.ref, Reason: m.reason,
			ByUserID: dbx.UUIDOpt(m.by), At: m.at, UnitCostE4: cost,
		}); err != nil {
			return connecterr.Internal(err)
		}
		before := after.OnHandMilli - m.qtyMilli
		if live(after) && before > after.LowStockMilli && after.OnHandMilli <= after.LowStockMilli {
			queue(ctx, realtime.Event{Type: "stock.low", ID: after.ID.String()})
		}
		delta[m.itemID] += m.qtyMilli
	}
	if err := queueNew86(ctx, q, delta); err != nil {
		return err
	}
	queue(ctx, realtime.Event{Type: "stock.updated", ID: "stock"})
	return nil
}

// queueNew86 compares the 86 suggestions before and after the deltas and queues
// stock.86suggest for each new one. Only dishes using a changed item can change.
func queueNew86(ctx context.Context, q *sqlc.Queries, delta map[uuid.UUID]int64) error {
	after, err := liveOnHand(ctx, q)
	if err != nil {
		return err
	}
	touched := false
	for id := range delta {
		if _, ok := after[id]; ok {
			touched = true
		}
	}
	if !touched {
		return nil
	}
	dishes, err := loadDishes(ctx, q)
	if err != nil {
		return err
	}
	var relevant []dish
	for _, d := range dishes {
		if usesAny(d, delta) {
			relevant = append(relevant, d)
		}
	}
	before := make(map[uuid.UUID]int64, len(after))
	for id, v := range after {
		before[id] = v - delta[id]
	}
	was := map[uuid.UUID]bool{}
	for _, s := range suggest86(relevant, before) {
		was[s.MenuItemID] = true
	}
	for _, s := range suggest86(relevant, after) {
		if !was[s.MenuItemID] {
			queue(ctx, realtime.Event{Type: "stock.86suggest", ID: s.MenuItemID.String()})
		}
	}
	return nil
}

func usesAny(d dish, ids map[uuid.UUID]int64) bool {
	for _, r := range append(slices.Clone(d.Variants), d.Recipe) {
		for _, l := range r {
			if _, ok := ids[l.ItemID]; ok {
				return true
			}
		}
	}
	return false
}

// liveOnHand maps every tracked, active item to its on hand.
func liveOnHand(ctx context.Context, q *sqlc.Queries) (map[uuid.UUID]int64, error) {
	items, err := q.ListInventoryItems(ctx, false)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := map[uuid.UUID]int64{}
	for _, it := range items {
		if live(it) {
			out[it.ID] = it.OnHandMilli
		}
	}
	return out, nil
}

// loadDishes reads every menu item with its recipes for the 86 check.
func loadDishes(ctx context.Context, q *sqlc.Queries) ([]dish, error) {
	items, err := q.ListItems(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	variants, err := q.ListVariants(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	rc, err := loadRecipes(ctx, q)
	if err != nil {
		return nil, err
	}
	byItem := map[uuid.UUID][]sqlc.ItemVariant{}
	for _, v := range variants {
		byItem[v.ItemID] = append(byItem[v.ItemID], v)
	}
	out := make([]dish, 0, len(items))
	for _, it := range items {
		d := dish{ID: it.ID, Active: it.Active, Is86d: it.Is86d, Recipe: rc.item[it.ID]}
		for _, v := range byItem[it.ID] {
			d.Variants = append(d.Variants, rc.forVariant(it.ID, v.ID))
		}
		out = append(out, d)
	}
	return out, nil
}

// recipes is every recipe in the menu, keyed by owner.
type recipes struct {
	item     map[uuid.UUID][]Line
	variant  map[uuid.UUID][]Line
	own      map[uuid.UUID]bool // variant has its own recipe
	modifier map[uuid.UUID][]Line
}

func (r recipes) forVariant(itemID, variantID uuid.UUID) []Line {
	if r.own[variantID] {
		return r.variant[variantID]
	}
	return r.item[itemID]
}

func loadRecipes(ctx context.Context, q *sqlc.Queries) (recipes, error) {
	r := recipes{item: map[uuid.UUID][]Line{}, variant: map[uuid.UUID][]Line{}, own: map[uuid.UUID]bool{}, modifier: map[uuid.UUID][]Line{}}
	il, err := q.ListAllMenuItemRecipeLines(ctx)
	if err != nil {
		return r, connecterr.Internal(err)
	}
	for _, l := range il {
		r.item[l.MenuItemID] = append(r.item[l.MenuItemID], Line{l.InventoryItemID, l.QuantityMilli})
	}
	own, err := q.ListVariantRecipeIDs(ctx)
	if err != nil {
		return r, connecterr.Internal(err)
	}
	for _, id := range own {
		r.own[id] = true
	}
	vl, err := q.ListAllVariantRecipeLines(ctx)
	if err != nil {
		return r, connecterr.Internal(err)
	}
	for _, l := range vl {
		r.variant[l.VariantID] = append(r.variant[l.VariantID], Line{l.InventoryItemID, l.QuantityMilli})
	}
	ml, err := q.ListAllModifierRecipeLines(ctx)
	if err != nil {
		return r, connecterr.Internal(err)
	}
	for _, l := range ml {
		r.modifier[l.ModifierID] = append(r.modifier[l.ModifierID], Line{l.InventoryItemID, l.QuantityMilli})
	}
	return r, nil
}

// RecipeFor is what one unit of a dish uses: the item recipe, or the variant's own
// recipe instead; modifier lines are added. Summed per ingredient.
func RecipeFor(ctx context.Context, q *sqlc.Queries, menuItemID uuid.UUID, variantID uuid.UUID, modifierIDs []uuid.UUID) ([]Line, error) {
	var base []Line
	own := false
	if variantID != uuid.Nil {
		var err error
		if own, err = q.VariantHasOwnRecipe(ctx, variantID); err != nil {
			return nil, connecterr.Internal(err)
		}
	}
	if own {
		vl, err := q.ListVariantRecipeLines(ctx, variantID)
		if err != nil {
			return nil, connecterr.Internal(err)
		}
		for _, l := range vl {
			base = append(base, Line{l.InventoryItemID, l.QuantityMilli})
		}
	} else {
		il, err := q.ListMenuItemRecipeLines(ctx, menuItemID)
		if err != nil {
			return nil, connecterr.Internal(err)
		}
		for _, l := range il {
			base = append(base, Line{l.InventoryItemID, l.QuantityMilli})
		}
	}
	var mods []Line
	if len(modifierIDs) > 0 {
		ml, err := q.ListModifierRecipeLinesFor(ctx, modifierIDs)
		if err != nil {
			return nil, connecterr.Internal(err)
		}
		for _, l := range ml {
			mods = append(mods, Line{l.InventoryItemID, l.QuantityMilli})
		}
	}
	return sumLines(base, mods), nil
}

// liveItems loads the tracked, active items among ids.
func liveItems(ctx context.Context, q *sqlc.Queries, lines []Line) (map[uuid.UUID]sqlc.InventoryItem, error) {
	if len(lines) == 0 {
		return nil, nil
	}
	ids := make([]uuid.UUID, 0, len(lines))
	for _, l := range lines {
		ids = append(ids, l.ItemID)
	}
	items, err := q.ListInventoryItemsByIDs(ctx, ids)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := map[uuid.UUID]sqlc.InventoryItem{}
	for _, it := range items {
		if live(it) {
			out[it.ID] = it
		}
	}
	return out, nil
}

// SaleLine is an order line that has just become SENT.
type SaleLine struct {
	OrderItemID uuid.UUID
	MenuItemID  uuid.UUID
	VariantID   uuid.UUID // uuid.Nil = none
	ModifierIDs []uuid.UUID
	Quantity    int32
	SentAt      time.Time
	By          uuid.UUID // uuid.Nil = unknown
}

// SaleLineOf builds a SaleLine from a stored order line (sent_at required).
func SaleLineOf(it sqlc.OrderItem, by uuid.UUID) SaleLine {
	sl := SaleLine{OrderItemID: it.ID, MenuItemID: it.MenuItemID, ModifierIDs: it.ModifierIds, Quantity: it.Quantity, By: by}
	if it.VariantID.Valid {
		sl.VariantID = uuid.UUID(it.VariantID.Bytes)
	}
	if it.SentAt.Valid {
		sl.SentAt = it.SentAt.Time
	} else {
		sl.SentAt = now()
	}
	return sl
}

// DeductSale writes SALE movements for lines sent to the kitchen: −(recipe qty ×
// quantity) per tracked, active ingredient, at each line's sent_at, ref = the order
// line. It never refuses (stock may go negative). Call inside the send
// transaction, once per send, so the stock rows are locked in one sorted pass.
func DeductSale(ctx context.Context, q *sqlc.Queries, sent ...SaleLine) error {
	var moves []move
	for _, l := range sent {
		lines, err := RecipeFor(ctx, q, l.MenuItemID, l.VariantID, l.ModifierIDs)
		if err != nil {
			return err
		}
		items, err := liveItems(ctx, q, lines)
		if err != nil {
			return err
		}
		for _, r := range lines {
			if _, ok := items[r.ItemID]; !ok {
				continue
			}
			moves = append(moves, move{
				itemID: r.ItemID, qtyMilli: -r.QtyMilli * int64(l.Quantity), typ: TypeSale, ref: l.OrderItemID,
				by: l.By, at: l.SentAt, costE4: -1,
			})
		}
	}
	return record(ctx, q, moves)
}

// ReverseSale puts back what a line's SALE took (same quantities and costs), for a
// void before the kitchen started. Idempotent. Call inside the void transaction.
func ReverseSale(ctx context.Context, q *sqlc.Queries, orderItemID, by uuid.UUID) error {
	done, err := q.ListMovementsByRef(ctx, sqlc.ListMovementsByRefParams{RefID: orderItemID, Type: TypeSaleReversal})
	if err != nil {
		return connecterr.Internal(err)
	}
	if len(done) > 0 {
		return nil
	}
	sales, err := q.ListMovementsByRef(ctx, sqlc.ListMovementsByRefParams{RefID: orderItemID, Type: TypeSale})
	if err != nil {
		return connecterr.Internal(err)
	}
	at := now()
	moves := make([]move, 0, len(sales))
	for _, m := range sales {
		moves = append(moves, move{itemID: m.InventoryItemID, qtyMilli: -m.QuantityMilli, typ: TypeSaleReversal, ref: orderItemID, by: by, at: at, costE4: m.UnitCostE4})
	}
	return record(ctx, q, moves)
}

// StockKeys stores what a line was ordered as (variant and modifiers), so a later
// send can expand its recipe.
func StockKeys(ctx context.Context, q *sqlc.Queries, orderItemID uuid.UUID, variantID uuid.UUID, modifierIDs []uuid.UUID) error {
	var v pgtype.UUID
	if variantID != uuid.Nil {
		v = dbx.UUID(variantID)
	}
	if modifierIDs == nil {
		modifierIDs = []uuid.UUID{}
	}
	if err := q.SetOrderItemStockKeys(ctx, sqlc.SetOrderItemStockKeysParams{ID: orderItemID, VariantID: v, ModifierIds: modifierIDs}); err != nil {
		return connecterr.Internal(err)
	}
	return nil
}
