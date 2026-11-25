package inventory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/audit"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/realtime"
)

var wasteReasons = map[restaurantv1.WasteReason]string{
	restaurantv1.WasteReason_WASTE_REASON_SPOILED:    "SPOILED",
	restaurantv1.WasteReason_WASTE_REASON_DROPPED:    "DROPPED",
	restaurantv1.WasteReason_WASTE_REASON_MISTAKE:    "MISTAKE",
	restaurantv1.WasteReason_WASTE_REASON_STAFF_MEAL: "STAFF_MEAL",
	restaurantv1.WasteReason_WASTE_REASON_RETURNED:   "RETURNED",
	restaurantv1.WasteReason_WASTE_REASON_OTHER:      "OTHER",
}

func wasteReasonEnum(s string) restaurantv1.WasteReason {
	for k, v := range wasteReasons {
		if v == s {
			return k
		}
	}
	return restaurantv1.WasteReason_WASTE_REASON_UNSPECIFIED
}

// wasteRow is a waste entry with the names of who logged and approved it.
type wasteRow struct {
	sqlc.WasteEntry
	byName, approvedByName string
}

func wasteView(w wasteRow, admin bool) *restaurantv1.WasteEntry {
	out := &restaurantv1.WasteEntry{
		Id: w.ID.String(), QuantityMilli: w.QuantityMilli, Name: local(w.NameEn, w.NameNe), Unit: unitEnum(w.Unit),
		Reason: wasteReasonEnum(w.Reason), Note: w.Note, ByUserId: w.ByUserID.String(), ByName: w.byName,
		ApprovedByUserId: pgID(w.ApprovedByUserID), ApprovedByName: w.approvedByName, AtUnix: w.At.Unix(),
	}
	if w.InventoryItemID.Valid {
		out.Target = &restaurantv1.WasteEntry_Inventory{Inventory: &restaurantv1.InventoryWasteTarget{InventoryItemId: pgID(w.InventoryItemID)}}
	} else {
		mt := &restaurantv1.MenuWasteTarget{MenuItemId: pgID(w.MenuItemID)}
		if w.VariantID.Valid {
			v := pgID(w.VariantID)
			mt.VariantId = &v
		}
		for _, m := range w.ModifierIds {
			mt.ModifierIds = append(mt.ModifierIds, m.String())
		}
		out.Target = &restaurantv1.WasteEntry_Menu{Menu: mt}
	}
	if admin {
		v := w.ValuePaisa
		out.ValuePaisa = &v
	}
	return out
}

func (s *Service) wasteRowOf(ctx context.Context, q *sqlc.Queries, w sqlc.WasteEntry) (wasteRow, error) {
	ids := []uuid.UUID{w.ByUserID}
	if w.ApprovedByUserID.Valid {
		ids = append(ids, uuid.UUID(w.ApprovedByUserID.Bytes))
	}
	names, err := userNames(ctx, q, ids...)
	if err != nil {
		return wasteRow{}, err
	}
	r := wasteRow{WasteEntry: w, byName: names[w.ByUserID]}
	if w.ApprovedByUserID.Valid {
		r.approvedByName = names[uuid.UUID(w.ApprovedByUserID.Bytes)]
	}
	return r, nil
}

// wasteLine is one ingredient leaving stock as waste.
type wasteLine struct {
	item     sqlc.InventoryItem
	qtyMilli int64
}

