package purchases

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/audit"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/bsdate"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/idem"
	"github.com/oneaccord/restro/api/internal/inventory"
	"github.com/oneaccord/restro/api/internal/realtime"
)

// --- purchase units ---

type purchaseUnit struct {
	key         string
	en, ne      string
	factorMilli int64
}

type packJSON struct {
	NameEn    string `json:"name_en"`
	NameNe    string `json:"name_ne"`
	SizeMilli int64  `json:"size_milli"`
}

// purchaseUnitsOf: the stock unit (factor 1), kg / L for g / ml, and each pack.
func purchaseUnitsOf(it sqlc.InventoryItem) []purchaseUnit {
	out := []purchaseUnit{{key: it.Unit, en: it.Unit, ne: it.Unit, factorMilli: 1000}}
	switch it.Unit {
	case "g":
		out = append(out, purchaseUnit{key: "kg", en: "kg", ne: "kg", factorMilli: 1_000_000})
	case "ml":
		out = append(out, purchaseUnit{key: "L", en: "L", ne: "L", factorMilli: 1_000_000})
	}
	var packs []packJSON
	_ = json.Unmarshal(it.Packs, &packs)
	for _, p := range packs {
		if p.SizeMilli > 0 {
			out = append(out, purchaseUnit{key: "pack:" + p.NameEn, en: p.NameEn, ne: p.NameNe, factorMilli: p.SizeMilli})
		}
	}
	return out
}

func findUnit(it sqlc.InventoryItem, key string) (purchaseUnit, bool) {
	for _, u := range purchaseUnitsOf(it) {
		if u.key == key {
			return u, true
		}
	}
	return purchaseUnit{}, false
}

func unitEnum(s string) restaurantv1.StockUnit {
	switch s {
	case "g":
		return restaurantv1.StockUnit_STOCK_UNIT_G
	case "ml":
		return restaurantv1.StockUnit_STOCK_UNIT_ML
	case "pcs":
		return restaurantv1.StockUnit_STOCK_UNIT_PCS
	}
	return restaurantv1.StockUnit_STOCK_UNIT_UNSPECIFIED
}

// --- views ---

func displayNumber(n int32) string { return "P-" + itoa(int64(n)) }

type purchaseRow struct {
	sqlc.Purchase
	recordedByName string
}

func toPurchaseLine(l sqlc.PurchaseLine) *restaurantv1.PurchaseLine {
	return &restaurantv1.PurchaseLine{
		Id: l.ID.String(), PurchaseId: l.PurchaseID.String(), InventoryItemId: idStr(l.InventoryItemID),
		Name: local(l.NameEn, l.NameNe), StockUnit: unitEnum(l.StockUnit), QuantityMilli: l.QuantityMilli, UnitKey: l.UnitKey,
		UnitLabel: local(l.UnitLabelEn, l.UnitLabelNe), FactorMilli: l.FactorMilli, UnitCostPaisa: l.UnitCostPaisa,
		VatApplies: l.VatApplies, AmountPaisa: l.AmountPaisa, DiscountPaisa: l.DiscountPaisa, VatPaisa: l.VatPaisa,
		StockUnitCostE4: l.StockUnitCostE4, ReturnedMilli: l.ReturnedMilli,
	}
}

func toReturn(r sqlc.ListPurchaseReturnsForRow, lines []sqlc.PurchaseReturnLine) *restaurantv1.PurchaseReturn {
	out := &restaurantv1.PurchaseReturn{
		Id: r.ID.String(), PurchaseId: r.PurchaseID.String(), NoteNo: r.NoteNo, Reason: r.Reason,
		TaxablePaisa: r.TaxablePaisa, ExemptPaisa: r.ExemptPaisa, VatPaisa: r.VatPaisa, TotalPaisa: r.TotalPaisa,
		RefundPaisa: r.RefundPaisa, RefundShiftId: idStr(r.RefundShiftID), ById: r.ByUserID.String(), ByName: r.ByName,
		AtUnix: r.At.Unix(), BsDate: r.BsDate, Lines: []*restaurantv1.PurchaseReturnLine{},
	}
	for _, l := range lines {
		if l.ReturnID == r.ID {
			out.Lines = append(out.Lines, &restaurantv1.PurchaseReturnLine{PurchaseLineId: l.PurchaseLineID.String(), QuantityMilli: l.QuantityMilli})
		}
	}
	return out
}

