package purchases

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/audit"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/bsdate"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/realtime"
)

func bsOf(t time.Time) string {
	d, err := bsdate.FromAD(t)
	if err != nil {
		return ""
	}
	return d.String()
}

// GetSupplierStatement: owner/manager. Opening balance, bills, returns, refunds and
// payments in an AD range (Kathmandu days, inclusive) with the running balance.
func (s *Service) GetSupplierStatement(ctx context.Context, req *restaurantv1.SupplierStatementRequest) (*restaurantv1.SupplierStatement, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	id, err := parseID("supplier_id", req.GetSupplierId())
	if err != nil {
		return nil, err
	}
	if _, err := parseDate("from", req.GetFrom()); err != nil {
		return nil, err
	}
	if _, err := parseDate("to", req.GetTo()); err != nil {
		return nil, err
	}
	from, to := req.GetFrom(), req.GetTo()
	supplier, err := supplierWithBalance(ctx, s.q, id)
	if err != nil {
		return nil, err
	}
	sup, err := s.q.GetSupplier(ctx, id)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	type event struct {
		date  string
		at    time.Time
		entry *restaurantv1.SupplierStatementEntry
	}
	var events []event
	add := func(date string, at time.Time, kind restaurantv1.SupplierStatementKind, ref string, amount int64, detail string) {
		bs := bsOf(at)
		if d, err := time.ParseInLocation(time.DateOnly, date, nepal); err == nil {
			bs = bsOf(d.Add(12 * time.Hour))
		}
		events = append(events, event{date, at, &restaurantv1.SupplierStatementEntry{
			Kind: kind, Ref: ref, AtUnix: at.Unix(), BsDate: bs, AmountPaisa: amount, Detail: detail,
		}})
	}
	if sup.OpeningBalancePaisa > 0 {
		add(ktmDate(sup.CreatedAt), sup.CreatedAt, restaurantv1.SupplierStatementKind_SUPPLIER_STATEMENT_KIND_OPENING, "", sup.OpeningBalancePaisa, "")
	}
	bills, err := s.q.ListSupplierStatementBills(ctx, id)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	for _, b := range bills {
		add(dateStr(b.BillDate), b.At, restaurantv1.SupplierStatementKind_SUPPLIER_STATEMENT_KIND_BILL, displayNumber(b.Number), b.TotalPaisa, b.SupplierBillNo)
	}
	returns, err := s.q.ListSupplierStatementReturns(ctx, id)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	for _, r := range returns {
		detail := strings.Join(slices.DeleteFunc([]string{r.NoteNo, r.Reason}, func(v string) bool { return v == "" }), " · ")
		add(ktmDate(r.At), r.At, restaurantv1.SupplierStatementKind_SUPPLIER_STATEMENT_KIND_RETURN, displayNumber(r.Number), -r.TotalPaisa, detail)
		if r.RefundPaisa > 0 {
			add(ktmDate(r.At), r.At, restaurantv1.SupplierStatementKind_SUPPLIER_STATEMENT_KIND_REFUND, displayNumber(r.Number), r.RefundPaisa, "")
		}
	}
	payments, err := s.q.ListSupplierPaymentRows(ctx, id)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	for _, p := range payments {
		if p.VoidedAt.Valid {
			continue
		}
		detail := strings.Join(slices.DeleteFunc([]string{p.Method, p.Reference}, func(v string) bool { return v == "" }), " · ")
		add(ktmDate(p.At), p.At, restaurantv1.SupplierStatementKind_SUPPLIER_STATEMENT_KIND_PAYMENT, paymentNumber(p.Number), -p.AmountPaisa, detail)
	}
	slices.SortStableFunc(events, func(a, b event) int {
		if c := strings.Compare(a.date, b.date); c != 0 {
			return c
		}
		return a.at.Compare(b.at)
	})
	out := &restaurantv1.SupplierStatement{Supplier: supplier, From: from, To: to, Entries: []*restaurantv1.SupplierStatementEntry{}}
	var balance int64
	for _, e := range events {
		if e.date > to {
			break
		}
		balance += e.entry.AmountPaisa
		if e.date < from {
			out.OpeningPaisa = balance
			continue
		}
		e.entry.BalancePaisa = balance
		out.Entries = append(out.Entries, e.entry)
	}
	out.ClosingPaisa = balance
	return out, nil
}