// LogWaste records waste of an inventory item, or of a menu item expanded through
// its recipe, as WASTE movements. A reason is required (OTHER needs a note); only
// tracked, active items move (ITEM_UNTRACKED otherwise). Over Rs 1,000 it needs a
// manager PIN (MANAGER_APPROVAL_REQUIRED). Idempotent on request_id.
func (s *Service) LogWaste(ctx context.Context, req *restaurantv1.LogWasteRequest) (*restaurantv1.WasteEntry, error) {
	actor, err := requireStock(ctx)
	if err != nil {
		return nil, err
	}
	admin := isAdmin(actor)
	requestID, err := parseID("request_id", req.GetRequestId())
	if err != nil {
		return nil, err
	}
	if prev, e := s.q.GetWasteByRequestID(ctx, requestID); e == nil {
		r, re := s.wasteRowOf(ctx, s.q, prev)
		if re != nil {
			return nil, re
		}
		return wasteView(r, admin), nil
	} else if !errors.Is(e, pgx.ErrNoRows) {
		return nil, connecterr.Internal(e)
	}
	if req.GetQuantityMilli() <= 0 {
		return nil, connecterr.Invalid("quantity_milli", "quantity must be more than zero")
	}
	reason, ok := wasteReasons[req.GetReason()]
	if !ok {
		return nil, connecterr.Invalid("reason", "choose a reason")
	}
	note := strings.TrimSpace(req.GetNote())
	if reason == "OTHER" && note == "" {
		return nil, connecterr.Invalid("note", "say what happened")
	}

	params := sqlc.InsertWasteEntryParams{
		RequestID: requestID, QuantityMilli: req.GetQuantityMilli(), Reason: reason, Note: note, ByUserID: actor.User.ID, ModifierIds: []uuid.UUID{},
	}
	var lines []wasteLine
	switch t := req.GetTarget().(type) {
	case *restaurantv1.LogWasteRequest_Inventory:
		id, e := parseID("inventory_item_id", t.Inventory.GetInventoryItemId())
		if e != nil {
			return nil, e
		}
		item, e := s.q.GetInventoryItem(ctx, id)
		if e != nil {
			return nil, notFound(e, "stock item")
		}
		if !live(item) {
			return nil, itemUntracked(item.NameEn + " is not tracked")
		}
		lines = []wasteLine{{item, req.GetQuantityMilli()}}
		params.InventoryItemID, params.NameEn, params.NameNe, params.Unit = dbx.UUID(id), item.NameEn, item.NameNe, item.Unit
	case *restaurantv1.LogWasteRequest_Menu:
		if lines, err = s.menuWaste(ctx, t.Menu, req.GetQuantityMilli(), &params); err != nil {
			return nil, err
		}
	default:
		return nil, connecterr.Invalid("target", "choose a stock item or a menu item")
	}

	terms := make([]term, 0, len(lines))
	for _, l := range lines {
		terms = append(terms, term{l.qtyMilli, l.item.UnitCostE4})
	}
	params.ValuePaisa = valuePaisa(terms...)
	var approver uuid.UUID
	if params.ValuePaisa > WasteApprovalPaisa {
		if req.GetApproval().GetManagerUserId() == "" {
			return nil, connecterr.ManagerApprovalRequired("waste over Rs 1,000 needs a manager PIN")
		}
		mgr, me := s.auth.VerifyManagerApproval(ctx, auth.ManagerApproval{ManagerUserID: req.GetApproval().GetManagerUserId(), Pin: req.GetApproval().GetPin()})
		if me != nil {
			return nil, me
		}
		approver = mgr.ID
		params.ApprovedByUserID = dbx.UUID(approver)
	}
	params.At = now()

	var out *restaurantv1.WasteEntry
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		entry, e := q.InsertWasteEntry(ctx, params)
		if errors.Is(e, pgx.ErrNoRows) { // the same request_id committed concurrently
			if entry, e = q.GetWasteByRequestID(ctx, requestID); e != nil {
				return connecterr.Internal(e)
			}
			r, re := s.wasteRowOf(ctx, q, entry)
			if re != nil {
				return re
			}
			out = wasteView(r, admin)
			return nil
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		moves := make([]move, 0, len(lines))
		for _, l := range lines {
			moves = append(moves, move{itemID: l.item.ID, qtyMilli: -l.qtyMilli, typ: TypeWaste, ref: entry.ID, reason: reason, by: actor.User.ID, at: entry.At, costE4: l.item.UnitCostE4})
		}
		if e := record(ctx, q, moves); e != nil {
			return e
		}
		qty := qtyText(entry.QuantityMilli, entry.Unit)
		if entry.Unit == "" {
			qty += " ×"
		}
		detail := fmt.Sprintf("%s · %s · %s · %d paisa", entry.NameEn, reason, qty, entry.ValuePaisa)
		if note != "" {
			detail += " · " + note
		}
		if e := audit.Write(ctx, q, audit.Entry{
			ActorID: actor.User.ID, Action: "WASTE_LOGGED", Entity: "waste_entry", EntityID: entry.ID.String(),
			Reason: detail, ApproverID: approver, After: audit.JSON(map[string]any{"amount": entry.ValuePaisa}),
		}); e != nil {
			return e
		}
		r, re := s.wasteRowOf(ctx, q, entry)
		if re != nil {
			return re
		}
		out = wasteView(r, admin)
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "waste.logged", ID: out.Id})
	return out, nil
}

