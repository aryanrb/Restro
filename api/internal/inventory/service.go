// Package inventory owns stock: inventory items, the stock ledger (movements are
// never edited), waste, blind stock counts and the stock reports. Recipes are saved
// by the catalog (menu) service; order commands call DeductSale / ReverseSale in
// their own transaction (spec: inventory-wastage-design).
package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/audit"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/realtime"
)

type Service struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
	hub  *realtime.Hub
	auth *auth.Service
	log  *zap.Logger
}

func NewService(pool *pgxpool.Pool, hub *realtime.Hub, authSvc *auth.Service, log *zap.Logger) *Service {
	return &Service{pool: pool, q: sqlc.New(pool), hub: hub, auth: authSvc, log: log}
}

func (s *Service) emit(ev realtime.Event) {
	if s.hub != nil {
		s.hub.Broadcast(ev)
	}
}

// --- roles ---

// requireStock admits the roles that work with stock: owner, manager, cashier, kitchen.
func requireStock(ctx context.Context) (*auth.Actor, error) {
	return auth.RequireRole(ctx, auth.RoleOwner, auth.RoleManager, auth.RoleCashier, auth.RoleKitchen)
}

func isAdmin(a *auth.Actor) bool {
	return a.User.Role == auth.RoleOwner || a.User.Role == auth.RoleManager
}

// --- errors ---

func unitLocked() *connecterr.Error {
	e := connecterr.New(connect.CodeFailedPrecondition, restaurantv1.ErrorReason_ERROR_REASON_UNIT_LOCKED, "the unit can't change once the item has stock movements")
	e.Field = "unit"
	return e
}

func itemUntracked(msg string) *connecterr.Error {
	return connecterr.New(connect.CodeFailedPrecondition, restaurantv1.ErrorReason_ERROR_REASON_ITEM_UNTRACKED, msg)
}

func countAlreadyPosted() *connecterr.Error {
	return connecterr.New(connect.CodeFailedPrecondition, restaurantv1.ErrorReason_ERROR_REASON_COUNT_ALREADY_POSTED, "this count is already posted")
}

func parseID(field, s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, connecterr.Invalid(field, "invalid id")
	}
	return id, nil
}

func notFound(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return connecterr.NotFound(what + " not found")
	}
	return connecterr.Internal(err)
}

// --- units ---