// loadPurchases builds the full views (lines, returns, owed after oldest-first
// allocation) on q, so a transaction sees its own writes.
func loadPurchases(ctx context.Context, q *sqlc.Queries, rows []purchaseRow) ([]*restaurantv1.Purchase, error) {
	out := make([]*restaurantv1.Purchase, 0, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	lines, err := q.ListPurchaseLinesFor(ctx, ids)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	returns, err := q.ListPurchaseReturnsFor(ctx, ids)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	retLines, err := q.ListPurchaseReturnLinesFor(ctx, ids)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	settledBy := map[uuid.UUID]map[uuid.UUID]int64{} // supplier → debt → settled
	for _, r := range rows {
		p := &restaurantv1.Purchase{
			Id: r.ID.String(), DisplayNumber: displayNumber(r.Number), SupplierId: r.SupplierID.String(), SupplierName: r.SupplierName,
			SupplierPan: r.SupplierPan, SupplierBillNo: r.SupplierBillNo, BillDate: dateStr(r.BillDate), BillDateBs: r.BillDateBs,
			FiscalYear: r.FiscalYear, IsVatBill: r.IsVatBill, DiscountPaisa: r.DiscountPaisa, TaxablePaisa: r.TaxablePaisa,
			ExemptPaisa: r.ExemptPaisa, VatPaisa: r.VatPaisa, TotalPaisa: r.TotalPaisa, Status: statusEnum(r.Status),
			VoidReason: r.VoidReason, VoidedById: idStr(r.VoidedByUserID), VoidedAtUnix: unixOf(r.VoidedAt),
			RecordedById: r.RecordedByUserID.String(), RecordedByName: r.recordedByName, AtUnix: r.At.Unix(),
			Lines: []*restaurantv1.PurchaseLine{}, Returns: []*restaurantv1.PurchaseReturn{},
		}
		for _, l := range lines {
			if l.PurchaseID == r.ID {
				p.Lines = append(p.Lines, toPurchaseLine(l))
			}
		}
		for _, ret := range returns {
			if ret.PurchaseID == r.ID {
				p.Returns = append(p.Returns, toReturn(ret, retLines))
				p.ReturnedTotalPaisa += ret.TotalPaisa
			}
		}
		if r.Status == "RECORDED" {
			settled, ok := settledBy[r.SupplierID]
			if !ok {
				sup, err := q.GetSupplier(ctx, r.SupplierID)
				if err != nil {
					return nil, connecterr.Internal(err)
				}
				if _, settled, err = allocate(ctx, q, sup); err != nil {
					return nil, connecterr.Internal(err)
				}
				settledBy[r.SupplierID] = settled
			}
			p.OwedPaisa = r.TotalPaisa - p.ReturnedTotalPaisa - settled[r.ID]
		}
		out = append(out, p)
	}
	return out, nil
}

func statusEnum(s string) restaurantv1.PurchaseStatus {
	if s == "VOIDED" {
		return restaurantv1.PurchaseStatus_PURCHASE_STATUS_VOIDED
	}
	return restaurantv1.PurchaseStatus_PURCHASE_STATUS_RECORDED
}

func loadPurchase(ctx context.Context, q *sqlc.Queries, id uuid.UUID) (*restaurantv1.Purchase, error) {
	r, err := q.GetPurchaseRow(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, connecterr.NotFound("purchase not found")
	}
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	list, err := loadPurchases(ctx, q, []purchaseRow{{Purchase: purchaseOf(r), recordedByName: r.RecordedByName}})
	if err != nil {
		return nil, err
	}
	return list[0], nil
}

func purchaseOf(r sqlc.GetPurchaseRowRow) sqlc.Purchase {
	return sqlc.Purchase{
		ID: r.ID, Number: r.Number, SupplierID: r.SupplierID, SupplierName: r.SupplierName, SupplierPan: r.SupplierPan,
		SupplierBillNo: r.SupplierBillNo, BillDate: r.BillDate, BillDateBs: r.BillDateBs, FiscalYear: r.FiscalYear,
		IsVatBill: r.IsVatBill, DiscountPaisa: r.DiscountPaisa, TaxablePaisa: r.TaxablePaisa, ExemptPaisa: r.ExemptPaisa,
		VatPaisa: r.VatPaisa, TotalPaisa: r.TotalPaisa, Status: r.Status, VoidReason: r.VoidReason,
		VoidedByUserID: r.VoidedByUserID, VoidedAt: r.VoidedAt, RecordedByUserID: r.RecordedByUserID, At: r.At,
	}
}

// --- record ---

type checkedLine struct {
	in      *restaurantv1.PurchaseLineInput
	item    *sqlc.InventoryItem
	unit    purchaseUnit
	nameEn  string
	nameNe  string
	amount  int64
	vatFlag bool
}

// RecordPurchase: owner, manager, cashier. Bill, lines, PURCHASE movements (moving
// average cost), any pay-now payment and the audit entries in one transaction.
// Idempotent on request_id.
func (s *Service) RecordPurchase(ctx context.Context, req *restaurantv1.RecordPurchaseRequest) (*restaurantv1.Purchase, error) {
	actor, err := requireBuyer(ctx)
	if err != nil {
		return nil, err
	}
	reqID, err := parseID("request_id", req.GetRequestId())
	if err != nil {
		return nil, err
	}
	supplierID, err := parseID("supplier_id", req.GetSupplierId())
	if err != nil {
		return nil, err
	}
	billDate, err := parseDate("bill_date", req.GetBillDate())
	if err != nil {
		return nil, err
	}
	if billDate.Format(time.DateOnly) > ktmDate(time.Now()) {
		return nil, connecterr.Invalid("bill_date", "the bill date is in the future")
	}
	if n := len(req.GetLines()); n == 0 || n > maxLines {
		return nil, connecterr.Invalid("lines", "a bill has 1 to 100 lines")
	}
	if req.GetDiscountPaisa() < 0 || req.GetVatPaisa() < 0 || req.GetVatPaisa() > maxMoneyBill {
		return nil, connecterr.Invalid("vat", "invalid amount")
	}
	billNo := strings.TrimSpace(req.GetSupplierBillNo())

	out := &restaurantv1.Purchase{}
	var tillShift string
	err = idem.Run(ctx, s.pool, s.q, reqID, actor.User.ID, out, func(q *sqlc.Queries) error {
		sup, e := q.LockSupplier(ctx, supplierID)
		if errors.Is(e, pgx.ErrNoRows) {
			return connecterr.Invalid("supplier_id", "supplier not found")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		if !sup.Active {
			return connecterr.Invalid("supplier_id", "this supplier is inactive") // web: SUPPLIER_INACTIVE
		}
		if req.GetIsVatBill() && (sup.Pan == "" || !sup.VatRegistered || billNo == "") {
			return notAVatBill()
		}
		if billNo != "" {
			taken, e := q.SupplierBillTaken(ctx, sqlc.SupplierBillTakenParams{SupplierID: sup.ID, BillNo: billNo})
			if e != nil {
				return connecterr.Internal(e)
			}
			if taken {
				return duplicateSupplierBill()
			}
		}
		lines := make([]checkedLine, 0, len(req.GetLines()))
		for _, l := range req.GetLines() {
			if l.GetQuantityMilli() <= 0 || l.GetQuantityMilli() > maxQtyMilli || l.GetUnitCostPaisa() < 0 || l.GetUnitCostPaisa() > maxUnitCost {
				return connecterr.Invalid("lines", "each line needs a quantity and a cost")
			}
			c := checkedLine{in: l, vatFlag: l.GetVatApplies(), amount: lineAmount(l.GetQuantityMilli(), l.GetUnitCostPaisa())}
			if l.GetInventoryItemId() == "" {
				d := strings.TrimSpace(l.GetDescription())
				if d == "" {
					return connecterr.Invalid("lines", "a non-stock line needs a description")
				}
				c.nameEn, c.nameNe, c.unit = d, d, purchaseUnit{factorMilli: 1000}
			} else {
				itemID, e := parseID("lines", l.GetInventoryItemId())
				if e != nil {
					return e
				}
				it, e := q.GetInventoryItem(ctx, itemID)
				if errors.Is(e, pgx.ErrNoRows) {
					return connecterr.Invalid("lines", "stock item not found")
				}
				if e != nil {
					return connecterr.Internal(e)
				}
				if !it.Active {
					return withField(connecterr.New(itemUntrackedCode, restaurantv1.ErrorReason_ERROR_REASON_ITEM_UNTRACKED,
						it.NameEn+" is inactive"), "lines") // web: ITEM_INACTIVE
				}
				u, ok := findUnit(it, l.GetUnitKey())
				if !ok {
					return connecterr.Invalid("lines", "unknown purchase unit "+l.GetUnitKey())
				}
				c.item, c.unit, c.nameEn, c.nameNe = &it, u, it.NameEn, it.NameNe
			}
			lines = append(lines, c)
		}
		money := make([]lineMoney, len(lines))
		for i, l := range lines {
			money[i] = lineMoney{amount: l.amount, vatApplies: l.vatFlag}
		}
		if gross := billTotals(money, 0).gross; req.GetDiscountPaisa() > gross {
			return connecterr.Invalid("discount", "the discount is more than the bill")
		}
		t := billTotals(money, req.GetDiscountPaisa())
		vatFlags := make([]bool, len(lines))
		for i, l := range lines {
			vatFlags[i] = l.vatFlag
		}
		vats := vatShares(t.nets, vatFlags, req.GetVatPaisa())
		total := t.taxable + t.exempt + req.GetVatPaisa()

		var pay *paymentCheck
		if pn := req.GetPayNow(); pn != nil && pn.GetAmountPaisa() != 0 {
			balance, e := q.SupplierBalance(ctx, sup.ID)
			if e != nil {
				return connecterr.Internal(e)
			}
			if pay, e = s.checkPayment(ctx, q, actor, pn.GetAmountPaisa(), pn.GetMethod(), req.GetApproval(), balance+total); e != nil {
				return e
			}
		}

		settings, e := q.GetSettings(ctx)
		if e != nil {
			return connecterr.Internal(e)
		}
		includeVat := !(settings.VatRegistered && req.GetIsVatBill())
		bsd, e := bsdate.FromAD(billDate.Add(12 * time.Hour))
		if e != nil {
			return connecterr.Invalid("bill_date", "date outside the supported calendar")
		}
		number, e := q.AllocatePurchaseCounter(ctx, "PURCHASE")
		if e != nil {
			return connecterr.Internal(e)
		}
		at := now()
		p, e := q.InsertPurchase(ctx, sqlc.InsertPurchaseParams{
			Number: number, SupplierID: sup.ID, SupplierName: sup.Name, SupplierPan: sup.Pan, SupplierBillNo: billNo,
			BillDate: pgDate(billDate), BillDateBs: bsd.String(), FiscalYear: bsd.FiscalYear(), IsVatBill: req.GetIsVatBill(),
			DiscountPaisa: req.GetDiscountPaisa(), TaxablePaisa: t.taxable, ExemptPaisa: t.exempt, VatPaisa: req.GetVatPaisa(),
			TotalPaisa: total, RecordedByUserID: actor.User.ID, At: at,
		})
		if e != nil {
			if isUnique(e) {
				return duplicateSupplierBill()
			}
			return connecterr.Internal(e)
		}
		var moves []inventory.PurchaseMove
		for i, l := range lines {
			params := sqlc.InsertPurchaseLineParams{
				PurchaseID: p.ID, Sort: int32(i), NameEn: l.nameEn, NameNe: l.nameNe, QuantityMilli: l.in.GetQuantityMilli(),
				FactorMilli: l.unit.factorMilli, UnitCostPaisa: l.in.GetUnitCostPaisa(), VatApplies: l.vatFlag,
				AmountPaisa: l.amount, DiscountPaisa: t.discounts[i], VatPaisa: vats[i],
				StockUnitCostE4: costPerStockUnitE4(t.nets[i], vats[i], l.in.GetQuantityMilli(), l.unit.factorMilli, includeVat),
			}
			if l.item != nil {
				params.InventoryItemID = dbx.UUID(l.item.ID)
				params.StockUnit, params.UnitKey, params.UnitLabelEn, params.UnitLabelNe = l.item.Unit, l.unit.key, l.unit.en, l.unit.ne
			}
			pl, e := q.InsertPurchaseLine(ctx, params)
			if e != nil {
				return connecterr.Internal(e)
			}
			if l.item != nil {
				moves = append(moves, inventory.PurchaseMove{ItemID: l.item.ID, QtyMilli: stockMilli(pl.QuantityMilli, pl.FactorMilli), CostE4: pl.StockUnitCostE4, LineID: pl.ID})
			}
		}
		if e := inventory.PostPurchase(ctx, q, moves, actor.User.ID, at); e != nil {
			return e
		}
		bill := billNo
		if bill == "" {
			bill = "-"
		}
		if e := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: ActionRecorded, Entity: "purchase", EntityID: p.ID.String(),
			Reason: sup.Name + " · bill " + bill + " · " + itoa(total) + " paisa",
			After:  audit.JSON(map[string]any{"supplier_id": sup.ID.String(), "number": number, "total": total, "is_vat_bill": req.GetIsVatBill()})}); e != nil {
			return e
		}
		if pay != nil {
			pn := req.GetPayNow()
			if _, e := s.createPayment(ctx, q, actor, sup, pn.GetAmountPaisa(), pn.GetMethod(), pn.GetReference(), "", pay); e != nil {
				return e
			}
			tillShift = pay.shiftID
		}
		view, e := loadPurchase(ctx, q, p.ID)
		if e != nil {
			return e
		}
		proto.Merge(out, view)
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: EventPurchaseRecorded, ID: out.Id}, realtime.Event{Type: EventSupplierUpdated, ID: out.SupplierId})
	if tillShift != "" {
		s.emit(realtime.Event{Type: "shift.updated", ID: tillShift})
	}
	return out, nil
}