// menuWaste expands whole portions of a dish through its recipe (tracked, active
// ingredients only) and fills the entry's menu fields.
func (s *Service) menuWaste(ctx context.Context, t *restaurantv1.MenuWasteTarget, qtyMilli int64, p *sqlc.InsertWasteEntryParams) ([]wasteLine, error) {
	if qtyMilli%1000 != 0 {
		return nil, connecterr.Invalid("quantity_milli", "waste whole portions of a menu item")
	}
	portions := qtyMilli / 1000
	itemID, err := parseID("menu_item_id", t.GetMenuItemId())
	if err != nil {
		return nil, err
	}
	item, err := s.q.GetItem(ctx, itemID)
	if err != nil {
		return nil, notFound(err, "menu item")
	}
	variants, err := s.q.ListVariantsForItem(ctx, itemID)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	var variant *sqlc.ItemVariant
	if t.VariantId != nil {
		i := slices.IndexFunc(variants, func(v sqlc.ItemVariant) bool { return v.ID.String() == t.GetVariantId() })
		if i < 0 {
			return nil, connecterr.Invalid("variant_id", "variant is not an option of "+item.NameEn)
		}
		variant = &variants[i]
	} else if len(variants) > 0 {
		return nil, connecterr.Invalid("variant_id", "choose a variant of "+item.NameEn)
	}
	mods := make([]uuid.UUID, 0, len(t.GetModifierIds()))
	for _, m := range t.GetModifierIds() {
		id, e := parseID("modifier_ids", m)
		if e != nil {
			return nil, e
		}
		mods = append(mods, id)
	}
	variantID := uuid.Nil
	if variant != nil {
		variantID = variant.ID
	}
	recipe, err := RecipeFor(ctx, s.q, itemID, variantID, mods)
	if err != nil {
		return nil, err
	}
	items, err := liveItems(ctx, s.q, recipe)
	if err != nil {
		return nil, err
	}
	var lines []wasteLine
	for _, l := range recipe {
		if it, ok := items[l.ItemID]; ok {
			lines = append(lines, wasteLine{it, l.QtyMilli * portions})
		}
	}
	if len(lines) == 0 {
		return nil, itemUntracked(item.NameEn + " has no tracked ingredients")
	}
	p.MenuItemID, p.ModifierIds, p.NameEn, p.NameNe, p.Unit = dbx.UUID(itemID), mods, item.NameEn, item.NameNe, ""
	if variant != nil {
		p.VariantID = dbx.UUID(variant.ID)
		p.NameEn = item.NameEn + " · " + variant.NameEn
		p.NameNe = orEn(item.NameNe, item.NameEn) + " · " + orEn(variant.NameNe, variant.NameEn)
	}
	return lines, nil
}

func orEn(ne, en string) string {
	if ne == "" {
		return en
	}
	return ne
}

// ListWaste is newest first; values are absent for cashier/kitchen.
func (s *Service) ListWaste(ctx context.Context, fromUnix, toUnix int64) ([]*restaurantv1.WasteEntry, error) {
	actor, err := requireStock(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.wasteInRange(ctx, fromUnix, toUnix)
	if err != nil {
		return nil, err
	}
	out := make([]*restaurantv1.WasteEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, wasteView(r, isAdmin(actor)))
	}
	return out, nil
}

func (s *Service) wasteInRange(ctx context.Context, fromUnix, toUnix int64) ([]wasteRow, error) {
	from, to := closedRange(fromUnix, toUnix)
	rows, err := s.q.ListWasteInRange(ctx, sqlc.ListWasteInRangeParams{FromTs: from, ToTs: to})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := make([]wasteRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, wasteRow{
			WasteEntry: sqlc.WasteEntry{
				ID: r.ID, RequestID: r.RequestID, InventoryItemID: r.InventoryItemID, MenuItemID: r.MenuItemID, VariantID: r.VariantID,
				ModifierIds: r.ModifierIds, NameEn: r.NameEn, NameNe: r.NameNe, Unit: r.Unit, QuantityMilli: r.QuantityMilli,
				Reason: r.Reason, Note: r.Note, ByUserID: r.ByUserID, ApprovedByUserID: r.ApprovedByUserID, At: r.At, ValuePaisa: r.ValuePaisa,
			},
			byName: r.ByName, approvedByName: r.ApprovedByName,
		})
	}
	return out, nil
}