func unitStr(u restaurantv1.StockUnit) string {
	switch u {
	case restaurantv1.StockUnit_STOCK_UNIT_G:
		return "g"
	case restaurantv1.StockUnit_STOCK_UNIT_ML:
		return "ml"
	case restaurantv1.StockUnit_STOCK_UNIT_PCS:
		return "pcs"
	}
	return ""
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

// qtyText renders milli as "4,200 g"-ish plain text for audit details: "4200 g", "1.5 g".
func qtyText(milli int64, unit string) string {
	s := fmt.Sprintf("%d", milli/1000)
	if r := abs(milli % 1000); r != 0 {
		s = strings.TrimRight(fmt.Sprintf("%s.%03d", s, r), "0")
		if milli < 0 && milli > -1000 {
			s = "-" + s
		}
	}
	if unit == "" {
		return s
	}
	return s + " " + unit
}

// --- packs ---

type packJSON struct {
	NameEn    string `json:"name_en"`
	NameNe    string `json:"name_ne"`
	SizeMilli int64  `json:"size_milli"`
}

func packsOf(raw []byte) []*restaurantv1.StockPack {
	var ps []packJSON
	_ = json.Unmarshal(raw, &ps)
	out := make([]*restaurantv1.StockPack, 0, len(ps))
	for _, p := range ps {
		out = append(out, &restaurantv1.StockPack{Name: local(p.NameEn, p.NameNe), SizeMilli: p.SizeMilli})
	}
	return out
}

func local(en, ne string) *restaurantv1.LocalText { return &restaurantv1.LocalText{En: en, Ne: ne} }

// --- views ---

func itemView(it sqlc.InventoryItem, admin, unitLocked bool) *restaurantv1.InventoryItem {
	out := &restaurantv1.InventoryItem{
		Id: it.ID.String(), Version: it.Version, Name: local(it.NameEn, it.NameNe), Unit: unitEnum(it.Unit),
		Tracked: it.Tracked, LowStockMilli: it.LowStockMilli, Active: it.Active, Packs: packsOf(it.Packs), Sort: it.Sort,
		OnHandMilli: it.OnHandMilli, Low: live(it) && it.OnHandMilli <= it.LowStockMilli, UnitLocked: unitLocked,
	}
	if admin {
		c := it.UnitCostE4
		out.UnitCostE4 = &c
	}
	return out
}

func (s *Service) itemViewQ(ctx context.Context, q *sqlc.Queries, it sqlc.InventoryItem, admin bool) (*restaurantv1.InventoryItem, error) {
	locked, err := q.InventoryItemHasMovements(ctx, it.ID)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	return itemView(it, admin, locked), nil
}

func unix(t pgtype.Timestamptz) int64 {
	if !t.Valid {
		return 0
	}
	return t.Time.Unix()
}

func pgID(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// --- items ---

func (s *Service) ListInventoryItems(ctx context.Context, includeInactive bool) ([]*restaurantv1.InventoryItem, error) {
	actor, err := requireStock(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.q.ListInventoryItems(ctx, includeInactive)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	moved, err := s.q.InventoryItemIDsWithMovements(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	locked := map[uuid.UUID]bool{}
	for _, id := range moved {
		locked[id] = true
	}
	out := make([]*restaurantv1.InventoryItem, 0, len(items))
	for _, it := range items {
		out = append(out, itemView(it, isAdmin(actor), locked[it.ID]))
	}
	return out, nil
}

func validateItem(in *restaurantv1.InventoryItemInput) (sqlc.UpdateInventoryItemParams, error) {
	p := sqlc.UpdateInventoryItemParams{
		NameEn: strings.TrimSpace(in.GetName().GetEn()), NameNe: strings.TrimSpace(in.GetName().GetNe()),
		Unit: unitStr(in.GetUnit()), LowStockMilli: in.GetLowStockMilli(), Active: in.GetActive(), UnitCostE4: in.GetUnitCostE4(),
	}
	if p.NameEn == "" {
		return p, connecterr.Invalid("name", "English name is required")
	}
	if p.Unit == "" {
		return p, connecterr.Invalid("unit", "choose g, ml or pcs")
	}
	if p.LowStockMilli < 0 {
		return p, connecterr.Invalid("low_stock_milli", "low-stock level can't be negative")
	}
	if p.UnitCostE4 < 0 {
		return p, connecterr.Invalid("unit_cost_e4", "unit cost can't be negative")
	}
	packs := make([]packJSON, 0, len(in.GetPacks()))
	for _, pk := range in.GetPacks() {
		name := strings.TrimSpace(pk.GetName().GetEn())
		if name == "" || pk.GetSizeMilli() <= 0 {
			return p, connecterr.Invalid("packs", "each pack needs a name and a size")
		}
		packs = append(packs, packJSON{NameEn: name, NameNe: strings.TrimSpace(pk.GetName().GetNe()), SizeMilli: pk.GetSizeMilli()})
	}
	p.Packs, _ = json.Marshal(packs)
	return p, nil
}

func (s *Service) SaveInventoryItem(ctx context.Context, req *restaurantv1.SaveInventoryItemRequest) (*restaurantv1.InventoryItem, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	p, err := validateItem(req.GetInput())
	if err != nil {
		return nil, err
	}
	var out *restaurantv1.InventoryItem
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		var saved sqlc.InventoryItem
		var detail string
		if req.GetInput().GetId() == "" {
			var e error
			saved, e = q.CreateInventoryItem(ctx, sqlc.CreateInventoryItemParams{
				NameEn: p.NameEn, NameNe: p.NameNe, Unit: p.Unit, LowStockMilli: p.LowStockMilli, Active: p.Active, UnitCostE4: p.UnitCostE4, Packs: p.Packs,
			})
			if e != nil {
				return connecterr.Internal(e)
			}
			detail = "added (" + saved.Unit + ")"
		} else {
			id, e := parseID("id", req.GetInput().GetId())
			if e != nil {
				return e
			}
			if req.BaseVersion == nil {
				return connecterr.Invalid("base_version", "base_version is required when editing")
			}
			cur, e := q.GetInventoryItemForUpdate(ctx, id)
			if e != nil {
				return notFound(e, "stock item")
			}
			if cur.Version != req.GetBaseVersion() {
				return connecterr.StaleVersion("this stock item was changed by someone else; reload")
			}
			if cur.Unit != p.Unit {
				moved, me := q.InventoryItemHasMovements(ctx, id)
				if me != nil {
					return connecterr.Internal(me)
				}
				if moved {
					return unitLocked()
				}
			}
			p.ID, p.Version = id, cur.Version
			saved, e = q.UpdateInventoryItem(ctx, p)
			if errors.Is(e, pgx.ErrNoRows) {
				return connecterr.StaleVersion("this stock item was changed by someone else; reload")
			}
			if e != nil {
				return connecterr.Internal(e)
			}
			if cur.UnitCostE4 != saved.UnitCostE4 {
				detail = fmt.Sprintf("unit cost %s → %s paisa/%s", costText(cur.UnitCostE4), costText(saved.UnitCostE4), saved.Unit)
			}
		}
		if e := audit.Write(ctx, q, audit.Entry{
			ActorID: actor.User.ID, Action: "INVENTORY_ITEM_CHANGED", Entity: "inventory_item", EntityID: saved.ID.String(),
			Reason: strings.TrimSpace(saved.NameEn + " " + detail), After: audit.JSON(saved),
		}); e != nil {
			return e
		}
		var e error
		out, e = s.itemViewQ(ctx, q, saved, true)
		return e
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "inventory.updated", ID: out.Id, Version: out.Version})
	return out, nil
}

// costText renders an e4 cost as "60.0000".
func costText(e4 int64) string { return fmt.Sprintf("%d.%04d", e4/10000, e4%10000) }

func (s *Service) SetTracked(ctx context.Context, req *restaurantv1.SetTrackedRequest) (*restaurantv1.InventoryItem, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID("id", req.GetId())
	if err != nil {
		return nil, err
	}
	var out *restaurantv1.InventoryItem
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		item, e := q.GetInventoryItemForUpdate(ctx, id)
		if e != nil {
			return notFound(e, "stock item")
		}
		if item.Tracked == req.GetTracked() {
			out, e = s.itemViewQ(ctx, q, item, true)
			return e
		}
		detail := "off"
		if req.GetTracked() {
			if req.OpeningMilli == nil || req.GetOpeningMilli() < 0 {
				return connecterr.Invalid("opening_milli", "count what is on hand to start tracking")
			}
			if item, e = q.SetInventoryTracked(ctx, sqlc.SetInventoryTrackedParams{ID: id, Tracked: true}); e != nil {
				return connecterr.Internal(e)
			}
			if e := record(ctx, q, []move{{
				itemID: id, qtyMilli: req.GetOpeningMilli() - item.OnHandMilli, typ: TypeOpening, ref: id,
				by: actor.User.ID, at: now(), costE4: item.UnitCostE4,
			}}); e != nil {
				return e
			}
			if item, e = q.GetInventoryItem(ctx, id); e != nil {
				return connecterr.Internal(e)
			}
			detail = "on, opening " + qtyText(req.GetOpeningMilli(), item.Unit)
		} else if item, e = q.SetInventoryTracked(ctx, sqlc.SetInventoryTrackedParams{ID: id, Tracked: false}); e != nil {
			return connecterr.Internal(e)
		}
		if e := audit.Write(ctx, q, audit.Entry{
			ActorID: actor.User.ID, Action: "TRACKING_CHANGED", Entity: "inventory_item", EntityID: id.String(), Reason: item.NameEn + " " + detail,
		}); e != nil {
			return e
		}
		out, e = s.itemViewQ(ctx, q, item, true)
		return e
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "inventory.updated", ID: out.Id, Version: out.Version})
	return out, nil
}

