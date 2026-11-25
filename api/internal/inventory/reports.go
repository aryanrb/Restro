package inventory

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/google/uuid"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
)

// point is a moment in the ledger: a time plus the last movement seq that existed
// then (orders movements stamped at the same instant).
type point struct {
	at  time.Time
	seq int64
}

func (p point) includes(m sqlc.StockMovement) bool {
	return m.At.Before(p.at) || (m.At.Equal(p.at) && m.Seq <= p.seq)
}

func (p point) before(o point) bool {
	return p.at.Before(o.at) || (p.at.Equal(o.at) && p.seq < o.seq)
}

func pointOf(l sqlc.StockCountLine) point { return point{l.CountedAt.Time, l.CountedSeq.Int64} }

// Variance (owner/manager) compares counted with expected for each counted line,
// over the period since the previous posted count of that item: opening (the
// previous count, plus opening stock), purchases, sales and waste. Biggest loss first.
func (s *Service) Variance(ctx context.Context, id string) (*restaurantv1.VarianceReport, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	cid, err := parseID("count_id", id)
	if err != nil {
		return nil, err
	}
	c, err := s.q.GetStockCount(ctx, cid)
	if err != nil {
		return nil, notFound(err, "count")
	}
	lines, err := s.q.ListCountLines(ctx, cid)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	var since, until time.Time
	out := &restaurantv1.VarianceReport{CountId: c.ID.String(), Number: c.Number, Status: countStatusEnum(c.Status), VoidedAfterCooking: &restaurantv1.VoidedAfterCooking{}}
	for _, l := range lines {
		if !l.CountedMilli.Valid || !l.CountedAt.Valid {
			continue
		}
		it, e := s.q.GetInventoryItem(ctx, l.InventoryItemID)
		if e != nil {
			return nil, connecterr.Internal(e)
		}
		cur := pointOf(l)
		if cur.at.After(until) {
			until = cur.at
		}
		// the latest earlier posted count of this item
		posted, e := s.q.ListPostedCountLinesForItem(ctx, it.ID)
		if e != nil {
			return nil, connecterr.Internal(e)
		}
		var prev *sqlc.StockCountLine
		for i := range posted {
			p := posted[i]
			if p.CountID == c.ID || !pointOf(p).before(cur) {
				continue
			}
			if prev == nil || pointOf(*prev).before(pointOf(p)) {
				prev = &posted[i]
			}
		}
		var opening, purchases, sales, waste int64
		if prev != nil {
			if since.IsZero() || prev.CountedAt.Time.Before(since) {
				since = prev.CountedAt.Time
			}
			opening = prev.CountedMilli.Int64
		}
		moves, e := s.q.ListItemMovementsUntil(ctx, sqlc.ListItemMovementsUntilParams{InventoryItemID: it.ID, At: cur.at, Seq: cur.seq})
		if e != nil {
			return nil, connecterr.Internal(e)
		}
		var onHand int64
		for _, m := range moves {
			onHand += m.QuantityMilli
			if prev != nil && (pointOf(*prev).includes(m) || (m.Type == TypeCountAdjust && m.RefID == prev.CountID)) {
				continue
			}
			switch m.Type {
			case TypeOpening:
				opening += m.QuantityMilli
			case TypePurchase, TypePurchaseReturn:
				purchases += m.QuantityMilli
			case TypeSale, TypeSaleReversal:
				sales -= m.QuantityMilli
			case TypeWaste:
				waste -= m.QuantityMilli
			}
		}
		expected := onHand
		variance := l.CountedMilli.Int64 - expected
		value := valuePaisa(term{variance, it.UnitCostE4})
		if c.Status == "POSTED" && l.ExpectedMilli.Valid {
			expected, variance, value = l.ExpectedMilli.Int64, l.VarianceMilli.Int64, l.ValuePaisa.Int64
		}
		out.Lines = append(out.Lines, &restaurantv1.VarianceLine{
			InventoryItemId: it.ID.String(), Name: local(it.NameEn, it.NameNe), Unit: unitEnum(it.Unit),
			OpeningMilli: opening, PurchasesMilli: purchases, SalesMilli: sales, WasteMilli: waste,
			ExpectedMilli: expected, CountedMilli: l.CountedMilli.Int64, VarianceMilli: variance, ValuePaisa: value,
			Highlight: highlighted(value, variance, expected), CountedAtUnix: cur.at.Unix(), Recount: l.Recount,
		})
		out.TotalPaisa += value
	}
	slices.SortStableFunc(out.Lines, func(a, b *restaurantv1.VarianceLine) int {
		return cmp.Or(cmp.Compare(a.ValuePaisa, b.ValuePaisa), cmp.Compare(a.Name.En, b.Name.En))
	})
	if !since.IsZero() {
		out.SinceUnix = since.Unix()
	}
	if !until.IsZero() {
		sinceTs := ts(since)
		if since.IsZero() {
			sinceTs.Valid = false
		}
		sales, e := s.q.ListVoidedAfterCookingSales(ctx, sqlc.ListVoidedAfterCookingSalesParams{Since: sinceTs, Until: until})
		if e != nil {
			return nil, connecterr.Internal(e)
		}
		byRef := map[uuid.UUID][]term{}
		var refs []uuid.UUID
		for _, m := range sales {
			if _, ok := byRef[m.RefID]; !ok {
				refs = append(refs, m.RefID)
			}
			byRef[m.RefID] = append(byRef[m.RefID], term{-m.QuantityMilli, m.UnitCostE4})
		}
		out.VoidedAfterCooking.Count = int32(len(refs))
		for _, r := range refs {
			out.VoidedAfterCooking.ValuePaisa += valuePaisa(byRef[r]...)
		}
	}
	return out, nil
}