// --- void ---

// VoidPurchase: owner/manager, with a reason. Reversing PURCHASE_RETURN movements
// (reason "void"); the bill leaves the balance and the purchase book, payments on
// it become supplier credit. A bill with returns can't be voided.
func (s *Service) VoidPurchase(ctx context.Context, req *restaurantv1.VoidPurchaseRequest) (*restaurantv1.Purchase, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID("id", req.GetId())
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(req.GetReason())
	if reason == "" {
		return nil, connecterr.Invalid("reason", "a reason is required")
	}
	var out *restaurantv1.Purchase
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		p, e := q.LockPurchase(ctx, id)
		if errors.Is(e, pgx.ErrNoRows) {
			return connecterr.NotFound("purchase not found")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		if _, e := q.LockSupplier(ctx, p.SupplierID); e != nil {
			return connecterr.Internal(e)
		}
		if p.Status != "RECORDED" {
			return connecterr.Invalid("id", "this bill is already voided")
		}
		n, e := q.CountPurchaseReturns(ctx, id)
		if e != nil {
			return connecterr.Internal(e)
		}
		if n > 0 {
			return connecterr.InUse("a bill with returns can't be voided") // web: PURCHASE_HAS_RETURNS
		}
		lines, e := q.ListPurchaseLinesFor(ctx, []uuid.UUID{id})
		if e != nil {
			return connecterr.Internal(e)
		}
		at := now()
		if e := inventory.PostPurchaseReturn(ctx, q, stockMoves(lines, func(l sqlc.PurchaseLine) int64 { return l.QuantityMilli }), actor.User.ID, at, "void"); e != nil {
			return e
		}
		if e := q.VoidPurchase(ctx, sqlc.VoidPurchaseParams{ID: id, VoidReason: reason, VoidedByUserID: dbx.UUID(actor.User.ID)}); e != nil {
			return connecterr.Internal(e)
		}
		if e := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: ActionVoided, Entity: "purchase", EntityID: id.String(),
			Reason: p.SupplierName + " · " + itoa(p.TotalPaisa) + " paisa · " + reason}); e != nil {
			return e
		}
		out, e = loadPurchase(ctx, q, id)
		return e
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: EventPurchaseRecorded, ID: out.Id}, realtime.Event{Type: EventSupplierUpdated, ID: out.SupplierId})
	return out, nil
}