func movementTypeStr(t restaurantv1.StockMovementType) string {
	switch t {
	case restaurantv1.StockMovementType_STOCK_MOVEMENT_TYPE_SALE:
		return TypeSale
	case restaurantv1.StockMovementType_STOCK_MOVEMENT_TYPE_SALE_REVERSAL:
		return TypeSaleReversal
	case restaurantv1.StockMovementType_STOCK_MOVEMENT_TYPE_WASTE:
		return TypeWaste
	case restaurantv1.StockMovementType_STOCK_MOVEMENT_TYPE_COUNT_ADJUST:
		return TypeCountAdjust
	case restaurantv1.StockMovementType_STOCK_MOVEMENT_TYPE_PURCHASE:
		return TypePurchase
	case restaurantv1.StockMovementType_STOCK_MOVEMENT_TYPE_PURCHASE_RETURN:
		return TypePurchaseReturn
	case restaurantv1.StockMovementType_STOCK_MOVEMENT_TYPE_OPENING:
		return TypeOpening
	}
	return ""
}

func movementTypeEnum(s string) restaurantv1.StockMovementType {
	for t := range restaurantv1.StockMovementType_name {
		if e := restaurantv1.StockMovementType(t); movementTypeStr(e) == s && s != "" {
			return e
		}
	}
	return restaurantv1.StockMovementType_STOCK_MOVEMENT_TYPE_UNSPECIFIED
}

