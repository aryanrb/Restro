// Package kitchen is the KDS domain: per-station ticket board and item status
// actions. Online-only (no offline queue). 86-ing reuses MenuService.Set86.
package kitchen

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
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
	log  *zap.Logger
}

func NewService(pool *pgxpool.Pool, hub *realtime.Hub, log *zap.Logger) *Service {
	return &Service{pool: pool, q: sqlc.New(pool), hub: hub, log: log}
}

// requireKitchenRole admits kitchen staff plus owners/managers (who may cover the
// pass); waiters and cashiers cannot drive the KDS.
func requireKitchenRole(ctx context.Context) (*auth.Actor, error) {
	return auth.RequireRole(ctx, auth.RoleOwner, auth.RoleManager, auth.RoleKitchen)
}

func (s *Service) emit(ev realtime.Event) {
	if s.hub != nil {
		s.hub.Broadcast(ev)
	}
}

func (s *Service) ListStationTickets(ctx context.Context, stationID string) ([]*restaurantv1.Ticket, error) {
	if _, err := requireKitchenRole(ctx); err != nil {
		return nil, err
	}
	sid, err := parseID("station_id", stationID)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListStationOpenItems(ctx, sid)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	ids := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	mods, err := s.q.ListModifiersByItemIDs(ctx, ids)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	modsByItem := map[uuid.UUID][]sqlc.OrderItemModifier{}
	for _, m := range mods {
		modsByItem[m.OrderItemID] = append(modsByItem[m.OrderItemID], m)
	}

	idx := map[uuid.UUID]*restaurantv1.Ticket{}
	var tickets []*restaurantv1.Ticket
	for _, r := range rows {
		t, ok := idx[r.OrderID]
		if !ok {
			t = &restaurantv1.Ticket{
				OrderId: r.OrderID.String(), OrderClientId: r.OrderClientID.String(),
				Type: orderTypeEnum(r.OrderType), TableName: r.TableName, CreatedAtUnix: r.CreatedAt.Unix(),
			}
			idx[r.OrderID] = t
			tickets = append(tickets, t)
		}
		ti := &restaurantv1.TicketItem{
			Id: r.ID.String(), Name: local(r.NameEn, r.NameNe), Quantity: r.Quantity, Note: r.Note, Status: itemStatusEnum(r.Status),
		}
		if r.HasVariant {
			ti.VariantName = local(r.VariantNameEn, r.VariantNameNe)
		}
		for _, m := range modsByItem[r.ID] {
			ti.Modifiers = append(ti.Modifiers, local(m.NameEn, m.NameNe))
		}
		t.Items = append(t.Items, ti)
	}
	return tickets, nil
}

func (s *Service) StartItems(ctx context.Context, itemIDs []string) error {
	return s.transition(ctx, itemIDs, "PREPARING", []string{"SENT"}, false)
}

func (s *Service) MarkItemsReady(ctx context.Context, itemIDs []string) error {
	return s.transition(ctx, itemIDs, "READY", []string{"SENT", "PREPARING"}, true)
}

// transition applies a KDS status change to the given items, bumps the affected
// orders, and emits order.updated (+ item.ready when notifyWaiter).
func (s *Service) transition(ctx context.Context, itemIDs []string, to string, from []string, notifyWaiter bool) error {
	if _, err := requireKitchenRole(ctx); err != nil {
		return err
	}
	ids, err := parseIDs("item_ids", itemIDs)
	if err != nil {
		return err
	}
	var orderIDs []string
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		if e := q.KdsSetItemsStatus(ctx, sqlc.KdsSetItemsStatusParams{Status: to, Ids: ids, FromStatus: from}); e != nil {
			return connecterr.Internal(e)
		}
		items, e := q.ListOrderItemsByIDs(ctx, ids)
		if e != nil {
			return connecterr.Internal(e)
		}
		seen := map[uuid.UUID]bool{}
		for _, it := range items {
			if !seen[it.OrderID] {
				seen[it.OrderID] = true
				if _, e := q.BumpOrderVersion(ctx, it.OrderID); e != nil {
					return connecterr.Internal(e)
				}
				orderIDs = append(orderIDs, it.OrderID.String())
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if notifyWaiter {
		for _, id := range itemIDs {
			s.emit(realtime.Event{Type: "item.ready", ID: id})
		}
	}
	for _, oid := range orderIDs {
		s.emit(realtime.Event{Type: "order.updated", ID: oid})
	}
	return nil
}

func (s *Service) BumpTicket(ctx context.Context, orderID, stationID string) error {
	if _, err := requireKitchenRole(ctx); err != nil {
		return err
	}
	oid, err := parseID("order_id", orderID)
	if err != nil {
		return err
	}
	sid, err := parseID("station_id", stationID)
	if err != nil {
		return err
	}
	var readied []uuid.UUID
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		r, e := q.BumpStationTicket(ctx, sqlc.BumpStationTicketParams{OrderID: oid, StationID: sid})
		if e != nil {
			return connecterr.Internal(e)
		}
		readied = r
		if _, e := q.BumpOrderVersion(ctx, oid); e != nil {
			return connecterr.Internal(e)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, id := range readied {
		s.emit(realtime.Event{Type: "item.ready", ID: id.String()})
	}
	s.emit(realtime.Event{Type: "order.updated", ID: orderID})
	return nil
}

// --- local helpers (kept here so the package is self-contained) ---

func local(en, ne string) *restaurantv1.LocalText { return &restaurantv1.LocalText{En: en, Ne: ne} }

func parseID(field, s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, connecterr.Invalid(field, "invalid id")
	}
	return id, nil
}

func parseIDs(field string, ss []string) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(ss))
	for _, s := range ss {
		id, err := parseID(field, s)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

func orderTypeEnum(t string) restaurantv1.OrderType {
	if t == "TAKEAWAY" {
		return restaurantv1.OrderType_ORDER_TYPE_TAKEAWAY
	}
	return restaurantv1.OrderType_ORDER_TYPE_DINE_IN
}

func itemStatusEnum(s string) restaurantv1.OrderItemStatus {
	switch s {
	case "SENT":
		return restaurantv1.OrderItemStatus_ORDER_ITEM_STATUS_SENT
	case "PREPARING":
		return restaurantv1.OrderItemStatus_ORDER_ITEM_STATUS_PREPARING
	case "READY":
		return restaurantv1.OrderItemStatus_ORDER_ITEM_STATUS_READY
	default:
		return restaurantv1.OrderItemStatus_ORDER_ITEM_STATUS_SENT
	}
}