// --- purchase settings ---

func toSettings(x sqlc.ItemPurchaseSetting) *restaurantv1.ItemPurchaseSettings {
	out := &restaurantv1.ItemPurchaseSettings{InventoryItemId: x.InventoryItemID.String(), PreferredSupplierId: idStr(x.PreferredSupplierID)}
	if x.DefaultUnitKey.Valid {
		out.DefaultUnitKey = x.DefaultUnitKey.String
	}
	if x.BuyUpToMilli.Valid {
		v := x.BuyUpToMilli.Int64
		out.BuyUpToMilli = &v
	}
	return out
}

func (s *Service) ListPurchaseSettings(ctx context.Context) ([]*restaurantv1.ItemPurchaseSettings, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	rows, err := s.q.ListItemPurchaseSettings(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := make([]*restaurantv1.ItemPurchaseSettings, 0, len(rows))
	for _, r := range rows {
		out = append(out, toSettings(r))
	}
	return out, nil
}

// SavePurchaseSettings: owner/manager. Preferred supplier, default purchase unit, buy-up-to level.
func (s *Service) SavePurchaseSettings(ctx context.Context, in *restaurantv1.ItemPurchaseSettings) (*restaurantv1.ItemPurchaseSettings, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	itemID, err := parseID("inventory_item_id", in.GetInventoryItemId())
	if err != nil {
		return nil, err
	}
	var out *restaurantv1.ItemPurchaseSettings
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		it, e := q.GetInventoryItem(ctx, itemID)
		if errors.Is(e, pgx.ErrNoRows) {
			return connecterr.Invalid("inventory_item_id", "stock item not found")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		p := sqlc.UpsertItemPurchaseSettingsParams{InventoryItemID: itemID}
		if in.GetPreferredSupplierId() != "" {
			sid, e := parseID("preferred_supplier_id", in.GetPreferredSupplierId())
			if e != nil {
				return e
			}
			if _, e := q.GetSupplier(ctx, sid); e != nil {
				return connecterr.Invalid("preferred_supplier_id", "supplier not found")
			}
			p.PreferredSupplierID = dbx.UUID(sid)
		}
		if k := in.GetDefaultUnitKey(); k != "" {
			if _, ok := findUnit(it, k); !ok {
				return connecterr.Invalid("default_unit_key", "unknown purchase unit")
			}
			p.DefaultUnitKey = pgtype.Text{String: k, Valid: true}
		}
		if in.BuyUpToMilli != nil {
			if in.GetBuyUpToMilli() < 0 {
				return connecterr.Invalid("buy_up_to", "can't be negative")
			}
			p.BuyUpToMilli = pgtype.Int8{Int64: in.GetBuyUpToMilli(), Valid: true}
		}
		saved, e := q.UpsertItemPurchaseSettings(ctx, p)
		if e != nil {
			return connecterr.Internal(e)
		}
		if e := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: ActionItemChanged, Entity: "inventory_item", EntityID: itemID.String(),
			Reason: it.NameEn + " · purchase settings", After: audit.JSON(toSettings(saved))}); e != nil {
			return e
		}
		out = toSettings(saved)
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "inventory.updated", ID: "inventory"})
	return out, nil
}

