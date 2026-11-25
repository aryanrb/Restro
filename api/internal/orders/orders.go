// Package orders owns dine-in/takeaway ordering: idempotent commands, append-only
// snapshotted items, 86 partial-accept, voids with manager approval, transfer/merge.
package orders

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/idem"
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

// requireOrderRole admits the front-of-house roles that take and manage orders.
// KITCHEN staff work tickets through the kitchen service, not orders.
func requireOrderRole(ctx context.Context) (*auth.Actor, error) {
	return auth.RequireRole(ctx, auth.RoleOwner, auth.RoleManager, auth.RoleWaiter, auth.RoleCashier)
}

// requireOpen rejects commands against an order that is no longer OPEN
// (BILLED/CLOSED/CANCELLED) with ORDER_CLOSED.
func requireOpen(o sqlc.Order) error {
	if o.Status != "OPEN" {
		return connecterr.OrderClosed("order is not open")
	}
	return nil
}

// requireServable allows MarkServed on OPEN and BILLED orders: a paid order whose food
// isn't handed over yet (pay-first counter, bill before the last dish) must still be servable.
func requireServable(o sqlc.Order) error {
	if o.Status != "OPEN" && o.Status != "BILLED" {
		return connecterr.OrderClosed("order is closed")
	}
	return nil
}

func (s *Service) emit(ev realtime.Event) {
	if s.hub != nil {
		s.hub.Broadcast(ev)
	}
}

// runIdempotent delegates to the shared idem package (kept as a method so the
// command code reads cleanly).
func (s *Service) runIdempotent(ctx context.Context, commandID, userID uuid.UUID, out proto.Message, fn func(q *sqlc.Queries) error) error {
	return idem.Run(ctx, s.pool, s.q, commandID, userID, out, fn)
}

// checkUser enforces the WRONG USER rule: a command that names the user who
// queued it (user_id) only runs under that user's session. There is no
// WRONG_USER reason in the contract yet, so it is NOT_ALLOWED with field
// "user_id". Callers run it before the idempotent replay so a replay under
// another session is refused too.
func checkUser(actor *auth.Actor, userID *string) error {
	if userID == nil || *userID == "" {
		return nil
	}
	if id, err := uuid.Parse(*userID); err == nil && id == actor.User.ID {
		return nil
	}
	e := connecterr.NotAllowed("this command was queued by a different user; sign in as that user to send it")
	e.Field = "user_id"
	return e
}

// assembleOrder loads an order with its items and their modifier snapshots.
func (s *Service) assembleOrder(ctx context.Context, q *sqlc.Queries, orderID uuid.UUID) (*restaurantv1.Order, error) {
	return AssembleOrder(ctx, q, orderID)
}

// AssembleOrder loads an order with its items, their modifier snapshots and the
// waiter's name, as the proto Order. Exported for the kitchen and billing
// services, which return the updated order.
func AssembleOrder(ctx context.Context, q *sqlc.Queries, orderID uuid.UUID) (*restaurantv1.Order, error) {
	o, err := q.GetOrderByID(ctx, orderID)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	items, err := q.ListOrderItems(ctx, orderID)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	mods, err := q.ListOrderItemModifiersForOrder(ctx, orderID)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	byItem := map[uuid.UUID][]sqlc.OrderItemModifier{}
	for _, m := range mods {
		byItem[m.OrderItemID] = append(byItem[m.OrderItemID], m)
	}
	waiter, err := q.GetUserName(ctx, o.WaiterUserID)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := toOrder(o, items, byItem)
	out.WaiterName = waiter
	return out, nil
}

// --- converters ---

func local(en, ne string) *restaurantv1.LocalText { return &restaurantv1.LocalText{En: en, Ne: ne} }

func pgUUIDStr(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

func toOrder(o sqlc.Order, items []sqlc.OrderItem, mods map[uuid.UUID][]sqlc.OrderItemModifier) *restaurantv1.Order {
	out := &restaurantv1.Order{
		Id: o.ID.String(), ClientId: o.ClientID.String(), Type: orderTypeEnum(o.Type),
		WaiterUserId: o.WaiterUserID.String(), Status: orderStatusEnum(o.Status), GuestCount: o.GuestCount, Version: o.Version,
		Label: o.Label, CreatedAtUnix: o.CreatedAt.Unix(),
	}
	if o.TableID.Valid {
		s := pgUUIDStr(o.TableID)
		out.TableId = &s
	}
	for _, it := range items {
		out.Items = append(out.Items, toOrderItem(it, mods[it.ID]))
	}
	return out
}

func toOrderItem(it sqlc.OrderItem, mods []sqlc.OrderItemModifier) *restaurantv1.OrderItem {
	oi := &restaurantv1.OrderItem{
		Id: it.ID.String(), ClientId: it.ClientID.String(), MenuItemId: it.MenuItemID.String(), Name: local(it.NameEn, it.NameNe),
		UnitPricePaisa: it.UnitPricePaisa, Quantity: it.Quantity, Note: it.Note, StationId: it.StationID.String(),
		Status: itemStatusEnum(it.Status), VoidReason: it.VoidReason,
	}
	if it.HasVariant {
		oi.VariantName = local(it.VariantNameEn, it.VariantNameNe)
	}
	for _, m := range mods {
		oi.Modifiers = append(oi.Modifiers, &restaurantv1.OrderItemModifier{Name: local(m.NameEn, m.NameNe), PriceDeltaPaisa: m.PriceDeltaPaisa})
	}
	if it.VoidedByUserID.Valid {
		oi.VoidedByUserId = pgUUIDStr(it.VoidedByUserID)
	}
	if it.VoidApprovedByUserID.Valid {
		oi.VoidApprovedByUserId = pgUUIDStr(it.VoidApprovedByUserID)
	}
	if it.SentAt.Valid {
		oi.SentAtUnix = it.SentAt.Time.Unix()
	}
	oi.AddedByUserId = pgUUIDStr(it.AddedByUserID)
	oi.InvoiceId = pgUUIDStr(it.InvoiceID)
	oi.Billed = it.InvoiceID.Valid
	return oi
}

func orderTypeEnum(t string) restaurantv1.OrderType {
	if t == "TAKEAWAY" {
		return restaurantv1.OrderType_ORDER_TYPE_TAKEAWAY
	}
	return restaurantv1.OrderType_ORDER_TYPE_DINE_IN
}

func orderStatusEnum(s string) restaurantv1.OrderStatus {
	switch s {
	case "BILLED":
		return restaurantv1.OrderStatus_ORDER_STATUS_BILLED
	case "CLOSED":
		return restaurantv1.OrderStatus_ORDER_STATUS_CLOSED
	case "CANCELLED":
		return restaurantv1.OrderStatus_ORDER_STATUS_CANCELLED
	default:
		return restaurantv1.OrderStatus_ORDER_STATUS_OPEN
	}
}

func itemStatusEnum(s string) restaurantv1.OrderItemStatus {
	switch s {
	case "SENT":
		return restaurantv1.OrderItemStatus_ORDER_ITEM_STATUS_SENT
	case "PREPARING":
		return restaurantv1.OrderItemStatus_ORDER_ITEM_STATUS_PREPARING
	case "READY":
		return restaurantv1.OrderItemStatus_ORDER_ITEM_STATUS_READY
	case "SERVED":
		return restaurantv1.OrderItemStatus_ORDER_ITEM_STATUS_SERVED
	case "VOIDED":
		return restaurantv1.OrderItemStatus_ORDER_ITEM_STATUS_VOIDED
	default:
		return restaurantv1.OrderItemStatus_ORDER_ITEM_STATUS_DRAFT
	}
}