func stockMoves(lines []sqlc.PurchaseLine, qty func(sqlc.PurchaseLine) int64) []inventory.PurchaseMove {
	var out []inventory.PurchaseMove
	for _, l := range lines {
		if !l.InventoryItemID.Valid || qty(l) <= 0 {
			continue
		}
		out = append(out, inventory.PurchaseMove{ItemID: l.InventoryItemID.Bytes, QtyMilli: stockMilli(qty(l), l.FactorMilli), CostE4: l.StockUnitCostE4, LineID: l.ID})
	}
	return out
}

// --- return ---

// ReturnPurchase: owner/manager. Quantities in the line's purchase unit, up to
// bought − already returned. Lowers stock (at the line's own cost; the average is
// unchanged) and what is owed. A refund handed back is a paid-in on the returner's
// open shift, up to the credit the return leaves. Idempotent on request_id.
func (s *Service) ReturnPurchase(ctx context.Context, req *restaurantv1.ReturnPurchaseRequest) (*restaurantv1.PurchaseReturn, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	reqID, err := parseID("request_id", req.GetRequestId())
	if err != nil {
		return nil, err
	}
	purchaseID, err := parseID("purchase_id", req.GetPurchaseId())
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(req.GetReason())
	if reason == "" {
		return nil, connecterr.Invalid("reason", "a reason is required")
	}
	wanted := map[uuid.UUID]int64{}
	var order []uuid.UUID
	for _, rl := range req.GetLines() {
		if rl.GetQuantityMilli() < 0 {
			return nil, connecterr.Invalid("lines", "invalid quantity")
		}
		lid, e := parseID("lines", rl.GetPurchaseLineId())
		if e != nil {
			return nil, e
		}
		if rl.GetQuantityMilli() == 0 {
			continue
		}
		if _, ok := wanted[lid]; !ok {
			order = append(order, lid)
		}
		wanted[lid] += rl.GetQuantityMilli()
	}
	if len(wanted) == 0 {
		return nil, connecterr.Invalid("lines", "choose what to return")
	}
	refund := req.GetRefundPaisa()

	out := &restaurantv1.PurchaseReturn{}
	var supplierID uuid.UUID
	var refundShift string
	err = idem.Run(ctx, s.pool, s.q, reqID, actor.User.ID, out, func(q *sqlc.Queries) error {
		p, e := q.LockPurchase(ctx, purchaseID)
		if errors.Is(e, pgx.ErrNoRows) {
			return connecterr.Invalid("purchase_id", "purchase not found")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		supplierID = p.SupplierID
		if _, e := q.LockSupplier(ctx, p.SupplierID); e != nil {
			return connecterr.Internal(e)
		}
		if p.Status != "RECORDED" {
			return connecterr.Invalid("purchase_id", "this bill is voided")
		}
		lines, e := q.ListPurchaseLinesFor(ctx, []uuid.UUID{p.ID})
		if e != nil {
			return connecterr.Internal(e)
		}
		retLines, e := q.ListPurchaseReturnLinesFor(ctx, []uuid.UUID{p.ID})
		if e != nil {
			return connecterr.Internal(e)
		}
		byID := map[uuid.UUID]sqlc.PurchaseLine{}
		for _, l := range lines {
			byID[l.ID] = l
		}
		type value struct{ net, vat int64 }
		values := map[uuid.UUID]value{}
		var taxable, exempt, vat int64
		for _, lid := range order {
			l, ok := byID[lid]
			if !ok {
				return connecterr.Invalid("lines", "not a line of this bill")
			}
			qty := wanted[lid]
			if l.ReturnedMilli+qty > l.QuantityMilli {
				return returnTooHigh()
			}
			already := lineValue{}
			for _, rl := range retLines {
				if rl.PurchaseLineID == lid {
					already.qtyMilli += rl.QuantityMilli
					already.net += rl.NetPaisa
					already.vat += rl.VatPaisa
				}
			}
			net, v := returnValue(lineValue{qtyMilli: l.QuantityMilli, net: l.AmountPaisa - l.DiscountPaisa, vat: l.VatPaisa}, qty, already)
			values[lid] = value{net, v}
			if l.VatApplies {
				taxable += net
			} else {
				exempt += net
			}
			vat += v
		}
		total := taxable + exempt + vat
		var shiftID = dbx.UUIDOpt(uuid.Nil)
		if refund > 0 {
			sh, e := lockOpenShift(ctx, q, actor.User.ID, "open a shift before taking a refund into the till")
			if e != nil {
				return e
			}
			balance, e := q.SupplierBalance(ctx, p.SupplierID)
			if e != nil {
				return connecterr.Internal(e)
			}
			if credit := -(balance - total); refund > credit {
				return connecterr.Invalid("refund", "the refund is more than the credit this return leaves")
			}
			shiftID = dbx.UUID(sh.ID)
			refundShift = sh.ID.String()
		}
		at := now()
		bsd, e := bsdate.FromAD(at)
		if e != nil {
			return connecterr.Internal(e)
		}
		ret, e := q.InsertPurchaseReturn(ctx, sqlc.InsertPurchaseReturnParams{
			PurchaseID: p.ID, NoteNo: strings.TrimSpace(req.GetNoteNo()), Reason: reason, TaxablePaisa: taxable, ExemptPaisa: exempt,
			VatPaisa: vat, TotalPaisa: total, RefundPaisa: refund, RefundShiftID: shiftID, ByUserID: actor.User.ID, At: at, BsDate: bsd.String(),
		})
		if e != nil {
			return connecterr.Internal(e)
		}
		for _, lid := range order {
			v := values[lid]
			if e := q.InsertPurchaseReturnLine(ctx, sqlc.InsertPurchaseReturnLineParams{ReturnID: ret.ID, PurchaseLineID: lid, QuantityMilli: wanted[lid], NetPaisa: v.net, VatPaisa: v.vat}); e != nil {
				return connecterr.Internal(e)
			}
			if e := q.AddPurchaseLineReturned(ctx, sqlc.AddPurchaseLineReturnedParams{ID: lid, Qty: wanted[lid]}); e != nil {
				return connecterr.Internal(e)
			}
		}
		if e := inventory.PostPurchaseReturn(ctx, q, stockMoves(lines, func(l sqlc.PurchaseLine) int64 { return wanted[l.ID] }), actor.User.ID, at, reason); e != nil {
			return e
		}
		detail := p.SupplierName + " · " + itoa(total) + " paisa"
		if ret.NoteNo != "" {
			detail += " · note " + ret.NoteNo
		}
		if refund > 0 {
			detail += " · refund " + itoa(refund) + " paisa"
		}
		if e := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: ActionReturned, Entity: "purchase", EntityID: p.ID.String(),
			Reason: detail + " · " + reason, After: audit.JSON(map[string]any{"return_id": ret.ID.String(), "total": total, "refund": refund})}); e != nil {
			return e
		}
		rows, e := q.ListPurchaseReturnsFor(ctx, []uuid.UUID{p.ID})
		if e != nil {
			return connecterr.Internal(e)
		}
		all, e := q.ListPurchaseReturnLinesFor(ctx, []uuid.UUID{p.ID})
		if e != nil {
			return connecterr.Internal(e)
		}
		for _, r := range rows {
			if r.ID == ret.ID {
				proto.Merge(out, toReturn(r, all))
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: EventPurchaseRecorded, ID: purchaseID.String()})
	if supplierID != uuid.Nil {
		s.emit(realtime.Event{Type: EventSupplierUpdated, ID: supplierID.String()})
	}
	if refundShift != "" {
		s.emit(realtime.Event{Type: "shift.updated", ID: refundShift})
	}
	return out, nil
}

// --- reads ---

// ListPurchases: owner, manager, cashier. By bill date window, newest first.
func (s *Service) ListPurchases(ctx context.Context, req *restaurantv1.ListPurchasesRequest) ([]*restaurantv1.Purchase, error) {
	if _, err := requireBuyer(ctx); err != nil {
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
	params := sqlc.ListPurchaseRowsParams{FromDate: pgDate(from), ToDate: pgDate(to), IncludeVoided: req.GetIncludeVoided()}
	if req.GetSupplierId() != "" {
		id, err := parseID("supplier_id", req.GetSupplierId())
		if err != nil {
			return nil, err
		}
		params.SupplierID = dbx.UUID(id)
	}
	if req.GetInventoryItemId() != "" {
		id, err := parseID("inventory_item_id", req.GetInventoryItemId())
		if err != nil {
			return nil, err
		}
		params.InventoryItemID = dbx.UUID(id)
	}
	rows, err := s.q.ListPurchaseRows(ctx, params)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	list := make([]purchaseRow, len(rows))
	for i, r := range rows {
		list[i] = purchaseRow{Purchase: purchaseOf(sqlc.GetPurchaseRowRow(r)), recordedByName: r.RecordedByName}
	}
	return loadPurchases(ctx, s.q, list)
}

func (s *Service) GetPurchase(ctx context.Context, id string) (*restaurantv1.Purchase, error) {
	if _, err := requireBuyer(ctx); err != nil {
		return nil, err
	}
	pid, err := parseID("id", id)
	if err != nil {
		return nil, err
	}
	return loadPurchase(ctx, s.q, pid)
}