// Wastage (owner/manager): waste in a range by reason, person and stock item, plus
// the lines voided after cooking (sent in the range).
func (s *Service) Wastage(ctx context.Context, fromUnix, toUnix int64) (*restaurantv1.WastageReport, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	rows, err := s.wasteInRange(ctx, fromUnix, toUnix)
	if err != nil {
		return nil, err
	}
	out := &restaurantv1.WastageReport{}
	type group struct {
		count int32
		value int64
	}
	reasons, people := map[string]*group{}, map[string]*group{}
	var reasonOrder, personOrder []string
	add := func(m map[string]*group, order *[]string, key string, v int64) {
		g, ok := m[key]
		if !ok {
			g = &group{}
			m[key] = g
			*order = append(*order, key)
		}
		g.count++
		g.value += v
	}
	ids := make([]uuid.UUID, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- { // oldest first, like the fake
		w := rows[i]
		add(reasons, &reasonOrder, w.Reason, w.ValuePaisa)
		add(people, &personOrder, w.byName, w.ValuePaisa)
		ids = append(ids, w.ID)
		out.TotalPaisa += w.ValuePaisa
	}
	for _, r := range rows {
		out.Entries = append(out.Entries, wasteView(r, true))
	}
	byValue := func(m map[string]*group) func(a, b string) int {
		return func(a, b string) int { return cmp.Compare(m[b].value, m[a].value) }
	}
	slices.SortStableFunc(reasonOrder, byValue(reasons))
	slices.SortStableFunc(personOrder, byValue(people))
	for _, k := range reasonOrder {
		out.ByReason = append(out.ByReason, &restaurantv1.WasteByReason{Reason: wasteReasonEnum(k), Count: reasons[k].count, ValuePaisa: reasons[k].value})
	}
	for _, k := range personOrder {
		out.ByPerson = append(out.ByPerson, &restaurantv1.WasteByPerson{Name: k, Count: people[k].count, ValuePaisa: people[k].value})
	}

	if len(ids) > 0 {
		moves, e := s.q.ListMovementsByRefs(ctx, sqlc.ListMovementsByRefsParams{Refs: ids, Type: TypeWaste})
		if e != nil {
			return nil, connecterr.Internal(e)
		}
		type itemSum struct {
			qty   int64
			terms []term
		}
		sums := map[uuid.UUID]*itemSum{}
		var order []uuid.UUID
		for _, m := range moves {
			g, ok := sums[m.InventoryItemID]
			if !ok {
				g = &itemSum{}
				sums[m.InventoryItemID] = g
				order = append(order, m.InventoryItemID)
			}
			g.qty -= m.QuantityMilli
			g.terms = append(g.terms, term{-m.QuantityMilli, m.UnitCostE4})
		}
		items, e := s.q.ListInventoryItemsByIDs(ctx, order)
		if e != nil {
			return nil, connecterr.Internal(e)
		}
		byID := map[uuid.UUID]sqlc.InventoryItem{}
		for _, it := range items {
			byID[it.ID] = it
		}
		for _, id := range order {
			it := byID[id]
			out.ByItem = append(out.ByItem, &restaurantv1.WasteByItem{
				Name: local(it.NameEn, it.NameNe), Unit: unitEnum(it.Unit), QuantityMilli: sums[id].qty, ValuePaisa: valuePaisa(sums[id].terms...),
			})
		}
		slices.SortStableFunc(out.ByItem, func(a, b *restaurantv1.WasteByItem) int { return cmp.Compare(b.ValuePaisa, a.ValuePaisa) })
	}

	from, to := closedRange(fromUnix, toUnix)
	voided, err := s.q.ListVoidedAfterCooking(ctx, sqlc.ListVoidedAfterCookingParams{FromTs: ts(from), ToTs: ts(to)})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	if len(voided) > 0 {
		refs := make([]uuid.UUID, 0, len(voided))
		for _, v := range voided {
			refs = append(refs, v.ID)
		}
		sales, e := s.q.ListMovementsByRefs(ctx, sqlc.ListMovementsByRefsParams{Refs: refs, Type: TypeSale})
		if e != nil {
			return nil, connecterr.Internal(e)
		}
		cost := map[uuid.UUID][]term{}
		for _, m := range sales {
			cost[m.RefID] = append(cost[m.RefID], term{-m.QuantityMilli, m.UnitCostE4})
		}
		for _, v := range voided {
			name := local(v.NameEn, v.NameNe)
			if v.HasVariant {
				name = local(v.NameEn+" · "+v.VariantNameEn, orEn(v.NameNe, v.NameEn)+" · "+orEn(v.VariantNameNe, v.VariantNameEn))
			}
			out.VoidedAfterCooking = append(out.VoidedAfterCooking, &restaurantv1.VoidedAfterCookingRow{
				Name: name, Quantity: v.Quantity, Reason: v.VoidReason, ApproverName: v.ApproverName, AtUnix: unix(v.SentAt), ValuePaisa: valuePaisa(cost[v.ID]...),
			})
		}
	}
	return out, nil
}

// LowStock (owner/manager): tracked, active items at or below their low level.
func (s *Service) LowStock(ctx context.Context) ([]*restaurantv1.LowStockRow, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	items, err := s.q.ListInventoryItems(ctx, false)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := []*restaurantv1.LowStockRow{}
	for _, it := range items {
		if live(it) && it.OnHandMilli <= it.LowStockMilli {
			out = append(out, &restaurantv1.LowStockRow{
				InventoryItemId: it.ID.String(), Name: local(it.NameEn, it.NameNe), Unit: unitEnum(it.Unit), OnHandMilli: it.OnHandMilli, LowStockMilli: it.LowStockMilli,
			})
		}
	}
	return out, nil
}

// ItemsWithoutRecipe (owner/manager): active dishes (or variants) that deduct nothing.
func (s *Service) ItemsWithoutRecipe(ctx context.Context) ([]*restaurantv1.WithoutRecipeRow, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	items, err := s.q.ListItems(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	variants, err := s.q.ListVariants(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	rc, err := loadRecipes(ctx, s.q)
	if err != nil {
		return nil, err
	}
	byItem := map[uuid.UUID][]sqlc.ItemVariant{}
	for _, v := range variants {
		byItem[v.ItemID] = append(byItem[v.ItemID], v)
	}
	out := []*restaurantv1.WithoutRecipeRow{}
	for _, it := range items {
		if !it.Active {
			continue
		}
		vs := byItem[it.ID]
		if len(vs) == 0 {
			if len(rc.item[it.ID]) == 0 {
				out = append(out, &restaurantv1.WithoutRecipeRow{MenuItemId: it.ID.String(), Name: local(it.NameEn, it.NameNe)})
			}
			continue
		}
		for _, v := range vs {
			if len(rc.forVariant(it.ID, v.ID)) == 0 {
				out = append(out, &restaurantv1.WithoutRecipeRow{MenuItemId: it.ID.String(), Name: local(it.NameEn, it.NameNe), VariantName: local(v.NameEn, v.NameNe)})
			}
		}
	}
	return out, nil
}

// Suggestions86 lists dishes that can't be made from what is on hand (owner,
// manager, cashier, kitchen). The system never 86es on its own.
func (s *Service) Suggestions86(ctx context.Context) ([]*restaurantv1.Suggestion86, error) {
	if _, err := requireStock(ctx); err != nil {
		return nil, err
	}
	onHand, err := liveOnHand(ctx, s.q)
	if err != nil {
		return nil, err
	}
	dishes, err := loadDishes(ctx, s.q)
	if err != nil {
		return nil, err
	}
	short := suggest86(dishes, onHand)
	out := []*restaurantv1.Suggestion86{}
	if len(short) == 0 {
		return out, nil
	}
	menu, err := s.q.ListItems(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	stock, err := s.q.ListInventoryItems(ctx, false)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	menuByID := map[uuid.UUID]sqlc.MenuItem{}
	for _, m := range menu {
		menuByID[m.ID] = m
	}
	stockByID := map[uuid.UUID]sqlc.InventoryItem{}
	for _, it := range stock {
		stockByID[it.ID] = it
	}
	for _, sh := range short {
		m, it := menuByID[sh.MenuItemID], stockByID[sh.InventoryItemID]
		out = append(out, &restaurantv1.Suggestion86{
			MenuItemId: m.ID.String(), MenuItemName: local(m.NameEn, m.NameNe), InventoryItemId: it.ID.String(),
			IngredientName: local(it.NameEn, it.NameNe), LeftMilli: sh.LeftMilli, Unit: unitEnum(it.Unit),
		})
	}
	return out, nil
}