// SuggestedPurchases: tracked, active items at or below their low-stock level,
// grouped by preferred supplier (no supplier last). Buy = buy-up-to − on hand,
// rounded up to whole default purchase units.
func (s *Service) SuggestedPurchases(ctx context.Context) ([]*restaurantv1.BuyListGroup, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	items, err := s.q.ListInventoryItems(ctx, false)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	settings, err := s.q.ListItemPurchaseSettings(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	byItem := map[uuid.UUID]sqlc.ItemPurchaseSetting{}
	for _, x := range settings {
		byItem[x.InventoryItemID] = x
	}
	prices, err := s.q.ListLastPurchasePrices(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	lastPrice := map[uuid.UUID]sqlc.ListLastPurchasePricesRow{}
	for _, p := range prices {
		lastPrice[p.InventoryItemID.Bytes] = p
	}
	suppliers, err := s.q.ListSuppliersWithBalance(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	supByID := map[uuid.UUID]sqlc.ListSuppliersWithBalanceRow{}
	for _, x := range suppliers {
		supByID[x.ID] = x
	}
	groups := map[uuid.UUID]*restaurantv1.BuyListGroup{}
	var order []uuid.UUID
	for _, it := range items {
		if !it.Tracked || !it.Active || it.OnHandMilli > it.LowStockMilli {
			continue
		}
		set := byItem[it.ID]
		units := purchaseUnitsOf(it)
		unit := units[0]
		if set.DefaultUnitKey.Valid {
			if u, ok := findUnit(it, set.DefaultUnitKey.String); ok {
				unit = u
			}
		}
		line := &restaurantv1.BuyListLine{
			InventoryItemId: it.ID.String(), Name: local(it.NameEn, it.NameNe), Unit: unitEnum(it.Unit), OnHandMilli: it.OnHandMilli,
			LowStockMilli: it.LowStockMilli, UnitKey: unit.key, UnitLabel: local(unit.en, unit.ne), FactorMilli: unit.factorMilli,
		}
		if set.BuyUpToMilli.Valid {
			v := set.BuyUpToMilli.Int64
			line.BuyUpToMilli = &v
			if v > it.OnHandMilli {
				b := ceilUnits(v-it.OnHandMilli, unit.factorMilli)
				line.Buy = &b
			}
		}
		if lp, ok := lastPrice[it.ID]; ok {
			c := lp.UnitCostPaisa
			line.LastUnitCostPaisa = &c
			line.LastUnitLabel = local(lp.UnitLabelEn, lp.UnitLabelNe)
		}
		key := uuid.Nil
		if set.PreferredSupplierID.Valid {
			if _, ok := supByID[set.PreferredSupplierID.Bytes]; ok {
				key = set.PreferredSupplierID.Bytes
			}
		}
		g, ok := groups[key]
		if !ok {
			g = &restaurantv1.BuyListGroup{}
			if key != uuid.Nil {
				g.SupplierId, g.SupplierName, g.Phone = key.String(), supByID[key].Name, supByID[key].Phone
			}
			groups[key] = g
			order = append(order, key)
		}
		g.Lines = append(g.Lines, line)
	}
	out := make([]*restaurantv1.BuyListGroup, 0, len(order))
	for _, k := range order {
		out = append(out, groups[k])
	}
	slices.SortStableFunc(out, func(a, b *restaurantv1.BuyListGroup) int {
		if (a.SupplierId == "") != (b.SupplierId == "") {
			if a.SupplierId == "" {
				return 1
			}
			return -1
		}
		return strings.Compare(a.SupplierName, b.SupplierName)
	})
	return out, nil
}

// PurchaseBook: owner/manager. VAT bills by bill date, and (negative) returns on
// them by return date, in an AD range (inclusive).
func (s *Service) PurchaseBook(ctx context.Context, req *restaurantv1.PurchaseBookRequest) ([]*restaurantv1.PurchaseBookRow, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	from, err := parseDate("from", req.GetFrom())
	if err != nil {
		return nil, err
	}
	to, err := parseDate("to", req.GetTo())
	if err != nil {
		return nil, err
	}
	bills, err := s.q.ListVatPurchasesInRange(ctx, sqlc.ListVatPurchasesInRangeParams{FromDate: pgDate(from), ToDate: pgDate(to)})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	returns, err := s.q.ListVatReturnsInRange(ctx, sqlc.ListVatReturnsInRangeParams{FromDate: pgDate(from), ToDate: pgDate(to)})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := make([]*restaurantv1.PurchaseBookRow, 0, len(bills)+len(returns))
	for _, p := range bills {
		out = append(out, &restaurantv1.PurchaseBookRow{
			Date: dateStr(p.BillDate), DateBs: p.BillDateBs, BillNo: p.SupplierBillNo, Kind: restaurantv1.PurchaseBookRowKind_PURCHASE_BOOK_ROW_KIND_BILL,
			SupplierName: p.SupplierName, SupplierPan: p.SupplierPan, TotalPaisa: p.TotalPaisa, ExemptPaisa: p.ExemptPaisa,
			TaxablePaisa: p.TaxablePaisa, VatPaisa: p.VatPaisa,
		})
	}
	for _, r := range returns {
		out = append(out, &restaurantv1.PurchaseBookRow{
			Date: ktmDate(r.At), DateBs: r.BsDate, BillNo: r.SupplierBillNo, NoteNo: r.NoteNo, Kind: restaurantv1.PurchaseBookRowKind_PURCHASE_BOOK_ROW_KIND_RETURN,
			SupplierName: r.SupplierName, SupplierPan: r.SupplierPan, TotalPaisa: -r.TotalPaisa, ExemptPaisa: -r.ExemptPaisa,
			TaxablePaisa: -r.TaxablePaisa, VatPaisa: -r.VatPaisa,
		})
	}
	slices.SortStableFunc(out, func(a, b *restaurantv1.PurchaseBookRow) int {
		if c := strings.Compare(a.Date, b.Date); c != 0 {
			return c
		}
		return int(a.Kind) - int(b.Kind) // bills before returns on a day
	})
	return out, nil
}

// Payables: owner/manager. Suppliers with a non-zero balance, oldest debt first.
func (s *Service) Payables(ctx context.Context) ([]*restaurantv1.PayableRow, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	rows, err := s.q.ListSuppliersWithBalance(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := []*restaurantv1.PayableRow{}
	for _, r := range rows {
		if r.BalancePaisa == 0 {
			continue
		}
		sup := sqlc.Supplier{ID: r.ID, OpeningBalancePaisa: r.OpeningBalancePaisa, CreatedAt: r.CreatedAt}
		ds, settled, err := allocate(ctx, s.q, sup)
		if err != nil {
			return nil, connecterr.Internal(err)
		}
		row := &restaurantv1.PayableRow{SupplierId: r.ID.String(), Name: r.Name, Phone: r.Phone, Active: r.Active, BalancePaisa: r.BalancePaisa}
		for _, d := range ds {
			if settled[d.id] < d.amount {
				row.OldestUnpaid = d.date
				break
			}
		}
		last, err := s.q.LastSupplierPaymentAt(ctx, r.ID)
		if err == nil {
			row.LastPaymentAtUnix = last.Unix()
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, connecterr.Internal(err)
		}
		out = append(out, row)
	}
	slices.SortStableFunc(out, func(a, b *restaurantv1.PayableRow) int {
		ka, kb := a.OldestUnpaid, b.OldestUnpaid
		if ka == "" {
			ka = "~"
		}
		if kb == "" {
			kb = "~"
		}
		if c := strings.Compare(ka, kb); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

// GetPurchasesToday: owner/manager dashboard tiles (Kathmandu day).
func (s *Service) GetPurchasesToday(ctx context.Context) (*restaurantv1.PurchasesToday, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	n := time.Now().In(nepal)
	start := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, nepal)
	end := start.AddDate(0, 0, 1)
	out := &restaurantv1.PurchasesToday{}
	var err error
	if out.PurchasesPaisa, err = s.q.RecordedPurchasesTotalBetween(ctx, sqlc.RecordedPurchasesTotalBetweenParams{FromTs: start, ToTs: end}); err != nil {
		return nil, connecterr.Internal(err)
	}
	if out.PaidFromTillPaisa, err = s.q.TillPaidBetween(ctx, sqlc.TillPaidBetweenParams{FromTs: start, ToTs: end}); err != nil {
		return nil, connecterr.Internal(err)
	}
	if out.OwedPaisa, err = s.q.OwedToSuppliers(ctx); err != nil {
		return nil, connecterr.Internal(err)
	}
	return out, nil
}