// rangeTs turns unix bounds into timestamps; 0 = unbounded.
func rangeTs(fromUnix, toUnix int64) (pgtype.Timestamptz, pgtype.Timestamptz) {
	var from, to pgtype.Timestamptz
	if fromUnix != 0 {
		from = ts(time.Unix(fromUnix, 0).UTC())
	}
	if toUnix != 0 {
		to = ts(time.Unix(toUnix, 0).UTC())
	}
	return from, to
}

// closedRange is rangeTs with the open ends filled in.
func closedRange(fromUnix, toUnix int64) (time.Time, time.Time) {
	from, to := time.Unix(0, 0).UTC(), time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	if fromUnix != 0 {
		from = time.Unix(fromUnix, 0).UTC()
	}
	if toUnix != 0 {
		to = time.Unix(toUnix, 0).UTC()
	}
	return from, to
}

func (s *Service) ListMovements(ctx context.Context, req *restaurantv1.ListMovementsRequest) ([]*restaurantv1.StockMovement, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	id, err := parseID("inventory_item_id", req.GetInventoryItemId())
	if err != nil {
		return nil, err
	}
	from, to := rangeTs(req.GetFromUnix(), req.GetToUnix())
	rows, err := s.q.ListMovements(ctx, sqlc.ListMovementsParams{InventoryItemID: id, FromTs: from, ToTs: to, Type: movementTypeStr(req.GetType())})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := make([]*restaurantv1.StockMovement, 0, len(rows))
	for _, m := range rows {
		out = append(out, &restaurantv1.StockMovement{
			Id: m.ID.String(), InventoryItemId: m.InventoryItemID.String(), QuantityMilli: m.QuantityMilli, Type: movementTypeEnum(m.Type),
			RefId: m.RefID.String(), Reason: m.Reason, ByUserId: pgID(m.ByUserID), ByName: m.ByName, AtUnix: m.At.Unix(), UnitCostE4: m.UnitCostE4,
		})
	}
	return out, nil
}

// userNames maps user ids to names.
func userNames(ctx context.Context, q *sqlc.Queries, ids ...uuid.UUID) (map[uuid.UUID]string, error) {
	rows, err := q.ListUserNames(ctx, ids)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := map[uuid.UUID]string{}
	for _, r := range rows {
		out[r.ID] = r.Name
	}
	return out, nil
}
